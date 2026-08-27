// SPDX-License-Identifier: AGPL-3.0-or-later

package branch

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Migration into VectoraDB. Because VectoraDB *is* PostgreSQL, "migrate to
// VectoraDB" always means "land the source in a fresh Postgres instance".
//
//   - a Postgres (or Postgres-wire) source → pg_dump | psql, full fidelity
//   - a .sql dump                          → psql
//   - a .csv                               → a table (text columns) via COPY
//   - a .json / .ndjson                    → a table of JSONB documents
//
// The loaders are stream-based, so the same code serves a local file path
// (CLI), stdin (a file streamed from the client machine), and a browser upload.

const pgImportOptions = "PGOPTIONS=-c vdb.allow_destructive=on"

type sourceKind int

const (
	srcSQL sourceKind = iota
	srcCSV
	srcJSON
)

// Import loads a Postgres source (postgres://…) or a local file (.sql/.csv/.json)
// into a new instance named target, returning the resolved instance name.
func Import(source, target string) (string, error) {
	if isPostgresDSN(source) {
		return importInstance(target, defaultTargetName(source), "PostgreSQL source (pg_dump)",
			func(t string) error { return loadPostgres(t, source) })
	}
	info, err := os.Stat(source)
	if err != nil || info.IsDir() {
		return "", fmt.Errorf("source must be a postgres:// connection string or a readable .sql/.csv/.json file (got %q)", source)
	}
	kind, err := kindFromExt(filepath.Ext(source))
	if err != nil {
		return "", err
	}
	f, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return ImportReader(f, kind, filepath.Base(source), target)
}

// ImportReader loads a stream (a .sql/.csv/.json file's contents, from stdin or a
// browser upload) into a new instance. srcname names the origin, used for the
// default instance name and the target table for CSV/JSON.
func ImportReader(r io.Reader, kind sourceKind, srcname, target string) (string, error) {
	base := sanitizeIdent(strings.TrimSuffix(srcname, filepath.Ext(srcname)))
	return importInstance(target, "import-"+base, describeKind(kind, srcname), func(t string) error {
		switch kind {
		case srcSQL:
			return loadSQL(t, r)
		case srcCSV:
			return loadCSV(t, r, base)
		case srcJSON:
			return loadJSON(t, r, base)
		}
		return fmt.Errorf("unsupported source kind")
	})
}

// importInstance creates the target instance, gives it a clean public schema,
// runs the loader, and prints a summary.
func importInstance(target, defName, desc string, load func(string) error) (string, error) {
	if target == "" {
		target = defName
	}
	fmt.Printf("Creating instance %q…\n", target)
	if err := Create(target, "main"); err != nil {
		return "", fmt.Errorf("create target instance %q: %w", target, err)
	}
	if err := prepareTarget(target); err != nil {
		return target, fmt.Errorf("prepare target: %w", err)
	}
	fmt.Printf("Importing %s → %q…\n", desc, target)
	if err := load(target); err != nil {
		return target, fmt.Errorf("import failed: %w", err)
	}
	fmt.Printf("\n✓ Imported into instance %q — %d table(s) now present.\n", target, TableCount(target))
	fmt.Printf("  Connect: postgres://vectoradb:<API_KEY>@localhost:6432/%s\n", target)
	fmt.Printf("  Browse:  the web console (Console / Ledger), branch = %q\n", target)
	return target, nil
}

// TableCount returns the number of user tables in an instance (excludes system
// schemas and the vdb ledger schema).
func TableCount(name string) int {
	out, _ := capture("docker", "exec", container(name),
		"psql", "-U", pgUser, "-d", pgDatabase, "-tAc",
		`SELECT count(*) FROM information_schema.tables
		 WHERE table_schema NOT IN ('pg_catalog','information_schema','vdb')`)
	n, _ := strconv.Atoi(strings.TrimSpace(out))
	return n
}

// --- source detection ---

func isPostgresDSN(s string) bool {
	return strings.HasPrefix(s, "postgres://") || strings.HasPrefix(s, "postgresql://")
}

func kindFromExt(ext string) (sourceKind, error) {
	switch strings.ToLower(ext) {
	case ".sql":
		return srcSQL, nil
	case ".csv":
		return srcCSV, nil
	case ".json", ".ndjson", ".jsonl":
		return srcJSON, nil
	}
	return 0, fmt.Errorf("unsupported file type %q — supported: .sql, .csv, .json", ext)
}

// ParseKind maps a short kind name ("sql"/"csv"/"json") to a source kind, for
// streamed (stdin / upload) imports where there is no filename to inspect.
func ParseKind(s string) (sourceKind, error) {
	return kindFromExt("." + strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), "."))
}

func describeKind(k sourceKind, name string) string {
	switch k {
	case srcSQL:
		return "SQL dump " + name
	case srcCSV:
		return "CSV " + name
	case srcJSON:
		return "JSON " + name + " (→ JSONB)"
	}
	return name
}

func defaultTargetName(dsn string) string {
	db := dsn
	if i := strings.LastIndex(db, "/"); i >= 0 {
		db = db[i+1:]
	}
	if j := strings.IndexAny(db, "?"); j >= 0 {
		db = db[:j]
	}
	if db == "" {
		return "import-db"
	}
	return "import-" + sanitizeIdent(db)
}

// --- loaders (all stream-based) ---

// prepareTarget gives the new instance a clean public schema, independent of
// main's current contents (the vdb ledger schema is left intact).
func prepareTarget(target string) error {
	return run("docker", "exec", "-e", pgImportOptions, container(target),
		"psql", "-q", "-U", pgUser, "-d", pgDatabase, "-c",
		"DROP SCHEMA IF EXISTS public CASCADE; CREATE SCHEMA public;")
}

func loadPostgres(target, dsn string) error {
	script := fmt.Sprintf("set -euo pipefail; pg_dump %s --no-owner --no-acl --no-comments | psql -q -U %s -d %s",
		shellQuote(dsn), pgUser, pgDatabase)
	return run("docker", "exec", "-e", pgImportOptions, container(target), "bash", "-c", script)
}

func loadSQL(target string, r io.Reader) error {
	return pipeInto(target, r, "psql", "-q", "-U", pgUser, "-d", pgDatabase)
}

func loadCSV(target string, r io.Reader, table string) error {
	br := bufio.NewReader(r)
	headerLine, err := br.ReadString('\n')
	if err != nil && headerLine == "" {
		return fmt.Errorf("empty CSV")
	}
	cols, err := csv.NewReader(strings.NewReader(headerLine)).Read()
	if err != nil || len(cols) == 0 {
		return fmt.Errorf("could not parse CSV header")
	}
	defs := make([]string, len(cols))
	for i, c := range cols {
		defs[i] = fmt.Sprintf("%q text", sanitizeIdent(c))
	}
	if err := run("docker", "exec", "-e", pgImportOptions, container(target),
		"psql", "-q", "-U", pgUser, "-d", pgDatabase, "-c",
		fmt.Sprintf("CREATE TABLE IF NOT EXISTS %q (%s);", table, strings.Join(defs, ", "))); err != nil {
		return err
	}
	fmt.Printf("  loading rows into table %q…\n", table)
	// The header line is already consumed, so COPY the remaining rows (HEADER false).
	return pipeInto(target, br, "psql", "-q", "-U", pgUser, "-d", pgDatabase, "-c",
		fmt.Sprintf(`\copy %q FROM STDIN WITH (FORMAT csv, HEADER false)`, table))
}

func loadJSON(target string, r io.Reader, table string) error {
	if err := run("docker", "exec", "-e", pgImportOptions, container(target),
		"psql", "-q", "-U", pgUser, "-d", pgDatabase, "-c",
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %q (id bigserial PRIMARY KEY, doc jsonb);`, table)); err != nil {
		return err
	}
	br := bufio.NewReader(r)
	first, err := peekNonSpace(br)
	if err != nil {
		return fmt.Errorf("empty JSON")
	}
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		w := bufio.NewWriter(pw)
		fmt.Fprintln(w, "BEGIN;")
		emit := func(raw string) {
			if raw = strings.TrimSpace(raw); raw != "" {
				fmt.Fprintf(w, `INSERT INTO %q(doc) VALUES ('%s'::jsonb);`+"\n", table, strings.ReplaceAll(raw, "'", "''"))
			}
		}
		if first == '[' { // a JSON array
			dec := json.NewDecoder(br)
			dec.Token() // consume '['
			for dec.More() {
				var m json.RawMessage
				if dec.Decode(&m) != nil {
					break
				}
				emit(string(m))
			}
		} else { // newline-delimited JSON (e.g. mongoexport)
			sc := bufio.NewScanner(br)
			sc.Buffer(make([]byte, 1<<20), 64<<20)
			for sc.Scan() {
				emit(sc.Text())
			}
		}
		fmt.Fprintln(w, "COMMIT;")
		w.Flush()
	}()
	fmt.Printf("  loading JSON documents into %q(doc jsonb)…\n", table)
	return pipeInto(target, pr, "psql", "-q", "-U", pgUser, "-d", pgDatabase, "-v", "ON_ERROR_STOP=1")
}

// pipeInto streams r into a command run inside the target's container.
func pipeInto(target string, r io.Reader, argv ...string) error {
	full := append([]string{"exec", "-i", "-e", pgImportOptions, container(target)}, argv...)
	cmd := exec.Command("sudo", append([]string{"docker"}, full...)...)
	cmd.Stdin = r
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// --- small helpers ---

func peekNonSpace(br *bufio.Reader) (byte, error) {
	for {
		b, err := br.Peek(1)
		if err != nil {
			return 0, err
		}
		switch b[0] {
		case ' ', '\n', '\r', '\t':
			_, _ = br.ReadByte()
		default:
			return b[0], nil
		}
	}
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func sanitizeIdent(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" || (out[0] >= '0' && out[0] <= '9') {
		out = "t_" + out
	}
	if len(out) > 63 {
		out = out[:63]
	}
	return out
}
