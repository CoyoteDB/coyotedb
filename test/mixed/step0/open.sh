#!/bin/sh
# Open a data dir made on ANOTHER platform, verify it, write to it, verify again, repack.
#   PGBIN=... WORK=... sh open.sh <source-label> <this-label>
set -eu
HERE="$(cd "$(dirname "$0")" && pwd)"; . "$HERE/lib.sh"
src="$1"; me="$2"; D="$WORK/pgdata-$src"; rm -rf "$D"
( cd "$WORK" && tar -xzf "pgdata-$src.tar.gz" ) && chmod -R go-rwx "$D"
export PGPASSWORD=step0pw
out="$WORK/$src-on-$me.verify"
{ echo "### opened on: $(uname -sm)"; echo "--- fingerprint as read here ---"; fingerprint "$D"; } > "$out" 2>&1 || { echo "pg_controldata REFUSED the files" >> "$out"; cat "$out"; exit 3; }
"$PGBIN/pg_checksums" --check -D "$D" | tail -2 >> "$out" 2>&1 || true
if ! start_pg "$D"; then echo "SERVER REFUSED TO START on foreign files" >> "$out"; tail -20 "$WORK/pg.log" >> "$out"; cat "$out"; exit 4; fi
verify "$src opened on $me (before writes)" >> "$out" 2>&1
# Now behave like a newly promoted primary on this platform: heavy writes through every index.
q >/dev/null <<SQL
INSERT INTO t(txt,vc,ch,n,i4,i8,f4,f8,b,u,ts,tstz,d,tm,iv,j,arr,tarr,bt,ip,r,tsr,fts)
SELECT txt || ' @$me', vc, ch, n + 1, i4 + 1, i8 - 1, f4, f8, NOT b, md5(u::text || '$me')::uuid, ts, tstz, d, tm, iv, j || jsonb_build_object('from','$me'), arr, tarr, bt, ip, int4range(lower(r)+1000000, upper(r)+1000000), tsr, fts
FROM t WHERE id % 5 = 0;
UPDATE t SET txt = reverse(txt), n = n * 1.0000001 WHERE id % 7 = 0;
DELETE FROM t WHERE id % 23 = 0;
INSERT INTO hp SELECT g + 1000000, '$me' || (SELECT w FROM words WHERE id = 1 + (g % 35)) || g, g FROM generate_series(1, 30000) g;
UPDATE hp SET v = v + 1 WHERE id % 3 = 0;
REFRESH MATERIALIZED VIEW CONCURRENTLY mv;
REINDEX INDEX t_trgm_gin; VACUUM (ANALYZE); CHECKPOINT;
SQL
verify "$src opened on $me (after writes)" >> "$out" 2>&1
stop_pg "$D"
"$PGBIN/pg_checksums" --check -D "$D" | tail -2 >> "$out" 2>&1
mv "$D" "$WORK/pgdata-$src-via-$me" && ( cd "$WORK" && tar -czf "pgdata-$src-via-$me.tar.gz" "pgdata-$src-via-$me" )
echo "done: $out"
