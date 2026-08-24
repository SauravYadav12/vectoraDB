#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Narrated feature tour of VectoraDB. Run inside the Linux dev VM:
#   lima bash "/Users/.../Distributed Database/scripts/demo.sh"
set -uo pipefail

S="${VECTORADB_BIN:-/tmp/vectoradb}"
PROXY="postgres://vectoradb:vectoradb@127.0.0.1:6432"
say() { echo; echo "──▶ $*"; echo; }
sql() { psql "$PROXY/$1" -c "$2"; }
pg()  { sudo docker exec -e PGPASSWORD=vectoradb "$1" psql -U vectoradb -d vectoradb "${@:2}"; }

say "Bringing VectoraDB up (stack + proxy + control API + agent API)"
$S start >/dev/null 2>&1; sleep 4
$S status 2>&1 | sed -n '1,12p'

say "Create an instant copy-on-write branch 'demo' (note the time)"
$S branch delete demo >/dev/null 2>&1
time $S branch create demo

say "CRUD on the branch, through the single proxy endpoint (dbname=demo)"
sql demo "CREATE TABLE notes(id serial PRIMARY KEY, body text);"
sql demo "INSERT INTO notes(body) VALUES ('hello'),('world') RETURNING *;"
sql demo "UPDATE notes SET body='edited' WHERE id=1;"
sql demo "SELECT * FROM notes ORDER BY id;"

say "Isolation — 'main' does NOT have that table"
pg vec-main -c "SELECT to_regclass('public.notes') AS notes_on_main;"

say "Copy-on-write: the branch stores only its delta (USED column)"
$S branch list

say "A database per AI agent, over HTTP"
curl -s -X POST localhost:8088/agents/alice/branch; echo

say "High availability — provision a streaming standby"
pg vec-main -c "CREATE TABLE IF NOT EXISTS ledger(n int);"
$S ha enable >/dev/null 2>&1; sleep 2
pg vec-main -x -c "SELECT application_name, state, sync_state FROM pg_stat_replication;"

say "Fail over — the SAME endpoint keeps working (write lands on the promoted standby)"
$S ha failover >/dev/null 2>&1; sleep 3
sql main "INSERT INTO ledger VALUES (1) RETURNING 'write ok after failover' AS result;"

say "Cleanup"
curl -s -X DELETE localhost:8088/agents/alice/branch >/dev/null
$S ha disable >/dev/null 2>&1; $S up >/dev/null 2>&1
$S branch delete demo >/dev/null 2>&1
echo
echo "Done.  Control API: http://localhost:8080/api/status   ·   Web UI: make web-dev (http://localhost:5173)"
