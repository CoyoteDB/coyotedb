#!/bin/sh
# Shared helpers for the Step 0 cross-platform file test (Unix side).
# Usage: . lib.sh ; needs PGBIN (bin dir of the pinned Postgres) and WORK (scratch dir).
set -eu
PORT="${PORT:-55440}"
pg() { "$PGBIN/$1" ; }
start_pg() { # $1 = data dir
  "$PGBIN/pg_ctl" -D "$1" -w -t 120 -l "$WORK/pg.log" \
    -o "-c listen_addresses=127.0.0.1 -c port=$PORT -c unix_socket_directories='' -c jit=off -c shared_buffers=128MB -c max_wal_size=2GB -c fsync=on" start >/dev/null
}
stop_pg() { "$PGBIN/pg_ctl" -D "$1" -w -t 120 -m fast stop >/dev/null; }
q() { "$PGBIN/psql" -X -q -h 127.0.0.1 -p "$PORT" -U postgres -d postgres -v ON_ERROR_STOP=1 "$@"; }
fingerprint() { # $1 = data dir
  LC_ALL=C "$PGBIN/pg_controldata" -D "$1" | grep -E "pg_control version|Catalog version|Maximum data alignment|Database block size|Blocks per segment|WAL block size|Bytes per WAL segment|Maximum length of identifiers|Maximum columns in an index|Maximum size of a TOAST chunk|Size of a large-object chunk|Date/time type storage|Float8 argument passing|Data page checksum version|Default char data signedness" | sed -E 's/  +/ /g' | sort
}
build_info() {
  echo "version: $("$PGBIN/postgres" --version)"
  echo "configure: $("$PGBIN/pg_config" --configure | tr ' ' '\n' | grep -E "^'?--(with|enable|disable|without)" | sort | tr '\n' ' ')"
}
verify() { # $1 = label ; runs every check against the running server, prints a block
  echo "=== $1: amcheck ==="
  "$PGBIN/pg_amcheck" -h 127.0.0.1 -p "$PORT" -U postgres -d postgres --heapallindexed --parent-check --rootdescend --install-missing 2>&1 | tail -5
  echo "amcheck exit: $?"
  q -At -c "SELECT 'gin_index_check ' || c.relname || ' ' || coalesce(gin_index_check(c.oid::regclass)::text,'ok') FROM pg_class c JOIN pg_am a ON a.oid=c.relam WHERE a.amname='gin' AND c.relkind='i' ORDER BY 1" 2>&1 | sed 's/ $/ ok/' | head -12
  echo "=== $1: index vs table ==="
  q -f "$HERE/idxscan.sql" 2>&1 | grep -v "^$"
  echo "=== $1: digests ==="
  q -f "$HERE/digest.sql" 2>&1 | grep -v "^$"
}
