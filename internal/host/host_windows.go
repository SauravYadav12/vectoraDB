//go:build windows

// SPDX-License-Identifier: AGPL-3.0-or-later

package host

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	// `wsl.exe -d <name> -- …` starts the distro on demand; no explicit start.
	guest := guestBin(name)
	cmd := exec.Command("wsl.exe", wslArgs(name, guest, args)...)
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

// wslRoot runs a shell command inside the distro as root (imported distros log in
// as root by default; setup steps need privilege without a sudo password prompt).
func wslRoot(name, script string) error {
	return wsl("-d", name, "-u", "root", "--", "sh", "-c", script).Run()
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

	if err := ensureKernel(); err != nil {
		return err
	}

	name := currentDistro()
	if distroExists(name) {
		fmt.Printf("WSL distro %q already exists. Bringing the stack up…\n", name)
	} else {
		if err := importDistro(name); err != nil {
			return err
		}
		if err := provisionGuestWSL(name); err != nil {
			return err
		}
	}
	return forward([]string{"start"})
}

// ensureKernel points %UserProfile%\.wslconfig at the bundled ZFS-enabled WSL2
// kernel and applies it. Without a ZFS kernel the engine's `zpool create` fails,
// so this is required, not optional.
func ensureKernel() error {
	kernel := bundledAsset("vectoradb-wsl-kernel")
	if kernel == "" {
		return fmt.Errorf("the ZFS-enabled WSL2 kernel (vectoradb-wsl-kernel) was not found next to vdb.exe — " +
			"reinstall with install.ps1, or set it manually in %%UserProfile%%\\.wslconfig")
	}
	cfgPath := filepath.Join(os.Getenv("USERPROFILE"), ".wslconfig")
	existing, _ := os.ReadFile(cfgPath) // absent file → empty, fine
	// .wslconfig wants a Windows path with escaped backslashes.
	kline := strings.ReplaceAll(kernel, `\`, `\\`)
	merged := mergeWslConfig(string(existing), kline)
	if err := os.WriteFile(cfgPath, []byte(merged), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", cfgPath, err)
	}
	fmt.Println("Applying the ZFS-enabled WSL2 kernel (this restarts WSL)…")
	// Note: .wslconfig kernel= is machine-wide; a ZFS kernel is a superset, so
	// other distros keep working.
	return wsl("--shutdown").Run()
}

// importDistro creates the dedicated distro from the bundled Ubuntu rootfs and
// enables systemd inside it.
func importDistro(name string) error {
	rootfs := bundledAsset("vectoradb-rootfs.tar")
	if rootfs == "" {
		return fmt.Errorf("the Ubuntu rootfs (vectoradb-rootfs.tar) was not found next to vdb.exe — reinstall with install.ps1")
	}
	installDir := filepath.Join(os.Getenv("LOCALAPPDATA"), "vectoradb", "wsl")
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		return err
	}
	fmt.Printf("Creating the %q WSL distro…\n", name)
	if err := wsl("--import", name, installDir, rootfs).Run(); err != nil {
		return fmt.Errorf("importing the WSL distro: %w", err)
	}
	// Enable systemd (needed for `systemctl enable --now docker`).
	if err := wslRoot(name, "printf '[boot]\\nsystemd=true\\n' > /etc/wsl.conf"); err != nil {
		return fmt.Errorf("enabling systemd in the distro: %w", err)
	}
	return wsl("--terminate", name).Run()
}

// provisionGuestWSL installs Docker + ZFS in the distro, loads the ZFS module, and
// installs the engine binary — mirroring provisionGuest on macOS.
func provisionGuestWSL(name string) error {
	fmt.Println("Installing Docker and ZFS in the WSL distro…")
	script := "set -e; apt-get update -y; " +
		"apt-get install -y zfsutils-linux docker.io; " +
		"systemctl enable --now docker; modprobe zfs"
	if err := wslRoot(name, script); err != nil {
		return fmt.Errorf("installing guest dependencies: %w", err)
	}
	return installGuestBinaryWSL(name)
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

// bundledAsset finds a support file (kernel, rootfs) shipped next to vdb.exe by
// the installer, or in ./dist for a dev build.
func bundledAsset(basename string) string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	dir := filepath.Dir(exe)
	for _, c := range []string{
		filepath.Join(dir, basename),
		filepath.Join(dir, "..", "share", "vectoradb", basename),
		filepath.Join(dir, "dist", basename),
	} {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c
		}
	}
	return ""
}
