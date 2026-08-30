# VectoraDB

**Postgres for AI agents — instant branches, and a record of every schema change.**

VectoraDB is a **serverless PostgreSQL** platform: stock Postgres on local NVMe
(native commit latency), with durability, branching, and time-travel moved *off*
the commit path — **ZFS copy-on-write** clones for instant branches and
**asynchronous WAL archival** (`wal-g`) for PITR. It speaks the native Postgres
wire protocol, so your existing driver, ORM, and SQL work unchanged. It is the
postgres.ai / Database Lab model, implemented in Go.

### What you get

- **[Schema Ledger](#the-schema-ledger)** — every `CREATE`/`ALTER`/`DROP`
  recorded with the actor (human or agent), tool, and branch, plus a
  destructive-DDL guardrail. This is the part no other Postgres branching tool has.
- **Instant branching** — `vdb branch create qa` clones the whole database in
  seconds (copy-on-write), fully isolated; `main` is untouched.
- **Time travel / PITR** — continuous WAL archival, restore to any point.
- **One serverless endpoint** — connect to `:6432`; the database name *is* the
  branch. Idle branches scale to zero and wake on connect.
- **A database per AI agent** — the Agent Branch API hands each agent its own
  instant, disposable branch over HTTP.
- **Migrate from anything** — import from PostgreSQL, MySQL/MariaDB, MongoDB, and
  `.sql`/`.csv`/`.json`/`.ndjson` files, each landing in a fresh branch.
- **ETL pipelines** *(experimental)* — dbt-style SQL models with data-quality
  tests, each run against a throwaway branch.
- **High availability** — a hot standby with transparent `ha failover`
  (single-VM demonstration).
- **Accounts & API keys** — email/password or GitHub/Google OAuth; keys mint the
  gateway password.
- **Web console** — a React UI (dashboard, SQL console, ledger viewer), served
  by the engine itself.

Connections are encrypted: clients with `sslmode=require` (Prisma's default, and
most cloud drivers) connect out of the box, and per-install credentials are
generated on first run — nothing hardcoded.

## The Schema Ledger

The differentiator. Three Postgres event triggers, installed into `main` and
inherited by every branch, capture **every** schema change and attribute it:

- **who** — the actor (a human email, or `agent-alice`) and whether it was a
  human or an agent;
- **what** — the command, the object, and the full statement;
- **context** — the tool (`application_name`, e.g. `cursor/opus`), the branch,
  and the session;
- **a guardrail** — destructive DDL (e.g. `DROP TABLE`) is blocked by policy
  unless explicitly overridden, and blocked attempts are recorded too.

```bash
vdb ledger          # every schema change on this branch, most recent first
```

The actor is set by the gateway from the API key (and by the Agent Branch API at
the database level), so DDL an agent runs is attributed to that agent — the
record developers building with AI agents actually need. There is a **Ledger**
page in the web console too.

> Tamper-evidence — an append-only, hash-chained ledger with `vdb ledger verify`
> — is on the near-term roadmap.

## Status

- **Phase 1 — durability + time-travel:** ✅ continuous WAL archival to object
  storage + point-in-time restore.
- **Phase 2 — instant branching:** ✅ ZFS copy-on-write branches in seconds,
  fully isolated; plus the **Agent Branch API** (one database per AI agent).
- **Phase 3 — serverless front door:** ✅ wire-protocol gateway (one endpoint,
  route by database name), **auto-suspend/resume** (idle branches stop; the
  gateway wakes them on connect), **background/daemon mode** (`start`/`stop`), and
  a **control-plane REST API**.
- **Phase 4 — High availability:** ✅ a hot standby streams from the primary;
  `ha failover` promotes it and the gateway reroutes `main` transparently.
- **Phase 5 — tests:** ✅ Go unit tests + a VM integration test.
- **Phase 6 — docs & packaging:** ✅ demo script, storage metric, architecture PDF.
- **Phase 7 — web app:** ✅ a standalone **React + TypeScript** UI in `web/`
  (landing, docs, dashboard, SQL console) consuming the REST API.
- **Later (optional):** control-plane polish — multi-tenant projects, auth/API keys.

## Documentation & demo

- **Web app** — `make web-dev`, then open **http://localhost:5173** for the
  landing page, docs, ops dashboard, and SQL console.
- **Guided demo** — `lima bash scripts/demo.sh` runs a narrated feature tour.
- **Architecture (plain English)** — [`docs/vectoradb-architecture.pdf`](docs/vectoradb-architecture.pdf).

## Architecture (dev)

Everything runs inside a single Linux VM (Lima) with ZFS + Docker:

- `minio` — S3-compatible object storage (WAL archive target).
- `vec-main` — the primary Postgres (our `postgres:16 + wal-g` image), data dir
  on ZFS dataset `vectoradb/branches/main`, archiving WAL to MinIO.
- `vec-<name>` — a branch: a `zfs clone` of main served by its own Postgres.
- `vec-restore` — a disposable point-in-time restore target (port 5433).

## Install

One line installs the `vdb` command; it sets up everything else for you — the
Linux VM (on macOS), Docker, ZFS, the copy-on-write pool, and the image.

**macOS** — needs [Lima](https://lima-vm.io) for the local VM:

```bash
brew install lima
curl -fsSL https://raw.githubusercontent.com/SauravYadav12/vectoraDB/main/deploy/install.sh | sh
vdb setup
```

**Linux**:

```bash
curl -fsSL https://raw.githubusercontent.com/SauravYadav12/vectoraDB/main/deploy/install.sh | sh
sudo vdb start
```

**Windows** — one command, **in PowerShell** (not Command Prompt — `irm`/`iex` are PowerShell
commands). Needs Windows 10 21H2+/11 with virtualization enabled; everything else is handled for you:

```powershell
irm https://raw.githubusercontent.com/SauravYadav12/vectoraDB/main/deploy/install.ps1 | iex
```

That installs WSL if you don't have it (no Linux distribution needed — VectoraDB brings its own),
downloads VectoraDB, puts it on your PATH, and runs `vdb setup`. If Windows needs a restart to
finish enabling WSL, it says so and continues automatically afterwards. Troubleshooting:
**[docs/windows-setup.md](docs/windows-setup.md)**.

`vdb setup` creates the WSL2 distro, installs Docker + ZFS into it, and brings the stack up — then
every command is just `vdb …`, forwarded into WSL2 transparently. Your kernel and `.wslconfig` are
not modified, so Docker Desktop, Rancher Desktop, and your other distros are unaffected. See
[docs/windows-setup.md](docs/windows-setup.md) for prerequisites, how ZFS gets in without a custom
kernel, and troubleshooting.

`vdb setup` (macOS/Windows) / `vdb start` (Linux) creates the VM, installs Docker + ZFS,
builds the pool and image, and brings the database, gateway, and APIs up — no
manual steps. On macOS (Lima) and Windows (WSL2), `vdb` transparently runs the engine inside the
VM, so every command below is just `vdb …` with no `lima`/`wsl` prefix.

> **Install from source (contributors):** clone the repo and `make vm-build`
> (builds the Linux binary into the VM) or `make build` (host binary). See
> [Development](#development).

## Usage

**One command brings everything up in the background** (stack + gateway + agent
API), detached, no terminal held open:

```bash
vdb start    # everything up (background)
vdb status   # servers + main + branches
vdb logs gateway
vdb stop      # stop servers + containers
```

Then run the **web UI** — a separate React app in `web/` — against the API:

```bash
make web-dev   # http://localhost:5173  (Landing · Docs · Dashboard · SQL Console)
```

Other endpoints: control API `http://localhost:8080/api`, gateway
`postgres://vectoradb:<API_KEY>@localhost:6432/<branch>`, agent API
`http://localhost:8088`, object storage `http://localhost:9001`.

### Web app (`web/`)

The UI is a **Vite + React + TypeScript** app that consumes the control-plane
REST API. In release builds it is **embedded into the engine binary** (the
`embedui` build tag) and served same-origin, so `vdb start` serves the web
console at `https://localhost:8080` with nothing else to run. For UI development,
`make web-dev` runs a hot-reloading dev server at `http://localhost:5173` against
the API (`VITE_API_URL` points it elsewhere). It has a landing page, docs, an ops
**dashboard** (live status + branch CRUD), a **SQL console**, and a **Ledger**
viewer. The API is also usable directly:

```bash
curl localhost:8080/api/status
curl localhost:8080/api/branches
curl -X POST localhost:8080/api/branches -d '{"name":"qa"}'
curl -X POST localhost:8080/api/branches/qa/query -d '{"sql":"SELECT 1"}'
curl -X DELETE localhost:8080/api/branches/qa
```

The individual commands below are for foreground/manual use:

```bash
vdb up                 # network + MinIO + primary 'main' (archiving)
vdb status             # readiness + backups + branches

vdb backup create      # base backup -> object storage
vdb restore --to latest        # PITR into a disposable container (port 5433)
vdb restore --to '2026-08-24 15:07:00+00'

vdb branch create qa   # instant copy-on-write branch
vdb branch list
vdb branch delete qa

vdb gateway --addr :6432 # one endpoint; route with dbname=<branch>

vdb down               # stop containers (ZFS datasets preserved)
```

### Single endpoint (serverless front door)

`vdb gateway` exposes one TLS PostgreSQL endpoint (`:6432`) and routes each
connection to the branch named by the `database` parameter — so clients use one
stable address instead of per-branch ports. **The gateway authenticates with an
API key used as the password.** `vdb setup` creates a local key for you and
prints a ready-to-paste connection string (also saved in `~/.vectoradb/config`);
mint more with `vdb apikey create <email>` or on the web *API keys* page.

```bash
export PGPASSWORD="vdb_…"                                                  # your API key
psql "postgresql://vectoradb@127.0.0.1:6432/main?sslmode=require"          # -> main
psql "postgresql://vectoradb@127.0.0.1:6432/agent-bob?sslmode=require"     # -> bob's branch
```

The gateway serves a self-signed certificate by default (point
`VECTORADB_TLS_CERT`/`VECTORADB_TLS_KEY` at a real pair for `sslmode=verify-full`).

> `VECTORADB_GATEWAY_NOAUTH=1` disables gateway auth for trusted/local use; it is
> being removed from release builds.

**Auto-suspend / auto-resume (serverless behaviour).** The gateway suspends any
branch idle longer than `--idle` (default `2m`, `--idle 0` to disable) and
transparently resumes it on the next connection:

```bash
vdb gateway --addr :6432 --idle 90s   # idle branches stop; wake on connect
vdb branch suspend qa               # manual stop (data preserved)
vdb branch resume qa                # manual start
```

Suspended branches keep their data (only the container stops); resume takes a
couple of seconds. Connect through the gateway for a stable address — a resumed
branch's direct per-branch port may change.

MinIO console: http://localhost:9001 (per-install credentials in `~/.vectoradb/secrets.json`).

> **PITR window:** a timestamp target must fall at or before the last *archived*
> transaction; use `--to latest` for the newest state, and
> `SELECT pg_switch_wal();` to flush recent writes to the archive promptly.

### Agent Branch API — one database branch per AI agent

Run the HTTP service:

```bash
vdb serve --addr :8088
```

Then each agent gets its own instant, isolated, disposable database:

```bash
curl -X POST localhost:8088/agents/alice/branch    # -> {"dsn":"postgresql://…:PORT/vectoradb", …}
curl localhost:8088/agents                          # list active agent branches
curl -X DELETE localhost:8088/agents/alice/branch   # tear it down
```

The agent connects to the returned `dsn` like any PostgreSQL, works in full
isolation from `main` and other agents, and the branch is discarded on delete.

### High availability

`ha enable` provisions a hot **standby** that streams WAL from the primary
(asynchronous — no commit-latency cost). `ha failover` promotes it and reroutes
`main` through the gateway, so clients keep the **same connection string** across a
primary failure.

```bash
vdb ha enable      # standby streaming from main
vdb ha status      # replication state + lag
vdb ha failover    # promote standby; 'main' now routes to it
vdb ha disable     # remove the standby
```

> Single-VM demonstration of the mechanism (replication → promotion →
> rerouting). Production HA additionally needs multi-host deployment, automatic
> failure detection, and fencing against split-brain.

## What VectoraDB is not

Being precise about the boundaries is part of being trustworthy:

- **Not distributed / not multi-host (yet).** Compute and storage are separated
  *logically* — stateless containers over persistent storage, scale-to-zero —
  but they still run on one host. Networked storage disaggregation (compute
  scheduled independently of its data) is on the roadmap, not shipped.
- **HA is a single-VM demonstration**, not a production multi-host deployment.
- **Not a vector database**, despite the name — it is PostgreSQL. (You can of
  course use `pgvector` on it.)
- **Single-tenant today** — authenticated users share one instance; there is no
  RBAC or project isolation yet.

## Development

Build from a source checkout instead of the installer:

```bash
make build       # host binary -> ./bin/vdb
make vm-build    # Linux engine binary -> /tmp/vdb inside the Lima VM
make release     # cross-compiled binaries -> ./dist (darwin+linux, amd64+arm64)
```

`make release` produces the artifacts the installer downloads; the ZFS pool and
Docker image are created automatically on the first `vdb start`/`vdb up`.

### Releasing (maintainers)

Releases are automated: pushing a `v*` tag runs `.github/workflows/release.yml`,
which builds every binary and the install assets and attaches them to the GitHub
release. `deploy/install.sh` / `install.ps1` download `vdb-<os>-<arch>` from the
**latest** release.

```bash
git tag -a v0.5.3 -m "…" && git push origin v0.5.3   # CI builds + publishes the release
```

The repository (and its releases) must be **public** for the `curl … | sh`
one-liner to work for others.

> **Pending: org move.** The module path is `github.com/vectoradb/vectoradb`; the
> repo is being moved to a `vectoradb` GitHub org so `go install` resolves. When
> that transfer happens, sweep every `SauravYadav12/vectoraDB` reference (the
> install one-liners in `deploy/`, this README, `docs/`, `web/src/pages/`, and
> the GHCR image org) to `vectoradb/vectoradb`.

## Testing

```bash
make test          # Go unit tests (host, no VM needed)
make integration   # full end-to-end test inside the Lima VM
```

Unit tests cover the pure logic (PostgreSQL wire-protocol startup parse/rewrite,
CLI flag parsing, branch-name validation, port parsing, DSN building). The
integration test drives the real lifecycle and asserts each outcome — branch
isolation, PITR, suspend/resume, the agent API, and HA failover — plus a
regression check that the auto-suspend reaper never stops the standby.

## License

- **Core / server** (this repo, except `clients/`): **AGPL-3.0-or-later** — see
  [`LICENSE`](LICENSE).
- **Clients & SDKs** (`clients/`): **Apache-2.0** — see
  [`clients/LICENSE`](clients/LICENSE).
