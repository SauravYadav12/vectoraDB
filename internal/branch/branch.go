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
)

const (
	datasetBase = "vectoradb/branches"
	mountBase   = "/vectoradb/branches"
	network     = "vectoradb"
	image       = "vectoradb/postgres-walg:16"
	pgUser      = "vectoradb"
	pgPassword  = "vectoradb"
	pgDatabase  = "vectoradb"
	pgUID       = "999" // the postgres user's uid inside the official image
)

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
		"-e", "POSTGRES_PASSWORD=" + pgPassword,
		"-e", "POSTGRES_DB=" + pgDatabase,
		"-e", "PGDATA=/var/lib/postgresql/data/pgdata",
		"-v", mountpoint(name) + ":/var/lib/postgresql/data",
	}
	if primary {
		// Publish the primary on a stable host port.
		args = append(args, "-p", "5432:5432")
	} else {
		// Publish an ephemeral host port so agents/developers can connect
		// directly to a branch.
		args = append(args, "-p", "0:5432")
	}
	if primary {
		args = append(args,
			"-e", "WALG_S3_PREFIX=s3://vectoradb-wal",
			"-e", "AWS_ACCESS_KEY_ID=minioadmin",
			"-e", "AWS_SECRET_ACCESS_KEY=minioadmin",
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
	for i := 0; i < 30; i++ {
		if exec.Command("sudo", "docker", "exec", container(name),
			"pg_isready", "-U", pgUser, "-d", pgDatabase).Run() == nil {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("branch %q did not become ready in time", name)
}

// Init creates the primary "main" dataset (if absent) and starts its Postgres
// container. On first run Postgres initializes a fresh cluster on the dataset.
func Init() error {
	if err := ensureNetwork(); err != nil {
		return err
	}
	if ContainerState("main") == "running" {
		return nil // already up — keep it (idempotent)
	}
	if !datasetExists(dataset("main")) {
		if err := run("zfs", "create", "-p", dataset("main")); err != nil {
			return err
		}
	}
	if err := run("chown", "-R", pgUID+":"+pgUID, mountpoint("main")); err != nil {
		return err
	}
	if err := startContainer("main", true); err != nil {
		return err
	}
	return waitReady("main")
}

// Create makes an instant copy-on-write branch of parent (default "main") and
// starts a Postgres container serving it.
func Create(name, parent string) error {
	if parent == "" {
		parent = "main"
	}
	if datasetExists(dataset(name)) {
		return fmt.Errorf("branch %q already exists", name)
	}
	if err := ensureNetwork(); err != nil {
		return err
	}
	// Flush the parent to disk so the clone starts from a clean checkpoint
	// (best-effort; crash recovery would handle it either way).
	quiet("docker", "exec", "-e", "PGPASSWORD="+pgPassword, container(parent),
		"psql", "-U", pgUser, "-d", pgDatabase, "-c", "CHECKPOINT;")

	snap := snapFor(parent, name)
	if err := run("zfs", "snapshot", snap); err != nil {
		return err
	}
	if err := run("zfs", "clone", snap, dataset(name)); err != nil {
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
	if err := run("zfs", "destroy", "-R", dataset(name)); err != nil {
		return err
	}
	// The origin snapshot lives on the parent; parent is "main" for MVP branches.
	quiet("zfs", "destroy", snapFor("main", name))
	return nil
}

// List shows branch datasets (with used/referenced space) and their containers.
func List() error {
	fmt.Println("=== branch datasets (USED shows copy-on-write delta) ===")
	if err := run("zfs", "list", "-r", "-o", "name,used,refer,mountpoint", datasetBase); err != nil {
		return err
	}
	fmt.Println("\n=== postgres containers ===")
	return run("docker", "ps", "--filter", "name=vec-",
		"--format", "table {{.Names}}\t{{.Status}}")
}

// SQL runs a single statement against a branch (used by the demo and, later,
// the Agent Branch API).
func SQL(name, stmt string) error {
	return run("docker", "exec", "-e", "PGPASSWORD="+pgPassword, container(name),
		"psql", "-U", pgUser, "-d", pgDatabase, "-c", stmt)
}

// Query runs a statement against a branch and returns trimmed stdout (unaligned,
// tuples-only) for programmatic checks.
func Query(name, stmt string) (string, error) {
	out, err := exec.Command("sudo", "docker", "exec",
		"-e", "PGPASSWORD="+pgPassword, container(name),
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
	if err := ensureNetwork(); err != nil {
		return err
	}
	if !minioRunning() {
		quiet("docker", "rm", "-f", "minio")
		if err := run("docker", "run", "-d",
			"--name", "minio", "--network", network,
			"-e", "MINIO_ROOT_USER=minioadmin",
			"-e", "MINIO_ROOT_PASSWORD=minioadmin",
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
		"until mc alias set local http://minio:9000 minioadmin minioadmin; do sleep 1; done; mc mb -p local/vectoradb-wal",
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
		"-e", "PGPASSWORD="+pgPassword, "-e", "PGDATABASE="+pgDatabase,
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
				"-e", "PGPASSWORD="+pgPassword, container(name),
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
		"-e", "PGPASSWORD="+pgPassword, container(name),
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

func dsn(port string) string {
	return fmt.Sprintf("postgresql://%s:%s@127.0.0.1:%s/%s", pgUser, pgPassword, port, pgDatabase)
}

// portOf returns the ephemeral host port docker published for a container's
// Postgres (mapped from container port 5432).
func portOf(cont string) (string, error) {
	out, err := capture("docker", "port", cont, "5432/tcp")
	if err != nil {
		return "", err
	}
	return parsePublishedPort(out)
}

// parsePublishedPort extracts the host port from `docker port` output such as
// "0.0.0.0:32781\n[::]:32781".
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

func branchPort(name string) (string, error) { return portOf(container(name)) }

// CreateAgentBranch gives agent id its own instant branch and returns how to
// connect to it.
func CreateAgentBranch(agentID string) (Info, error) {
	name := agentBranch(agentID)
	if err := Create(name, "main"); err != nil {
		return Info{}, err
	}
	port, err := branchPort(name)
	if err != nil {
		return Info{}, err
	}
	return Info{Agent: agentID, Branch: name, Host: "127.0.0.1", Port: port, DSN: dsn(port), Status: "ready"}, nil
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
	port, err := portOf(cont)
	if err != nil {
		return "", err
	}
	return "127.0.0.1:" + port, nil
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
	out, err := capture("zfs", "list", "-H", "-o", "name,used,refer", "-r", datasetBase)
	if err != nil {
		return nil, err
	}
	var infos []BranchInfo
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 3 || f[0] == datasetBase {
			continue
		}
		name := strings.TrimPrefix(f[0], datasetBase+"/")
		if strings.Contains(name, "/") {
			continue // only direct children
		}
		bi := BranchInfo{
			Name:    name,
			Used:    f[1],
			Refer:   f[2],
			Primary: name == "main",
			Agent:   strings.HasPrefix(name, "agent-"),
			State:   ContainerState(name),
		}
		if bi.State == "running" {
			if p, err := branchPort(name); err == nil {
				bi.Port = p
			}
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

// StorageInfo reports ZFS pool usage.
func StorageInfo() Storage {
	out, err := capture("zfs", "list", "-H", "-o", "used,avail", "vectoradb")
	if err != nil {
		return Storage{}
	}
	if f := strings.Fields(out); len(f) >= 2 {
		return Storage{Used: f[0], Avail: f[1]}
	}
	return Storage{}
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
		if !datasetExists(dataset(name)) {
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
	out, err := capture("docker", "exec", "-e", "PGPASSWORD="+pgPassword, container(name),
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
		port, _ := branchPort(bn)
		infos = append(infos, Info{
			Agent:  strings.TrimPrefix(bn, "agent-"),
			Branch: bn, Host: "127.0.0.1", Port: port, DSN: dsn(port), Status: "ready",
		})
	}
	return infos, nil
}
