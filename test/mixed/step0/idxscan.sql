-- Index-vs-table agreement for index kinds amcheck cannot verify. Each line must say 0.
\pset tuples_only on
\pset format unaligned
CREATE OR REPLACE FUNCTION pg_temp.cmp(label text, q text) RETURNS text LANGUAGE plpgsql AS $$
DECLARE a text; b text;
BEGIN
  SET LOCAL enable_seqscan = off; SET LOCAL enable_indexscan = on; SET LOCAL enable_bitmapscan = on;
  EXECUTE 'SELECT md5(coalesce(string_agg(id::text, '','' ORDER BY id), '''')) FROM (' || q || ') s' INTO a;
  SET LOCAL enable_seqscan = on; SET LOCAL enable_indexscan = off; SET LOCAL enable_bitmapscan = off; SET LOCAL enable_indexonlyscan = off;
  EXECUTE 'SELECT md5(coalesce(string_agg(id::text, '','' ORDER BY id), '''')) FROM (' || q || ') s' INTO b;
  RETURN label || ' mismatch=' || (a <> b)::int;
END $$;
SELECT pg_temp.cmp('hash txt', $q$SELECT id FROM t WHERE txt IN (SELECT txt FROM t WHERE id % 997 = 0)$q$);
SELECT pg_temp.cmp('hash uuid', $q$SELECT id FROM t WHERE u IN (SELECT u FROM t WHERE id % 991 = 0)$q$);
SELECT pg_temp.cmp('btree txt range', $q$SELECT id FROM t WHERE txt BETWEEN 'a' AND 'n'$q$);
SELECT pg_temp.cmp('btree low', $q$SELECT id FROM t WHERE low >= 'ä' AND low < 'я'$q$);
SELECT pg_temp.cmp('btree upper expr', $q$SELECT id FROM t WHERE upper(txt) LIKE 'Z%'$q$);
SELECT pg_temp.cmp('btree f8', $q$SELECT id FROM t WHERE f8 > 1e299 OR f8 = 'NaN'$q$);
SELECT pg_temp.cmp('btree numeric', $q$SELECT id FROM t WHERE n BETWEEN -1000000 AND 1000000000$q$);
SELECT pg_temp.cmp('btree tstz', $q$SELECT id FROM t WHERE tstz >= '2005-03-27 00:00+00' AND tstz < '2005-11-01 00:00+00'$q$);
SELECT pg_temp.cmp('gin jsonb', $q$SELECT id FROM t WHERE j @> '{"a":[3,5]}'$q$);
SELECT pg_temp.cmp('gin jsonb word', $q$SELECT id FROM t WHERE j @> '{"w":"Москва"}'$q$);
SELECT pg_temp.cmp('gin arr', $q$SELECT id FROM t WHERE arr @> ARRAY[7,77]$q$);
SELECT pg_temp.cmp('gin fts', $q$SELECT id FROM t WHERE fts @@ to_tsquery('english','fox & jump')$q$);
SELECT pg_temp.cmp('gin fts simple', $q$SELECT id FROM t WHERE fts @@ to_tsquery('simple','北京 | straße | αθήνα')$q$);
SELECT pg_temp.cmp('gin trgm', $q$SELECT id FROM t WHERE txt LIKE '%stanb%' OR txt ILIKE '%ZÜR%'$q$);
SELECT pg_temp.cmp('gist trgm', $q$SELECT id FROM t WHERE vc LIKE '%abc%'$q$);
SELECT pg_temp.cmp('gist range', $q$SELECT id FROM t WHERE r && int4range(1000, 1200)$q$);
SELECT pg_temp.cmp('gist tstzrange', $q$SELECT id FROM t WHERE tsr @> timestamptz '2021-06-01 12:00+00'$q$);
SELECT pg_temp.cmp('spgist inet', $q$SELECT id FROM t WHERE ip << inet '10.7.0.0/16'$q$);
SELECT pg_temp.cmp('spgist text', $q$SELECT id FROM t WHERE vc LIKE 'ab%'$q$);
SELECT pg_temp.cmp('brin date', $q$SELECT id FROM t WHERE d BETWEEN '1990-01-01' AND '1991-01-01'$q$);
SELECT pg_temp.cmp('brin i8', $q$SELECT id FROM t WHERE i8 BETWEEN -1000000000000000 AND 1000000000000000$q$);
SELECT pg_temp.cmp('hash part', $q$SELECT id FROM hp WHERE k IN (SELECT k FROM hp WHERE id % 499 = 0)$q$);
SELECT 'hash part routing mismatch=' || count(*) FROM hp WHERE NOT satisfies_hash_partition('hp'::regclass, 4, (substring(tableoid::regclass::text from 3))::int, k);
