//go:build !windows

// SPDX-License-Identifier: AGPL-3.0-or-later

package daemon

import (
	"os/exec"
	"syscall"
)

// processAlive reports whether pid is a live process, using signal 0.
func processAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// configureDetach puts the child in its own session so it outlives the CLI.
func configureDetach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// terminate asks pid to shut down gracefully.
func terminate(pid int) { _ = syscall.Kill(pid, syscall.SIGTERM) }

// kill forcibly terminates pid (uninterceptable).
func kill(pid int) { _ = syscall.Kill(pid, syscall.SIGKILL) }
