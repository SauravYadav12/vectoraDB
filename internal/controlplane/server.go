// SPDX-License-Identifier: AGPL-3.0-or-later

// Package controlplane serves VectoraDB's management REST API (JSON only),
// gated by internal/auth. The web UI is a separate app (see web/).
package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/vectoradb/vectoradb/internal/auth"
	"github.com/vectoradb/vectoradb/internal/branch"
	"github.com/vectoradb/vectoradb/internal/daemon"
)

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)

// Serve starts the control-plane REST API on addr (e.g. ":8080").
func Serve(addr string) error {
	store, err := auth.OpenFromEnv()
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	store.MountPublic(mux) // /auth/* (register, login, logout, me, providers, oauth)

	api := http.NewServeMux()
	registerAPI(api)
	store.MountKeys(api) // /api/keys (protected via Authn below)
	mux.Handle("/api/", store.Authn(api))

	log.Printf("control-plane API on %s (auth on; UI origin %s)", addr, store.WebOrigin())
	return http.ListenAndServe(addr, cors(store.WebOrigin())(logging(mux)))
}

func registerAPI(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		bs, err := branch.Branches()
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		var nBranch, nAgent int
		mainReady := false
		for _, b := range bs {
			switch {
			case b.Primary:
				mainReady = b.State == "running"
			case b.Agent:
				nAgent++
			default:
				nBranch++
			}
		}
		writeJSON(w, 200, map[string]any{
			"mainReady": mainReady,
			"branches":  nBranch,
			"agents":    nAgent,
			"ha":        branch.HAInfo(),
			"storage":   branch.StorageInfo(),
			"servers": map[string]bool{
				"gateway": daemon.Alive("gateway"),
				"api":     daemon.Alive("api"),
			},
		})
	})

	mux.HandleFunc("GET /api/branches", func(w http.ResponseWriter, r *http.Request) {
		bs, err := branch.Branches()
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		if bs == nil {
			bs = []branch.BranchInfo{}
		}
		writeJSON(w, 200, bs)
	})

	mux.HandleFunc("POST /api/branches", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if !nameRe.MatchString(body.Name) {
			writeErr(w, 400, fmt.Errorf("invalid name (use lowercase letters, digits, dashes)"))
			return
		}
		if err := branch.Create(body.Name, ""); err != nil {
			writeErr(w, 409, err)
			return
		}
		writeJSON(w, 201, map[string]string{"name": body.Name, "status": "created"})
	})

	mux.HandleFunc("DELETE /api/branches/{name}", func(w http.ResponseWriter, r *http.Request) {
		if err := branch.Delete(r.PathValue("name")); err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, map[string]string{"status": "deleted"})
	})

	mux.HandleFunc("POST /api/branches/{name}/suspend", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if name == "main" {
			writeErr(w, 400, fmt.Errorf("refusing to suspend the primary 'main'"))
			return
		}
		if err := branch.Suspend(name); err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, map[string]string{"status": "suspended"})
	})

	mux.HandleFunc("POST /api/branches/{name}/resume", func(w http.ResponseWriter, r *http.Request) {
		if err := branch.Wake(r.PathValue("name")); err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, map[string]string{"status": "resumed"})
	})

	// Run SQL against a branch (powers the web SQL console).
	mux.HandleFunc("POST /api/branches/{name}/query", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		var body struct {
			SQL string `json:"sql"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if strings.TrimSpace(body.SQL) == "" {
			writeErr(w, 400, fmt.Errorf("sql is required"))
			return
		}
		addr, err := branch.EnsureRunning(name)
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		writeJSON(w, 200, runQuery(addr, body.SQL))
	})
}

// runQuery executes SQL against a branch backend and returns columns/rows (or an
// error message the console can render). Capped and time-bounded.
func runQuery(addr, sql string) map[string]any {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := pgx.Connect(ctx, fmt.Sprintf("postgres://vectoradb:vectoradb@%s/vectoradb", addr))
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	defer conn.Close(ctx)

	rows, err := conn.Query(ctx, sql)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	defer rows.Close()

	fds := rows.FieldDescriptions()
	cols := make([]string, len(fds))
	for i, f := range fds {
		cols[i] = f.Name
	}
	out := [][]any{}
	for rows.Next() {
		if len(out) >= 1000 {
			break
		}
		vals, err := rows.Values()
		if err != nil {
			return map[string]any{"error": err.Error()}
		}
		row := make([]any, len(vals))
		for i, v := range vals {
			row[i] = cell(v)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return map[string]any{"error": err.Error()}
	}
	return map[string]any{"columns": cols, "rows": out, "command": rows.CommandTag().String()}
}

func cell(v any) any {
	switch t := v.(type) {
	case nil, bool, string, int16, int32, int64, float32, float64:
		return v
	case []byte:
		return string(t)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// cors echoes the specific UI origin and allows credentials (cookies), which
// forbids the "*" wildcard.
func cors(origin string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", allowOrigin(origin, r.Header.Get("Origin")))
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-API-Key")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// allowOrigin echoes the request Origin when it is the configured UI origin or
// any localhost origin (so localhost vs 127.0.0.1 and alternate dev ports all
// work with credentialed CORS); otherwise it falls back to the configured one.
func allowOrigin(configured, reqOrigin string) string {
	if reqOrigin != "" && (reqOrigin == configured || isLocalhostOrigin(reqOrigin)) {
		return reqOrigin
	}
	return configured
}

func isLocalhostOrigin(o string) bool {
	u, err := url.Parse(o)
	if err != nil {
		return false
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if r.URL.Path != "/api/status" && r.URL.Path != "/api/branches" {
			log.Printf("%s %s (%s)", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
		}
	})
}
