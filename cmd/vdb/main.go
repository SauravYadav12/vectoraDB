// SPDX-License-Identifier: AGPL-3.0-or-later

// Command vdb is the control CLI for the VectoraDB serverless-Postgres
// platform. It runs inside the Linux dev VM (ZFS + Docker) and manages the
// unified stack: object storage (MinIO), the primary Postgres ("main") with WAL
// archiving, point-in-time restore, and instant copy-on-write branches.
package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/vectoradb/vectoradb/internal/agentapi"
	"github.com/vectoradb/vectoradb/internal/auth"
	"github.com/vectoradb/vectoradb/internal/branch"
	"github.com/vectoradb/vectoradb/internal/controlplane"
	"github.com/vectoradb/vectoradb/internal/daemon"
	"github.com/vectoradb/vectoradb/internal/host"
	"github.com/vectoradb/vectoradb/internal/proxy"
	"github.com/vectoradb/vectoradb/internal/version"
	"github.com/vectoradb/vectoradb/web"
)

// background services managed by `start`/`stop` (name -> subcommand + flags).
var services = map[string][]string{
	"controlplane": {"controlplane", "--addr", ":8080"},
	"gateway":      {"gateway", "--addr", ":6432", "--idle", "2m"},
	"api":          {"serve", "--addr", ":8088"},
}

const usage = `VectoraDB — serverless Postgres control CLI

Usage:
  vdb <command> [args]

Setup:
  setup                One-time: create/start the local engine VM (macOS: Lima, Windows: WSL2) and bring the stack up

Stack:
  start                Bring EVERYTHING up in the background: stack + gateway + APIs
  stop                 Stop background servers and all containers
  up                   Bring up the stack: network + MinIO + primary 'main' (archiving)
  down                 Stop MinIO and all Postgres containers (ZFS data preserved)
  status               Show servers, main readiness, stored backups, and branches
  logs [gateway|api]   Print a background server's log
  psql                 Open a psql shell on the primary 'main'

Durability / time-travel:
  backup create        Base backup of 'main' -> object storage
  backup list          List base backups in object storage
  restore --to <ts>    PITR into a disposable container on port 5433 (ts or 'latest')

Branching:
  branch create <name>  Instant copy-on-write branch of main (ZFS clone)
  branch list           List branches and their containers
  branch delete <name>  Stop and destroy a branch
  branch suspend <name> Stop a branch (data preserved); resumes on next connect
  branch resume <name>  Start a suspended branch

Schema ledger (RECORD layer):
  ledger [branch] [--limit N]  Show captured DDL changes — attributed & policy-checked
  ledger revert --to <ts>      Time-travel revert of a branch's schema+data to a moment

Migration:
  import --from <src> [--as <name>]  Migrate a DB into a new instance. <src> is a
                       connection string — postgres://, mysql://, mariadb://, mongodb:// —
                       or a .sql / .csv / .json file (picked from anywhere)
  import --from <pg-dsn> --continuous [--as <name>]
                       Continuous logical replication from a Postgres source
                       (initial copy + streaming) for zero-downtime cutover
  import-cutover <name>  Stop replication for a --continuous instance, keeping its data
  pipeline run <spec.json> [--as <name>]  Run an ETL pipeline (extract → land raw →
                       SQL transform models → data-quality tests) into a fresh instance

High availability:
  ha enable            Provision a hot standby streaming from main
  ha status            Show replication status (primary + standby)
  ha failover          Promote the standby to primary (reroutes 'main')
  ha disable           Remove the standby

Serverless front door:
  gateway [--addr :6432] [--idle 2m]
                       Smart SQL gateway: reads dbname=<branch> and routes to it,
                       auto-resuming suspended branches, auto-suspending idle ones (--idle 0 = off)
  controlplane [--addr :8080]
                       Control-plane REST API (JSON; the web UI is a separate app in web/)

Agent Branch API:
  serve [--addr :8088] Run the HTTP API: one database branch per AI agent

Auth (admin):
  user create <email>            Create an account (prompts for a password)
  apikey create <email> [name]   Mint an API key (shown once)
  apikey list <email>            List a user's API keys
  apikey revoke <email> <id>     Revoke an API key

  version              Print the vdb version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}

	// On macOS/Windows, forward engine commands into the managed Linux VM so the
	// user only ever runs `vdb …`. On Linux (or inside the VM) this is a no-op.
	if handled, err := host.Maybe(os.Args[1:]); handled {
		must(err)
		return
	}

	switch os.Args[1] {
	case "version", "-v", "--version":
		fmt.Printf("vdb %s\n", version.Version)
	case "setup":
		must(host.Setup())
	case "start":
		must(branch.Up())
		for name, args := range services {
			must(daemon.Start(name, args))
		}
		apiKey := bootstrapLocalKey()
		fmt.Println("\nVectoraDB is up (background):")
		if web.FS() != nil {
			fmt.Println("  web UI       https://localhost:8080")
		}
		fmt.Println("  control API  https://localhost:8080/api/status")
		fmt.Println("  agent API    https://localhost:8088   (POST /agents/{id}/branch)")
		if apiKey != "" {
			fmt.Printf("  gateway(SQL) postgresql://vectoradb:%s@localhost:6432/main?sslmode=require\n", apiKey)
		} else {
			fmt.Println("  gateway(SQL) postgresql://vectoradb:<API_KEY>@localhost:6432/<branch>?sslmode=require")
		}
		fmt.Println("  storage      http://localhost:9001   (console login in ~/.vectoradb/secrets.json)")
		if web.FS() == nil {
			fmt.Println("\nThe web UI isn't embedded in this build — run it with:  make web-dev   (http://localhost:5173)")
		}
		fmt.Println("\nThe connection string above uses a ready-to-go API key (also saved in ~/.vectoradb/config).")
		fmt.Println("Stop everything with: vdb stop")
	case "stop":
		for name := range services {
			daemon.Stop(name)
		}
		must(branch.Down())
		fmt.Println("stopped: control API, agent API, gateway, and all containers")
	case "logs":
		svc := "gateway"
		if len(os.Args) > 2 {
			svc = os.Args[2]
		}
		b, err := os.ReadFile(daemon.LogPath(svc))
		if err != nil {
			fmt.Printf("no log for %q yet\n", svc)
		} else {
			fmt.Print(string(b))
		}
	case "up":
		must(branch.Up())
	case "down":
		for name := range services {
			daemon.Stop(name)
		}
		must(branch.Down())
	case "status":
		fmt.Println("=== servers ===")
		fmt.Printf("control API: %s\n", daemon.Status("controlplane"))
		fmt.Printf("gateway: %s\n", daemon.Status("gateway"))
		fmt.Printf("agent API: %s\n", daemon.Status("api"))
		fmt.Println()
		must(branch.Status())
	case "psql":
		must(branch.PsqlShell("main"))
	case "backup":
		if len(os.Args) < 3 {
			fmt.Println("usage: vdb backup <create|list>")
			os.Exit(2)
		}
		switch os.Args[2] {
		case "create":
			must(branch.Backup())
		case "list":
			must(branch.BackupList())
		default:
			fmt.Printf("unknown backup subcommand: %s\n", os.Args[2])
			os.Exit(2)
		}
	case "restore":
		ts := restoreArg(os.Args[2:])
		if ts == "" {
			fmt.Println("usage: vdb restore --to '<timestamp>'|latest")
			os.Exit(2)
		}
		must(branch.Restore(ts))
	case "branch":
		branchCmd(os.Args[2:])
	case "ledger":
		ledgerCmd(os.Args[2:])
	case "import":
		importCmd(os.Args[2:])
	case "import-cutover":
		if len(os.Args) < 3 {
			fmt.Println("usage: vdb import-cutover <instance>")
			os.Exit(2)
		}
		must(branch.ImportCutover(os.Args[2]))
	case "pipeline":
		pipelineCmd(os.Args[2:])
	case "ha":
		haCmd(os.Args[2:])
	case "user":
		if len(os.Args) < 4 || os.Args[2] != "create" {
			fmt.Println("usage: vdb user create <email>")
			os.Exit(2)
		}
		must(userCreate(os.Args[3]))
	case "apikey":
		apikeyCmd(os.Args[2:])
	case "serve":
		must(agentapi.Serve(addrFlag(os.Args[2:], ":8088")))
	case "controlplane":
		must(controlplane.Serve(addrFlag(os.Args[2:], ":8080")))
	case "gateway":
		// The internal package is still named "proxy"; the user-facing command is "gateway".
		must(proxy.Serve(addrFlag(os.Args[2:], ":6432"), durFlag(os.Args[2:], "--idle", 2*time.Minute)))
	default:
		fmt.Printf("unknown command: %s\n\n", os.Args[1])
		fmt.Print(usage)
		os.Exit(2)
	}
}

// restoreArg accepts either `--to <ts>` or a bare `<ts>`.
func restoreArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	if args[0] == "--to" {
		if len(args) < 2 {
			return ""
		}
		return args[1]
	}
	return args[0]
}

// importCmd handles `vdb import --from <source> [--as <instance>]`, migrating a
// Postgres source or a .sql/.csv/.json file into a fresh VectoraDB instance.
func importCmd(args []string) {
	var source, target, kind, srcname string
	var continuous bool
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--continuous":
			continuous = true
		case "--from":
			if i+1 < len(args) {
				source, i = args[i+1], i+1
			}
		case "--as", "--to":
			if i+1 < len(args) {
				target, i = args[i+1], i+1
			}
		case "--kind": // for streamed (--from -) imports: sql|csv|json
			if i+1 < len(args) {
				kind, i = args[i+1], i+1
			}
		case "--srcname": // origin name for a streamed import (default target/table)
			if i+1 < len(args) {
				srcname, i = args[i+1], i+1
			}
		default:
			if source == "" && !strings.HasPrefix(args[i], "-") {
				source = args[i]
			}
		}
	}
	if source == "" {
		fmt.Println("usage: vdb import --from <postgres://… | file.sql|.csv|.json> [--as <instance>]")
		os.Exit(2)
	}
	// A dash means the file is streamed on stdin (used when the launcher forwards
	// a local file from the client machine into the VM).
	if source == "-" {
		k, err := branch.ParseKind(kind)
		must(err)
		if srcname == "" {
			srcname = "stdin"
		}
		_, err = branch.ImportReader(os.Stdin, k, srcname, target)
		must(err)
		return
	}
	if continuous {
		_, err := branch.ImportContinuous(source, target)
		must(err)
		return
	}
	_, err := branch.Import(source, target)
	must(err)
}

// pipelineCmd handles `vdb pipeline run <spec.json> [--as <instance>]`: an ETL
// pipeline (extract → land raw → SQL transforms → tests) into a fresh instance.
func pipelineCmd(args []string) {
	if len(args) < 2 || args[0] != "run" {
		fmt.Println("usage: vdb pipeline run <spec.json> [--as <instance>]")
		os.Exit(2)
	}
	specPath, target := args[1], ""
	for i := 2; i < len(args); i++ {
		if args[i] == "--as" && i+1 < len(args) {
			target, i = args[i+1], i+1
		}
	}
	b, err := os.ReadFile(specPath)
	must(err)
	var spec branch.PipelineSpec
	must(json.Unmarshal(b, &spec))
	if target == "" {
		target = "pl-" + strings.TrimSuffix(filepath.Base(specPath), filepath.Ext(specPath))
	}
	res, err := branch.RunPipeline(&branch.Progress{Log: os.Stdout}, spec, target)
	must(err)
	if res.Failed {
		os.Exit(1)
	}
}

// ledgerCmd handles `vdb ledger [branch] [--limit N]` and
// `vdb ledger revert --to <ts>` (time-travel restore of the branch's schema+data).
func ledgerCmd(args []string) {
	if len(args) > 0 && args[0] == "revert" {
		ts := restoreArg(args[1:])
		if ts == "" {
			fmt.Println("usage: vdb ledger revert --to '<timestamp>'|latest")
			os.Exit(2)
		}
		fmt.Println("Reverting via time-travel restore (disposable container on :5433)…")
		must(branch.Restore(ts))
		return
	}
	if len(args) > 0 && args[0] == "verify" {
		name := "main"
		if len(args) > 1 && !strings.HasPrefix(args[1], "-") {
			name = args[1]
		}
		must(branch.LedgerVerify(name))
		return
	}
	name, limit := "main", 50
	for i := 0; i < len(args); i++ {
		if args[i] == "--limit" && i+1 < len(args) {
			if n, err := strconv.Atoi(args[i+1]); err == nil {
				limit = n
			}
			i++
		} else if !strings.HasPrefix(args[i], "-") {
			name = args[i]
		}
	}
	must(branch.Ledger(name, limit))
}

// addrFlag parses `--addr <addr>`, falling back to def.
func addrFlag(args []string, def string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--addr" {
			return args[i+1]
		}
	}
	return def
}

// durFlag parses `<name> <duration>` (e.g. --idle 90s), falling back to def.
func durFlag(args []string, name string, def time.Duration) time.Duration {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == name {
			if d, err := time.ParseDuration(args[i+1]); err == nil {
				return d
			}
		}
	}
	return def
}

// branchCmd dispatches `vdb branch <subcommand>`.
func branchCmd(args []string) {
	if len(args) == 0 {
		fmt.Println("usage: vdb branch <create|list|delete|reset|suspend|resume> [name]")
		os.Exit(2)
	}
	switch args[0] {
	case "create":
		if len(args) < 2 {
			fmt.Println("usage: vdb branch create <name>")
			os.Exit(2)
		}
		must(branch.Create(args[1], ""))
	case "list":
		must(branch.List())
	case "delete":
		if len(args) < 2 {
			fmt.Println("usage: vdb branch delete <name>")
			os.Exit(2)
		}
		must(branch.Delete(args[1]))
	case "reset":
		if len(args) < 2 {
			fmt.Println("usage: vdb branch reset <name> [--from <parent>]")
			os.Exit(2)
		}
		parent := "main"
		for i := 2; i < len(args); i++ {
			if args[i] == "--from" && i+1 < len(args) {
				parent = args[i+1]
				i++
			}
		}
		must(branch.Reset(args[1], parent))
	case "suspend":
		if len(args) < 2 {
			fmt.Println("usage: vdb branch suspend <name>")
			os.Exit(2)
		}
		must(branch.Suspend(args[1]))
	case "resume":
		if len(args) < 2 {
			fmt.Println("usage: vdb branch resume <name>")
			os.Exit(2)
		}
		must(branch.Wake(args[1]))
	default:
		fmt.Printf("unknown branch subcommand: %s\n", args[0])
		os.Exit(2)
	}
}

// haCmd dispatches `vdb ha <subcommand>`.
func haCmd(args []string) {
	if len(args) == 0 {
		fmt.Println("usage: vdb ha <enable|status|failover|disable>")
		os.Exit(2)
	}
	switch args[0] {
	case "enable":
		must(branch.HAEnable())
	case "status":
		must(branch.HAStatus())
	case "failover":
		must(branch.HAFailover())
	case "disable":
		must(branch.HADisable())
	default:
		fmt.Printf("unknown ha subcommand: %s\n", args[0])
		os.Exit(2)
	}
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func openStore() *auth.Store {
	s, err := auth.OpenFromEnv()
	must(err)
	return s
}

func vectoradbDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.TempDir()
	}
	return filepath.Join(home, ".vectoradb")
}

func configPath() string { return filepath.Join(vectoradbDir(), "config") }

// bootstrapLocalKey makes a fresh install usable with no manual account steps.
// On first run it creates a local user and an API key, caching the key in
// ~/.vectoradb/config so `vdb start` can always print a working connection
// string. Returns the API key, or "" if it can't be determined (in which case
// the banner falls back to a <API_KEY> placeholder). Never fatal — a bootstrap
// hiccup must not stop the stack from coming up.
func bootstrapLocalKey() string {
	if k := readCachedKey(); k != "" {
		return k
	}
	store, err := auth.OpenFromEnv()
	if err != nil {
		return ""
	}
	if store.HasAnyUser() {
		return "" // accounts exist but no cached key — the key is shown only once
	}
	pw := make([]byte, 16)
	if _, err := rand.Read(pw); err != nil {
		return ""
	}
	u, err := store.CreateUser("local@vectoradb", hex.EncodeToString(pw))
	if err != nil {
		return ""
	}
	key, _, err := store.CreateAPIKey(u.ID, "setup")
	if err != nil {
		return ""
	}
	writeCachedKey(key)
	return key
}

func readCachedKey() string {
	b, err := os.ReadFile(configPath())
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "api_key="); ok {
			return v
		}
	}
	return ""
}

func writeCachedKey(key string) {
	_ = os.MkdirAll(vectoradbDir(), 0o700)
	_ = os.WriteFile(configPath(), []byte("api_key="+key+"\n"), 0o600)
}

func userCreate(email string) error {
	fmt.Print("password (min 8 chars): ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	pw := strings.TrimSpace(line)
	if len(pw) < 8 {
		return fmt.Errorf("password must be at least 8 characters")
	}
	u, err := openStore().CreateUser(email, pw)
	if err != nil {
		return err
	}
	fmt.Printf("created user %s (id %d)\n", u.Email, u.ID)
	return nil
}

func apikeyCmd(args []string) {
	if len(args) < 2 {
		fmt.Println("usage: vdb apikey <create|list|revoke> <email> [name|id]")
		os.Exit(2)
	}
	s := openStore()
	u, ok := s.UserByEmail(args[1])
	if !ok {
		must(fmt.Errorf("no such user: %s (create it with: vdb user create %s)", args[1], args[1]))
	}
	switch args[0] {
	case "create":
		name := "key"
		if len(args) > 2 {
			name = args[2]
		}
		secret, info, err := s.CreateAPIKey(u.ID, name)
		must(err)
		fmt.Printf("API key %q created — copy it now, it won't be shown again:\n\n  %s\n", info.Name, secret)
	case "list":
		keys, err := s.ListKeys(u.ID)
		must(err)
		if len(keys) == 0 {
			fmt.Println("no API keys")
			return
		}
		for _, k := range keys {
			fmt.Printf("  %s  %-16s  %s…\n", k.ID, k.Name, k.Prefix)
		}
	case "revoke":
		if len(args) < 3 {
			fmt.Println("usage: vdb apikey revoke <email> <id>")
			os.Exit(2)
		}
		must(s.RevokeKey(u.ID, args[2]))
		fmt.Println("revoked")
	default:
		fmt.Printf("unknown apikey subcommand: %s\n", args[0])
		os.Exit(2)
	}
}
