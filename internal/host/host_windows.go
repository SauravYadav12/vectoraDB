//go:build windows

// SPDX-License-Identifier: AGPL-3.0-or-later

package host

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/vectoradb/vectoradb/internal/version"
)

// hostSetup is the Windows bootstrap: ensure a ZFS-capable WSL2 distro and bring
// the stack up.
func hostSetup() error { return setupWindows() }

// currentDistro resolves the WSL2 distro name: an explicit override, else the
// dedicated "vectoradb" distro.
func currentDistro() string { return resolveWSLDistro(os.Getenv("VECTORADB_WSL_DISTRO")) }

func wslInstalled() bool {
	_, err := exec.LookPath("wsl.exe")
	return err == nil
}

func distroExists(name string) bool {
	out, err := exec.Command("wsl.exe", "--list", "--quiet").Output()
	if err != nil {
		return false
	}
	for _, l := range strings.Fields(decodeWSLOutput(out)) {
		if strings.EqualFold(l, name) {
			return true
		}
	}
	return false
}

func distroRunning(name string) bool {
	out, err := exec.Command("wsl.exe", "--list", "--verbose").Output()
	if err != nil {
		return false
	}
	for _, d := range decodeWSLList(out) {
		if strings.EqualFold(d.Name, name) {
			return strings.EqualFold(d.State, "Running")
		}
	}
	return false
}

// guestBin resolves the engine binary path inside the distro.
func guestBin(name string) string {
	if v := strings.TrimSpace(os.Getenv("VECTORADB_GUEST_BIN")); v != "" {
		return v
	}
	out, err := exec.Command("wsl.exe", "-d", name, "--",
		"sh", "-c", "command -v vdb || echo /tmp/vdb").Output()
	if err == nil {
		if p := strings.TrimSpace(decodeWSLOutput(out)); p != "" {
			return p
		}
	}
	return "/tmp/vdb"
}

func forward(args []string) error { return forwardStdin(args, os.Stdin) }

func forwardStdin(args []string, stdin io.Reader) error {
	if !wslInstalled() {
		return fmt.Errorf("WSL is required on Windows. Install it with `wsl --install` (admin, then reboot), then run `vdb setup`")
	}
	name := currentDistro()
	if !distroExists(name) {
		return fmt.Errorf("no VectoraDB WSL distro yet — run `vdb setup` once to create it")
	}
	// `wsl.exe -d <name> -- …` starts a stopped distro on demand but returns as
	// soon as the command can run — well before systemd has brought Docker up.
	// WSL stops idle distros, so without this wait the first command after an
	// idle timeout or a reboot fails with "cannot reach the Docker daemon".
	// The macOS path has the same guard; there `limactl start` blocks for us.
	if !distroRunning(name) {
		fmt.Printf("Starting the VectoraDB distro (%s)…\n", name)
		if err := waitForSystemd(name); err != nil {
			return err
		}
		// Only on a cold start: the pool unit runs at boot, and if it could not
		// bring the pool up the engine must not proceed to create a new one.
		if err := checkZpoolUnit(name); err != nil {
			return err
		}
	}
	guest := guestBin(name)
	cmd := exec.Command("wsl.exe", wslArgs(name, guest, guestEnv(), args)...)
	cmd.Stdin = stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// forwardQuiet forwards a command with its output logged rather than printed.
//
// Only setup uses it. The engine's own `vdb start` is worth watching when a user
// types it, but during an install the image build and registry pulls are several
// hundred lines that bury the progress the user actually wants to see.
func forwardQuiet(args []string) error {
	name := currentDistro()
	guest := guestBin(name)
	out, err := exec.Command("wsl.exe", wslArgs(name, guest, guestEnv(), args)...).CombinedOutput()
	text := decodeWSLOutput(out)
	logf("$ vdb %s\n%s\n", strings.Join(args, " "), text)
	if err != nil {
		return fmt.Errorf("%w\n%s", err, lastLines(text, 20))
	}
	return nil
}

// wsl runs an interactive wsl.exe command wired to the console.
func wsl(args ...string) *exec.Cmd {
	cmd := exec.Command("wsl.exe", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd
}

// wslQuiet runs a wsl.exe management command with its output logged rather than
// printed. wsl.exe narrates routine success ("The operation completed
// successfully.") in UTF-16, which is noise during setup and renders badly.
func wslQuiet(args ...string) error {
	out, err := exec.Command("wsl.exe", args...).CombinedOutput()
	text := decodeWSLOutput(out)
	logf("$ wsl %s\n%s\n", strings.Join(args, " "), text)
	if err != nil {
		return fmt.Errorf("%w\n%s", err, lastLines(text, 10))
	}
	return nil
}

// guestPath prefixes setup scripts that use the ZFS userland. It installs under
// /usr/local, which is on Debian's default root PATH and sudo secure_path — but
// the environment `wsl.exe -- sh -c` inherits is not guaranteed to be either, so
// the setup scripts state it rather than assume it.
const guestPath = "export PATH=/usr/local/sbin:/usr/local/bin:$PATH"

// setupLog is the transcript of everything setup runs in the guest. apt and
// docker are extremely chatty, and streaming them made a working install look
// alarming and a broken one impossible to read. The detail goes here instead,
// and is surfaced on failure.
func setupLogPath() string { return filepath.Join(installDir(), "install.log") }

// logf appends to the setup log. Logging is best-effort: failing to write a log
// must never fail an install.
func logf(format string, args ...any) {
	f, err := os.OpenFile(setupLogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, format, args...)
}

// step announces a stage before running it, so a stall is attributable to a
// named step rather than to silence.
func step(msg string) {
	fmt.Printf("  %s…\n", msg)
	logf("\n=== %s ===\n", msg)
}

// wslRoot runs a shell command inside the distro as root (setup steps need
// privilege without a sudo password prompt), capturing its output to the setup
// log rather than the console.
//
// On failure the captured tail is included in the error, so the detail appears
// exactly when it is wanted and nowhere else.
func wslRoot(name, script string) error {
	cmd := exec.Command("wsl.exe", "-d", name, "-u", "root", "--", "sh", "-c", script)
	out, err := cmd.CombinedOutput()
	text := decodeWSLOutput(out)
	logf("$ %s\n%s\n", script, text)
	if err != nil {
		return fmt.Errorf("%w\n%s", err, lastLines(text, 15))
	}
	return nil
}

// lastLines returns the final n non-empty lines, the part of a failed command's
// output that actually says what went wrong.
func lastLines(s string, n int) string {
	var keep []string
	for _, ln := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(ln) != "" {
			keep = append(keep, strings.TrimRight(ln, "\r"))
		}
	}
	if len(keep) > n {
		keep = keep[len(keep)-n:]
	}
	return strings.Join(keep, "\n")
}

// wslRootOut is wslRoot but captures stdout, for probes.
func wslRootOut(name, script string) ([]byte, error) {
	return exec.Command("wsl.exe", "-d", name, "-u", "root", "--", "sh", "-c", script).Output()
}

func setupWindows() error {
	if !wslInstalled() {
		return fmt.Errorf("WSL is not installed.\n" +
			"Install it (Administrator PowerShell):\n  wsl --install\n" +
			"reboot, then run `vdb setup` again")
	}
	// A quick health probe; `--status` fails if the WSL platform isn't enabled.
	if err := exec.Command("wsl.exe", "--status").Run(); err != nil {
		return fmt.Errorf("WSL is present but not healthy (is virtualization enabled in the BIOS, " +
			"and the 'Virtual Machine Platform' feature on?). Run `wsl --install` / `wsl --update`, then retry")
	}

	fmt.Println("Setting up VectoraDB…")
	logf("\n---- vdb setup %s ----\n", version.Version)

	name := currentDistro()
	if distroExists(name) {
		step(fmt.Sprintf("Reusing the existing %q distro", name))
	} else if err := importDistro(name); err != nil {
		return err
	}
	// Both paths land here, so both wait: WSL starts a distro on demand and
	// systemd needs a moment after that, whether it was just imported or merely
	// stopped. Every systemctl call below depends on this.
	if err := waitForSystemd(name); err != nil {
		return err
	}
	// Provisioning runs on every setup, not only after a fresh import: a setup
	// interrupted partway leaves the distro existing but incomplete, and `vdb
	// setup` is the command a user re-runs to repair that. Each step below
	// checks its own work first, so a healthy distro costs a few probes.
	if err := provisionGuestWSL(name); err != nil {
		return err
	}
	// Re-checked every time for the same reason, plus one of its own: a WSL
	// update can move the kernel out from under an already-provisioned distro.
	if err := verifyZFS(name); err != nil {
		return err
	}
	step("Starting VectoraDB")
	err := forwardQuiet([]string{"start"})
	if err == nil {
		return finishSetup(name)
	}
	// One retry, because the very first start races the Postgres container's
	// initdb: the engine connects as soon as the socket appears, but the
	// entrypoint's temporary server is still creating the database, so psql gets
	// `database "vectoradb" does not exist`. initdb is slow enough on a fresh
	// ZFS pool to lose that race reliably here, and `start` is idempotent — by
	// the retry the container is initialised and it succeeds.
	//
	// This compensates for an engine-side readiness check; if that gains a
	// proper wait, drop this.
	step("Retrying (the first start raced container initialisation)")
	if err := forwardQuiet([]string{"start"}); err != nil {
		return err
	}
	return finishSetup(name)
}

// finishSetup completes a successful setup and tells the user what they have.
//
// The engine's own start banner is captured during setup, so without this the
// install would end in silence.
func finishSetup(name string) error {
	if err := shareMountPropagation(name); err != nil {
		return err
	}
	fmt.Println()
	fmt.Println("VectoraDB is running.")
	fmt.Println("  Try:      vdb status")
	fmt.Println("  Connect:  postgres://vectoradb@localhost:6432/main")
	fmt.Println("  Log:      " + setupLogPath())
	return nil
}

// shareMountPropagation puts the pool's mounts under shared propagation, once
// the pool exists.
//
// vectoradb-zpool.service does this on every boot, but on the very first run the
// pool is created by `vdb start` after that unit has already run. Without it the
// first session's mounts stay private, and any sandboxed systemd service that
// starts afterwards pins them — see the note on finish() in zpoolUpScript.
func shareMountPropagation(name string) error {
	// Best-effort: a failure here costs a stale mount, not correctness, and the
	// next boot fixes it.
	_ = wslRoot(name, "mount --make-rshared /vectoradb 2>/dev/null || true")
	return nil
}

// importDistro creates the dedicated distro from the bundled Ubuntu rootfs and
// enables systemd inside it.
func importDistro(name string) error {
	rootfs := bundledRootfs()
	if rootfs == "" {
		return fmt.Errorf("the Ubuntu rootfs was not found next to vdb.exe — reinstall with install.ps1")
	}
	installDir := filepath.Join(os.Getenv("LOCALAPPDATA"), "vectoradb", "wsl")
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		return err
	}
	step(fmt.Sprintf("Creating the %q WSL distro", name))
	if err := wslQuiet("--import", name, installDir, rootfs); err != nil {
		return fmt.Errorf("importing the WSL distro: %w", err)
	}
	// systemd for `systemctl enable --now docker` and the ZFS import units;
	// root as the default user so the engine's `sudo` never prompts.
	//
	// appendWindowsPath=false keeps the Windows PATH out of this distro. It is a
	// dedicated engine distro that needs nothing from Windows, and leaving it on
	// means `docker` resolves to Docker Desktop's docker.exe on machines that
	// have it — which silently shadows the Linux Docker the engine must talk to.
	conf := `printf '[boot]\nsystemd=true\n\n[user]\ndefault=root\n\n[interop]\nappendWindowsPath=false\n' > /etc/wsl.conf`
	if err := wslRoot(name, conf); err != nil {
		return fmt.Errorf("configuring the distro: %w", err)
	}
	// The distro must restart to pick up /etc/wsl.conf. The caller waits for
	// systemd to come back, on this path and on the already-exists path alike.
	if err := wslQuiet("--terminate", name); err != nil {
		return fmt.Errorf("restarting the distro to apply systemd: %w", err)
	}
	return nil
}

// waitForSystemd blocks until the distro's systemd has finished booting. The
// distro is restarted right before this to pick up /etc/wsl.conf, and systemctl
// calls issued during that window fail with "system has not been booted".
func waitForSystemd(name string) error {
	// Announced only if it actually takes a moment: on a warm distro systemd is
	// already up and a progress line for a no-op is just noise.
	announced := false
	for i := 0; i < 60; i++ {
		out, _ := wslRootOut(name, "systemctl is-system-running 2>&1 || true")
		switch strings.TrimSpace(decodeWSLOutput(out)) {
		case "running", "degraded":
			return nil
		}
		if !announced {
			step("Waiting for the distro to finish booting")
			announced = true
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("systemd did not finish booting in the %q distro after 60s — "+
		"try `wsl --terminate %s` and re-run `vdb setup`", name, name)
}

// provisionGuestWSL installs Docker + ZFS in the distro and installs the engine
// binary — mirroring provisionGuest on macOS. Every step is idempotent so this
// doubles as the repair path for an interrupted setup.
func provisionGuestWSL(name string) error {
	if err := installDocker(name); err != nil {
		return err
	}
	if err := installZFS(name); err != nil {
		return err
	}
	if err := ensureZpoolDevice(name); err != nil {
		return err
	}
	if err := loadPreloadedImages(name); err != nil {
		return err
	}
	if err := stageImageContext(name); err != nil {
		return err
	}
	return installGuestBinaryWSL(name)
}

// loadPreloadedImages imports the container images baked into the distro image.
//
// The prebuilt distro ships them as a docker save tarball rather than a
// populated layer store, because building that store would mean running a second
// dockerd inside a chroot on the builder. Loading here is local disk work and
// replaces a docker build plus three registry pulls — the slowest and least
// reliable part of a first start.
//
// A plain Ubuntu rootfs has no tarball, in which case this is a no-op and the
// engine builds and pulls as before.
func loadPreloadedImages(name string) error {
	const tar = "/usr/local/share/vectoradb/images/vectoradb-images.tar"
	if wslRoot(name, fmt.Sprintf("test -f %q", tar)) != nil {
		return nil
	}
	// Already loaded (a re-run): the engine's own check is `docker image
	// inspect`, so match it rather than guessing from the tarball's presence.
	if wslRoot(name, "docker image inspect vectoradb/postgres-walg:16 >/dev/null 2>&1") == nil {
		return nil
	}
	step("Loading the preinstalled container images")
	if err := wslRoot(name, fmt.Sprintf("set -e; docker load -i %q", tar)); err != nil {
		// Not fatal: the engine can still build and pull. Losing the fast path
		// is better than failing an install over it.
		logf("loading preinstalled images failed, falling back to build/pull: %v\n", err)
	}
	return nil
}

// zpoolUpScript attaches the pool image to a fixed loop device and imports the
// pool, at every distro boot.
//
// It exists because two engine assumptions don't hold here. First, a file vdev
// cannot back a pool on the WSL2 kernel, so the image needs a loop device.
// Second, the engine never imports: `ensurePool` falls through to
// `zpool create -f` when it sees no pool, which would silently destroy an
// existing one — and WSL stops idle distros, so that reboot happens routinely.
// ZFS's own zfs-import-cache.service cannot cover this, because no zpool.cache
// is written for a loop-backed pool.
//
// Hence the interlock at the end: if the image already carries a pool that would
// not import, the loop device is detached again, so the engine finds no vdev and
// fails loudly instead of recreating the pool over live data.
const zpoolUpScript = `#!/bin/sh
# Managed by vdb setup. Attaches the VectoraDB pool image to a loop device and
# imports the pool before the engine runs.
#
# Two rules keep this safe:
#   - Never detach a loop device. Pulling one out from under an imported pool
#     suspends its I/O, and every later operation fails with "pool I/O is
#     currently suspended".
#   - Use "if cmd; then" rather than "cmd && exit 0". Under set -e a failing
#     && list aborts the script, so the latter kills this unit on a fresh
#     machine, exactly when the pool legitimately does not exist yet.
set -e
export PATH=/usr/local/sbin:/usr/local/bin:$PATH
IMG=/var/lib/vectoradb-zpool.img
DEV=/dev/vectoradb-pool
SIZE="${VECTORADB_ZPOOL_SIZE:-30G}"
MODSRC=/usr/local/lib/vectoradb/modules

# Mount the datasets and put them under shared propagation.
#
# Sandboxed systemd services (ProtectSystem=strict, e.g. systemd-timedated)
# clone the mount tree into a private namespace at start-up, so they hold a
# read-only copy of every branch dataset that existed then. Unmounting only
# propagates to those copies when the mount is shared — and WSL, unlike a normal
# systemd boot, leaves / private, so nothing is shared by default. The copies
# then pin the dataset forever and "vdb branch delete" fails with
# "dataset is busy" for any branch that existed at boot.
#
# Making these mounts shared restores the propagation a normal Linux system
# already has. Order matters: mount first, then share, and do both before
# docker.service and the sandboxed services capture the tree.
finish() {
	zfs mount -a 2>/dev/null || true
	mount --make-rshared /vectoradb 2>/dev/null || true
}

# Put the ZFS modules back before anything needs them.
#
# /usr/lib/modules/<rel> is an overlay whose upper layer lives in the WSL VM, not
# on this distro's disk: it survives 'wsl --terminate' but not a VM shutdown, and
# WSL shuts the VM down on its own once the last distro goes idle. So the modules
# are kept under /usr/local (a real filesystem) and reinstated here at every boot.
# Without this, ZFS quietly disappears between sessions and the pool -- the user's
# data -- cannot be imported.
ensure_modules() {
	if modprobe zfs 2>/dev/null; then
		return 0
	fi
	K="$(uname -r)"
	if [ ! -d "$MODSRC/$K" ]; then
		echo "vectoradb: no ZFS modules for kernel $K." >&2
		echo "vectoradb: run 'vdb setup' to install them." >&2
		exit 1
	fi
	mkdir -p "/usr/lib/modules/$K/extra"
	cp -a "$MODSRC/$K/." "/usr/lib/modules/$K/extra/"
	depmod -a "$K"
	modprobe zfs
}

ensure_modules
[ -f "$IMG" ] || truncate -s "$SIZE" "$IMG"

# Bind the image to exactly one loop device, and expose it under a stable name.
#
# The device number cannot be hardcoded. Every WSL2 distro shares one kernel, so
# /dev/loop* is a global resource: Docker Desktop and Rancher Desktop take loop
# devices, and a distro that is unregistered while its pool is attached leaves
# its binding behind for as long as the VM lives. Picking a fixed number
# therefore fails with EBUSY on exactly the machines this has to work on.
#
# Exactly one binding also matters: two loop devices over the same file are two
# independent block devices over the same bytes, which corrupts the pool and
# suspends it with I/O errors.
# Match on the backing inode, not the path. "losetup -j" compares path strings,
# and an unregistered distro leaves its binding behind for as long as the VM
# lives — with the very same /var/lib/vectoradb-zpool.img string, but pointing
# at a file on a filesystem that no longer exists. Adopting one of those gives a
# pool backed by nothing, which faults and suspends on first write.
img_ino="$(stat -c %i "$IMG")"
cands="$(losetup -l -O NAME,BACK-INO,BACK-FILE --noheadings 2>/dev/null |
	awk -v f="$IMG" '$3 == f { print $1, $2 }')"

devs=""
stale=""
while read -r d ino; do
	[ -n "$d" ] || continue
	if [ "$ino" = "$img_ino" ]; then
		devs="$devs $d"
	else
		stale="$stale $d"
	fi
done <<CANDS
$cands
CANDS

# Bindings carrying our path but a different inode belong to a distro that was
# unregistered while its pool was attached. The kernel keeps them for the life of
# the VM, and every distro shares that VM.
for d in $stale; do
	losetup -d "$d" 2>/dev/null || true
done

# A corpse we could not detach is still holding an old pool. Continuing past it
# risks ZFS attaching to a device whose backing file is gone, which faults on
# first write, suspends the pool and wedges the distro. Only restarting the whole
# VM clears these — terminating this distro is not enough, because the binding
# lives in the VM, not the distro.
for d in $stale; do
	if losetup -l -O NAME --noheadings 2>/dev/null | grep -qx "$d"; then
		echo "vectoradb: $d still points at a deleted VectoraDB pool image." >&2
		echo "vectoradb: this happens after unregistering the distro without restarting WSL." >&2
		echo "vectoradb: run 'wsl --shutdown', then run 'vdb setup' again." >&2
		exit 1
	fi
done

count="$(printf '%s\n' $devs | grep -c . || true)"

if [ "$count" -eq 0 ]; then
	attached="$(losetup --find --show "$IMG")"
elif [ "$count" -eq 1 ]; then
	attached="$(printf '%s\n' $devs | head -1)"
else
	# Duplicates can only be cleaned while no pool is imported off them;
	# detaching under a live pool is what suspends I/O.
	if zpool list vectoradb >/dev/null 2>&1; then
		echo "vectoradb: $IMG is attached to multiple loop devices while the pool is live." >&2
		echo "vectoradb: refusing to continue; run 'wsl --shutdown' and retry." >&2
		exit 1
	fi
	attached="$(printf '%s\n' "$devs" | head -1)"
	for d in $devs; do
		[ "$d" = "$attached" ] || losetup -d "$d" || true
	done
fi

if [ -z "$attached" ]; then
	echo "vectoradb: could not attach $IMG to a loop device." >&2
	exit 1
fi

# The launcher passes $DEV to the engine as VECTORADB_ZPOOL_DEVICE, so it has to
# be a name that survives the loop number changing between boots.
ln -sfn "$attached" "$DEV"

health="$(zpool list -H -o health vectoradb 2>/dev/null || true)"
if [ -n "$health" ]; then
	# A SUSPENDED pool lost its backing device and cannot be repaired in place;
	# limping on just wedges the distro. A fresh boot drops every loop binding
	# and the import, after which the normalisation above runs clean.
	if [ "$health" = "SUSPENDED" ]; then
		echo "vectoradb: the ZFS pool is SUSPENDED (it lost its backing device)." >&2
		echo "vectoradb: run 'wsl --shutdown', then run 'vdb setup' again." >&2
		exit 1
	fi
	finish
	exit 0
fi

# Import from our device alone, never a scan of /dev. A scan can find the label
# of an older pool on a stale binding left by an unregistered distro and import
# onto a device whose backing file is gone — which faults on the first write and
# suspends the pool.
if zpool import -d "$DEV" vectoradb >/dev/null 2>&1; then
	finish
	exit 0
fi

# No pool imported. A virgin image is the normal first-run case: leave the
# device attached so the engine can create the pool on it.
if ! blkid -p "$attached" 2>/dev/null | grep -q zfs_member; then
	finish
	exit 0
fi

# The image already holds a pool that would not import. Fail the unit; the
# launcher refuses to run the engine when this unit is not active, which is what
# stops the engine's "zpool create -f" from overwriting it.
echo "vectoradb: an existing ZFS pool in $IMG could not be imported." >&2
echo "vectoradb: refusing to continue so it cannot be overwritten." >&2
exit 1
`

const zpoolUnit = `[Unit]
Description=VectoraDB ZFS pool (loop-backed)
After=local-fs.target
Before=zfs-mount.service docker.service
Wants=zfs-mount.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/lib/vectoradb/zpool-up.sh

[Install]
WantedBy=multi-user.target
`

// ensureZpoolDevice installs and enables the loop-device unit above.
func ensureZpoolDevice(name string) error {
	step("Preparing the ZFS pool device")
	// Masking happens here, not in installZFS, because installZFS short-circuits
	// once ZFS works — this must also reach distros provisioned by an older
	// build. zfs-import-scan runs `zpool import -aN -d /dev`, and that wide scan
	// can import an old pool's label off a stale loop binding left by an
	// unregistered distro, onto a device whose backing file is gone. Importing
	// is this unit's job alone, scoped to the device we attached.
	script := "set -e; mkdir -p /usr/local/lib/vectoradb; " +
		writeFileB64("/usr/local/lib/vectoradb/zpool-up.sh", zpoolUpScript) + "; " +
		"chmod 0755 /usr/local/lib/vectoradb/zpool-up.sh; " +
		writeFileB64("/etc/systemd/system/vectoradb-zpool.service", zpoolUnit) + "; " +
		"systemctl daemon-reload; " +
		"systemctl mask zfs-import-scan.service zfs-import-cache.service >/dev/null 2>&1 || true; " +
		"systemctl enable --now vectoradb-zpool.service"
	if err := wslRoot(name, script); err != nil {
		return fmt.Errorf("preparing the ZFS pool device: %w", err)
	}
	// systemctl's exit code does not reliably reflect a failed --now start, and a
	// silently dead unit means no pool device — so confirm the unit really came up.
	return checkZpoolUnit(name)
}

// checkZpoolUnit fails unless vectoradb-zpool.service is active.
//
// This is the interlock protecting existing data: when the unit cannot import a
// pool that already exists in the image, it fails deliberately, and refusing to
// go further is what stops the engine's `ensurePool` from reaching
// `zpool create -f` and overwriting it.
func checkZpoolUnit(name string) error {
	out, _ := wslRootOut(name, "systemctl is-active vectoradb-zpool.service 2>&1 || true")
	if strings.TrimSpace(decodeWSLOutput(out)) == "active" {
		return nil
	}
	detail, _ := wslRootOut(name, "systemctl status vectoradb-zpool.service --no-pager -l 2>&1 | tail -n 12 || true")
	return fmt.Errorf("the VectoraDB ZFS pool device is not ready in the %q distro.\n"+
		"Refusing to continue: the engine would create a new pool and could overwrite existing data.\n\n%s",
		name, strings.TrimSpace(decodeWSLOutput(detail)))
}

// writeFileB64 renders a shell fragment that writes content to path. The content
// is base64'd so multi-line scripts with their own quoting survive the trip
// through wsl.exe and sh -c intact.
func writeFileB64(path, content string) string {
	return fmt.Sprintf("printf %%s %q | base64 -d > %q",
		base64.StdEncoding.EncodeToString([]byte(content)), path)
}

// installDocker installs and enables Docker, skipping the (slow) apt work when
// it is already there.
//
// The probe deliberately looks for /usr/bin/dockerd rather than `command -v
// docker`: WSL appends the Windows PATH into distros, so on a machine with
// Docker Desktop installed `docker` resolves to docker.exe and the distro looks
// provisioned when it has no Docker at all. dockerd is Linux-only and cannot be
// supplied by interop.
func installDocker(name string) error {
	if wslRoot(name, "test -x /usr/bin/dockerd") == nil {
		// Already present — either a previous setup installed it, or it came
		// baked into the prebuilt distro image. Still ensure it is running: WSL
		// starts distros with nothing up.
		return wslRoot(name, "systemctl enable --now docker")
	}
	step("Installing Docker in the WSL distro")
	// Acquire::Retries because a single flaky mirror connection should not end a
	// setup run — Ubuntu's mirrors resolve to both IPv6 and IPv4, and a stalled
	// IPv4 path otherwise leaves docker.io with "no installation candidate".
	const retries = "-o Acquire::Retries=3"
	script := "set -e; export DEBIAN_FRONTEND=noninteractive; " +
		"apt-get update -y " + retries + "; " +
		"apt-get install -y " + retries + " docker.io; " +
		"systemctl enable --now docker"
	if err := wslRoot(name, script); err != nil {
		return fmt.Errorf("installing Docker in the distro: %w\n"+
			"If this was a network timeout, re-run `vdb setup` — it resumes where it stopped", err)
	}
	return nil
}

// guestKernelRelease reports `uname -r` inside the distro. Every WSL2 distro
// shares one kernel, so this is really the WSL kernel version.
func guestKernelRelease(name string) (string, error) {
	out, err := wslRootOut(name, "uname -r")
	if err != nil {
		return "", fmt.Errorf("reading the WSL kernel version: %w", err)
	}
	rel := parseKernelRelease(out)
	if rel == "" {
		return "", fmt.Errorf("could not read the WSL kernel version")
	}
	return rel, nil
}

// installZFS unpacks the ZFS modules + userland built for this exact WSL kernel
// into the distro.
//
// WSL mounts /usr/lib/modules/<rel> as an overlay: the stock module set is a
// read-only lower layer and the upper layer lives on this distro's own disk. So
// adding zfs.ko here is durable, private to this distro, and needs no custom
// kernel and no machine-wide .wslconfig change — other distros (Docker Desktop,
// Rancher Desktop) are untouched.
func installZFS(name string) error {
	rel, err := guestKernelRelease(name)
	if err != nil {
		return err
	}
	// Already working (a re-run, or a distro restart that reloaded the module):
	// nothing to do. Checked against the live kernel, so a kernel bump that
	// invalidated the modules still falls through to a reinstall.
	if wslRoot(name, guestPath+"; modprobe zfs 2>/dev/null && zfs version >/dev/null 2>&1") == nil {
		return nil
	}
	// A release from before the modules/userland split ships one combined
	// bundle; prefer the split one but accept either, so a pinned vdb.exe keeps
	// working against its own release.
	bundle := bundledAsset(zfsBundleName(rel))
	if bundle == "" {
		bundle = bundledAsset(legacyZFSBundleName(rel))
	}
	if bundle == "" {
		// Not staged: fetch the one this kernel needs. This is the first moment
		// the right file is knowable — the installer runs before WSL may even
		// exist, which is why it no longer tries to choose.
		var err error
		if bundle, err = fetchZFSBundle(rel); err != nil {
			return err
		}
	}
	step("Installing ZFS for kernel " + rel)
	// The tarball lands modules under lib/modules/<rel>/extra (which usrmerge
	// resolves into the writable module overlay) and userland under /usr/local.
	// daemon-reload is required before enabling: the units arrive with the
	// tarball, so systemd has not seen them yet.
	//
	// ZFS's own import units are masked, and importing is left entirely to
	// vectoradb-zpool.service (see ensureZpoolDevice). zfs-import-cache could
	// never work anyway — a loop-backed pool writes no /etc/zfs/zpool.cache, so
	// its ConditionPathExists never holds — and zfs-import-scan is actively
	// harmful here: it runs `zpool import -aN -d /dev`, and a wide scan can find
	// an old pool's label on a loop binding left behind by an unregistered
	// distro, importing onto a device whose backing file is gone. That faults on
	// the first write and suspends the pool, which then wedges the distro.
	//
	// --keep-directory-symlink is not optional: the bundle carries a ./lib entry
	// and /lib is a symlink to /usr/lib on a usrmerged Ubuntu. Without the flag
	// tar replaces that symlink with a real directory and splits the distro's
	// libraries in two.
	// A persistent copy is kept alongside the overlay install, because the module
	// tree is NOT durable: /usr/lib/modules/<rel> is an overlay whose upper layer
	// lives in the WSL VM's own namespace, not on this distro's disk. It survives
	// `wsl --terminate` but is destroyed when the VM shuts down — which WSL does
	// on its own once the last distro goes idle. Without the copy, ZFS silently
	// disappears between sessions and the pool cannot be imported.
	// vectoradb-zpool.service reinstalls from modulesDir at every boot.
	script := fmt.Sprintf("set -e; %s; tar -C / --keep-directory-symlink -xzf %q; "+
		"mkdir -p %q/%q; cp -a /usr/lib/modules/%q/extra/. %q/%q/; "+
		"depmod -a %q; ldconfig; modprobe zfs; "+
		"systemctl daemon-reload; "+
		"systemctl mask zfs-import-scan.service zfs-import-cache.service >/dev/null 2>&1 || true; "+
		"systemctl enable --now zfs-mount.service zfs.target",
		guestPath, winPathToMnt(bundle),
		guestModulesDir, rel, rel, guestModulesDir, rel,
		rel)
	if err := wslRoot(name, script); err != nil {
		return fmt.Errorf("installing ZFS into the distro: %w", err)
	}
	return nil
}

// fetchZFSBundle downloads the ZFS bundle for a kernel release and caches it
// next to vdb.exe, so a later setup (or a re-run after a distro wipe) is offline.
//
// It downloads to a temporary file and renames on success: a half-written bundle
// left under the real name would be found by bundledAsset next time and fail as
// a corrupt archive, which is a far more confusing error than a missing file.
func fetchZFSBundle(kernelRelease string) (string, error) {
	if err := os.MkdirAll(installDir(), 0o755); err != nil {
		return "", err
	}
	step("Downloading ZFS for kernel " + kernelRelease)

	// Try the split module bundle first, then the combined one. Which exists
	// depends on the release this binary was stamped for, and a 404 on the first
	// is the normal case against an older release rather than an error.
	var lastErr error
	for _, asset := range []string{zfsBundleName(kernelRelease), legacyZFSBundleName(kernelRelease)} {
		url := releaseAssetURL(vectoradbRepo, version.Version, asset)
		dst := filepath.Join(installDir(), asset)
		err := downloadResumable(url, dst, asset)
		if err == nil {
			if err := verifyReleaseAsset(dst, asset); err != nil {
				os.Remove(dst)
				return "", err
			}
			return dst, nil
		}
		if !errors.Is(err, errAssetNotFound) {
			return "", err
		}
		lastErr = err
		logf("no %s in this release, trying the next name\n", asset)
	}
	return "", fmt.Errorf("no ZFS module bundle published for this WSL kernel (%s).\n"+
		"VectoraDB ships the ZFS module built for one exact kernel, and yours is not among them.\n"+
		"Check for a newer VectoraDB release, or build it yourself with `make wsl-zfs`.\n"+
		"  (%v)", kernelRelease, lastErr)
}

// verifyReleaseAsset checks a downloaded asset against the release's SHA256SUMS.
//
// The bundle is unpacked into the distro and its modules are loaded into the
// kernel, so "it came over TLS" is not on its own a good enough answer to what
// this file is. A release with no SHA256SUMS is not a failure — releases predate
// the file — but a listed asset whose hash disagrees is.
func verifyReleaseAsset(path, asset string) error {
	want, err := releaseChecksum(asset)
	if err != nil {
		logf("checksum lookup for %s skipped: %v\n", asset, err)
		return nil
	}
	if want == "" {
		logf("%s is not listed in SHA256SUMS; skipping verification\n", asset)
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("checksum mismatch for %s\n  expected %s\n  got      %s\n"+
			"The download does not match what this VectoraDB release published. "+
			"It was discarded; re-run `vdb setup` to try again", asset, want, got)
	}
	logf("checksum OK for %s\n", asset)
	return nil
}

// releaseChecksum returns the expected SHA256 for an asset, or "" when the
// release lists no such file.
func releaseChecksum(asset string) (string, error) {
	resp, err := http.Get(releaseAssetURL(vectoradbRepo, version.Version, "SHA256SUMS"))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("SHA256SUMS: %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	return checksumFor(string(body), asset), nil
}

// downloadResumable fetches url to dst, resuming and retrying on a dropped
// connection.
//
// This matters more than it looks: the bundle is ~85 MB, and a single dropped
// connection on an ordinary home network otherwise fails the whole install with
// a wsarecv error and leaves a half-created distro behind. Progress is kept in a
// .part file so a retry continues rather than starting again.
func downloadResumable(url, dst, asset string) error {
	part := dst + ".part"
	const attempts = 4

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		var have int64
		if fi, err := os.Stat(part); err == nil {
			have = fi.Size()
		}

		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		if have > 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
		}

		resp, err := (&http.Client{Timeout: 30 * time.Minute}).Do(req)
		if err != nil {
			lastErr = err
			logf("download attempt %d/%d failed: %v\n", attempt, attempts, err)
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
			continue
		}

		switch resp.StatusCode {
		case http.StatusNotFound:
			resp.Body.Close()
			return fmt.Errorf("%w: %s", errAssetNotFound, url)
		case http.StatusOK:
			// The server ignored our Range (or we had nothing): start over.
			have = 0
		case http.StatusPartialContent:
			// Resuming where we left off.
		default:
			resp.Body.Close()
			lastErr = fmt.Errorf("%s", resp.Status)
			logf("download attempt %d/%d: %s\n", attempt, attempts, resp.Status)
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
			continue
		}

		flags := os.O_CREATE | os.O_WRONLY
		if have > 0 {
			flags |= os.O_APPEND
		} else {
			flags |= os.O_TRUNC
		}
		f, err := os.OpenFile(part, flags, 0o644)
		if err != nil {
			resp.Body.Close()
			return err
		}
		_, copyErr := io.Copy(f, resp.Body)
		resp.Body.Close()
		if closeErr := f.Close(); copyErr == nil {
			copyErr = closeErr
		}
		if copyErr != nil {
			lastErr = copyErr
			logf("download attempt %d/%d interrupted: %v\n", attempt, attempts, copyErr)
			fmt.Printf("  download interrupted, resuming (%d/%d)…\n", attempt, attempts)
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
			continue
		}
		if err := os.Rename(part, dst); err != nil {
			return fmt.Errorf("saving %s: %w", asset, err)
		}
		return nil
	}
	return fmt.Errorf("downloading %s failed after %d attempts: %w\n"+
		"Check your connection and re-run `vdb setup` — the partial download is resumed, not restarted",
		asset, attempts, lastErr)
}

// verifyZFS is the gate on the whole approach: modules built for a different
// kernel load nowhere, and every later failure would be a confusing symptom of
// that one cause.
func verifyZFS(name string) error {
	rel, err := guestKernelRelease(name)
	if err != nil {
		return err
	}
	if err := wslRoot(name, guestPath+"; modprobe zfs && zfs version >/dev/null && zpool version >/dev/null"); err != nil {
		return fmt.Errorf("ZFS is not usable in the %q distro (kernel %s): %w\n"+
			"The ZFS modules must be built for this exact kernel. Stage %s next to vdb.exe\n"+
			"and re-run `vdb setup`, or rebuild it with `make wsl-zfs`",
			name, rel, err, zfsBundleName(rel))
	}
	return nil
}

// stageImageContext copies the Postgres image build context into the distro.
// The engine discovers a build context relative to the working directory, which
// finds nothing for a user who installed vdb rather than cloning the repo; the
// forwarded environment points VECTORADB_IMAGE_CONTEXT here instead.
func stageImageContext(name string) error {
	// The prebuilt distro image already carries the context (and the built
	// image), so there is nothing to stage.
	if wslRoot(name, fmt.Sprintf("test -f %q/Dockerfile", guestImageContext)) == nil {
		return nil
	}
	src := bundledDir("docker-context")
	if src == "" {
		logf("no bundled docker build context; vdb start will look relative to the working directory\n")
		return nil
	}
	step("Staging the Postgres image build context")
	script := fmt.Sprintf("set -e; mkdir -p %q; cp -r %q/. %q/; chmod +x %q/restore-entrypoint.sh",
		guestImageContext, winPathToMnt(src), guestImageContext, guestImageContext)
	if err := wslRoot(name, script); err != nil {
		return fmt.Errorf("staging the image build context: %w", err)
	}
	return nil
}

// installGuestBinaryWSL copies the bundled linux engine binary into the distro.
func installGuestBinaryWSL(name string) error {
	bin := strings.TrimSpace(os.Getenv("VECTORADB_GUEST_BINARY"))
	if bin == "" {
		bin = bundledLinuxBinary("amd64") // WSL2 is x86_64
	}
	if bin == "" {
		fmt.Println("Note: no bundled Linux vdb binary found — the guest will use /tmp/vdb " +
			"if you built it from source (VECTORADB_GUEST_BINARY overrides this).")
		return nil
	}
	step("Installing the vdb engine into the WSL distro")
	src := winPathToMnt(bin)
	return wslRoot(name, fmt.Sprintf("install -m 0755 %q /usr/local/bin/vdb", src))
}

// vectoradbRepo is where setup fetches assets the installer did not stage.
const vectoradbRepo = "SauravYadav12/vectoraDB"

// errAssetNotFound distinguishes "this release has no such asset" from a real
// download failure, so callers can try an alternative name rather than give up.
var errAssetNotFound = errors.New("release asset not found")

// installDir is the directory holding vdb.exe — where the installer stages
// assets and where setup caches anything it downloads.
func installDir() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Dir(exe)
	}
	return filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "vectoradb")
}

// assetDirs lists the places the installer (or a dev build) puts support files,
// relative to vdb.exe.
func assetDirs() []string {
	exe, err := os.Executable()
	if err != nil {
		return nil
	}
	dir := filepath.Dir(exe)
	return []string{
		dir,
		filepath.Join(dir, "..", "share", "vectoradb"),
		filepath.Join(dir, "..", "dist"),
		filepath.Join(dir, "dist"),
	}
}

// bundledAsset finds a support file (ZFS bundle, rootfs) shipped next to vdb.exe
// by the installer, or in ./dist for a dev build.
func bundledAsset(basename string) string {
	for _, d := range assetDirs() {
		c := filepath.Join(d, basename)
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c
		}
	}
	return ""
}

// bundledDir is bundledAsset for a directory (the docker build context).
func bundledDir(basename string) string {
	for _, d := range assetDirs() {
		c := filepath.Join(d, basename)
		if fi, err := os.Stat(c); err == nil && fi.IsDir() {
			return c
		}
	}
	return ""
}

// bundledRootfs finds the Ubuntu rootfs tarball, which install.ps1 fetches and
// which `wsl --import` accepts gzipped or plain.
// The prebuilt image comes first: it already contains Docker, the ZFS userland,
// the engine and the container images, so importing it skips an apt install, a
// docker build and three registry pulls. A plain Ubuntu rootfs still works, and
// setup does that extra work itself.
func bundledRootfs() string {
	for _, n := range []string{distroImageName, "vectoradb-rootfs.tar.gz", "vectoradb-rootfs.tar"} {
		if p := bundledAsset(n); p != "" {
			return p
		}
	}
	return ""
}

// prebuiltDistro reports whether the imported distro came from our image, in
// which case setup can skip what the image already did.
func prebuiltDistro(name string) bool {
	return wslRoot(name, "test -x /usr/bin/dockerd && test -x /usr/local/sbin/zpool") == nil
}
