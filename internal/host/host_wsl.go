// SPDX-License-Identifier: AGPL-3.0-or-later

// Pure, OS-independent helpers for the Windows/WSL2 launcher. They live in an
// untagged file (not host_windows.go) so they unit-test on any platform — the
// side-effecting wsl.exe / .wslconfig orchestration that calls them is in
// host_windows.go.
package host

import (
	"bytes"
	"strings"
	"unicode/utf16"
)

// defaultWSLDistro is the dedicated distro `vdb setup` creates on Windows.
const defaultWSLDistro = "vectoradb"

// resolveWSLDistro picks the distro name from an override (e.g. an env var),
// falling back to the dedicated default.
func resolveWSLDistro(override string) string {
	if s := strings.TrimSpace(override); s != "" {
		return s
	}
	return defaultWSLDistro
}

// wslArgs builds the `wsl.exe` argument list that runs the guest engine binary
// inside a distro, marking the process as in-guest so it never forwards again.
//
//	wsl.exe -d <distro> -- env VECTORADB_IN_GUEST=1 <guest> <args…>
func wslArgs(distro, guest string, args []string) []string {
	base := []string{"-d", distro, "--", "env", envInGuest + "=1", guest}
	return append(base, args...)
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

// mergeWslConfig returns %UserProfile%\.wslconfig content with the [wsl2] section's
// kernel= set to kernelPath: replacing an existing kernel=, inserting into an
// existing [wsl2] section, or appending a new section — preserving other keys.
func mergeWslConfig(existing, kernelPath string) string {
	kline := "kernel=" + kernelPath
	if strings.TrimSpace(existing) == "" {
		return "[wsl2]\n" + kline + "\n"
	}
	lines := strings.Split(strings.ReplaceAll(existing, "\r\n", "\n"), "\n")

	// First pass: does a [wsl2] section exist, and does it already have kernel=?
	hasWsl2, hasKernel, inWsl2 := false, false, false
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "[") {
			inWsl2 = strings.EqualFold(t, "[wsl2]")
			hasWsl2 = hasWsl2 || inWsl2
			continue
		}
		if inWsl2 && strings.HasPrefix(strings.ToLower(t), "kernel=") {
			hasKernel = true
		}
	}
	if !hasWsl2 {
		return strings.TrimRight(existing, "\n") + "\n\n[wsl2]\n" + kline + "\n"
	}

	// Second pass: rewrite in place.
	var out []string
	inWsl2 = false
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "[") {
			inWsl2 = strings.EqualFold(t, "[wsl2]")
			out = append(out, ln)
			if inWsl2 && !hasKernel {
				out = append(out, kline) // insert right after the [wsl2] header
			}
			continue
		}
		if inWsl2 && hasKernel && strings.HasPrefix(strings.ToLower(t), "kernel=") {
			out = append(out, kline) // replace the existing kernel=
			continue
		}
		out = append(out, ln)
	}
	return strings.Join(out, "\n")
}
