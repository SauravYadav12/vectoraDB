#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# End-to-end integration test. Run inside the Linux dev VM (ZFS + Docker):
#   lima bash "/Users/.../Distributed Database/scripts/integration_test.sh"
# Exits non-zero if any assertion fails.
set -uo pipefail

S="${VECTORADB_BIN:-/tmp/vdb}"
# The Gateway authenticates with an API key, passed via PGPASSWORD where used.
GATEWAY="postgresql://vectoradb@127.0.0.1:6432"
PASS=0
FAIL=0

ok()  { echo "  PASS: $1"; PASS=$((PASS + 1)); }
bad() { echo "  FAIL: $1"; FAIL=$((FAIL + 1)); }
assert_eq() { if [ "$2" = "$3" ]; then ok "$1"; else bad "$1 (got '$2', want '$3')"; fi; }
# pg <container> <sql>  -> tuples-only result
pg() { local c="$1"; shift; sudo docker exec -e PGPASSWORD=vectoradb "$c" psql -U vectoradb -d vectoradb -tAc "$*" 2>/dev/null; }
jget() { python3 -c 'import sys,json; print(json.load(sys.stdin)[sys.argv[1]])' "$1"; }

echo "### 1. fresh start + auth"
$S stop >/dev/null 2>&1; sleep 1
$S start >/dev/null 2>&1; sleep 5
printf 'password123\n' | $S user create test@vectoradb.dev >/dev/null 2>&1 || true
KEY="$($S apikey create test@vectoradb.dev ci 2>/dev/null | grep -o 'vdb_[A-Za-z0-9_-]*')"
AUTH="Authorization: Bearer $KEY"
assert_eq "unauthenticated API is rejected" "$(curl -s -o /dev/null -w '%{http_code}' localhost:8080/api/status)" "401"
assert_eq "control plane reports main ready" "$(curl -s -H "$AUTH" localhost:8080/api/status | jget mainReady)" "True"
assert_eq "gateway rejects a bad key" "$(PGPASSWORD=nope psql "$GATEWAY/main" -tAc 'select 1' 2>&1 | grep -c 'invalid API key')" "1"
assert_eq "gateway accepts the API key" "$(PGPASSWORD="$KEY" psql "$GATEWAY/main" -tAc 'select 1' 2>/dev/null)" "1"

echo "### 2. branch isolation"
$S branch delete itb >/dev/null 2>&1
$S branch create itb >/dev/null 2>&1
pg vec-itb "CREATE TABLE iso(x int); INSERT INTO iso VALUES (1);" >/dev/null
assert_eq "branch sees its own write" "$(pg vec-itb 'SELECT count(*) FROM iso')" "1"
assert_eq "main isolated from branch" "$(pg vec-main "SELECT to_regclass('public.iso') IS NULL")" "t"

echo "### 3. time-travel / PITR"
pg vec-main "DROP TABLE IF EXISTS pit; CREATE TABLE pit(id int); INSERT INTO pit SELECT generate_series(1,3);" >/dev/null
$S backup create >/dev/null 2>&1
pg vec-main "SELECT pg_switch_wal();" >/dev/null; sleep 3
$S restore --to latest >/dev/null 2>&1; sleep 1
assert_eq "PITR restores 3 rows" "$(pg vec-restore 'SELECT count(*) FROM pit')" "3"
sudo docker rm -f vec-restore >/dev/null 2>&1

echo "### 4. suspend / resume"
$S branch suspend itb >/dev/null 2>&1
assert_eq "branch suspends" "$(sudo docker inspect -f '{{.State.Status}}' vec-itb 2>/dev/null)" "exited"
$S branch resume itb >/dev/null 2>&1
assert_eq "branch resumes" "$(sudo docker inspect -f '{{.State.Status}}' vec-itb 2>/dev/null)" "running"

echo "### 5. agent branch API"
RESP="$(curl -s -H "$AUTH" -X POST localhost:8088/agents/itest/branch)"
DSN="$(echo "$RESP" | jget dsn)"
psql "$DSN" -c "CREATE TABLE a(x int); INSERT INTO a VALUES (7);" >/dev/null 2>&1
assert_eq "agent DB is usable via its DSN" "$(psql "$DSN" -tAc 'SELECT x FROM a' 2>/dev/null)" "7"
curl -s -H "$AUTH" -X DELETE localhost:8088/agents/itest/branch >/dev/null

echo "### 6. HA: replication + failover"
$S ha enable >/dev/null 2>&1; sleep 2
assert_eq "standby is streaming" "$(pg vec-main "SELECT count(*) FROM pg_stat_replication WHERE state='streaming'")" "1"
$S ha failover >/dev/null 2>&1; sleep 3
W="$(PGPASSWORD="$KEY" psql "$GATEWAY/main" -tAqc "INSERT INTO pit VALUES (99) RETURNING 'okwrite'" 2>&1 | head -1)"
assert_eq "write succeeds via gateway after failover" "$W" "okwrite"
$S ha disable >/dev/null 2>&1; $S up >/dev/null 2>&1; sleep 3

echo "### 7. regression: reaper never suspends the standby"
$S ha enable >/dev/null 2>&1; sleep 2
$S branch create rgn >/dev/null 2>&1
nohup "$S" gateway --addr :6501 --idle 8s >/tmp/rgngateway.log 2>&1 &
RP=$!
sleep 28
assert_eq "standby survives the reaper" "$(sudo docker inspect -f '{{.State.Status}}' vec-standby 2>/dev/null)" "running"
assert_eq "ordinary branch is suspended" "$(sudo docker inspect -f '{{.State.Status}}' vec-rgn 2>/dev/null)" "exited"
kill "$RP" 2>/dev/null
$S branch delete rgn >/dev/null 2>&1
$S ha disable >/dev/null 2>&1

echo "### 8. schema ledger (RECORD layer)"
pg vec-main "SET vdb.allow_destructive=on; DROP TABLE IF EXISTS ledg CASCADE" >/dev/null 2>&1
# Clean slate: the ledger is append-only now, so clearing it for a deterministic
# count requires deliberately disabling triggers (session_replication_role).
pg vec-main "SET session_replication_role=replica; DELETE FROM vdb.schema_ledger; SET session_replication_role=DEFAULT" >/dev/null 2>&1
PGPASSWORD="$KEY" psql "$GATEWAY/main" -qc "CREATE TABLE ledg(x int)" >/dev/null 2>&1
assert_eq "ledger captures DDL, attributed to the human key" \
  "$(pg vec-main "SELECT actor_kind FROM vdb.schema_ledger WHERE command_tag='CREATE TABLE' AND object_identity='public.ledg'")" "human"
BLK="$(PGPASSWORD="$KEY" psql "$GATEWAY/main" -qc "DROP TABLE ledg" 2>&1 | grep -c 'blocked by policy')"
assert_eq "guardrail blocks a destructive DROP" "$BLK" "1"
assert_eq "blocked attempt is recorded durably" \
  "$(pg vec-main "SELECT count(*) FROM vdb.schema_ledger WHERE status='BLOCKED' AND command_tag='DROP TABLE'")" "1"
# Tamper-evidence: the ledger is append-only, and the hash chain verifies intact.
assert_eq "ledger is append-only (a plain DELETE is blocked)" \
  "$(pg vec-main "DELETE FROM vdb.schema_ledger WHERE id=(SELECT max(id) FROM vdb.schema_ledger)" 2>&1 | grep -c 'append-only')" "1"
assert_eq "hash chain verifies intact (0 broken rows)" \
  "$(pg vec-main "SELECT count(*) FROM (SELECT (row_hash <> vdb._ledger_hash(s.*) OR prev_hash IS DISTINCT FROM coalesce(lag(row_hash) OVER (ORDER BY id),'')) AS broken FROM vdb.schema_ledger s WHERE row_hash IS NOT NULL) x WHERE broken")" "0"
pg vec-main "SET vdb.allow_destructive=on; DROP TABLE IF EXISTS ledg" >/dev/null 2>&1

echo "### 9. ETL pipeline (extract -> transform -> test)"
sudo docker rm -f mongo-src >/dev/null 2>&1
sudo docker run -d --name mongo-src --network vectoradb mongo:7 >/dev/null 2>&1
for i in $(seq 1 40); do sudo docker exec mongo-src mongosh --quiet --eval 'db.runCommand({ping:1}).ok' >/dev/null 2>&1 && break; sleep 1; done
sudo docker exec mongo-src mongosh --quiet shop --eval \
  'db.buildings.insertMany([{name:"Empire State",floors:102,addr:{city:"New York"}},{name:"Willis Tower",floors:108,addr:{city:"Chicago"}},{name:"Aon Center",floors:83,addr:{city:"Chicago"}}])' >/dev/null 2>&1
$S branch delete etltest >/dev/null 2>&1
cat > /tmp/etl_ok.json << 'JSON'
{"source":"mongodb://mongo-src/shop","models":[{"name":"stg_buildings","sql":"SELECT name, floors, addr->>'city' AS city FROM {{ source('buildings') }}"},{"name":"city_counts","sql":"SELECT city, count(*) AS n FROM {{ ref('stg_buildings') }} GROUP BY city"}],"tests":[{"name":"name not null","type":"not_null","model":"stg_buildings","column":"name"}]}
JSON
$S pipeline run /tmp/etl_ok.json --as etltest >/dev/null 2>&1
assert_eq "ETL passing pipeline exits 0" "$?" "0"
assert_eq "ETL landed raw source in the raw schema" "$(pg vec-etltest "SELECT to_regclass('raw.buildings') IS NOT NULL")" "t"
assert_eq "ETL model flattened jsonb into a column" "$(pg vec-etltest "SELECT city FROM public.stg_buildings WHERE name='Empire State'")" "New York"
assert_eq "ETL aggregate model computed" "$(pg vec-etltest "SELECT n FROM public.city_counts WHERE city='Chicago'")" "2"

$S branch delete etlfail >/dev/null 2>&1
cat > /tmp/etl_fail.json << 'JSON'
{"source":"mongodb://mongo-src/shop","models":[{"name":"stg_b","sql":"SELECT name FROM {{ source('buildings') }}"}],"tests":[{"name":"needs 100 rows","type":"row_count_min","model":"stg_b","min":100}]}
JSON
$S pipeline run /tmp/etl_fail.json --as etlfail >/dev/null 2>&1
assert_eq "ETL failing test yields non-zero exit" "$?" "1"
assert_eq "ETL failed run keeps its data" "$(pg vec-etlfail "SELECT count(*) FROM public.stg_b")" "3"

echo "### 10. schema fidelity (source field names preserved exactly)"
sudo docker exec mongo-src mongosh --quiet shop --eval \
  'db.leaseAiChats.insertOne({userId:new ObjectId(), createdAt:new Date(), gallery:[{fileName:"a.jpg"}]})' >/dev/null 2>&1
$S branch delete faithtest >/dev/null 2>&1
cat > /tmp/faith.json << 'JSON'
{"source":"mongodb://mongo-src/shop","models":[{"name":"stg","sql":"SELECT \"userId\", \"createdAt\", \"gallery\" FROM {{ source('leaseAiChats') }}"}]}
JSON
$S pipeline run /tmp/faith.json --as faithtest >/dev/null 2>&1
assert_eq "camelCase column preserved (userId, not userid)" "$(pg vec-faithtest "SELECT count(*) FROM information_schema.columns WHERE table_schema='raw' AND table_name='leaseAiChats' AND column_name='userId'")" "1"
assert_eq "nested array kept as jsonb" "$(pg vec-faithtest "SELECT data_type FROM information_schema.columns WHERE table_schema='raw' AND table_name='leaseAiChats' AND column_name='gallery'")" "jsonb"
assert_eq "transform resolves the case-sensitive source" "$(pg vec-faithtest "SELECT count(*) FROM public.stg")" "1"

echo "### cleanup"
$S branch delete itb >/dev/null 2>&1
$S branch delete etltest >/dev/null 2>&1
$S branch delete etlfail >/dev/null 2>&1
$S branch delete faithtest >/dev/null 2>&1
sudo docker rm -f mongo-src >/dev/null 2>&1

echo
echo "==== ${PASS} passed, ${FAIL} failed ===="
[ "$FAIL" -eq 0 ]
