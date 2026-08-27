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
	"path/filepath"
	"regexp"
	"strconv"
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

	// Migration: import a source database into a new instance. The web wizard
	// only accepts a postgres:// source (no arbitrary server file paths from a
	// browser); file imports (.sql/.csv/.json) go through the `vdb import` CLI.
	mux.HandleFunc("POST /api/import", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Source string `json:"source"`
			Target string `json:"target"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		body.Source = strings.TrimSpace(body.Source)
		if !strings.HasPrefix(body.Source, "postgres://") && !strings.HasPrefix(body.Source, "postgresql://") {
			writeErr(w, 400, fmt.Errorf("web import needs a postgres:// source; use `vdb import` for .sql/.csv/.json files"))
			return
		}
		target, err := branch.Import(body.Source, strings.TrimSpace(body.Target))
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, map[string]any{"status": "imported", "target": target, "tables": branch.TableCount(target)})
	})

	// Migration via file upload: a .sql/.csv/.json file streamed from the browser
	// into a new instance (kind inferred from the filename).
	mux.HandleFunc("POST /api/import/file", func(w http.ResponseWriter, r *http.Request) {
		file, hdr, err := r.FormFile("file")
		if err != nil {
			writeErr(w, 400, fmt.Errorf("a file is required"))
			return
		}
		defer file.Close()
		kind, err := branch.ParseKind(filepath.Ext(hdr.Filename))
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		target, err := branch.ImportReader(file, kind, hdr.Filename, strings.TrimSpace(r.FormValue("target")))
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, map[string]any{"status": "imported", "target": target, "tables": branch.TableCount(target)})
	})

	// Schema ledger (RECORD layer): the queryable history of DDL changes on a
	// branch, filterable by actor/table/risk/status/kind/time.
	mux.HandleFunc("GET /api/branches/{name}/ledger", func(w http.ResponseWriter, r *http.Request) {
		addr, err := branch.EnsureRunning(r.PathValue("name"))
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		writeJSON(w, 200, runQuery(addr, ledgerSQL(r.URL.Query())))
	})
}

func sqlEsc(s string) string { return strings.ReplaceAll(s, "'", "''") }

// ledgerSQL builds a filtered, bounded query over vdb.schema_ledger. Filter
// values are single-quote-escaped and only ever appear as string literals.
func ledgerSQL(q url.Values) string {
	where := []string{"true"}
	like := func(col, v string) {
		if v = strings.TrimSpace(v); v != "" {
			where = append(where, fmt.Sprintf("%s ILIKE '%%%s%%'", col, sqlEsc(v)))
		}
	}
	eq := func(col, v string) {
		if v = strings.TrimSpace(v); v != "" {
			where = append(where, fmt.Sprintf("%s = '%s'", col, sqlEsc(v)))
		}
	}
	like("actor", q.Get("actor"))
	like("object_identity", q.Get("table"))
	eq("risk", q.Get("risk"))
	eq("status", q.Get("status"))
	eq("actor_kind", q.Get("kind"))
	if v := strings.TrimSpace(q.Get("since")); v != "" {
		where = append(where, fmt.Sprintf("at >= '%s'", sqlEsc(v)))
	}
	if v := strings.TrimSpace(q.Get("until")); v != "" {
		where = append(where, fmt.Sprintf("at <= '%s'", sqlEsc(v)))
	}
	limit := 200
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 && n <= 2000 {
		limit = n
	}
	return fmt.Sprintf(`SELECT to_char(at,'YYYY-MM-DD HH24:MI:SS') AS at, actor, actor_kind, tool,
		branch, command_tag, object_identity, statement, status, risk
		FROM vdb.schema_ledger WHERE %s ORDER BY at DESC LIMIT %d`,
		strings.Join(where, " AND "), limit)
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
