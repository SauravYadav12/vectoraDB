# VectoraDB

A **serverless PostgreSQL** platform with instant branching, time-travel /
point-in-time recovery, and an agent-branch API — built so that **transaction
speed is never compromised**.

VectoraDB keeps the hot transaction path on **stock PostgreSQL on local NVMe**
(native commit and read latency) and moves durability, branching, and
time-travel **off** the commit path: **ZFS copy-on-write** clones for instant
branches, and **asynchronous WAL archival** (via `wal-g`) to object storage for
durability and PITR. It is the postgres.ai / Database Lab model, implemented in
Go.

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

`vdb setup` (macOS) / `vdb start` (Linux) creates the VM, installs Docker + ZFS,
builds the pool and image, and brings the database, gateway, and APIs up — no
manual steps. On macOS, `vdb` transparently runs the engine inside the VM, so
every command below is just `vdb …` with no `lima` prefix.

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
`postgres://vectoradb:vectoradb@localhost:6432/<branch>`, agent API
`http://localhost:8088`, object storage `http://localhost:9001`.

### Web app (`web/`)

The UI is a standalone **Vite + React + TypeScript** app that consumes the
control-plane REST API — it is **not** embedded in the Go binary. It has a
landing page, docs, an ops **dashboard** (live status + branch CRUD), and a
**SQL console**. Run it with `make web-dev`; point it at a different API with
`VITE_API_URL`. The API is also usable directly:

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

`vdb gateway` exposes one PostgreSQL endpoint (`:6432`) and routes each
connection to the branch named by the `database` parameter — so clients use one
stable address instead of per-branch ports:

```bash
psql "postgresql://vectoradb:vectoradb@127.0.0.1:6432/main"        # -> main
psql "postgresql://vectoradb:vectoradb@127.0.0.1:6432/agent-bob"   # -> bob's branch
```

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

MinIO console: http://localhost:9001 (`minioadmin` / `minioadmin`).

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

Cut a versioned release so the installer one-liner can fetch prebuilt binaries:

```bash
make release VERSION=0.1.0                 # -> dist/vdb-{darwin,linux}-{amd64,arm64}
gh release create v0.1.0 dist/* \
  --title v0.1.0 --notes "First release"    # uploads the binaries as release assets
```

`deploy/install.sh` downloads `vdb-<os>-<arch>` from the **latest** release.
For the public `curl … | sh` one-liner to work for others, the repository (and
thus its releases) must be **public** — on a private repo, asset downloads
require an authenticated GitHub token.

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
