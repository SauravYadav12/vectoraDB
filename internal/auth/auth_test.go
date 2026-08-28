// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

// testStore opens a Store in a temp dir and closes it when the test ends.
//
// The close is not optional on Windows: t.TempDir's cleanup deletes the
// directory, and Windows refuses to unlink a file that is still open, so a
// leaked handle fails the test in cleanup rather than in the test body.
func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(Config{DBPath: filepath.Join(t.TempDir(), "t.db"), WebOrigin: "http://x", SignupOpen: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("closing the store: %v", err)
		}
	})
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

// TestVerifyKeyConcurrent reproduces the SQLITE_BUSY regression: many
// simultaneous authenticated requests must all verify, not be rejected as
// invalid because a concurrent last_used write hit a lock.
func TestVerifyKeyConcurrent(t *testing.T) {
	s := testStore(t)
	u, _ := s.CreateUser("c@x.com", "password1")
	secret, _, err := s.CreateAPIKey(u.ID, "ci")
	if err != nil {
		t.Fatal(err)
	}
	const n = 32
	var wg sync.WaitGroup
	var fails int64
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := s.VerifyKey(secret); !ok {
				atomic.AddInt64(&fails, 1)
			}
		}()
	}
	wg.Wait()
	if fails != 0 {
		t.Errorf("%d/%d concurrent VerifyKey calls were rejected (SQLITE_BUSY regression)", fails, n)
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
