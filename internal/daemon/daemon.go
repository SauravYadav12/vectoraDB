// SPDX-License-Identifier: AGPL-3.0-or-later

// Package daemon runs VectoraDB's long-lived servers (proxy, agent API) as
// detached background processes so they don't hold a terminal. Each service is
// the vectoradb binary re-invoked with its subcommand, started in a new session
// (setsid) with a pidfile and a log file under ~/.vectoradb.
package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func runDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "/tmp"
	}
	d := filepath.Join(home, ".vectoradb")
	_ = os.MkdirAll(d, 0o755)
	return d
}

func pidPath(name string) string { return filepath.Join(runDir(), name+".pid") }

// LogPath is the log file for a service.
func LogPath(name string) string { return filepath.Join(runDir(), name+".log") }

func readPid(name string) int {
	b, err := os.ReadFile(pidPath(name))
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid
}

// Alive reports whether the service's recorded process is still running.
func Alive(name string) bool {
	pid := readPid(name)
	return pid > 0 && syscall.Kill(pid, 0) == nil
}

// Start launches a service detached (no-op if already running). args are the
// vectoradb subcommand and flags, e.g. {"proxy","--addr",":6432"}.
func Start(name string, args []string) error {
	if Alive(name) {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logf, err := os.OpenFile(LogPath(name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, args...)
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // detach from our session
	if err := cmd.Start(); err != nil {
		return err
	}
	return os.WriteFile(pidPath(name), []byte(strconv.Itoa(cmd.Process.Pid)), 0o644)
}

// Stop signals a service to terminate and clears its pidfile.
func Stop(name string) {
	if pid := readPid(name); pid > 0 {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	_ = os.Remove(pidPath(name))
}

// Status returns "running (pid N)" or "stopped".
func Status(name string) string {
	if Alive(name) {
		return fmt.Sprintf("running (pid %d)", readPid(name))
	}
	return "stopped"
}
