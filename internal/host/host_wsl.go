// SPDX-License-Identifier: AGPL-3.0-or-later

// Pure, OS-independent helpers for the Windows/WSL2 launcher. They live in an
// untagged file (not host_windows.go) so they unit-test on any platform — the
// side-effecting wsl.exe orchestration that calls them is in host_windows.go.
package host

import (
	"bytes"
	"path"
	"strings"
	"unicode/utf16"
)

// defaultWSLDistro is the dedicated distro `vdb setup` creates on Windows.
const defaultWSLDistro = "vectoradb"

// guestImageContext is where setup stages the docker/postgres build context
// inside the distro. The engine's ensureImage() reads it from
// VECTORADB_IMAGE_CONTEXT, so an installed user (who has no repo checkout, and
// therefore nothing for findImageContext to discover) can still build the image.
const guestImageContext = "/usr/local/share/vectoradb/docker/postgres"

// resolveWSLDistro picks the distro name from an override (e.g. an env var),
// falling back to the dedicated default.
func resolveWSLDistro(override string) string {
	if s := strings.TrimSpace(override); s != "" {
		return s
	}
	return defaultWSLDistro
}

// guestPATH is Debian's default root PATH. The engine resolves zfs, zpool and
// docker with exec.LookPath, and ZFS installs under /usr/local — but the
// environment `wsl.exe -- env` inherits depends on how the distro was created,
// so the forwarded command is given an explicit PATH rather than a hopeful one.
const guestPATH = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// guestZpoolDevice is the block device backing the ZFS pool inside the distro.
//
// The engine's default is a plain file vdev, which the WSL2 kernel cannot back a
// pool with — `zpool create` on a file fails there with "no such pool or
// dataset", while the same file behind a loop device works. So setup attaches
// the pool image to a loop device and points the engine here through
// VECTORADB_ZPOOL_DEVICE, a knob the engine already honours.
//
// It is a symlink, not a loop device directly: every WSL2 distro shares one
// kernel, so loop numbers are global and contended (Docker Desktop and Rancher
// Desktop take them, and an unregistered distro leaves its binding behind).
// vectoradb-zpool.service picks whichever device is free and repoints this name.
const guestZpoolDevice = "/dev/vectoradb-pool"

// guestEnv is the environment every forwarded command runs with inside the
// distro: the in-guest marker (so the guest vdb never forwards again), a PATH
// that finds the ZFS userland, the staged docker build context, and the
// loop-backed pool vdev.
func guestEnv() []string {
	return []string{
		envInGuest + "=1",
		"PATH=" + guestPATH,
		"VECTORADB_IMAGE_CONTEXT=" + guestImageContext,
		"VECTORADB_ZPOOL_DEVICE=" + guestZpoolDevice,
	}
}

// wslArgs builds the `wsl.exe` argument list that runs the guest engine binary
// inside a distro under the given environment.
//
//	wsl.exe -d <distro> -- env <env…> <guest> <args…>
func wslArgs(distro, guest string, env, args []string) []string {
	out := []string{"-d", distro, "--", "env"}
	out = append(out, env...)
	out = append(out, guest)
	return append(out, args...)
}

type wslDistro struct{ Name, State string }

// decodeWSLOutput decodes wsl.exe output, which is UTF-16LE (with a BOM) on real
// Windows but may be plain UTF-8 in tests or odd shells.
func decodeWSLOutput(b []byte) string {
	if bytes.IndexByte(b, 0) < 0 { // no NUL bytes → already UTF-8
		return string(b)
	}
	if len(b) >= 2 && b[0] == 0xFF && b[1] == 0xFE { // strip UTF-16LE BOM
		b = b[2:]
	}
	u16 := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u16 = append(u16, uint16(b[i])|uint16(b[i+1])<<8)
	}
	return string(utf16.Decode(u16))
}

// decodeWSLList parses `wsl.exe -l -v` output into distros, tolerating the
// UTF-16LE encoding, the `*` default marker, and the header row.
func decodeWSLList(raw []byte) []wslDistro {
	var out []wslDistro
	for _, ln := range strings.Split(decodeWSLOutput(raw), "\n") {
		ln = strings.TrimRight(ln, "\r")
		fields := strings.Fields(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(ln), "*")))
		if len(fields) < 2 || strings.EqualFold(fields[0], "NAME") {
			continue
		}
		out = append(out, wslDistro{Name: fields[0], State: fields[1]})
	}
	return out
}

// winPathToMnt converts a Windows path to its WSL /mnt/<drive> form.
//
//	C:\Users\x\vdb-linux-amd64  ->  /mnt/c/Users/x/vdb-linux-amd64
func winPathToMnt(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	if len(p) >= 2 && p[1] == ':' {
		return "/mnt/" + strings.ToLower(p[:1]) + p[2:]
	}
	return p
}

// parseKernelRelease extracts the kernel release from `uname -r` output (or from
// the .release file shipped with the ZFS bundle), tolerating the UTF-16LE
// wrapping and trailing newlines that wsl.exe adds.
func parseKernelRelease(raw []byte) string {
	return strings.TrimSpace(decodeWSLOutput(raw))
}

// zfsBundleName is the ZFS module artifact for a kernel release. The release is
// part of the filename because the modules only load against the exact kernel
// they were built for — a stale bundle must be a missing file, not a silent
// mismatch.
//
// Modules only: the ZFS userland is kernel-independent and ships inside the
// distro image, which is what keeps this download ~2 MB rather than ~80 MB.
func zfsBundleName(kernelRelease string) string {
	return "vectoradb-zfs-modules-" + strings.TrimSpace(kernelRelease) + ".tar.gz"
}

// legacyZFSBundleName is the pre-split artifact, carrying modules and userland
// together. Releases before the split only have this one, so a vdb.exe pinned to
// such a release must still be able to find it.
func legacyZFSBundleName(kernelRelease string) string {
	return "vectoradb-zfs-" + strings.TrimSpace(kernelRelease) + ".tar.gz"
}

// distroImageName is the prebuilt distro: Ubuntu with Docker, the ZFS userland,
// the engine and the container images already in place. Importing it replaces
// an apt install, a docker build and three registry pulls on the user's machine.
const distroImageName = "vectoradb-distro.tar.gz"

// checksumFor finds an asset's SHA256 in a sha256sum-style listing, or "" if the
// listing does not mention it.
//
// Lines are "<64 hex>  <name>", with an optional leading "*" on the name for
// binary mode. Matching is on the base name so a listing generated with paths
// still resolves.
func checksumFor(listing, asset string) string {
	for _, ln := range strings.Split(strings.ReplaceAll(listing, "\r\n", "\n"), "\n") {
		fields := strings.Fields(strings.TrimSpace(ln))
		if len(fields) != 2 || len(fields[0]) != 64 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name == asset || path.Base(name) == asset {
			return strings.ToLower(fields[0])
		}
	}
	return ""
}

// releaseAssetURL is where `vdb setup` fetches an asset the installer did not
// stage. The ZFS bundle is fetched here rather than by install.ps1 because only
// setup knows which one is needed: it has just created the distro, so it can ask
// the running kernel, whereas the installer would need WSL to already exist.
//
// The version is the one stamped into this binary, so a v0.4.0 vdb.exe takes
// v0.4.0 assets and never silently drifts onto a newer release's. An unstamped
// dev build has no matching release, so it falls back to latest.
func releaseAssetURL(repo, version, asset string) string {
	v := strings.TrimSpace(version)
	if v == "" || strings.Contains(v, "dev") {
		return "https://github.com/" + repo + "/releases/latest/download/" + asset
	}
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	return "https://github.com/" + repo + "/releases/download/" + v + "/" + asset
}
