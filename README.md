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
- **Phase 3 — serverless front door:** ✅ wire-protocol proxy (one endpoint,
  route by database name), **auto-suspend/resume** (idle branches stop; the
  proxy wakes them on connect), **background/daemon mode** (`start`/`stop`), a
  **web SQL console**, and a **control-plane REST API + dashboard** (create /
  suspend / resume / delete branches from the browser).
- **Phase 4 — High availability:** ✅ a hot standby streams from the primary;
  `ha failover` promotes it and the proxy reroutes `main` transparently.
- **Later:** test suite, developer docs site; control-plane polish (multi-tenant
  projects, auth).

## Architecture (dev)

Everything runs inside a single Linux VM (Lima) with ZFS + Docker:

- `minio` — S3-compatible object storage (WAL archive target).
- `vec-main` — the primary Postgres (our `postgres:16 + wal-g` image), data dir
  on ZFS dataset `vectoradb/branches/main`, archiving WAL to MinIO.
- `vec-<name>` — a branch: a `zfs clone` of main served by its own Postgres.
- `vec-restore` — a disposable point-in-time restore target (port 5433).

## Dev environment (macOS)

```bash
brew install lima
limactl start --tty=false            # Ubuntu VM named "default"; repo is auto-mounted
lima sudo apt-get update && lima sudo apt-get install -y zfsutils-linux docker.io golang-go
# ZFS pool on a file vdev:
lima sudo truncate -s 30G /var/lib/vectoradb-zpool.img
lima sudo zpool create -f vectoradb /var/lib/vectoradb-zpool.img
lima sudo zfs create vectoradb/branches
```

Build the wal-g image and the CLI (inside the VM):

```bash
lima bash -c 'cd "'"$PWD"'" && sudo docker build -t vectoradb/postgres-walg:16 docker/postgres && go build -o /tmp/vectoradb ./cmd/vectoradb'
```

## Usage (run via `lima`)

**Fastest path — one command brings everything up in the background** (stack +
proxy + agent API + console), detached, no terminal held open:

```bash
lima /tmp/vectoradb start    # everything up (background)
lima /tmp/vectoradb status   # servers + main + branches
lima /tmp/vectoradb logs proxy
lima /tmp/vectoradb stop      # stop servers + containers
```

Then, from your Mac (Lima forwards the ports), open the **dashboard** —
**http://localhost:8080** — to see status and create/suspend/resume/delete
branches by clicking. Other endpoints: proxy
`postgres://vectoradb:vectoradb@localhost:6432/<branch>` · agent API
http://localhost:8088 · SQL console http://localhost:8081 · storage
http://localhost:9001.

### Dashboard & control-plane API

The dashboard (`vectoradb dashboard`, port `:8080`) is a web UI over a small REST
API — live status cards plus a table of branches with one-click actions. The API
is usable directly:

```bash
curl localhost:8080/api/status
curl localhost:8080/api/branches
curl -X POST localhost:8080/api/branches -d '{"name":"qa"}'
curl -X POST localhost:8080/api/branches/qa/suspend
curl -X DELETE localhost:8080/api/branches/qa
```

The individual commands below are for foreground/manual use:

```bash
lima /tmp/vectoradb up                 # network + MinIO + primary 'main' (archiving)
lima /tmp/vectoradb status             # readiness + backups + branches

lima /tmp/vectoradb backup create      # base backup -> object storage
lima /tmp/vectoradb restore --to latest        # PITR into a disposable container (port 5433)
lima /tmp/vectoradb restore --to '2026-08-24 15:07:00+00'

lima /tmp/vectoradb branch create qa   # instant copy-on-write branch
lima /tmp/vectoradb branch list
lima /tmp/vectoradb branch delete qa

lima /tmp/vectoradb console            # web SQL console -> http://localhost:8081
lima /tmp/vectoradb proxy --addr :6432 # one endpoint; route with dbname=<branch>

lima /tmp/vectoradb down               # stop containers (ZFS datasets preserved)
```

### Web console (try your DB in a browser)

`vectoradb console [branch]` runs a pgweb UI connected to a branch (default
`main`). Open **http://localhost:8081** on your Mac — browse tables, run SQL, no
setup.

### Single endpoint (serverless front door)

`vectoradb proxy` exposes one PostgreSQL endpoint (`:6432`) and routes each
connection to the branch named by the `database` parameter — so clients use one
stable address instead of per-branch ports:

```bash
psql "postgresql://vectoradb:vectoradb@127.0.0.1:6432/main"        # -> main
psql "postgresql://vectoradb:vectoradb@127.0.0.1:6432/agent-bob"   # -> bob's branch
```

**Auto-suspend / auto-resume (serverless behaviour).** The proxy suspends any
branch idle longer than `--idle` (default `2m`, `--idle 0` to disable) and
transparently resumes it on the next connection:

```bash
vectoradb proxy --addr :6432 --idle 90s   # idle branches stop; wake on connect
vectoradb branch suspend qa               # manual stop (data preserved)
vectoradb branch resume qa                # manual start
```

Suspended branches keep their data (only the container stops); resume takes a
couple of seconds. Connect through the proxy for a stable address — a resumed
branch's direct per-branch port may change.

MinIO console: http://localhost:9001 (`minioadmin` / `minioadmin`).

> **PITR window:** a timestamp target must fall at or before the last *archived*
> transaction; use `--to latest` for the newest state, and
> `SELECT pg_switch_wal();` to flush recent writes to the archive promptly.

### Agent Branch API — one database branch per AI agent

Run the HTTP service:

```bash
lima /tmp/vectoradb serve --addr :8088
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
`main` through the proxy, so clients keep the **same connection string** across a
primary failure.

```bash
vectoradb ha enable      # standby streaming from main
vectoradb ha status      # replication state + lag
vectoradb ha failover    # promote standby; 'main' now routes to it
vectoradb ha disable     # remove the standby
```

> Single-VM demonstration of the mechanism (replication → promotion →
> rerouting). Production HA additionally needs multi-host deployment, automatic
> failure detection, and fencing against split-brain.

## License

- **Core / server** (this repo, except `clients/`): **AGPL-3.0-or-later** — see
  [`LICENSE`](LICENSE).
- **Clients & SDKs** (`clients/`): **Apache-2.0** — see
  [`clients/LICENSE`](clients/LICENSE).
