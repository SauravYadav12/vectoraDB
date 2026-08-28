// SPDX-License-Identifier: AGPL-3.0-or-later

package branch

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
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
	// pgloader can't read MySQL 8.x (its caching_sha2_password handshake and its
	// information_schema layout), so route those to a mysqldump-based path whose
	// client speaks the modern protocol. MariaDB and MySQL ≤5.7 stay on pgloader,
	// which maps their indexes/constraints more richly.
	if mysqlNeedsNative(dsn) {
		return loadMySQLNative(target, dsn)
	}
	if err := ensurePgloaderImage(); err != nil {
		return fmt.Errorf("prepare pgloader: %w", err)
	}
	dsn = strings.Replace(dsn, "mariadb://", "mysql://", 1)
	pgTarget := fmt.Sprintf("postgresql://%s:%s@%s:5432/%s", pgUser, pgPassword, container(target), pgDatabase)
	fmt.Println("  running pgloader (schema + data + type mapping)…")
	// pgloader exits 0 even when it fails outright (a bad connection) or silently
	// loads nothing (e.g. it can't read a MySQL 8.0 source's metadata), so the exit
	// code can't be trusted. Show the operator its log, then treat "no tables
	// landed" as the real success signal.
	out, err := exec.Command("sudo", "docker", "run", "--rm", "--network", network,
		pgloaderImage, "pgloader", dsn, pgTarget).CombinedOutput()
	fmt.Print(string(out))
	if err != nil {
		return fmt.Errorf("pgloader: %w", err)
	}
	if TableCount(target) == 0 {
		if strings.Contains(string(out), "Failed to connect") || strings.Contains(string(out), "UNSUPPORTED-AUTHENTICATION") {
			return fmt.Errorf("pgloader could not authenticate to the source (see log above); MySQL 8 needs the server started with mysql_native_password — its default caching_sha2_password handshake is unsupported")
		}
		return fmt.Errorf("pgloader loaded no tables (see log above); the source may be empty or unreadable — or export it to a .sql/.csv/.json file and run `vdb import` on that")
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

// --- MySQL 8.x native path (mysqldump; the client speaks caching_sha2_password) ---

type myDSN struct{ host, port, user, pass, db string }

func parseMyDSN(dsn string) myDSN {
	u, err := url.Parse(dsn)
	if err != nil {
		return myDSN{}
	}
	c := myDSN{host: u.Hostname(), port: u.Port(), db: strings.TrimPrefix(u.Path, "/")}
	if c.port == "" {
		c.port = "3306"
	}
	if u.User != nil {
		c.user = u.User.Username()
		c.pass, _ = u.User.Password()
	}
	return c
}

// myArgs builds a `docker run … mysql:8 <tool> -h… -u…` argument list.
func myArgs(c myDSN, tool string, extra ...string) []string {
	a := []string{"docker", "run", "--rm", "--network", network, mysqlImage, tool,
		"-h", c.host, "-P", c.port, "-u", c.user}
	if c.pass != "" {
		a = append(a, "-p"+c.pass)
	}
	return append(a, extra...)
}

// mysqlNeedsNative reports whether a source needs the mysqldump path: true only
// for MySQL ≥8. MariaDB and MySQL ≤5.7 (and an unreachable probe) return false so
// pgloader handles them. stdout carries only the version; warnings go to stderr.
func mysqlNeedsNative(dsn string) bool {
	c := parseMyDSN(dsn)
	if c.host == "" {
		return false
	}
	out, err := exec.Command("sudo", myArgs(c, "mysql", "-N", "-B", "-e", "SELECT VERSION()")...).Output()
	if err != nil {
		return false
	}
	v := strings.ToLower(strings.TrimSpace(string(out)))
	if v == "" || strings.Contains(v, "mariadb") {
		return false
	}
	maj := 0
	for _, ch := range v {
		if ch < '0' || ch > '9' {
			break
		}
		maj = maj*10 + int(ch-'0')
	}
	return maj >= 8
}

// loadMySQLNative imports a MySQL 8.x source: it reads the schema from
// information_schema (reliable across versions) and streams data via mysqldump,
// translating MySQL's dialect/escaping for Postgres.
func loadMySQLNative(target, dsn string) error {
	c := parseMyDSN(dsn)
	if c.db == "" {
		return fmt.Errorf("the MySQL connection string must include a database name (…/dbname)")
	}
	fmt.Println("  MySQL 8.x source — importing via mysqldump (pgloader can't read this version)…")
	tables, err := mysqlTables(c)
	if err != nil {
		return err
	}
	if len(tables) == 0 {
		return fmt.Errorf("no tables found in database %q", c.db)
	}
	fmt.Printf("  %d table(s): %s\n", len(tables), strings.Join(tables, ", "))

	// 1. Schema — build CREATE TABLE from information_schema.
	var ddl strings.Builder
	for _, t := range tables {
		stmt, err := mysqlCreateTable(c, t)
		if err != nil {
			return fmt.Errorf("read schema of %q: %w", t, err)
		}
		ddl.WriteString(stmt)
	}
	if err := pipeInto(target, strings.NewReader(ddl.String()),
		"psql", "-q", "-U", pgUser, "-d", pgDatabase, "-v", "ON_ERROR_STOP=1"); err != nil {
		return fmt.Errorf("create tables: %w", err)
	}

	// 2. Data — mysqldump (ANSI: double-quoted identifiers, one row per INSERT),
	// loaded with MySQL-style backslash escaping enabled for the session.
	fmt.Println("  copying rows…")
	data, err := mysqldumpData(c)
	if err != nil {
		return err
	}
	preamble := "SET client_encoding='UTF8';\nSET standard_conforming_strings=off;\nSET backslash_quote=on;\nSET client_min_messages=warning;\n"
	if err := pipeInto(target, strings.NewReader(preamble+data),
		"psql", "-q", "-U", pgUser, "-d", pgDatabase, "-v", "ON_ERROR_STOP=1"); err != nil {
		return fmt.Errorf("load rows: %w", err)
	}
	return nil
}

func mysqlTables(c myDSN) ([]string, error) {
	q := fmt.Sprintf("SELECT table_name FROM information_schema.tables WHERE table_schema=%s AND table_type='BASE TABLE' ORDER BY table_name", sqlQuote(c.db))
	out, err := exec.Command("sudo", myArgs(c, "mysql", "-N", "-B", "-e", q)...).Output()
	if err != nil {
		return nil, fmt.Errorf("could not reach the MySQL source: %w", err)
	}
	var t []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			t = append(t, s)
		}
	}
	return t, nil
}

// mysqlCreateTable renders a Postgres CREATE TABLE for one source table. mysqldump
// emits full-row INSERTs (no column list), so column order here must match the
// source's ordinal_position — and generated columns are skipped in both places.
func mysqlCreateTable(c myDSN, table string) (string, error) {
	q := fmt.Sprintf(`SELECT column_name, data_type, column_type, is_nullable, column_key, extra `+
		`FROM information_schema.columns WHERE table_schema=%s AND table_name=%s ORDER BY ordinal_position`,
		sqlQuote(c.db), sqlQuote(table))
	out, err := exec.Command("sudo", myArgs(c, "mysql", "-N", "-B", "-e", q)...).Output()
	if err != nil {
		return "", err
	}
	var defs, pk []string
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 6 {
			continue
		}
		name, dtype, ctype := f[0], strings.ToLower(f[1]), strings.ToLower(f[2])
		nullable, key, extra := f[3], f[4], strings.ToUpper(f[5])
		// Skip only true generated columns (VIRTUAL/STORED), which mysqldump omits
		// from INSERTs — NOT "DEFAULT_GENERATED", which just marks a column default.
		if strings.Contains(extra, "VIRTUAL GENERATED") || strings.Contains(extra, "STORED GENERATED") {
			continue
		}
		def := pgIdent(name) + " " + mapMySQLType(dtype, ctype)
		if nullable == "NO" {
			def += " NOT NULL"
		}
		defs = append(defs, def)
		if key == "PRI" {
			pk = append(pk, pgIdent(name))
		}
	}
	if len(defs) == 0 {
		return "", fmt.Errorf("no columns")
	}
	body := strings.Join(defs, ", ")
	if len(pk) > 0 {
		body += ", PRIMARY KEY (" + strings.Join(pk, ", ") + ")"
	}
	return fmt.Sprintf("CREATE TABLE %s (%s);\n", pgIdent(table), body), nil
}

// mapMySQLType maps a MySQL column type to a Postgres type. It preserves numeric
// precision and widens for unsigned ranges; text/binary/enum and anything unknown
// fall back to text so a data migration never fails on a length or type mismatch.
func mapMySQLType(dataType, columnType string) string {
	unsigned := strings.Contains(columnType, "unsigned")
	switch dataType {
	case "tinyint":
		return "smallint"
	case "smallint":
		if unsigned {
			return "integer"
		}
		return "smallint"
	case "mediumint", "year":
		return "integer"
	case "int", "integer":
		if unsigned {
			return "bigint"
		}
		return "integer"
	case "bigint":
		if unsigned {
			return "numeric"
		}
		return "bigint"
	case "decimal", "numeric":
		if i := strings.Index(columnType, "("); i >= 0 {
			return "numeric" + columnType[i:strings.Index(columnType, ")")+1]
		}
		return "numeric"
	case "float":
		return "real"
	case "double", "real":
		return "double precision"
	case "bit":
		return "smallint"
	case "date":
		return "date"
	case "datetime", "timestamp":
		return "timestamp"
	case "time":
		return "time"
	case "json":
		return "jsonb"
	default: // char/varchar/text/enum/set/blob/binary/… → text
		return "text"
	}
}

// mysqldumpData returns the source's data as one INSERT statement per row, with
// ANSI-quoted identifiers and MySQL-style value escaping. Non-INSERT lines
// (LOCK/UNLOCK/SET/comments) are dropped; each INSERT is a single physical line.
func mysqldumpData(c myDSN) (string, error) {
	args := []string{"docker", "run", "--rm", "--network", network, mysqlImage, "mysqldump",
		"-h", c.host, "-P", c.port, "-u", c.user}
	if c.pass != "" {
		args = append(args, "-p"+c.pass)
	}
	args = append(args, "--no-create-info", "--compatible=ansi", "--skip-extended-insert",
		"--no-tablespaces", "--skip-comments", "--skip-set-charset", "--default-character-set=utf8mb4", c.db)
	out, err := exec.Command("sudo", args...).Output()
	if err != nil {
		return "", fmt.Errorf("mysqldump failed: %w", err)
	}
	var b strings.Builder
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "INSERT INTO ") {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return b.String(), nil
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

// pgIdent renders s as a double-quoted SQL identifier (matching mysqldump's ANSI
// quoting so the generated schema and the dumped INSERTs agree on names).
func pgIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

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
