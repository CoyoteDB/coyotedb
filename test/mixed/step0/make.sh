#!/bin/sh
# Create a fresh data dir with CoyoteDB's initdb settings, load the workload,
# verify, stop cleanly, and pack it.   PGBIN=... WORK=... sh make.sh <label>
set -eu
HERE="$(cd "$(dirname "$0")" && pwd)"; . "$HERE/lib.sh"
label="$1"; D="$WORK/pgdata-$label"; rm -rf "$D"; mkdir -p "$WORK"
printf 'step0pw\n' > "$WORK/pw"
"$PGBIN/initdb" -D "$D" -U postgres --pwfile="$WORK/pw" --auth=scram-sha-256 --locale-provider=builtin --builtin-locale=C.UTF-8 --encoding=UTF8 --locale=C --data-checksums >/dev/null
export PGPASSWORD=step0pw
{ echo "### platform: $(uname -sm)"; build_info; echo "--- fingerprint ---"; fingerprint "$D"; } > "$WORK/$label.info"
start_pg "$D"
q -f "$HERE/workload.sql" >/dev/null
verify "$label native" > "$WORK/$label.native.verify" 2>&1
stop_pg "$D"
"$PGBIN/pg_checksums" --check -D "$D" | tail -2 >> "$WORK/$label.native.verify"
( cd "$WORK" && tar -czf "pgdata-$label.tar.gz" "pgdata-$label" )
ls -la "$WORK/pgdata-$label.tar.gz" | awk '{print "packed bytes:", $5}'
