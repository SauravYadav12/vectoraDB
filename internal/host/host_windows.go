//go:build windows

// SPDX-License-Identifier: AGPL-3.0-or-later

package host

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
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

// wsl runs an interactive wsl.exe command wired to the console.
func wsl(args ...string) *exec.Cmd {
	cmd := exec.Command("wsl.exe", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd
}

// guestPath prefixes setup scripts that use the ZFS userland. It installs under
// /usr/local, which is on Debian's default root PATH and sudo secure_path — but
// the environment `wsl.exe -- sh -c` inherits is not guaranteed to be either, so
// the setup scripts state it rather than assume it.
const guestPath = "export PATH=/usr/local/sbin:/usr/local/bin:$PATH"

// wslRoot runs a shell command inside the distro as root (setup steps need
// privilege without a sudo password prompt).
func wslRoot(name, script string) error {
	return wsl("-d", name, "-u", "root", "--", "sh", "-c", script).Run()
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

	name := currentDistro()
	if distroExists(name) {
		fmt.Printf("WSL distro %q already exists.\n", name)
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
	err := forward([]string{"start"})
	if err == nil {
		return shareMountPropagation(name)
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
	fmt.Println("First start raced container initialisation — retrying…")
	if err := forward([]string{"start"}); err != nil {
		return err
	}
	return shareMountPropagation(name)
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
	fmt.Printf("Creating the %q WSL distro…\n", name)
	if err := wsl("--import", name, installDir, rootfs).Run(); err != nil {
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
	if err := wsl("--terminate", name).Run(); err != nil {
		return fmt.Errorf("restarting the distro to apply systemd: %w", err)
	}
	return nil
}

// waitForSystemd blocks until the distro's systemd has finished booting. The
// distro is restarted right before this to pick up /etc/wsl.conf, and systemctl
// calls issued during that window fail with "system has not been booted".
func waitForSystemd(name string) error {
	fmt.Print("Waiting for systemd in the distro…")
	defer fmt.Println()
	for i := 0; i < 60; i++ {
		out, _ := wslRootOut(name, "systemctl is-system-running 2>&1 || true")
		switch strings.TrimSpace(decodeWSLOutput(out)) {
		case "running", "degraded":
			return nil
		}
		fmt.Print(".")
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
	if err := stageImageContext(name); err != nil {
		return err
	}
	return installGuestBinaryWSL(name)
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

modprobe zfs
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

# Bindings carrying our path but a dead inode belong to an unregistered distro.
for d in $stale; do
	losetup -d "$d" 2>/dev/null || true
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
		echo "vectoradb: refusing to continue; run 'wsl --terminate vectoradb' and retry." >&2
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

if zpool list vectoradb >/dev/null 2>&1; then
	finish
	exit 0
fi
if zpool import -d /dev vectoradb >/dev/null 2>&1; then
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
	fmt.Println("Preparing the ZFS pool device…")
	script := "set -e; mkdir -p /usr/local/lib/vectoradb; " +
		writeFileB64("/usr/local/lib/vectoradb/zpool-up.sh", zpoolUpScript) + "; " +
		"chmod 0755 /usr/local/lib/vectoradb/zpool-up.sh; " +
		writeFileB64("/etc/systemd/system/vectoradb-zpool.service", zpoolUnit) + "; " +
		"systemctl daemon-reload; systemctl enable --now vectoradb-zpool.service"
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
		// Still ensure it is running: WSL starts distros with nothing up.
		return wslRoot(name, "systemctl enable --now docker")
	}
	fmt.Println("Installing Docker in the WSL distro…")
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
	bundle := bundledAsset(zfsBundleName(rel))
	if bundle == "" {
		return fmt.Errorf("no ZFS bundle for this WSL kernel (%s).\n"+
			"Expected %s next to vdb.exe.\n"+
			"Your WSL kernel is newer than this VectoraDB release — update VectoraDB, "+
			"or build the bundle with `make wsl-zfs` (see docs/windows-setup.md)",
			rel, zfsBundleName(rel))
	}
	fmt.Printf("Installing ZFS for kernel %s…\n", rel)
	// The tarball lands modules under lib/modules/<rel>/extra (which usrmerge
	// resolves into the writable module overlay) and userland under /usr/local.
	// daemon-reload is required before enabling: the units arrive with the
	// tarball, so systemd has not seen them yet.
	//
	// Only zfs-mount/zfs.target are enabled here. Importing the pool is left to
	// vectoradb-zpool.service (see ensureZpoolDevice): zfs-import-cache.service
	// could never do it, because a loop-backed pool writes no /etc/zfs/zpool.cache
	// and that unit's ConditionPathExists therefore never holds.
	//
	// --keep-directory-symlink is not optional: the bundle carries a ./lib entry
	// and /lib is a symlink to /usr/lib on a usrmerged Ubuntu. Without the flag
	// tar replaces that symlink with a real directory and splits the distro's
	// libraries in two.
	script := fmt.Sprintf("set -e; %s; tar -C / --keep-directory-symlink -xzf %q; "+
		"depmod -a %q; ldconfig; modprobe zfs; "+
		"systemctl daemon-reload; "+
		"systemctl enable --now zfs-mount.service zfs.target",
		guestPath, winPathToMnt(bundle), rel)
	if err := wslRoot(name, script); err != nil {
		return fmt.Errorf("installing ZFS into the distro: %w", err)
	}
	return nil
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
	src := bundledDir("docker-context")
	if src == "" {
		fmt.Println("Note: no bundled docker build context found — `vdb start` will look for " +
			"docker/postgres relative to the current directory.")
		return nil
	}
	fmt.Println("Staging the Postgres image build context…")
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
	fmt.Println("Installing the vdb engine into the WSL distro…")
	src := winPathToMnt(bin)
	return wslRoot(name, fmt.Sprintf("install -m 0755 %q /usr/local/bin/vdb", src))
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
func bundledRootfs() string {
	for _, n := range []string{"vectoradb-rootfs.tar.gz", "vectoradb-rootfs.tar"} {
		if p := bundledAsset(n); p != "" {
			return p
		}
	}
	return ""
}
