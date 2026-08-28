// SPDX-License-Identifier: AGPL-3.0-or-later

package host

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf16"
)

// TC1.1 — distro name resolution.
func TestResolveWSLDistro(t *testing.T) {
	cases := map[string]string{"": "vectoradb", "  ": "vectoradb", "mine": "mine", "  spaced  ": "spaced"}
	for in, want := range cases {
		if got := resolveWSLDistro(in); got != want {
			t.Errorf("resolveWSLDistro(%q) = %q, want %q", in, got, want)
		}
	}
}

// TC1.2 — the wsl.exe argument list, including the in-guest marker.
func TestWSLArgs(t *testing.T) {
	got := wslArgs("vectoradb", "/usr/local/bin/vdb", []string{"branch", "list"})
	want := []string{"-d", "vectoradb", "--", "env", "VECTORADB_IN_GUEST=1", "/usr/local/bin/vdb", "branch", "list"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("wslArgs = %q, want %q", got, want)
	}
}

func encodeUTF16LE(s string, bom bool) []byte {
	var b []byte
	if bom {
		b = append(b, 0xFF, 0xFE)
	}
	for _, u := range utf16.Encode([]rune(s)) {
		b = append(b, byte(u), byte(u>>8))
	}
	return b
}

// TC1.3 — parse `wsl -l -v` UTF-16LE output (BOM + `*` default marker + header).
func TestDecodeWSLList(t *testing.T) {
	raw := "  NAME            STATE           VERSION\n" +
		"* Ubuntu          Running         2\n" +
		"  vectoradb       Stopped         2\n"
	got := decodeWSLList(encodeUTF16LE(raw, true))
	want := []wslDistro{{"Ubuntu", "Running"}, {"vectoradb", "Stopped"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("decodeWSLList = %+v, want %+v", got, want)
	}
	// Also tolerate plain UTF-8 (no NUL bytes).
	if got := decodeWSLList([]byte(raw)); !reflect.DeepEqual(got, want) {
		t.Errorf("decodeWSLList(utf8) = %+v, want %+v", got, want)
	}
}

// TC1.6 — Windows path → WSL /mnt path.
func TestWinPathToMnt(t *testing.T) {
	cases := map[string]string{
		`C:\Users\x\vdb-linux-amd64`: "/mnt/c/Users/x/vdb-linux-amd64",
		`D:\a b\file`:                "/mnt/d/a b/file",
		`/already/posix`:             "/already/posix",
	}
	for in, want := range cases {
		if got := winPathToMnt(in); got != want {
			t.Errorf("winPathToMnt(%q) = %q, want %q", in, got, want)
		}
	}
}

// TC1.5 — .wslconfig merge: empty, existing-section-no-kernel, existing-kernel.
// Uses a backslash-free sentinel path (KP/KP2) — mergeWslConfig treats the path
// as opaque, so this keeps the assertions free of escaping ambiguity.
func TestMergeWslConfig(t *testing.T) {
	// empty → new section
	if got := mergeWslConfig("", "KP"); got != "[wsl2]\nkernel=KP\n" {
		t.Errorf("empty: got %q", got)
	}
	// existing [wsl2] without kernel → inserted, other keys preserved
	got := mergeWslConfig("[wsl2]\nmemory=4GB\n", "KP")
	if !strings.Contains(got, "kernel=KP") || !strings.Contains(got, "memory=4GB") {
		t.Errorf("insert: got %q", got)
	}
	// existing kernel → replaced (no duplicate)
	got = mergeWslConfig("[wsl2]\nkernel=OLD\nswap=0\n", "KP2")
	if strings.Contains(got, "OLD") || strings.Count(got, "kernel=") != 1 || !strings.Contains(got, "swap=0") {
		t.Errorf("replace: got %q", got)
	}
	// no [wsl2] section but other section present → appends [wsl2]
	got = mergeWslConfig("[experimental]\nfoo=1\n", "KP")
	if !strings.Contains(got, "[wsl2]") || !strings.Contains(got, "kernel=KP") || !strings.Contains(got, "foo=1") {
		t.Errorf("append: got %q", got)
	}
}

// TC1.4 — importLocalFile rewrites a local file to a stdin stream; pass-through
// for postgres:// and non-files. (Shared launcher logic, OS-independent.)
func TestImportLocalFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "people.csv")
	if err := os.WriteFile(path, []byte("id,name\n1,a\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, f, ok := importLocalFile([]string{"import", "--from", path, "--as", "p"})
	if !ok || f == nil {
		t.Fatalf("expected rewrite for a local file; ok=%v", ok)
	}
	defer f.Close()
	want := []string{"import", "--from", "-", "--kind", "csv", "--srcname", "people.csv", "--as", "p"}
	if !reflect.DeepEqual(out, want) {
		t.Errorf("rewrite = %q, want %q", out, want)
	}

	if _, _, ok := importLocalFile([]string{"import", "--from", "postgres://h/db"}); ok {
		t.Error("postgres:// should pass through (ok=false)")
	}
	if _, _, ok := importLocalFile([]string{"import", "--from", filepath.Join(dir, "nope.csv")}); ok {
		t.Error("missing file should pass through (ok=false)")
	}
	if _, _, ok := importLocalFile([]string{"branch", "list"}); ok {
		t.Error("non-import command should pass through (ok=false)")
	}
}
