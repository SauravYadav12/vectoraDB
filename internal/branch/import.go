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
	switch {
	case isPostgresDSN(source):
		return importInstance(target, defaultTargetName(source), "PostgreSQL source (pg_dump)",
			func(t string) error { return loadPostgres(t, source) })
	case hasScheme(source, "mysql", "mariadb"):
		return importInstance(target, defaultTargetName(source), "MySQL/MariaDB source (pgloader)",
			func(t string) error { return loadMySQL(t, source) })
	case hasScheme(source, "mongodb", "mongodb+srv"):
		return importInstance(target, defaultTargetName(source), "MongoDB source (collections → JSONB)",
			func(t string) error { return loadMongo(t, source) })
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
	// Relax the destructive-DDL guardrail for the bulk load — external loaders
	// (pgloader) use their own connections, so PGOPTIONS alone isn't enough. The
	// logging triggers stay on, so the migration is still recorded in the ledger.
	setGuard(target, false)
	defer setGuard(target, true)
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

// ImportContinuous sets up continuous logical replication from a Postgres source
// into a fresh instance: an initial copy followed by streaming changes, so you can
// cut over with zero downtime. The source must allow logical replication
// (wal_level=logical), and the connecting role must be replication-capable and own
// (or be able to read) the tables.
func ImportContinuous(source, target string) (string, error) {
	if !isPostgresDSN(source) {
		return "", fmt.Errorf("--continuous requires a postgres:// source (logical replication)")
	}
	if target == "" {
		target = defaultTargetName(source)
	}
	fmt.Printf("Creating instance %q…\n", target)
	if err := Create(target, "main"); err != nil {
		return "", fmt.Errorf("create target instance %q: %w", target, err)
	}
	setGuard(target, false)
	defer setGuard(target, true)
	if err := prepareTarget(target); err != nil {
		return target, fmt.Errorf("prepare target: %w", err)
	}
	// Copy the schema (structure only) first: logical replication replicates data,
	// not DDL, so the subscriber needs the tables to exist before it can populate
	// them. --no-publications/--no-subscriptions keep the source's own replication
	// objects out of the target.
	fmt.Println("Copying schema (structure only)…")
	schemaScript := fmt.Sprintf("set -euo pipefail; pg_dump %s --schema-only --no-owner --no-acl --no-comments "+
		"--no-publications --no-subscriptions | psql -q -U %s -d %s", shellQuote(source), pgUser, pgDatabase)
	if err := run("docker", "exec", "-e", pgImportOptions, container(target), "bash", "-c", schemaScript); err != nil {
		return target, fmt.Errorf("copy schema from source: %w", err)
	}
	// Best-effort: create a publication covering all tables on the source. If the
	// role lacks privilege or a publication named vdb_pub already exists, continue —
	// the subscription below surfaces any real problem.
	fmt.Println("Ensuring a publication on the source…")
	_ = run("docker", "exec", container(target), "psql", source, "-v", "ON_ERROR_STOP=0",
		"-c", "CREATE PUBLICATION vdb_pub FOR ALL TABLES;")
	// Subscribe on the target: initial copy + streaming changes.
	fmt.Println("Creating subscription (initial copy, then streaming)…")
	sub := fmt.Sprintf("CREATE SUBSCRIPTION vdb_sub CONNECTION %s PUBLICATION vdb_pub;", sqlQuote(source))
	if err := run("docker", "exec", "-e", pgImportOptions, container(target),
		"psql", "-U", pgUser, "-d", pgDatabase, "-v", "ON_ERROR_STOP=1", "-c", sub); err != nil {
		return target, fmt.Errorf("could not start replication — the source must allow logical replication "+
			"(wal_level=logical), expose a replication-capable role, and be reachable from VectoraDB: %w", err)
	}
	fmt.Printf("\n✓ Continuous replication active into %q.\n", target)
	fmt.Println("  The initial copy runs now; subsequent changes stream continuously.")
	fmt.Println("  Progress:  SELECT * FROM pg_stat_subscription;")
	fmt.Printf("  Cut over once caught up:  vdb import-cutover %s\n", target)
	return target, nil
}

// ImportCutover stops the continuous replication started by ImportContinuous,
// leaving the copied data in place so the instance becomes standalone.
func ImportCutover(target string) error {
	fmt.Printf("Finalizing %q — stopping replication, keeping data…\n", target)
	if err := run("docker", "exec", container(target), "psql", "-U", pgUser, "-d", pgDatabase,
		"-c", "DROP SUBSCRIPTION IF EXISTS vdb_sub;"); err != nil {
		return err
	}
	fmt.Printf("✓ %q is now a standalone instance (%d tables).\n", target, TableCount(target))
	return nil
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

// loadMySQL migrates a MySQL/MariaDB source using pgloader (schema + data + type
// mapping), run as a throwaway container on the shared network so it can reach
// both the source and the target branch's Postgres.
func loadMySQL(target, dsn string) error {
	if err := ensurePgloaderImage(); err != nil {
		return fmt.Errorf("prepare pgloader: %w", err)
	}
	dsn = strings.Replace(dsn, "mariadb://", "mysql://", 1)
	pgTarget := fmt.Sprintf("postgresql://%s:%s@%s:5432/%s", pgUser, pgPassword, container(target), pgDatabase)
	fmt.Println("  running pgloader (schema + data + type mapping)…")
	// pgloader logs a fatal connection error but still exits 0, so inspect its
	// output rather than trusting the exit code alone.
	out, err := exec.Command("sudo", "docker", "run", "--rm", "--network", network,
		pgloaderImage, "pgloader", dsn, pgTarget).CombinedOutput()
	fmt.Print(string(out))
	if err != nil {
		return fmt.Errorf("pgloader: %w", err)
	}
	if strings.Contains(string(out), "Failed to connect") || strings.Contains(string(out), "UNSUPPORTED-AUTHENTICATION") {
		return fmt.Errorf("pgloader could not read the source (see log above); for MySQL 8, connect as a user with mysql_native_password authentication")
	}
	return nil
}

// ensurePgloaderImage builds the local, arch-native pgloader image if it is not
// already present. Idempotent; the build runs only on the first migration.
func ensurePgloaderImage() error {
	if exec.Command("sudo", "docker", "image", "inspect", pgloaderImage).Run() == nil {
		return nil
	}
	fmt.Println("  building the pgloader image (first run only)…")
	dockerfile := "FROM debian:stable-slim\n" +
		"RUN apt-get update && apt-get install -y --no-install-recommends pgloader ca-certificates && rm -rf /var/lib/apt/lists/*\n"
	cmd := exec.Command("sudo", "docker", "build", "-t", pgloaderImage, "-")
	cmd.Stdin = strings.NewReader(dockerfile)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// loadMongo migrates every collection in a MongoDB source into its own JSONB
// table, using the mongo image's shell to enumerate and export documents.
func loadMongo(target, uri string) error {
	cols, err := mongoCollections(uri)
	if err != nil {
		return err
	}
	if len(cols) == 0 {
		return fmt.Errorf("no collections found at the source")
	}
	fmt.Printf("  %d collection(s): %s\n", len(cols), strings.Join(cols, ", "))
	for _, c := range cols {
		fmt.Printf("  collection %q → table %q…\n", c, sanitizeIdent(c))
		if err := mongoImportCollection(target, uri, c); err != nil {
			return fmt.Errorf("collection %q: %w", c, err)
		}
	}
	return nil
}

func mongoCollections(uri string) ([]string, error) {
	out, err := exec.Command("sudo", "docker", "run", "--rm", "--network", network, mongoImage,
		"mongosh", uri, "--quiet", "--eval", "db.getCollectionNames().forEach(c=>print(c))").Output()
	if err != nil {
		return nil, fmt.Errorf("could not reach the MongoDB source: %w", err)
	}
	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l := strings.TrimSpace(line); l != "" && !strings.HasPrefix(l, "Warning") {
			cols = append(cols, l)
		}
	}
	return cols, nil
}

func mongoImportCollection(target, uri, coll string) error {
	// Stream each document as one JSON line (extended JSON) into the JSON loader.
	script := fmt.Sprintf("db.getCollection(%s).find().forEach(d=>print(EJSON.stringify(d)))", jsStr(coll))
	cmd := exec.Command("sudo", "docker", "run", "--rm", "--network", network, mongoImage,
		"mongosh", uri, "--quiet", "--eval", script)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	if err := loadJSON(target, stdout, sanitizeIdent(coll)); err != nil {
		_ = cmd.Wait()
		return err
	}
	return cmd.Wait()
}

// setGuard enables/disables the ledger's destructive-DDL guardrail on an instance.
func setGuard(target string, enabled bool) {
	verb := "DISABLE"
	if enabled {
		verb = "ENABLE"
	}
	_ = run("docker", "exec", container(target), "psql", "-q", "-U", pgUser, "-d", pgDatabase,
		"-c", fmt.Sprintf("ALTER EVENT TRIGGER vdb_guard_start %s;", verb))
}

func hasScheme(s string, schemes ...string) bool {
	for _, sc := range schemes {
		if strings.HasPrefix(s, sc+"://") {
			return true
		}
	}
	return false
}

func jsStr(s string) string { b, _ := json.Marshal(s); return string(b) }

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

// sqlQuote renders s as a single-quoted SQL string literal.
func sqlQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

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
