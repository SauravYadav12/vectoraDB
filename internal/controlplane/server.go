// SPDX-License-Identifier: AGPL-3.0-or-later

// Package controlplane serves VectoraDB's management REST API (JSON only),
// gated by internal/auth. The web UI is a separate app (see web/).
package controlplane

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/vectoradb/vectoradb/internal/auth"
	"github.com/vectoradb/vectoradb/internal/branch"
	"github.com/vectoradb/vectoradb/internal/daemon"
	"github.com/vectoradb/vectoradb/internal/secrets"
	"github.com/vectoradb/vectoradb/internal/tlsutil"
	"github.com/vectoradb/vectoradb/web"
)

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)

// openapiSpec is the canonical API description, served at GET /api/openapi.yaml
// so any client generator can consume it.
//
//go:embed openapi.yaml
var openapiSpec []byte

// Serve starts the control-plane REST API on addr (e.g. ":8080").
func Serve(addr string) error {
	store, err := auth.OpenFromEnv()
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	store.MountPublic(mux) // /auth/* (register, login, logout, me, providers, oauth)

	// The API description, public so client generators can fetch it. More
	// specific than the "/api/" auth gate below, so it wins and stays open.
	mux.HandleFunc("GET /api/openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(openapiSpec)
	})

	api := http.NewServeMux()
	registerAPI(api)
	registerPipelines(api, store) // /api/pipelines* (ETL)
	store.MountKeys(api)          // /api/keys (protected via Authn below)
	mux.Handle("/api/", store.Authn(api))

	handler := cors(store.WebOrigin())(logging(mux))

	// TLS when a certificate is available (self-signed on first run, or a real
	// one via VECTORADB_TLS_CERT/KEY), so API keys and session tokens are never
	// sent in cleartext. Falls back to HTTP only if the cert can't be loaded.
	scheme := "https"
	cert, key, tlsErr := tlsutil.EnsureCert()
	if tlsErr != nil {
		scheme = "http"
		log.Printf("control-plane TLS disabled (%v) — serving plain HTTP", tlsErr)
	}

	if ui := web.FS(); ui != nil {
		serveUI(mux, ui)
		log.Printf("web UI served at %s://localhost%s/", scheme, addr)
	}

	log.Printf("control-plane API on %s://localhost%s (auth on; UI origin %s)", scheme, addr, store.WebOrigin())
	if tlsErr != nil {
		return http.ListenAndServe(addr, handler)
	}
	return http.ListenAndServeTLS(addr, cert, key, handler)
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
		u, _ := auth.UserFrom(r.Context())
		writeJSON(w, 200, runQuery(addr, body.SQL, u.Email))
	})

	// Migration: import a source database into a new instance from a connection
	// string. The web wizard accepts a URL source (no arbitrary server file paths
	// from a browser); file imports (.sql/.csv/.json) go through /api/import/file.
	mux.HandleFunc("POST /api/import", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Source     string `json:"source"`
			Target     string `json:"target"`
			Continuous bool   `json:"continuous"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		body.Source = strings.TrimSpace(body.Source)
		send, p, ok := newSSE(w)
		if !ok {
			writeErr(w, 500, fmt.Errorf("streaming is not supported by this server"))
			return
		}
		if !isConnString(body.Source) {
			send("error", map[string]string{"message": "web import needs a connection string (postgres://, mysql://, mariadb://, mongodb://); use the file panel for .sql/.csv/.json"})
			return
		}
		status, target, err := "imported", "", error(nil)
		if body.Continuous {
			status = "replicating"
			target, err = branch.ImportContinuousTo(p, body.Source, strings.TrimSpace(body.Target))
		} else {
			target, err = branch.ImportTo(p, body.Source, strings.TrimSpace(body.Target))
		}
		if err != nil {
			send("error", map[string]string{"message": err.Error()})
			return
		}
		send("done", map[string]any{"status": status, "target": target, "tables": branch.TableCount(target)})
	})

	// Migration via file upload: a .sql/.csv/.json file streamed from the browser
	// into a new instance (kind inferred from the filename). Progress streams as SSE.
	mux.HandleFunc("POST /api/import/file", func(w http.ResponseWriter, r *http.Request) {
		file, hdr, ferr := r.FormFile("file")
		send, p, ok := newSSE(w)
		if !ok {
			writeErr(w, 500, fmt.Errorf("streaming is not supported by this server"))
			return
		}
		if ferr != nil {
			send("error", map[string]string{"message": "a file is required"})
			return
		}
		defer file.Close()
		kind, err := branch.ParseKind(filepath.Ext(hdr.Filename))
		if err != nil {
			send("error", map[string]string{"message": err.Error()})
			return
		}
		target, err := branch.ImportReaderTo(p, file, kind, hdr.Filename, strings.TrimSpace(r.FormValue("target")))
		if err != nil {
			send("error", map[string]string{"message": err.Error()})
			return
		}
		send("done", map[string]any{"status": "imported", "target": target, "tables": branch.TableCount(target)})
	})

	// Schema ledger (RECORD layer): the queryable history of DDL changes on a
	// branch, filterable by actor/table/risk/status/kind/time.
	mux.HandleFunc("GET /api/branches/{name}/ledger", func(w http.ResponseWriter, r *http.Request) {
		addr, err := branch.EnsureRunning(r.PathValue("name"))
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		writeJSON(w, 200, runQuery(addr, ledgerSQL(r.URL.Query()), ""))
	})

	// Tamper-evidence: recompute the ledger's hash chain and report whether it
	// is intact (columns: legacy, chained, broken, first_broken).
	mux.HandleFunc("GET /api/branches/{name}/ledger/verify", func(w http.ResponseWriter, r *http.Request) {
		addr, err := branch.EnsureRunning(r.PathValue("name"))
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		writeJSON(w, 200, runQuery(addr, branch.LedgerVerifySQL, ""))
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
	offset := 0
	if n, err := strconv.Atoi(q.Get("offset")); err == nil && n > 0 {
		offset = n
	}
	return fmt.Sprintf(`SELECT to_char(at,'YYYY-MM-DD HH24:MI:SS') AS at, actor, actor_kind, tool,
		branch, command_tag, object_identity, statement, status, risk
		FROM vdb.schema_ledger WHERE %s ORDER BY at DESC LIMIT %d OFFSET %d`,
		strings.Join(where, " AND "), limit, offset)
}

// runQuery executes SQL against a branch backend and returns columns/rows (or an
// error message the console can render). Capped and time-bounded.
func runQuery(addr, sql, actor string) map[string]any {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Connect as the non-superuser client role, so the web console is bound by the
	// same rules as any other client (RLS, and the append-only ledger).
	conn, err := pgx.Connect(ctx,
		fmt.Sprintf("postgres://vdbclient:%s@%s/vectoradb", secrets.Load().PGPassword, addr))
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	defer conn.Close(ctx)

	// Attribute console DDL in the schema ledger to the signed-in user — our own
	// tool should not be the blind spot. Reads pass actor="" and skip this.
	if actor != "" {
		if _, err := conn.Exec(ctx,
			"SELECT set_config('vdb.actor',$1,false), set_config('vdb.actor_kind','human',false), set_config('application_name','console',false)",
			actor); err != nil {
			return map[string]any{"error": err.Error()}
		}
	}

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
	case time.Time:
		return t.Format(time.RFC3339Nano)
	default:
		// jsonb objects/arrays and other composites: render as real JSON so they're
		// readable and copy-pasteable (pgx decodes jsonb into Go maps/slices, which
		// %v would print as `map[$oid:…]`). Fall back to %v if it can't be marshaled.
		if b, err := json.Marshal(v); err == nil {
			return string(b)
		}
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

// registerPipelines mounts the ETL pipeline CRUD + run endpoints. All are behind
// Authn and scoped to the calling user.
func registerPipelines(mux *http.ServeMux, store *auth.Store) {
	mux.HandleFunc("GET /api/pipelines", func(w http.ResponseWriter, r *http.Request) {
		u, _ := auth.UserFrom(r.Context())
		ps, err := store.ListPipelines(u.ID)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, map[string]any{"pipelines": ps})
	})
	mux.HandleFunc("POST /api/pipelines", func(w http.ResponseWriter, r *http.Request) {
		u, _ := auth.UserFrom(r.Context())
		name, spec, err := decodePipelineBody(r)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		p, err := store.CreatePipeline(u.ID, name, spec)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, p)
	})
	mux.HandleFunc("GET /api/pipelines/{id}", func(w http.ResponseWriter, r *http.Request) {
		u, _ := auth.UserFrom(r.Context())
		p, ok := store.GetPipeline(r.PathValue("id"), u.ID)
		if !ok {
			writeErr(w, 404, fmt.Errorf("pipeline not found"))
			return
		}
		writeJSON(w, 200, p)
	})
	mux.HandleFunc("PUT /api/pipelines/{id}", func(w http.ResponseWriter, r *http.Request) {
		u, _ := auth.UserFrom(r.Context())
		name, spec, err := decodePipelineBody(r)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		if err := store.UpdatePipeline(r.PathValue("id"), u.ID, name, spec); err != nil {
			writeErr(w, 404, err)
			return
		}
		writeJSON(w, 200, map[string]string{"status": "updated"})
	})
	mux.HandleFunc("DELETE /api/pipelines/{id}", func(w http.ResponseWriter, r *http.Request) {
		u, _ := auth.UserFrom(r.Context())
		if err := store.DeletePipeline(r.PathValue("id"), u.ID); err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, map[string]string{"status": "deleted"})
	})
	mux.HandleFunc("GET /api/pipelines/{id}/runs", func(w http.ResponseWriter, r *http.Request) {
		u, _ := auth.UserFrom(r.Context())
		runs, err := store.ListRuns(r.PathValue("id"), u.ID)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, map[string]any{"runs": runs})
	})
	// Run a pipeline, streaming its log/progress as SSE (same contract as import).
	mux.HandleFunc("POST /api/pipelines/{id}/run", func(w http.ResponseWriter, r *http.Request) {
		u, _ := auth.UserFrom(r.Context())
		pl, ok := store.GetPipeline(r.PathValue("id"), u.ID)
		send, _, sok := newSSE(w)
		if !sok {
			writeErr(w, 500, fmt.Errorf("streaming is not supported by this server"))
			return
		}
		if !ok {
			send("error", map[string]string{"message": "pipeline not found"})
			return
		}
		var spec branch.PipelineSpec
		if err := json.Unmarshal([]byte(pl.Spec), &spec); err != nil {
			send("error", map[string]string{"message": "invalid pipeline spec: " + err.Error()})
			return
		}
		target := pipelineBranch(pl)
		// Stream the log to the client AND capture it for the run record.
		var logBuf strings.Builder
		p := &branch.Progress{
			Log: writeFunc(func(b []byte) (int, error) {
				logBuf.Write(b)
				for _, line := range strings.Split(string(b), "\n") {
					if line != "" {
						send("log", map[string]string{"line": line})
					}
				}
				return len(b), nil
			}),
			Step: func(done, total int, label string) {
				send("progress", map[string]any{"done": done, "total": total, "label": label})
			},
		}
		runID, _ := store.StartRun(pl.ID, u.ID)
		res, err := branch.RunPipeline(p, spec, target)
		if err != nil {
			_ = store.FinishRun(runID, "error", 0, "[]", logBuf.String())
			send("error", map[string]string{"message": err.Error()})
			return
		}
		status := "success"
		if res.Failed {
			status = "failed"
		}
		testsJSON, _ := json.Marshal(res.Tests)
		_ = store.FinishRun(runID, status, res.Tables, string(testsJSON), logBuf.String())
		send("done", map[string]any{
			"status": status, "target": target, "tables": res.Tables,
			"tests": res.Tests, "failed": res.Failed,
		})
	})
}

// decodePipelineBody reads a {name, spec} body and validates the spec.
func decodePipelineBody(r *http.Request) (name, spec string, err error) {
	var body struct {
		Name string          `json:"name"`
		Spec json.RawMessage `json:"spec"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	name = strings.TrimSpace(body.Name)
	if name == "" {
		return "", "", fmt.Errorf("a pipeline name is required")
	}
	if len(body.Spec) == 0 {
		return "", "", fmt.Errorf("a pipeline spec is required")
	}
	var ps branch.PipelineSpec
	if err := json.Unmarshal(body.Spec, &ps); err != nil {
		return "", "", fmt.Errorf("invalid spec: %w", err)
	}
	if strings.TrimSpace(ps.Source) == "" {
		return "", "", fmt.Errorf("the pipeline spec needs a source connection string")
	}
	return name, string(body.Spec), nil
}

// pipelineBranch derives a stable, connectable instance name for a pipeline's runs.
func pipelineBranch(p auth.Pipeline) string {
	var b strings.Builder
	for _, r := range strings.ToLower(p.Name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	name := strings.Trim(b.String(), "-")
	if name == "" || name[0] < 'a' || name[0] > 'z' {
		name = "pl-" + p.ID
	}
	if len(name) > 40 {
		name = strings.Trim(name[:40], "-")
	}
	return name
}

// serveUI serves the embedded single-page app on "/" (more specific /api/ and
// /auth/ patterns take precedence). Real assets are served from the build; any
// other path falls back to index.html so client-side routes work on refresh.
func serveUI(mux *http.ServeMux, ui fs.FS) {
	fsrv := http.FileServer(http.FS(ui))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p != "" {
			if _, err := fs.Stat(ui, p); err == nil {
				fsrv.ServeHTTP(w, r) // a real asset (JS/CSS/favicon/…)
				return
			}
		}
		b, err := fs.ReadFile(ui, "index.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(b)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

// writeFunc adapts a function into an io.Writer.
type writeFunc func([]byte) (int, error)

func (f writeFunc) Write(b []byte) (int, error) { return f(b) }

// newSSE turns w into a Server-Sent Events stream and returns a `send(event,data)`
// emitter plus a branch.Progress that streams the engine's log lines (as `log`
// events) and item-level steps (as `progress` events). ok is false if the server
// can't stream (no http.Flusher).
func newSSE(w http.ResponseWriter) (send func(string, any), p *branch.Progress, ok bool) {
	fl, ok := w.(http.Flusher)
	if !ok {
		return nil, nil, false
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // don't let a proxy buffer the stream
	w.WriteHeader(http.StatusOK)
	send = func(event string, data any) {
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		fl.Flush()
	}
	p = &branch.Progress{
		Log: writeFunc(func(b []byte) (int, error) {
			for _, line := range strings.Split(string(b), "\n") {
				if line != "" {
					send("log", map[string]string{"line": line})
				}
			}
			return len(b), nil
		}),
		Step: func(done, total int, label string) {
			send("progress", map[string]any{"done": done, "total": total, "label": label})
		},
	}
	return send, p, true
}

// isConnString reports whether s is a database connection URL the import engine
// understands (as opposed to a file path, which the browser must not send).
func isConnString(s string) bool {
	for _, scheme := range []string{"postgres://", "postgresql://", "mysql://", "mariadb://", "mongodb://", "mongodb+srv://"} {
		if strings.HasPrefix(s, scheme) {
			return true
		}
	}
	return false
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
