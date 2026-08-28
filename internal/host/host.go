// SPDX-License-Identifier: AGPL-3.0-or-later

// Package host makes `vdb` a single cross-platform entry point.
//
// The branching engine needs Linux + ZFS + Docker, which don't exist natively
// on macOS or Windows. Rather than make users manage a VM by hand, `vdb` hides
// it: on Linux the engine runs in-process; on macOS engine commands are
// forwarded into a Lima VM and on Windows into a WSL2 distro, transparently, so
// a user only ever types `vdb …`.
//
// This file holds the OS-independent dispatch. The per-OS transport lives in
// host_darwin.go (Lima), host_windows.go (WSL2), and host_other.go (Linux/other),
// each providing forward/forwardStdin/hostSetup. Pure, testable WSL helpers are
// in host_wsl.go (untagged, so they unit-test on any OS).
package host

import (
	"os"
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
	return true, hostForward(args)
}

// Setup runs the one-time host bootstrap (create/start the VM) and is invoked by
// the `setup` command. Local commands reach it via the normal switch in main.
func Setup() error { return hostSetup() }

// hostForward forwards an engine command into the managed VM. `vdb import --from
// <local file>` is special-cased: the file is streamed from THIS machine into the
// VM over stdin (so imports work from ANY path, not just a VM-mounted home).
func hostForward(args []string) error {
	if newArgs, f, ok := importLocalFile(args); ok {
		defer f.Close()
		return forwardStdin(newArgs, f)
	}
	return forward(args)
}

// importLocalFile detects `vdb import --from <path>` where <path> is a readable
// file on THIS machine, and rewrites it to stream that file into the VM over
// stdin (`--from -`). Returns false for a postgres:// source or a path that
// isn't a local file (which is forwarded unchanged — it may exist in the VM).
func importLocalFile(args []string) ([]string, *os.File, bool) {
	if len(args) == 0 || args[0] != "import" {
		return nil, nil, false
	}
	path, fromFlag := "", false
	for i := 1; i < len(args); i++ {
		if args[i] == "--from" && i+1 < len(args) {
			path, fromFlag = args[i+1], true
			break
		}
	}
	if path == "" {
		for i := 1; i < len(args); i++ {
			if !strings.HasPrefix(args[i], "-") && !isPostgresURL(args[i]) {
				path = args[i]
				break
			}
		}
	}
	if path == "" || isPostgresURL(path) {
		return nil, nil, false
	}
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		return nil, nil, false // not a local file — forward as-is (may exist in the VM)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, false
	}
	var rest []string
	for i := 1; i < len(args); i++ {
		if fromFlag && args[i] == "--from" && i+1 < len(args) {
			i++ // drop `--from <val>`
			continue
		}
		if !fromFlag && args[i] == path {
			continue // drop the bare source
		}
		rest = append(rest, args[i])
	}
	kind := strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
	out := append([]string{"import", "--from", "-", "--kind", kind, "--srcname", filepath.Base(path)}, rest...)
	return out, f, true
}

func isPostgresURL(s string) bool {
	return strings.HasPrefix(s, "postgres://") || strings.HasPrefix(s, "postgresql://")
}

// bundledLinuxBinary looks for a prebuilt linux vdb (vdb-linux-<arch>) shipped
// alongside the host binary by the installer, or in ./dist for a dev build. Used
// by both the macOS (Lima) and Windows (WSL2) setup paths to seed the guest.
func bundledLinuxBinary(arch string) string {
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
