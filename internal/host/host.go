// SPDX-License-Identifier: AGPL-3.0-or-later

// Package host makes `vdb` a single cross-platform entry point.
//
// The branching engine needs Linux + ZFS + Docker, which don't exist natively
// on macOS or Windows. Rather than make users manage a VM by hand, `vdb` hides
// it: on Linux the engine runs in-process; on macOS engine commands are
// forwarded into a Lima VM transparently, so a user only ever types `vdb …`.
package host

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Guest environment variable marks a vdb process that is already running inside
// the managed Linux VM, so it never tries to forward again.
const envInGuest = "VECTORADB_IN_GUEST"

// localCommands run on the host machine itself and are never forwarded.
var localCommands = map[string]bool{
	"": true, "help": true, "-h": true, "--help": true,
	"version": true, "-v": true, "--version": true,
	"setup": true, "vm": true,
}

// Maybe performs host-side dispatch.
//
//   - Linux, or already inside the guest VM: returns (false, nil) — the caller
//     runs the engine in-process.
//   - macOS/Windows engine command: forwards into the VM and returns (true, err).
//   - Local commands (version/help/setup) always return (false, nil).
func Maybe(args []string) (handled bool, err error) {
	if runtime.GOOS == "linux" || os.Getenv(envInGuest) != "" {
		return false, nil
	}
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	if localCommands[sub] {
		return false, nil
	}
	switch runtime.GOOS {
	case "darwin":
		return true, forward(args)
	case "windows":
		return true, fmt.Errorf(
			"native Windows support is on the way — for now enable WSL2 and run vdb inside it")
	default:
		return true, fmt.Errorf("unsupported host OS %q", runtime.GOOS)
	}
}

// Setup runs the one-time host bootstrap (create/start the VM) and is invoked by
// the `setup` command. Local commands reach it via the normal switch in main.
func Setup() error {
	switch runtime.GOOS {
	case "linux":
		// On Linux the engine provisions itself on first `up`; nothing to do.
		fmt.Println("Linux host — no VM needed. Run `vdb start`.")
		return nil
	case "darwin":
		return setupDarwin()
	case "windows":
		return fmt.Errorf("native Windows support is on the way — for now enable WSL2 and run vdb inside it")
	default:
		return fmt.Errorf("unsupported host OS %q", runtime.GOOS)
	}
}

// --- macOS / Lima ---

func instance() string {
	if v := strings.TrimSpace(os.Getenv("VECTORADB_LIMA_INSTANCE")); v != "" {
		return v
	}
	// Prefer a dedicated instance; fall back to an existing "default" VM so this
	// works with a machine already set up the old way.
	for _, name := range []string{"vectoradb", "default"} {
		if instanceExists(name) {
			return name
		}
	}
	return "vectoradb"
}

func limactl(args ...string) *exec.Cmd {
	cmd := exec.Command("limactl", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd
}

func instanceExists(name string) bool {
	out, err := exec.Command("limactl", "list", "--format", "{{.Name}}").Output()
	if err != nil {
		return false
	}
	for _, l := range strings.Fields(string(out)) {
		if l == name {
			return true
		}
	}
	return false
}

func instanceRunning(name string) bool {
	out, err := exec.Command("limactl", "list", name, "--format", "{{.Status}}").Output()
	return err == nil && strings.TrimSpace(string(out)) == "Running"
}

// guestBin resolves the vdb binary path inside the VM: an explicit override, or
// `vdb` on the guest PATH, else the dev build at /tmp/vdb.
func guestBin(name string) string {
	if v := strings.TrimSpace(os.Getenv("VECTORADB_GUEST_BIN")); v != "" {
		return v
	}
	out, err := exec.Command("limactl", "shell", name, "--",
		"sh", "-c", "command -v vdb || echo /tmp/vdb").Output()
	if err == nil {
		if p := strings.TrimSpace(string(out)); p != "" {
			return p
		}
	}
	return "/tmp/vdb"
}

func forward(args []string) error {
	if _, err := exec.LookPath("limactl"); err != nil {
		return fmt.Errorf("Lima is required on macOS. Install it with `brew install lima`, then run `vdb setup`")
	}
	name := instance()
	if !instanceExists(name) {
		return fmt.Errorf("no VectoraDB VM yet — run `vdb setup` once to create it")
	}
	if !instanceRunning(name) {
		fmt.Printf("Starting the VectoraDB VM (%s)…\n", name)
		if err := limactl("start", name).Run(); err != nil {
			return fmt.Errorf("starting the VM: %w", err)
		}
	}
	guest := guestBin(name)
	full := append([]string{"shell", name, "--", "env", envInGuest + "=1", guest}, args...)
	return limactl(full...).Run()
}

func setupDarwin() error {
	if _, err := exec.LookPath("limactl"); err != nil {
		return fmt.Errorf("Lima is required on macOS.\n" +
			"Install it with:\n  brew install lima\n" +
			"then run `vdb setup` again")
	}
	name := instance()
	if instanceExists(name) {
		if !instanceRunning(name) {
			fmt.Printf("Starting existing VM %q…\n", name)
			if err := limactl("start", name).Run(); err != nil {
				return err
			}
		}
		fmt.Printf("VM %q is ready. Bringing the stack up…\n", name)
	} else {
		fmt.Printf("Creating the VectoraDB VM %q (first run downloads Ubuntu; a few minutes)…\n", name)
		if err := limactl("start", "--name", name, "--tty=false", "template://ubuntu").Run(); err != nil {
			return fmt.Errorf("creating the VM: %w", err)
		}
		if err := provisionGuest(name); err != nil {
			return err
		}
	}
	// Bring the stack up inside the guest.
	return forward([]string{"start"})
}

// provisionGuest installs Docker + ZFS inside a freshly created VM. The engine
// itself auto-creates the ZFS pool and builds the image on first `up`.
func provisionGuest(name string) error {
	fmt.Println("Installing Docker and ZFS in the VM…")
	script := "set -e; sudo apt-get update -y; " +
		"sudo apt-get install -y zfsutils-linux docker.io; " +
		"sudo systemctl enable --now docker"
	if err := limactl("shell", name, "--", "sh", "-c", script).Run(); err != nil {
		return fmt.Errorf("installing guest dependencies: %w", err)
	}
	return installGuestBinary(name)
}

// installGuestBinary copies the bundled Linux vdb binary into the VM and puts it
// on PATH, so `vdb` inside the guest is the real engine. Skipped (with a note) if
// no bundled binary is found — e.g. a source checkout that builds its own.
func installGuestBinary(name string) error {
	bin := strings.TrimSpace(os.Getenv("VECTORADB_GUEST_BINARY"))
	if bin == "" {
		bin = bundledLinuxBinary(name)
	}
	if bin == "" {
		fmt.Println("Note: no bundled Linux vdb binary found — the guest will use /tmp/vdb " +
			"if you built it from source (VECTORADB_GUEST_BINARY overrides this).")
		return nil
	}
	fmt.Println("Installing the vdb engine into the VM…")
	if err := limactl("copy", bin, name+":/tmp/vdb.new").Run(); err != nil {
		return fmt.Errorf("copying the engine binary into the VM: %w", err)
	}
	return limactl("shell", name, "--",
		"sudo", "install", "-m", "0755", "/tmp/vdb.new", "/usr/local/bin/vdb").Run()
}

// guestArch reports the Go arch string for the VM ("arm64"/"amd64").
func guestArch(name string) string {
	out, err := exec.Command("limactl", "shell", name, "--", "uname", "-m").Output()
	if err == nil && strings.TrimSpace(string(out)) == "x86_64" {
		return "amd64"
	}
	return "arm64" // Lima defaults to the host arch; Apple Silicon is arm64
}

// bundledLinuxBinary looks for a prebuilt linux vdb shipped alongside the host
// binary by the installer (or in ./dist for a dev build).
func bundledLinuxBinary(name string) string {
	arch := guestArch(name)
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	dir := filepath.Dir(exe)
	for _, c := range []string{
		filepath.Join(dir, "vdb-linux-"+arch),
		filepath.Join(dir, "..", "share", "vectoradb", "vdb-linux-"+arch),
		filepath.Join(dir, "..", "dist", "vdb-linux-"+arch),
		filepath.Join(dir, "dist", "vdb-linux-"+arch),
	} {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c
		}
	}
	return ""
}
