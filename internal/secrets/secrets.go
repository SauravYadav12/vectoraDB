// SPDX-License-Identifier: AGPL-3.0-or-later

// Package secrets holds the per-install credentials VectoraDB generates on first
// run — the Postgres role password and the MinIO/object-store key pair — instead
// of shipping hardcoded defaults in a public source tree.
//
// The values are generated once with crypto/rand, persisted 0600 under
// ~/.vectoradb/secrets.json, and read by every process that needs them: the
// engine (which sets them on the containers it starts) and the gateway (which
// authenticates to the backend with the same Postgres password). Both run in the
// same guest and share ~/.vectoradb, so they converge on one set. Environment
// variables override any field, for callers who manage their own secrets.
package secrets

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Secrets is the per-install credential set.
type Secrets struct {
	PGPassword    string `json:"pg_password"`    // Postgres role "vectoradb" password
	MinioUser     string `json:"minio_user"`     // MinIO root user / AWS access key id
	MinioPassword string `json:"minio_password"` // MinIO root password / AWS secret key
}

const (
	envPG         = "VECTORADB_PG_PASSWORD"
	envMinioUser  = "VECTORADB_MINIO_USER"
	envMinioPass  = "VECTORADB_MINIO_PASSWORD"
	secretsFile   = "secrets.json"
	configDirName = ".vectoradb"
)

var (
	once   sync.Once
	cached Secrets
)

// Load returns the per-install secrets, generating and persisting them on first
// use. It is cached for the life of the process, so repeated calls are cheap and
// always return the same values. A read/write failure falls back to freshly
// generated in-memory values rather than blocking the engine.
func Load() Secrets {
	once.Do(func() { cached = loadOrCreate() })
	return cached
}

func configDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.TempDir()
	}
	return filepath.Join(home, configDirName)
}

func loadOrCreate() Secrets {
	path := filepath.Join(configDir(), secretsFile)

	s := readFile(path) // zero-value Secrets if absent/unreadable

	// Fill any missing field: generate it, then persist the completed set so the
	// gateway and engine read identical values. Env overrides win and are not
	// persisted (the caller owns them).
	changed := false
	if s.PGPassword == "" {
		s.PGPassword = randToken()
		changed = true
	}
	if s.MinioUser == "" {
		s.MinioUser = "vdb" + randToken()[:12]
		changed = true
	}
	if s.MinioPassword == "" {
		s.MinioPassword = randToken()
		changed = true
	}
	if changed {
		writeFile(path, s)
	}

	// Environment overrides take precedence but are never written to disk.
	if v := os.Getenv(envPG); v != "" {
		s.PGPassword = v
	}
	if v := os.Getenv(envMinioUser); v != "" {
		s.MinioUser = v
	}
	if v := os.Getenv(envMinioPass); v != "" {
		s.MinioPassword = v
	}
	return s
}

func readFile(path string) Secrets {
	var s Secrets
	b, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	_ = json.Unmarshal(b, &s)
	return s
}

func writeFile(path string, s Secrets) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}
	// Write via a temp file + rename so a concurrent reader never sees a
	// half-written file.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// randToken returns a 32-hex-char (128-bit) random token.
func randToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is unrecoverable; make it loud rather than silently
		// weak.
		panic(fmt.Sprintf("secrets: crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(b)
}
