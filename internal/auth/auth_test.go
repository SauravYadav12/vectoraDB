// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"path/filepath"
	"testing"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(Config{DBPath: filepath.Join(t.TempDir(), "t.db"), WebOrigin: "http://x", SignupOpen: true})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPasswordLogin(t *testing.T) {
	s := testStore(t)
	if _, err := s.CreateUser("A@x.com", "password1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Login("a@x.com", "password1"); err != nil { // case-insensitive
		t.Errorf("login should succeed: %v", err)
	}
	if _, err := s.Login("a@x.com", "wrong"); err == nil {
		t.Error("wrong password should fail")
	}
	if _, err := s.CreateUser("a@x.com", "password1"); err == nil {
		t.Error("duplicate email should fail")
	}
}

func TestAPIKeys(t *testing.T) {
	s := testStore(t)
	u, _ := s.CreateUser("k@x.com", "password1")
	secret, info, err := s.CreateAPIKey(u.ID, "ci")
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := s.VerifyKey(secret); !ok || got.ID != u.ID {
		t.Errorf("verify key failed: ok=%v", ok)
	}
	if _, ok := s.VerifyKey("vdb_wrong"); ok {
		t.Error("bad key should not verify")
	}
	if err := s.RevokeKey(u.ID, info.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.VerifyKey(secret); ok {
		t.Error("revoked key should not verify")
	}
}

func TestSessions(t *testing.T) {
	s := testStore(t)
	u, _ := s.CreateUser("s@x.com", "password1")
	tok, err := s.createSession(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := s.userBySession(tok); !ok || got.ID != u.ID {
		t.Error("session lookup failed")
	}
	s.deleteSession(tok)
	if _, ok := s.userBySession(tok); ok {
		t.Error("deleted session should be invalid")
	}
}
