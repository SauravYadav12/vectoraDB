#!/bin/bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Disposable point-in-time restore target. Fetches the latest base backup from
# object storage, replays archived WAL up to RECOVERY_TARGET_TIME, then promotes
# to a normal read/write instance so it can be queried.
set -euo pipefail

: "${PGDATA:=/var/lib/postgresql/data/pgdata}"
: "${RECOVERY_TARGET_TIME:?set RECOVERY_TARGET_TIME to a timestamp or the literal 'latest'}"

mkdir -p "$PGDATA"
chown -R postgres:postgres "$(dirname "$PGDATA")"

echo ">> fetching latest base backup from object storage..."
gosu postgres wal-g backup-fetch "$PGDATA" LATEST

# 'latest' = recover ALL archived WAL and promote (no target time). A specific
# timestamp must fall within the archived WAL window (i.e. at or before the last
# archived transaction), otherwise Postgres fatals with "recovery ended before
# target reached".
if [ "$RECOVERY_TARGET_TIME" = "latest" ]; then
  echo ">> configuring recovery to: latest (end of archived WAL)"
  cat >> "$PGDATA/postgresql.auto.conf" <<EOF
restore_command = 'wal-g wal-fetch %f %p'
recovery_target_action = 'promote'
archive_mode = off
EOF
else
  echo ">> configuring recovery to: $RECOVERY_TARGET_TIME"
  cat >> "$PGDATA/postgresql.auto.conf" <<EOF
restore_command = 'wal-g wal-fetch %f %p'
recovery_target_time = '$RECOVERY_TARGET_TIME'
recovery_target_action = 'promote'
archive_mode = off
EOF
fi
gosu postgres touch "$PGDATA/recovery.signal"
chmod 700 "$PGDATA"

echo ">> starting postgres to perform recovery..."
exec gosu postgres postgres -c listen_addresses='*'
