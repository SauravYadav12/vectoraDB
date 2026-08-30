# VectoraDB

**Postgres for AI agents — instant branches, and a tamper-evident record of every schema change.**

VectoraDB is a **serverless PostgreSQL** platform. It keeps the hot transaction
path on stock Postgres on local NVMe (native commit latency) and moves
durability, branching, and time-travel *off* the commit path — **ZFS
copy-on-write** clones for instant branches and **asynchronous WAL archival**
(`wal-g`) for point-in-time recovery. It speaks the native Postgres wire
protocol, so your existing driver, ORM, and SQL work unchanged. It is the
postgres.ai / Database Lab model, implemented in Go.

The command-line tool is **`vdb`**. Everything below is a `vdb …` command.

> **New here?** Jump to [Install](#install) · [Quickstart](#quickstart) · [The Schema Ledger](#the-schema-ledger)

---

## Screenshots

The web console is served by the engine itself at **https://localhost:8080** — no
separate dev server to run.

| Ops dashboard | Schema Ledger | SQL console |
| --- | --- | --- |
| ![Dashboard](docs/screenshots/dashboard.png) | ![Ledger](docs/screenshots/ledger.png) | ![Console](docs/screenshots/console.png) |

---

## What you get

- **[Schema Ledger](#the-schema-ledger)** — every `CREATE`/`ALTER`/`DROP`/`GRANT`
  recorded with the actor (human or agent), tool, and branch; **tamper-evident**
  (hash-chained, append-only, `vdb ledger verify`) and **non-forgeable** (clients
  connect as a per-user role, so the recorded actor is the login identity). No
  other Postgres branching tool has this.
- **Instant branching** — `vdb branch create qa` clones the whole database in
  seconds (copy-on-write), fully isolated; `main` is untouched. Plus
  `vdb branch reset` (start over) and `vdb branch diff` (what changed, from the ledger).
- **Time travel / PITR** — continuous WAL archival; restore to any point.
- **One serverless endpoint** — connect to `:6432`; the database name *is* the
  branch. Idle branches scale to zero and wake on connect. TLS on by default, so
  `sslmode=require` clients connect out of the box.
- **A database per AI agent** — the Agent Branch API over HTTP, or the
  **Model Context Protocol** (`vdb mcp`): an agent gets a database, runs SQL, sees
  what it changed (from the ledger), and throws it away — one standard interface.
- **Migrate from anything** — import from PostgreSQL, MySQL/MariaDB, MongoDB, and
  `.sql`/`.csv`/`.json`/`.ndjson` files, each landing in a fresh branch.
- **ETL pipelines** *(experimental)* — dbt-style SQL models with data-quality
  tests, each run against a throwaway branch.
- **High availability** — a hot standby with transparent `ha failover`
  (single-VM demonstration).
- **Accounts, API keys, RLS** — email/password or GitHub/Google OAuth; keys are
  the gateway password; Postgres row-level security and GRANTs apply to clients.
- **Web console** — a React UI served by the engine: dashboard, SQL console,
  Ledger viewer, import, pipelines, API keys.
- **Client SDKs** — an OpenAPI spec (served at `/api/openapi.yaml`) plus
  dependency-free [Python and TypeScript clients](clients/).

Per-install credentials are generated on first run — **nothing is hardcoded**.

---

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
vdb ledger              # every schema change on this branch, most recent first
vdb ledger verify       # prove the record has not been tampered with
```

**It cannot be quietly rewritten.** Each row is hash-chained to the one before
it, so a deleted or edited entry breaks the chain and `vdb ledger verify` catches
it — even if a superuser disabled the triggers. The table is append-only. And
because the gateway logs each client in as a **per-user Postgres role**, the
recorded actor is the login identity: a client cannot forge who made a change,
even by `SET`-ting a session variable. There is a **Ledger** page in the web
console too.

---

## Install

One command installs the `vdb` CLI; a second brings everything up. On macOS and
Windows the Linux engine (ZFS/btrfs + Docker + Postgres) runs transparently
inside a managed VM, so day-to-day you only ever type `vdb …`.

Every install pulls the **latest** engine build and (re)installs it, so re-running
setup always updates you to the newest version rather than reusing an old one.

### macOS

Needs [Lima](https://lima-vm.io) for the local Linux VM.

```bash
brew install lima
curl -fsSL https://raw.githubusercontent.com/vectoradb/vectoraDB/main/deploy/install.sh | sh
vdb setup
```

`vdb setup` creates a dedicated Lima VM, installs Docker + ZFS in it, and brings
the stack up. Your first `vdb setup` downloads Ubuntu (a few minutes); after that
it's fast.

### Linux

The engine runs directly (no VM). ZFS + Docker are provisioned on first start.

```bash
curl -fsSL https://raw.githubusercontent.com/vectoradb/vectoraDB/main/deploy/install.sh | sh
sudo vdb start
```

### Windows

Run **in PowerShell** (not Command Prompt — `irm`/`iex` are PowerShell commands).
Needs Windows 10 21H2+/11 with virtualization enabled; everything else is handled
for you. The engine runs in a dedicated **WSL2** distro that stores branches on
**btrfs**, so it works on any WSL kernel — nothing kernel-specific to build.

```powershell
irm https://raw.githubusercontent.com/vectoradb/vectoraDB/main/deploy/install.ps1 | iex
```

That installs WSL if absent (no Linux distribution of your own needed —
VectoraDB brings its own dedicated distro), downloads VectoraDB, puts it on your
PATH, and runs `vdb setup`. If Windows needs a reboot to finish enabling WSL, it
says so and resumes automatically afterwards. Your other WSL distros and Docker
Desktop are untouched. Full prerequisites and troubleshooting:
**[docs/windows-setup.md](docs/windows-setup.md)**.

### From source (contributors)

```bash
make build       # host binary -> ./bin/vdb
make vm-build    # build the Linux engine into the Lima VM (macOS)
```

Set `VECTORADB_NO_REFRESH=1` when running `vdb setup` from a source build, so it
keeps your locally-built engine instead of downloading a release.

---

## Quickstart

After `vdb setup` (macOS/Windows) or `vdb start` (Linux), the banner prints a
ready-to-paste connection string and a local API key (also saved in
`~/.vectoradb/config`). Then:

```bash
vdb status                       # servers, primary readiness, branches
vdb branch create qa             # instant copy-on-write branch of main
```

Connect any Postgres client through the gateway — the **database name is the
branch**, and the **password is your API key**:

```bash
psql "postgresql://vectoradb:<API_KEY>@localhost:6432/qa?sslmode=require"
```

```sql
CREATE TABLE notes (id serial PRIMARY KEY, body text, created_at timestamptz DEFAULT now());
INSERT INTO notes(body) VALUES ('hello');
```

```bash
vdb ledger qa                    # see that CREATE TABLE, attributed to you
vdb branch delete qa             # throw it away; main is untouched
```

Mint more keys with `vdb apikey create <email>` (or on the web *API keys* page).

---

## The web console

`vdb start` serves the console at **https://localhost:8080** (a self-signed cert,
so your browser shows a one-time "not private" warning to accept; point
`VECTORADB_TLS_CERT`/`_KEY` at a real pair to avoid it). It has:

- a **dashboard** (live status + branch create/suspend/resume/delete),
- a **SQL console** (run queries against any branch, expand rows as JSON),
- a **Ledger** viewer (filter by actor, table, risk, kind),
- **import** and **pipelines** pages, and **API keys**.

> The web app is embedded in the engine binary and served same-origin — there is
> no separate dev server to run.

---

## Usage

```bash
vdb start        # bring EVERYTHING up in the background: stack + gateway + APIs
vdb status       # servers, main readiness, backups, HA, branches
vdb logs gateway # tail a background server's log
vdb stop         # stop servers and containers (data preserved)
```

**Branching**

```bash
vdb branch create qa            # instant copy-on-write branch of main
vdb branch list                 # branches and their containers
vdb branch reset qa             # re-clone from parent, discarding changes
vdb branch diff main qa         # schema changes distinguishing two branches (from the ledger)
vdb branch suspend qa           # stop a branch (data preserved); wakes on connect
vdb branch resume qa
vdb branch delete qa
```

**Schema ledger**

```bash
vdb ledger [branch] [--limit N] # captured DDL — attributed and policy-checked
vdb ledger verify [branch]      # verify the tamper-evident hash chain
vdb ledger revert --to <ts>     # time-travel a branch's schema+data to a moment
```

**Durability / time travel**

```bash
vdb backup create               # base backup -> object storage
vdb backup list
vdb restore --to latest         # PITR into a disposable container on port 5433
vdb restore --to '2026-08-24 15:07:00+00'
```

**High availability**

```bash
vdb ha enable                   # hot standby streaming from main
vdb ha status
vdb ha failover                 # promote the standby; 'main' reroutes to it
vdb ha disable
```

**Accounts & keys**

```bash
vdb apikey create <email> [name]   # mint an API key (shown once)
vdb apikey list <email>
vdb apikey revoke <email> <id>
vdb user create <email>            # create an account (prompts for a password)
```

---

## Connect your app

VectoraDB **is** PostgreSQL, so every driver and ORM connects unchanged — set one
env var to the gateway address (database = branch, password = API key):

```bash
DATABASE_URL="postgresql://vectoradb:<API_KEY>@localhost:6432/main?sslmode=require"
```

Point Prisma, Drizzle, SQLAlchemy, Django, GORM, ActiveRecord, etc. at that URL.

For the REST API there is an **OpenAPI spec** (served at
`https://localhost:8080/api/openapi.yaml`) and thin, dependency-free SDKs under
[`clients/`](clients/):

```python
from vectoradb import VectoraDB
db = VectoraDB(api_key="vdb_…", verify_tls=False)   # local self-signed cert
db.create_branch("qa")
print(db.query("qa", "select 1"))
print(db.verify_ledger("qa"))
```

Generate a client for any other language from the spec (see [`clients/README.md`](clients/README.md)).

---

## A database per AI agent

Give each agent its own instant, disposable database — over HTTP:

```bash
vdb serve --addr :8088          # the Agent Branch API
curl -k -H "Authorization: Bearer $VDB_KEY" -X POST https://localhost:8088/agents/alice/branch
# -> { "dsn": "postgresql://…", … }   the agent connects to that dsn
curl -k -H "Authorization: Bearer $VDB_KEY" -X DELETE https://localhost:8088/agents/alice/branch
```

…or over the **Model Context Protocol**, so an agent framework drives it through
one standard interface:

```bash
vdb mcp                         # MCP server on stdio
```

MCP tools: `create_branch`, `run_sql`, `changes` (what did I change, from the
ledger), `verify_ledger`, `list_branches`, `delete_branch`. DDL an agent runs is
attributed to that agent automatically.

---

## Import & migration

Every import lands in a **fresh branch**, so migration is safe and reversible.

```bash
vdb import --from postgres://user:pw@host/db  --as prod-copy
vdb import --from mysql://user:pw@host/db     --as legacy
vdb import --from mongodb://host/db           --as events     # collections -> JSONB
vdb import --from ./dump.sql                  --as fromfile    # .sql/.csv/.json/.ndjson

vdb import --from postgres://… --continuous --as live          # logical replication
vdb import-cutover live                                         # finish the cutover
```

---

## What VectoraDB is not

- **Not distributed / not multi-host (yet).** Compute and storage are separated
  *logically* (stateless containers over persistent storage, scale-to-zero) but
  still run on one host. Networked storage disaggregation is on the roadmap.
- **HA is a single-VM demonstration**, not a production multi-host deployment.
- **Not a vector database**, despite the name — it is PostgreSQL (use `pgvector`
  on it if you like).
- **Single-tenant** — authenticated users share one instance; there is no project
  isolation yet.

---

## Development

```bash
make build        # host binary -> ./bin/vdb
make vm-build     # Linux engine binary into the Lima VM (macOS)
make vet test     # go vet + unit tests (also run in CI on Linux and Windows)
make integration  # full end-to-end test inside the VM
```

The web app lives in `web/` (Vite + React + TypeScript) and is embedded into the
engine binary via the `embedui` build tag, then served same-origin by `vdb start`.

Releases are automated: pushing a `v*` tag builds every binary + the install
assets and publishes the multi-arch Postgres image to GHCR.

---

## License

- **Core / server** (this repo, except `clients/`): **AGPL-3.0-or-later** — see
  [`LICENSE`](LICENSE).
- **Clients & SDKs** (`clients/`): **Apache-2.0** — see [`clients/LICENSE`](clients/LICENSE).

> **Using VectoraDB does not make your application AGPL.** Connecting over the
> Postgres wire protocol is not a derivative work, and the client libraries are
> Apache-2.0. The copyleft applies only if you modify VectoraDB itself and offer
> it to others as a service.
