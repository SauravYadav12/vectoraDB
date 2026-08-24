#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# End-to-end integration test. Run inside the Linux dev VM (ZFS + Docker):
#   lima bash "/Users/.../Distributed Database/scripts/integration_test.sh"
# Exits non-zero if any assertion fails.
set -uo pipefail

S="${VECTORADB_BIN:-/tmp/vectoradb}"
PROXY="postgresql://vectoradb:vectoradb@127.0.0.1:6432"
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
W="$(psql "$PROXY/main" -tAqc "INSERT INTO pit VALUES (99) RETURNING 'okwrite'" 2>&1 | head -1)"
assert_eq "write succeeds via proxy after failover" "$W" "okwrite"
$S ha disable >/dev/null 2>&1; $S up >/dev/null 2>&1; sleep 3

echo "### 7. regression: reaper never suspends the standby"
$S ha enable >/dev/null 2>&1; sleep 2
$S branch create rgn >/dev/null 2>&1
nohup "$S" proxy --addr :6501 --idle 8s >/tmp/rgnproxy.log 2>&1 &
RP=$!
sleep 28
assert_eq "standby survives the reaper" "$(sudo docker inspect -f '{{.State.Status}}' vec-standby 2>/dev/null)" "running"
assert_eq "ordinary branch is suspended" "$(sudo docker inspect -f '{{.State.Status}}' vec-rgn 2>/dev/null)" "exited"
kill "$RP" 2>/dev/null
$S branch delete rgn >/dev/null 2>&1
$S ha disable >/dev/null 2>&1

echo "### cleanup"
$S branch delete itb >/dev/null 2>&1

echo
echo "==== ${PASS} passed, ${FAIL} failed ===="
[ "$FAIL" -eq 0 ]
