// SPDX-License-Identifier: AGPL-3.0-or-later

package branch

import (
	"strings"
	"testing"
)

func TestParsePublishedPort(t *testing.T) {
	cases := []struct {
		in   string
		want string
		err  bool
	}{
		{"0.0.0.0:32781\n[::]:32781", "32781", false},
		{"0.0.0.0:5432", "5432", false},
		{"", "", true},
		{"garbage", "", true},
	}
	for _, c := range cases {
		got, err := parsePublishedPort(c.in)
		if c.err {
			if err == nil {
				t.Errorf("parsePublishedPort(%q): expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parsePublishedPort(%q): unexpected error %v", c.in, err)
		} else if got != c.want {
			t.Errorf("parsePublishedPort(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDSN(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolate the generated secrets file from the dev's home
	got := dsn("12345")
	// The password is a per-install secret, so assert the shape, not the value.
	if !strings.HasPrefix(got, "postgresql://vectoradb:") ||
		!strings.HasSuffix(got, "@127.0.0.1:12345/vectoradb") {
		t.Errorf("dsn = %q, want postgresql://vectoradb:<pw>@127.0.0.1:12345/vectoradb", got)
	}
}

func TestAgentBranchName(t *testing.T) {
	if got := agentBranch("alice"); got != "agent-alice" {
		t.Errorf("agentBranch = %q, want agent-alice", got)
	}
}

func TestContainerName(t *testing.T) {
	if got := container("main"); got != "vec-main" {
		t.Errorf("container = %q, want vec-main", got)
	}
}
