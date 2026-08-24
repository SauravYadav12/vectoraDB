// SPDX-License-Identifier: AGPL-3.0-or-later

// Command vectoradb is the control CLI for the vectoradb serverless-Postgres
// platform. It runs inside the Linux dev VM (ZFS + Docker) and manages the
// unified stack: object storage (MinIO), the primary Postgres ("main") with WAL
// archiving, point-in-time restore, and instant copy-on-write branches.
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/vectoradb/vectoradb/internal/agentapi"
	"github.com/vectoradb/vectoradb/internal/branch"
	"github.com/vectoradb/vectoradb/internal/controlplane"
	"github.com/vectoradb/vectoradb/internal/daemon"
	"github.com/vectoradb/vectoradb/internal/proxy"
	"github.com/vectoradb/vectoradb/internal/version"
)

// background services managed by `start`/`stop` (name -> subcommand + flags).
var services = map[string][]string{
	"dashboard": {"dashboard", "--addr", ":8080"},
	"proxy":     {"proxy", "--addr", ":6432", "--idle", "2m"},
	"api":       {"serve", "--addr", ":8088"},
}

const usage = `VectoraDB — serverless Postgres control CLI

Usage:
  vectoradb <command> [args]

Stack:
  start                Bring EVERYTHING up in the background: stack + proxy + API + console
  stop                 Stop background servers and all containers
  up                   Bring up the stack: network + MinIO + primary 'main' (archiving)
  down                 Stop MinIO and all Postgres containers (ZFS data preserved)
  status               Show servers, main readiness, stored backups, and branches
  logs [proxy|api]     Print a background server's log
  psql                 Open a psql shell on the primary 'main'
  console [branch]     Web SQL console (pgweb) at http://localhost:8081

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

High availability:
  ha enable            Provision a hot standby streaming from main
  ha status            Show replication status (primary + standby)
  ha failover          Promote the standby to primary (reroutes 'main')
  ha disable           Remove the standby

Serverless front door:
  proxy [--addr :6432] [--idle 2m]
                       Wire-protocol proxy: route by dbname=<branch>, auto-resume
                       suspended branches, auto-suspend idle ones (--idle 0 = off)
  dashboard [--addr :8080]
                       Management REST API + web dashboard

Agent Branch API:
  serve [--addr :8088] Run the HTTP API: one database branch per AI agent

  version              Print the vectoradb version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}

	switch os.Args[1] {
	case "version", "-v", "--version":
		fmt.Printf("vectoradb %s\n", version.Version)
	case "start":
		must(branch.Up())
		for name, args := range services {
			must(daemon.Start(name, args))
		}
		must(branch.Console("main"))
		fmt.Println("\nVectoraDB is up (background):")
		fmt.Println("  dashboard  http://localhost:8080   <- start here")
		fmt.Println("  proxy      postgres://vectoradb:vectoradb@localhost:6432/<branch>")
		fmt.Println("  agent API  http://localhost:8088   (POST /agents/{id}/branch)")
		fmt.Println("  console    http://localhost:8081")
		fmt.Println("  storage    http://localhost:9001   (minioadmin/minioadmin)")
		fmt.Println("\nStop everything with: vectoradb stop")
	case "stop":
		for name := range services {
			daemon.Stop(name)
		}
		must(branch.Down())
		fmt.Println("stopped: proxy, agent API, console, and all containers")
	case "logs":
		svc := "proxy"
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
		fmt.Printf("proxy: %s\n", daemon.Status("proxy"))
		fmt.Printf("agent API: %s\n", daemon.Status("api"))
		fmt.Println()
		must(branch.Status())
	case "psql":
		must(branch.PsqlShell("main"))
	case "console":
		name := ""
		if len(os.Args) > 2 {
			name = os.Args[2]
		}
		must(branch.Console(name))
	case "backup":
		if len(os.Args) < 3 {
			fmt.Println("usage: vectoradb backup <create|list>")
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
			fmt.Println("usage: vectoradb restore --to '<timestamp>'|latest")
			os.Exit(2)
		}
		must(branch.Restore(ts))
	case "branch":
		branchCmd(os.Args[2:])
	case "ha":
		haCmd(os.Args[2:])
	case "serve":
		must(agentapi.Serve(addrFlag(os.Args[2:], ":8088")))
	case "dashboard":
		must(controlplane.Serve(addrFlag(os.Args[2:], ":8080")))
	case "proxy":
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

// branchCmd dispatches `vectoradb branch <subcommand>`.
func branchCmd(args []string) {
	if len(args) == 0 {
		fmt.Println("usage: vectoradb branch <create|list|delete> [name]")
		os.Exit(2)
	}
	switch args[0] {
	case "create":
		if len(args) < 2 {
			fmt.Println("usage: vectoradb branch create <name>")
			os.Exit(2)
		}
		must(branch.Create(args[1], ""))
	case "list":
		must(branch.List())
	case "delete":
		if len(args) < 2 {
			fmt.Println("usage: vectoradb branch delete <name>")
			os.Exit(2)
		}
		must(branch.Delete(args[1]))
	case "suspend":
		if len(args) < 2 {
			fmt.Println("usage: vectoradb branch suspend <name>")
			os.Exit(2)
		}
		must(branch.Suspend(args[1]))
	case "resume":
		if len(args) < 2 {
			fmt.Println("usage: vectoradb branch resume <name>")
			os.Exit(2)
		}
		must(branch.Wake(args[1]))
	default:
		fmt.Printf("unknown branch subcommand: %s\n", args[0])
		os.Exit(2)
	}
}

// haCmd dispatches `vectoradb ha <subcommand>`.
func haCmd(args []string) {
	if len(args) == 0 {
		fmt.Println("usage: vectoradb ha <enable|status|failover|disable>")
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
