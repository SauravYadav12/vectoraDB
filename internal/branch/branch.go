// SPDX-License-Identifier: AGPL-3.0-or-later

// Package branch implements instant copy-on-write database branching on ZFS.
//
// Each branch is a `zfs clone` of a parent dataset, served by its own Postgres
// container. Cloning is O(1) and space-efficient (copy-on-write), so a branch
// is created in seconds regardless of database size — the parent is untouched
// and the branch only stores the blocks it changes.
//
// This runs inside the Linux dev VM (ZFS + Docker). zfs and docker need root
// there, so privileged commands are run through sudo (passwordless in Lima).
package branch

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/vectoradb/vectoradb/internal/ledger"
	"github.com/vectoradb/vectoradb/internal/secrets"
)

const (
	datasetBase = "vectoradb/branches"
	mountBase   = "/vectoradb/branches"
	network     = "vectoradb"
	image       = "vectoradb/postgres-walg:16"
	// Throwaway loader images used by the migration adapters, run on the shared
	// network so they can reach both the source and the target instance.
	// pgloaderImage is built locally on first use — Debian packages pgloader for
	// both amd64 and arm64, unlike the amd64-only Docker Hub image.
	pgloaderImage = "vectoradb/pgloader:local" // MariaDB / MySQL ≤5.7 → Postgres
	mysqlImage    = "mysql:8"                  // client + mysqldump; speaks the MySQL 8.x protocol pgloader can't
	mongoImage    = "mongo:7"                  // ships mongosh for enumerating/exporting collections
	pgUser        = "vectoradb"
	pgDatabase    = "vectoradb"
	pgUID         = "999" // the postgres user's uid inside the official image
)

// Credentials are generated per install (internal/secrets), not hardcoded. The
// engine sets them on the containers it starts; the gateway reads the same
// Postgres password to authenticate to the backend.
func pgPass() string    { return secrets.Load().PGPassword }
func minioUser() string { return secrets.Load().MinioUser }
func minioPass() string { return secrets.Load().MinioPassword }

func dataset(name string) string    { return datasetBase + "/" + name }
func mountpoint(name string) string { return mountBase + "/" + name }
func container(name string) string  { return "vec-" + name }
func snapFor(parent, name string) string {
	return dataset(parent) + "@for-" + name
}

// run executes a privileged command and streams output to the terminal.
func run(name string, args ...string) error {
	cmd := exec.Command("sudo", append([]string{name}, args...)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// quiet executes a privileged command, discarding output and errors. Used for
// best-effort cleanup where "not found" is not a failure.
func quiet(name string, args ...string) {
	_ = exec.Command("sudo", append([]string{name}, args...)...).Run()
}

// capture runs a privileged command and returns its trimmed stdout.
func capture(name string, args ...string) (string, error) {
	out, err := exec.Command("sudo", append([]string{name}, args...)...).Output()
	return strings.TrimSpace(string(out)), err
}

func datasetExists(ds string) bool {
	return exec.Command("sudo", "zfs", "list", "-H", "-o", "name", ds).Run() == nil
}

// ensureNetwork creates the shared docker network (idempotent) so Postgres
// containers can reach MinIO by name.
func ensureNetwork() error {
	if exec.Command("sudo", "docker", "network", "inspect", network).Run() == nil {
		return nil
	}
	return run("docker", "network", "create", network)
}

// startContainer (re)starts the Postgres container for a branch, bind-mounting
// its ZFS dataset as the data directory. The primary ("main") additionally
// archives WAL to object storage (MinIO); branches do not archive — they are
// ephemeral copy-on-write clones.
func startContainer(name string, primary bool) error {
	quiet("docker", "rm", "-f", container(name))
	args := []string{"run", "-d",
		"--name", container(name),
		"--network", network,
		"-e", "POSTGRES_USER=" + pgUser,
		"-e", "POSTGRES_PASSWORD=" + pgPass(),
		"-e", "POSTGRES_DB=" + pgDatabase,
		"-e", "PGDATA=/var/lib/postgresql/data/pgdata",
		"-v", mountpoint(name) + ":/var/lib/postgresql/data",
	}
	// Branch Postgres is reached by the in-guest gateway over the docker network
	// (BackendAddr -> containerIP), so no host port is published by default. That
	// keeps branch databases unreachable from outside the VM, where a direct
	// connection would bypass the gateway, its API key, TLS, and ledger
	// attribution. VECTORADB_DEBUG_PORTS publishes a host port for debugging.
	if os.Getenv("VECTORADB_DEBUG_PORTS") != "" {
		if primary {
			args = append(args, "-p", "5432:5432")
		} else {
			args = append(args, "-p", "0:5432")
		}
	}
	if primary {
		args = append(args,
			"-e", "WALG_S3_PREFIX=s3://vectoradb-wal",
			"-e", "AWS_ACCESS_KEY_ID="+minioUser(),
			"-e", "AWS_SECRET_ACCESS_KEY="+minioPass(),
			"-e", "AWS_ENDPOINT=http://minio:9000",
			"-e", "AWS_S3_FORCE_PATH_STYLE=true",
			"-e", "AWS_REGION=us-east-1",
			"-e", "WALG_COMPRESSION_METHOD=lz4",
		)
	}
	args = append(args, image)
	if primary {
		args = append(args,
			"postgres",
			"-c", "wal_level=replica",
			"-c", "archive_mode=on",
			"-c", "archive_command=wal-g wal-push %p",
			"-c", "archive_timeout=60",
			"-c", "listen_addresses=*",
		)
	}
	return run("docker", args...)
}

func waitReady(name string) error {
	// Poll frequently: a resumed branch's Postgres is usually accepting in ~0.4s,
	// so a coarse sleep between probes dominates the auto-resume wake time. The
	// probe (docker exec pg_isready) is itself the main cost per cycle.
	deadline := time.Now().Add(60 * time.Second)
	for {
		if exec.Command("sudo", "docker", "exec", container(name),
			"pg_isready", "-U", pgUser, "-d", pgDatabase).Run() == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("branch %q did not become ready in time", name)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Init creates the primary "main" dataset (if absent) and starts its Postgres
// container. On first run Postgres initializes a fresh cluster on the dataset.
func Init() error {
	if err := ensureNetwork(); err != nil {
		return err
	}
	if ContainerState("main") == "running" {
		if err := syncRolePassword("main"); err != nil { // ensure the role matches the generated secret
			return err
		}
		activeStorage().protectPrimary()
		return InstallLedger("main") // already up — ensure the ledger is present
	}
	store := activeStorage()
	if !store.exists("main") {
		if err := store.createEmpty("main"); err != nil {
			return err
		}
	}
	if err := run("chown", "-R", pgUID+":"+pgUID, mountpoint("main")); err != nil {
		return err
	}
	if err := startContainer("main", true); err != nil {
		return err
	}
	if err := waitReady("main"); err != nil {
		return err
	}
	// The data dir may have been initialized with a different POSTGRES_PASSWORD
	// (an older install, or before per-install secrets existed). Sync the role
	// password to the generated secret so the gateway's TCP login matches.
	if err := syncRolePassword("main"); err != nil {
		return err
	}
	// Reserve pool space for the primary so branches can't take it read-only
	// (idempotent — also applies on an upgrade of an existing install).
	store.protectPrimary()
	// Install the schema ledger into main; every branch (a ZFS clone) inherits it.
	return InstallLedger("main")
}

// syncRolePassword sets the Postgres role password to the per-install secret.
// It connects over the container's local socket (trust auth), so it works even
// when the stored password differs from the current secret.
func syncRolePassword(name string) error {
	return psqlStdin(name, fmt.Sprintf("ALTER USER %s WITH PASSWORD %s;", pgUser, quoteLiteral(pgPass())))
}

// quoteLiteral renders s as a single-quoted SQL string literal (doubling any
// embedded quotes). The generated password is hex, but quote defensively.
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// psqlStdin runs a (possibly multi-statement) SQL script on a branch's Postgres
// by piping it to psql over stdin, aborting on the first error.
func psqlStdin(name, sql string) error {
	cmd := exec.Command("sudo", "docker", "exec", "-i",
		"-e", "PGPASSWORD="+pgPass(), container(name),
		"psql", "-U", pgUser, "-d", pgDatabase, "-v", "ON_ERROR_STOP=1", "-q")
	cmd.Stdin = strings.NewReader(sql)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// InstallLedger installs (or upgrades) the schema ledger into a branch. It is
// idempotent and safe to run repeatedly.
func InstallLedger(name string) error {
	return psqlStdin(name, ledger.Schema)
}

// Ledger prints a branch's schema ledger (most recent first) — the RECORD layer:
// every DDL change, attributed and policy-checked.
func Ledger(name string, limit int) error {
	if name == "" {
		name = "main"
	}
	if limit <= 0 {
		limit = 50
	}
	q := fmt.Sprintf(`SELECT to_char(at,'MM-DD HH24:MI:SS') AS time,
		coalesce(actor,'-') AS actor, actor_kind AS kind, coalesce(tool,'-') AS tool,
		command_tag AS command, coalesce(object_identity,'') AS object,
		status, coalesce(risk,'') AS risk
		FROM vdb.schema_ledger ORDER BY at DESC LIMIT %d`, limit)
	return run("docker", "exec", "-e", "PGPASSWORD="+pgPass(), container(name),
		"psql", "-U", pgUser, "-d", pgDatabase, "-P", "pager=off", "-c", q)
}

// Create makes an instant copy-on-write branch of parent (default "main") and
// starts a Postgres container serving it.
func Create(name, parent string) error {
	if parent == "" {
		parent = "main"
	}
	if activeStorage().exists(name) {
		return fmt.Errorf("branch %q already exists", name)
	}
	if err := ensureNetwork(); err != nil {
		return err
	}
	// Flush the parent to disk so the clone starts from a clean checkpoint
	// (best-effort; crash recovery would handle it either way).
	quiet("docker", "exec", "-e", "PGPASSWORD="+pgPass(), container(parent),
		"psql", "-U", pgUser, "-d", pgDatabase, "-c", "CHECKPOINT;")

	if err := activeStorage().clone(parent, name); err != nil {
		return err
	}
	if err := run("chown", "-R", pgUID+":"+pgUID, mountpoint(name)); err != nil {
		return err
	}
	if err := startContainer(name, false); err != nil {
		return err
	}
	return waitReady(name)
}

// Delete stops a branch's container and destroys its dataset and origin
// snapshot. Refuses to delete "main".
func Delete(name string) error {
	if name == "main" {
		return fmt.Errorf("refusing to delete the primary branch 'main'")
	}
	quiet("docker", "rm", "-f", container(name))
	return activeStorage().destroy(name)
}

// List shows the branches and their containers.
func List() error {
	store := activeStorage()
	// The heading names what the driver can actually report: ZFS gives a
	// per-branch copy-on-write delta, btrfs does not without quota groups, and
	// promising a column that is not there reads as a bug.
	if store.name() == "zfs" {
		fmt.Println("=== branch datasets (USED shows copy-on-write delta) ===")
	} else {
		fmt.Println("=== branches (copy-on-write subvolumes) ===")
	}
	if err := store.list(); err != nil {
		return err
	}
	fmt.Println("\n=== postgres containers ===")
	return run("docker", "ps", "--filter", "name=vec-",
		"--format", "table {{.Names}}\t{{.Status}}")
}

// SQL runs a single statement against a branch (used by the demo and, later,
// the Agent Branch API).
func SQL(name, stmt string) error {
	return run("docker", "exec", "-e", "PGPASSWORD="+pgPass(), container(name),
		"psql", "-U", pgUser, "-d", pgDatabase, "-c", stmt)
}

// Query runs a statement against a branch and returns trimmed stdout (unaligned,
// tuples-only) for programmatic checks.
func Query(name, stmt string) (string, error) {
	out, err := exec.Command("sudo", "docker", "exec",
		"-e", "PGPASSWORD="+pgPass(), container(name),
		"psql", "-U", pgUser, "-d", pgDatabase, "-tAc", stmt).Output()
	return strings.TrimSpace(string(out)), err
}

// --- unified VM stack: MinIO + primary + backups + PITR ---

func minioRunning() bool {
	out, _ := capture("docker", "ps", "--filter", "name=^minio$", "--format", "{{.Names}}")
	return out == "minio"
}

// Up brings up the full stack: docker network, MinIO (object storage) with its
// WAL bucket, and the primary "main" branch (which archives WAL to MinIO).
func Up() error {
	if err := Provision(); err != nil {
		return err
	}
	if err := ensureNetwork(); err != nil {
		return err
	}
	if !minioRunning() {
		quiet("docker", "rm", "-f", "minio")
		if err := run("docker", "run", "-d",
			"--name", "minio", "--network", network,
			"-e", "MINIO_ROOT_USER="+minioUser(),
			"-e", "MINIO_ROOT_PASSWORD="+minioPass(),
			"-p", "9000:9000", "-p", "9001:9001",
			"-v", "vectoradb-minio:/data",
			"minio/minio:latest", "server", "/data", "--console-address", ":9001",
		); err != nil {
			return err
		}
	}
	// Create the WAL bucket (idempotent).
	if err := run("docker", "run", "--rm", "--network", network,
		"--entrypoint", "sh", "minio/mc:latest", "-c",
		fmt.Sprintf("until mc alias set local http://minio:9000 %s %s; do sleep 1; done; mc mb -p local/vectoradb-wal", minioUser(), minioPass()),
	); err != nil {
		return err
	}
	return Init()
}

// Down stops MinIO and all Postgres containers (branches + main). ZFS datasets
// are preserved.
func Down() error {
	out, _ := capture("docker", "ps", "-a", "--format", "{{.Names}}")
	for _, n := range strings.Fields(out) {
		if n == "minio" || n == "vectoradb-console" || strings.HasPrefix(n, "vec-") {
			quiet("docker", "rm", "-f", n)
		}
	}
	return nil
}

// Backup takes a base backup of main and pushes it to object storage.
func Backup() error {
	return run("docker", "exec",
		"-e", "PGHOST=localhost", "-e", "PGUSER="+pgUser,
		"-e", "PGPASSWORD="+pgPass(), "-e", "PGDATABASE="+pgDatabase,
		container("main"), "wal-g", "backup-push", "/var/lib/postgresql/data/pgdata")
}

// BackupList lists base backups in object storage.
func BackupList() error {
	return run("docker", "exec", container("main"), "wal-g", "backup-list", "--detail")
}

// Restore performs point-in-time recovery into a disposable container on port
// 5433. ts is a timestamp within the archived WAL window, or "latest".
func Restore(ts string) error {
	name := "restore"
	quiet("docker", "rm", "-f", container(name))
	if err := run("docker", "run", "-d",
		"--name", container(name), "--network", network,
		"-e", "WALG_S3_PREFIX=s3://vectoradb-wal",
		"-e", "AWS_ACCESS_KEY_ID=minioadmin",
		"-e", "AWS_SECRET_ACCESS_KEY=minioadmin",
		"-e", "AWS_ENDPOINT=http://minio:9000",
		"-e", "AWS_S3_FORCE_PATH_STYLE=true",
		"-e", "AWS_REGION=us-east-1",
		"-e", "PGDATA=/var/lib/postgresql/data/pgdata",
		"-e", "RECOVERY_TARGET_TIME="+ts,
		"-p", "5433:5432",
		"--entrypoint", "/usr/local/bin/restore-entrypoint.sh",
		image,
	); err != nil {
		return err
	}
	if err := waitRecovered(name); err != nil {
		return err
	}
	fmt.Printf("restored to %q, ready as container %s (port 5433). Query it with:\n"+
		"  sudo docker exec -e PGPASSWORD=vectoradb %s psql -U vectoradb -d vectoradb -c 'SELECT ...'\n",
		ts, container(name), container(name))
	return nil
}

func waitRecovered(name string) error {
	for i := 0; i < 40; i++ {
		if exec.Command("sudo", "docker", "exec", container(name),
			"pg_isready", "-h", "localhost", "-U", pgUser, "-d", pgDatabase).Run() == nil {
			out, _ := exec.Command("sudo", "docker", "exec",
				"-e", "PGPASSWORD="+pgPass(), container(name),
				"psql", "-h", "localhost", "-U", pgUser, "-d", pgDatabase,
				"-tAc", "SELECT pg_is_in_recovery();").Output()
			if strings.TrimSpace(string(out)) == "f" {
				return nil
			}
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("restore did not complete in time")
}

// Status prints primary readiness, stored backups, and branches.
func Status() error {
	fmt.Println("=== main readiness ===")
	_ = run("docker", "exec", container("main"), "pg_isready", "-U", pgUser, "-d", pgDatabase)
	fmt.Println("\n=== base backups ===")
	_ = BackupList()
	fmt.Println("\n=== branches ===")
	return List()
}

// PsqlShell opens an interactive psql session on a branch.
func PsqlShell(name string) error {
	cmd := exec.Command("sudo", "docker", "exec", "-it",
		"-e", "PGPASSWORD="+pgPass(), container(name),
		"psql", "-U", pgUser, "-d", pgDatabase)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// --- Agent Branch API support: one branch per agent ---

// Info describes an agent's branch and how to connect to it.
type Info struct {
	Agent  string `json:"agent,omitempty"`
	Branch string `json:"branch"`
	Host   string `json:"host"`
	Port   string `json:"port"`
	DSN    string `json:"dsn"`
	Status string `json:"status"`
}

func agentBranch(id string) string { return "agent-" + id }

func dsn(host, port string) string {
	return fmt.Sprintf("postgresql://%s:%s@%s:%s/%s", pgUser, pgPass(), host, port, pgDatabase)
}

// containerIP returns a container's IP on the vectoradb docker network. The
// in-guest gateway routes to it directly, so branch Postgres needs no published
// host port and stays unreachable from outside the VM.
func containerIP(cont string) (string, error) {
	out, err := capture("docker", "inspect", "-f",
		fmt.Sprintf("{{.NetworkSettings.Networks.%s.IPAddress}}", network), cont)
	if err != nil {
		return "", err
	}
	ip := strings.TrimSpace(out)
	if ip == "" {
		return "", fmt.Errorf("container %q has no IP on the %q network", cont, network)
	}
	return ip, nil
}

// parsePublishedPort extracts the host port from `docker port` output such as
// "0.0.0.0:32781\n[::]:32781". Retained for VECTORADB_DEBUG_PORTS tooling.
func parsePublishedPort(out string) (string, error) {
	line := out
	if i := strings.IndexByte(out, '\n'); i >= 0 {
		line = out[:i]
	}
	i := strings.LastIndex(line, ":")
	if i < 0 || i+1 >= len(line) {
		return "", fmt.Errorf("could not parse published port from %q", out)
	}
	return strings.TrimSpace(line[i+1:]), nil
}

// CreateAgentBranch gives agent id its own instant branch and returns how to
// connect to it.
func CreateAgentBranch(agentID string) (Info, error) {
	name := agentBranch(agentID)
	if err := Create(name, "main"); err != nil {
		return Info{}, err
	}
	ip, err := containerIP(container(name))
	if err != nil {
		return Info{}, err
	}
	// Default this branch's attribution to the agent, so its schema ledger records
	// direct connections as agent activity even without the Gateway in the path.
	actor := "agent-" + strings.ReplaceAll(agentID, "'", "''")
	_ = psqlStdin(name, fmt.Sprintf(
		"ALTER DATABASE %s SET vdb.actor = '%s'; ALTER DATABASE %s SET vdb.actor_kind = 'agent';",
		pgDatabase, actor, pgDatabase))
	// The DSN reaches the branch over the docker network (VM-internal); a host
	// agent should connect through the gateway. A gateway-routed, key-scoped DSN
	// is the planned replacement.
	return Info{Agent: agentID, Branch: name, Host: ip, Port: "5432", DSN: dsn(ip, "5432"), Status: "ready"}, nil
}

// DeleteAgentBranch tears down agent id's branch.
func DeleteAgentBranch(agentID string) error {
	return Delete(agentBranch(agentID))
}

// BackendAddr returns host:port where a branch's Postgres is reachable, used by
// the wire-protocol proxy to route connections. For "main" it resolves the
// container currently serving as primary (which changes after an HA failover).
func BackendAddr(name string) (string, error) {
	if name == "" {
		name = "main"
	}
	cont := container(name)
	if name == "main" {
		cont = PrimaryContainer()
	}
	ip, err := containerIP(cont)
	if err != nil {
		return "", err
	}
	return ip + ":5432", nil
}

// primaryFile records which branch container currently serves as the "main"
// primary ("main" normally, "standby" after a failover).
func primaryFile() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "/tmp"
	}
	return filepath.Join(home, ".vectoradb", "primary")
}

// PrimaryContainer is the container currently acting as the "main" primary.
func PrimaryContainer() string {
	if b, err := os.ReadFile(primaryFile()); err == nil {
		if n := strings.TrimSpace(string(b)); n != "" {
			return container(n)
		}
	}
	return container("main")
}

// setPrimary records the branch name now serving as primary.
func setPrimary(name string) error {
	_ = os.MkdirAll(filepath.Dir(primaryFile()), 0o755)
	return os.WriteFile(primaryFile(), []byte(name), 0o644)
}

// BranchInfo describes a branch for the control-plane API / dashboard.
type BranchInfo struct {
	Name        string `json:"name"`
	Primary     bool   `json:"primary"`
	Agent       bool   `json:"agent"`
	State       string `json:"state"` // running | exited | created | absent
	Used        string `json:"used"`  // copy-on-write delta (human)
	Refer       string `json:"refer"` // logical size referenced (human)
	Connections int    `json:"connections"`
	Port        string `json:"port"`
}

// Branches returns structured info for every branch (including main), enriched
// with container state, connections, and copy-on-write size.
func Branches() ([]BranchInfo, error) {
	usages, err := activeStorage().usage()
	if err != nil {
		return nil, err
	}
	var infos []BranchInfo
	for _, u := range usages {
		name := u.Name
		if strings.Contains(name, "/") {
			continue // only direct children
		}
		bi := BranchInfo{
			Name:    name,
			Used:    u.Used,
			Refer:   u.Refer,
			Primary: name == "main",
			Agent:   strings.HasPrefix(name, "agent-"),
			State:   ContainerState(name),
		}
		if bi.State == "running" {
			bi.Port = "5432" // in-container port; branches are reached via the gateway
			if c, err := ActiveConnections(name); err == nil {
				bi.Connections = c
			}
		}
		infos = append(infos, bi)
	}
	return infos, nil
}

// Storage summarises pool usage for the dashboard.
type Storage struct {
	Used  string `json:"used"`
	Avail string `json:"avail"`
}

// StorageInfo reports how much of the copy-on-write substrate is in use.
func StorageInfo() Storage {
	used, avail, err := activeStorage().capacity()
	if err != nil {
		return Storage{}
	}
	return Storage{Used: used, Avail: avail}
}

// --- auto-suspend / auto-resume ---

// ContainerState returns "running", "exited"/"created", or "absent".
func ContainerState(name string) string {
	out, err := capture("docker", "inspect", "-f", "{{.State.Status}}", container(name))
	if err != nil {
		return "absent"
	}
	return out
}

// Suspend stops a branch's container. The ZFS dataset (its data) is preserved,
// so it can be resumed later with no data loss.
func Suspend(name string) error {
	return run("docker", "stop", container(name))
}

// Resume starts a suspended branch container and waits until it is ready.
func Resume(name string) error {
	if err := run("docker", "start", container(name)); err != nil {
		return err
	}
	return waitReady(name)
}

// EnsureRunning makes sure a branch is running (resuming it if suspended) and
// returns its current backend address. Errors if the branch does not exist.
func EnsureRunning(name string) (string, error) {
	if name == "main" {
		// The primary's lifecycle is managed by `up`/`ha`. Never auto-start it
		// here — that could revive a stepped-down old primary after a failover
		// and cause split-brain. Just route to the current primary.
		return BackendAddr("main")
	}
	switch ContainerState(name) {
	case "running":
		// already up
	case "absent":
		// No container. If the dataset survives (e.g. after a full stop),
		// recreate the container on it; otherwise the branch truly doesn't exist.
		if !activeStorage().exists(name) {
			return "", fmt.Errorf("branch %q does not exist", name)
		}
		if err := startContainer(name, name == "main"); err != nil {
			return "", err
		}
		if err := waitReady(name); err != nil {
			return "", err
		}
	default: // exited, created, paused, ...
		if err := Resume(name); err != nil {
			return "", err
		}
	}
	return BackendAddr(name)
}

// Wake ensures a branch is running (resuming or recreating as needed).
func Wake(name string) error {
	_, err := EnsureRunning(name)
	return err
}

// ActiveConnections returns the number of client connections currently open to a
// branch (used to avoid suspending a branch that is in use).
func ActiveConnections(name string) (int, error) {
	out, err := capture("docker", "exec", "-e", "PGPASSWORD="+pgPass(), container(name),
		"psql", "-U", pgUser, "-d", pgDatabase, "-tAc",
		"SELECT count(*) FROM pg_stat_activity WHERE backend_type='client backend' AND pid<>pg_backend_pid();")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(out))
}

// SuspendableBranches lists running branches eligible for auto-suspend (every
// vec-* container except the primary "main" and the disposable "restore").
func SuspendableBranches() ([]string, error) {
	out, err := capture("docker", "ps", "--filter", "name=vec-", "--format", "{{.Names}}")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, n := range strings.Fields(out) {
		bn := strings.TrimPrefix(n, "vec-")
		if bn == "main" || bn == "restore" || bn == "standby" {
			continue // primary, restore target, and HA standby never auto-suspend
		}
		names = append(names, bn)
	}
	return names, nil
}

// ListAgentBranches lists all running agent branches.
func ListAgentBranches() ([]Info, error) {
	out, err := capture("docker", "ps", "--filter", "name=vec-agent-", "--format", "{{.Names}}")
	if err != nil {
		return nil, err
	}
	var infos []Info
	for _, n := range strings.Fields(out) {
		bn := strings.TrimPrefix(n, "vec-")
		ip, _ := containerIP(n)
		infos = append(infos, Info{
			Agent:  strings.TrimPrefix(bn, "agent-"),
			Branch: bn, Host: ip, Port: "5432", DSN: dsn(ip, "5432"), Status: "ready",
		})
	}
	return infos, nil
}
