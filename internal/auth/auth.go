// SPDX-License-Identifier: AGPL-3.0-or-later

// Package auth gates VectoraDB's surfaces. It stores users, API keys, sessions,
// and OAuth identities in a local SQLite file (pure-Go driver, no cgo) and
// provides an HTTP middleware plus login/OAuth/API-key handlers. Scope is
// single-tenant: authenticated users share one VectoraDB instance.
package auth

import (
	crand "crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

// OAuthApp holds a provider's client credentials.
type OAuthApp struct{ ClientID, ClientSecret string }

func (a OAuthApp) enabled() bool { return a.ClientID != "" && a.ClientSecret != "" }

// Config is the auth configuration (usually built from the environment).
type Config struct {
	DBPath     string
	WebOrigin  string // where the web UI is served (for CORS + OAuth redirect back)
	PublicURL  string // this API's public base URL (for OAuth callbacks)
	SignupOpen bool
	GitHub     OAuthApp
	Google     OAuthApp
}

// User is an authenticated account.
type User struct {
	ID    int64  `json:"id"`
	Email string `json:"email"`
}

// Store is the auth data layer.
type Store struct {
	db  *sql.DB
	cfg Config
}

const schema = `
CREATE TABLE IF NOT EXISTS users (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  email TEXT UNIQUE NOT NULL,
  pw_hash TEXT NOT NULL DEFAULT '',
  created INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS api_keys (
  id TEXT PRIMARY KEY, user_id INTEGER NOT NULL, name TEXT NOT NULL,
  key_hash TEXT NOT NULL, prefix TEXT NOT NULL, created INTEGER NOT NULL, last_used INTEGER
);
CREATE TABLE IF NOT EXISTS sessions (
  token TEXT PRIMARY KEY, user_id INTEGER NOT NULL, expires INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS oauth_identities (
  provider TEXT NOT NULL, subject TEXT NOT NULL, user_id INTEGER NOT NULL,
  PRIMARY KEY (provider, subject)
);
CREATE TABLE IF NOT EXISTS pipelines (
  id TEXT PRIMARY KEY, user_id INTEGER NOT NULL, name TEXT NOT NULL,
  spec TEXT NOT NULL, created INTEGER NOT NULL, updated INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS pipeline_runs (
  id TEXT PRIMARY KEY,
  pipeline_id TEXT NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
  user_id INTEGER NOT NULL, status TEXT NOT NULL,
  started INTEGER NOT NULL, finished INTEGER, tables INTEGER NOT NULL DEFAULT 0,
  tests TEXT NOT NULL DEFAULT '[]', log TEXT NOT NULL DEFAULT ''
);`

// Open opens (and migrates) the SQLite store.
//
// The store is shared across several processes (control plane, gateway, agent
// API), so it is opened in WAL mode with a busy timeout: concurrent writers
// (e.g. the api_keys.last_used bump on every authenticated request) wait for the
// lock instead of failing with SQLITE_BUSY — which would otherwise surface to
// clients as a spurious 401.
func Open(cfg Config) (*Store, error) {
	dsn := cfg.DBPath + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, cfg: cfg}, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// OpenFromEnv builds Config from VECTORADB_* env vars and opens the store.
func OpenFromEnv() (*Store, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "/tmp"
	}
	dir := filepath.Join(home, ".vectoradb")
	_ = os.MkdirAll(dir, 0o755)
	return Open(Config{
		DBPath:     envOr("VECTORADB_DB", filepath.Join(dir, "vectoradb.db")),
		WebOrigin:  envOr("VECTORADB_WEB_ORIGIN", "http://localhost:5173"),
		PublicURL:  envOr("VECTORADB_PUBLIC_URL", "http://localhost:8080"),
		SignupOpen: os.Getenv("VECTORADB_SIGNUP") != "closed",
		GitHub:     OAuthApp{os.Getenv("VECTORADB_GITHUB_CLIENT_ID"), os.Getenv("VECTORADB_GITHUB_CLIENT_SECRET")},
		Google:     OAuthApp{os.Getenv("VECTORADB_GOOGLE_CLIENT_ID"), os.Getenv("VECTORADB_GOOGLE_CLIENT_SECRET")},
	})
}

// WebOrigin is the configured UI origin (used for CORS).
func (s *Store) WebOrigin() string { return s.cfg.WebOrigin }

// HasAnyUser reports whether any account exists (for bootstrap hints).
func (s *Store) HasAnyUser() bool {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n > 0
}

func randToken(n int) string {
	b := make([]byte, n)
	_, _ = crand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// ---- users ----

// CreateUser creates an account (password may be "" for OAuth-only users).
func (s *Store) CreateUser(email, password string) (User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return User{}, errors.New("email required")
	}
	var hash string
	if password != "" {
		h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			return User{}, err
		}
		hash = string(h)
	}
	res, err := s.db.Exec(`INSERT INTO users(email, pw_hash, created) VALUES(?,?,?)`, email, hash, time.Now().Unix())
	if err != nil {
		return User{}, fmt.Errorf("email already registered")
	}
	id, _ := res.LastInsertId()
	return User{ID: id, Email: email}, nil
}

// Login verifies email + password.
func (s *Store) Login(email, password string) (User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	var u User
	var hash string
	if err := s.db.QueryRow(`SELECT id, email, pw_hash FROM users WHERE email=?`, email).Scan(&u.ID, &u.Email, &hash); err != nil {
		return User{}, errors.New("invalid credentials")
	}
	if hash == "" || bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return User{}, errors.New("invalid credentials")
	}
	return u, nil
}

func (s *Store) userByID(id int64) (User, error) {
	var u User
	err := s.db.QueryRow(`SELECT id, email FROM users WHERE id=?`, id).Scan(&u.ID, &u.Email)
	return u, err
}

// ---- sessions ----

const sessionTTL = 30 * 24 * time.Hour

func (s *Store) createSession(userID int64) (string, error) {
	tok := randToken(24)
	_, err := s.db.Exec(`INSERT INTO sessions(token, user_id, expires) VALUES(?,?,?)`,
		tok, userID, time.Now().Add(sessionTTL).Unix())
	return tok, err
}

func (s *Store) userBySession(tok string) (User, bool) {
	var uid, exp int64
	if err := s.db.QueryRow(`SELECT user_id, expires FROM sessions WHERE token=?`, tok).Scan(&uid, &exp); err != nil {
		return User{}, false
	}
	if time.Now().Unix() > exp {
		_, _ = s.db.Exec(`DELETE FROM sessions WHERE token=?`, tok)
		return User{}, false
	}
	u, err := s.userByID(uid)
	return u, err == nil
}

func (s *Store) deleteSession(tok string) {
	_, _ = s.db.Exec(`DELETE FROM sessions WHERE token=?`, tok)
}

// ---- api keys ----

// KeyInfo is a non-secret view of an API key.
type KeyInfo struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Prefix  string `json:"prefix"`
	Created int64  `json:"created"`
}

func hashKey(k string) string { h := sha256.Sum256([]byte(k)); return hex.EncodeToString(h[:]) }

// CreateAPIKey returns the full secret (shown once) plus its stored metadata.
func (s *Store) CreateAPIKey(userID int64, name string) (string, KeyInfo, error) {
	if strings.TrimSpace(name) == "" {
		name = "key"
	}
	secret := "vdb_" + randToken(24)
	id := randToken(8)
	prefix := secret[:12]
	now := time.Now().Unix()
	if _, err := s.db.Exec(`INSERT INTO api_keys(id,user_id,name,key_hash,prefix,created) VALUES(?,?,?,?,?,?)`,
		id, userID, name, hashKey(secret), prefix, now); err != nil {
		return "", KeyInfo{}, err
	}
	return secret, KeyInfo{ID: id, Name: name, Prefix: prefix, Created: now}, nil
}

// VerifyKey resolves an API key to its user. A false result means "not
// authenticated"; a genuine store failure (as opposed to an unknown key) is
// logged so it is diagnosable rather than silently masquerading as a bad key.
func (s *Store) VerifyKey(key string) (User, bool) {
	h := hashKey(key)
	var uid int64
	switch err := s.db.QueryRow(`SELECT user_id FROM api_keys WHERE key_hash=?`, h).Scan(&uid); {
	case errors.Is(err, sql.ErrNoRows):
		return User{}, false // no such key — an ordinary auth failure
	case err != nil:
		log.Printf("auth: VerifyKey store error (client will see this as unauthenticated): %v", err)
		return User{}, false
	}
	// Best-effort, throttled last_used bump: only when stale (>60s), so a burst of
	// concurrent auth checks doesn't turn into a burst of writes on the store.
	// (This makes last_used coarse by ~a minute — fine for "last active", not for
	// real-time anomaly detection.)
	now := time.Now().Unix()
	_, _ = s.db.Exec(`UPDATE api_keys SET last_used=? WHERE key_hash=? AND (last_used IS NULL OR last_used < ?)`,
		now, h, now-60)
	u, err := s.userByID(uid)
	return u, err == nil
}

func (s *Store) listAPIKeys(userID int64) ([]KeyInfo, error) {
	rows, err := s.db.Query(`SELECT id,name,prefix,created FROM api_keys WHERE user_id=? ORDER BY created DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KeyInfo
	for rows.Next() {
		var k KeyInfo
		_ = rows.Scan(&k.ID, &k.Name, &k.Prefix, &k.Created)
		out = append(out, k)
	}
	return out, nil
}

func (s *Store) revokeAPIKey(userID int64, id string) error {
	_, err := s.db.Exec(`DELETE FROM api_keys WHERE id=? AND user_id=?`, id, userID)
	return err
}

// UserByEmail looks up a user by email without a password check (CLI admin use).
func (s *Store) UserByEmail(email string) (User, bool) {
	var u User
	if err := s.db.QueryRow(`SELECT id,email FROM users WHERE email=?`,
		strings.ToLower(strings.TrimSpace(email))).Scan(&u.ID, &u.Email); err != nil {
		return User{}, false
	}
	return u, true
}

// ListKeys and RevokeKey are exported wrappers for CLI admin use.
func (s *Store) ListKeys(userID int64) ([]KeyInfo, error) { return s.listAPIKeys(userID) }
func (s *Store) RevokeKey(userID int64, id string) error  { return s.revokeAPIKey(userID, id) }

// ---- oauth upsert ----

func (s *Store) upsertOAuth(provider, subject, email string) (User, error) {
	var uid int64
	if err := s.db.QueryRow(`SELECT user_id FROM oauth_identities WHERE provider=? AND subject=?`, provider, subject).Scan(&uid); err == nil {
		return s.userByID(uid)
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if email != "" {
		var u User
		if e := s.db.QueryRow(`SELECT id,email FROM users WHERE email=?`, email).Scan(&u.ID, &u.Email); e == nil {
			_, _ = s.db.Exec(`INSERT INTO oauth_identities(provider,subject,user_id) VALUES(?,?,?)`, provider, subject, u.ID)
			return u, nil
		}
	}
	if email == "" {
		email = provider + "-" + subject + "@oauth.local"
	}
	nu, err := s.CreateUser(email, "")
	if err != nil {
		return User{}, err
	}
	_, _ = s.db.Exec(`INSERT INTO oauth_identities(provider,subject,user_id) VALUES(?,?,?)`, provider, subject, nu.ID)
	return nu, nil
}
