//go:build darwin

// SPDX-License-Identifier: AGPL-3.0-or-later

package host

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// hostSetup is the macOS bootstrap: create/start the Lima VM and bring the stack up.
func hostSetup() error { return setupDarwin() }

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

func forward(args []string) error { return forwardStdin(args, os.Stdin) }

func forwardStdin(args []string, stdin io.Reader) error {
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
	cmd := exec.Command("limactl", full...)
	cmd.Stdin = stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func setupDarwin() error {
	if _, err := exec.LookPath("limactl"); err != nil {
		return fmt.Errorf("Lima is required on macOS.\n" +
			"Install it with:\n  brew install lima\n" +
			"then run `vdb setup` again")
	}
	// Fetch the latest engine build up front, so even an existing VM is updated
	// (not just a freshly created one).
	refreshEngineBinary(runtime.GOARCH)

	name := instance()
	if instanceExists(name) {
		if !instanceRunning(name) {
			fmt.Printf("Starting existing VM %q…\n", name)
			if err := limactl("start", name).Run(); err != nil {
				return err
			}
		}
		fmt.Printf("VM %q is ready.\n", name)
	} else {
		fmt.Printf("Creating the VectoraDB VM %q (first run downloads Ubuntu; a few minutes)…\n", name)
		if err := limactl("start", "--name", name, "--tty=false", "template://ubuntu").Run(); err != nil {
			return fmt.Errorf("creating the VM: %w", err)
		}
		if err := provisionGuest(name); err != nil {
			return err
		}
	}
	// Always (re)install the engine binary, so re-running `vdb setup` picks up a
	// newer build instead of keeping the one already inside the VM.
	if err := installGuestBinary(name); err != nil {
		return err
	}
	fmt.Println("Bringing the stack up…")
	return forward([]string{"start"})
}

// provisionGuest installs Docker + ZFS inside a freshly created VM. The engine
// binary is installed separately (installGuestBinary), on every setup. The engine
// itself auto-creates the ZFS pool and builds the image on first `up`.
func provisionGuest(name string) error {
	fmt.Println("Installing Docker and ZFS in the VM…")
	script := "set -e; sudo apt-get update -y; " +
		"sudo apt-get install -y zfsutils-linux docker.io; " +
		"sudo systemctl enable --now docker"
	if err := limactl("shell", name, "--", "sh", "-c", script).Run(); err != nil {
		return fmt.Errorf("installing guest dependencies: %w", err)
	}
	return nil
}

// installGuestBinary copies the bundled Linux vdb binary into the VM and puts it
// on PATH, so `vdb` inside the guest is the real engine. Skipped (with a note) if
// no bundled binary is found — e.g. a source checkout that builds its own.
func installGuestBinary(name string) error {
	bin := strings.TrimSpace(os.Getenv("VECTORADB_GUEST_BINARY"))
	if bin == "" {
		bin = bundledLinuxBinary(guestArch(name))
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
