-- Step 0 portability workload: every common type and index kind, text in many scripts.
-- Deterministic (setseed) so every platform builds the same logical content.
\set ON_ERROR_STOP on
CREATE EXTENSION IF NOT EXISTS amcheck;
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE EXTENSION IF NOT EXISTS btree_gist;
SET timezone = 'UTC';  -- so timestamptz literals mean the same instant on every machine
SELECT setseed(0.42);

CREATE TABLE words(id int PRIMARY KEY, w text NOT NULL);
INSERT INTO words VALUES
 (1,'apple'),(2,'Äpfel'),(3,'éclair'),(4,'naïve'),(5,'Zürich'),(6,'İstanbul'),(7,'ıssız'),(8,'straße'),
 (9,'ΑΘΗΝΑ'),(10,'αθήνα'),(11,'Москва'),(12,'москва'),(13,'北京'),(14,'東京'),(15,'서울'),(16,'القاهرة'),
 (17,'ירושלים'),(18,'नमस्ते'),(19,'ไทย'),(20,'😀 emoji'),(21,'ǅ digraph'),(22,'ß'),(23,'ẞ'),(24,'ﬁ ligature'),
 (25,'e'||U&'\0301'),(26,'é'),(27,''),(28,' leading'),(29,'UPPER'),(30,'lower'),(31,'MiXeD'),(32,'z'),(33,'Z'),(34,'a'),(35,'A');

CREATE TABLE t(
  id bigserial PRIMARY KEY,
  txt text NOT NULL,
  vc varchar(64),
  ch "char",
  n numeric(30,10),
  i4 int, i8 bigint, f4 real, f8 double precision,
  b boolean, u uuid, ts timestamp, tstz timestamptz, d date, tm time, iv interval,
  j jsonb, arr int[], tarr text[], bt bytea, ip inet, r int4range, tsr tstzrange,
  fts tsvector,
  low text GENERATED ALWAYS AS (lower(txt)) STORED
);
INSERT INTO t(txt,vc,ch,n,i4,i8,f4,f8,b,u,ts,tstz,d,tm,iv,j,arr,tarr,bt,ip,r,tsr,fts)
SELECT
  (SELECT w FROM words WHERE id = 1 + (g % 35)) || ' ' || md5(g::text),
  left(md5((g*7)::text), 1 + g % 60),
  chr(32 + g % 90)::"char",
  (random()*1e12)::numeric(30,10) - 5e11,
  (random()*2e9)::int - 1000000000, (random()*9e18)::bigint - 4500000000000000000,
  (random()*1e6)::real, CASE g % 97 WHEN 0 THEN 'NaN' WHEN 1 THEN 'Infinity' WHEN 2 THEN '-0' ELSE (random()*1e300)::text END::float8,
  g % 3 = 0, md5(g::text)::uuid,
  timestamp '2000-01-01' + (g || ' minutes')::interval,
  timestamptz '1999-12-31 23:00:00+00' + (g * 3600 || ' seconds')::interval,
  date '1970-01-01' + g % 40000, time '00:00' + (g % 86400 || ' seconds')::interval,
  (g % 1000 || ' days ' || g % 24 || ' hours')::interval,
  jsonb_build_object('k', g, 's', md5(g::text), 'a', jsonb_build_array(g%7, g%11), 'n', null, 'w', (SELECT w FROM words WHERE id = 1 + (g % 35))),
  ARRAY[g%10, g%100, g%1000], ARRAY[md5(g::text), (SELECT w FROM words WHERE id = 1 + (g % 35))],
  decode(md5(g::text) || md5((g+1)::text), 'hex'),
  ('10.' || g%256 || '.' || (g/256)%256 || '.' || (g/65536)%256)::inet,
  int4range(g, g + 1 + g % 50), tstzrange(timestamptz '2020-01-01' + (g||' hours')::interval, timestamptz '2020-01-01' + (g+5||' hours')::interval),
  to_tsvector('english', 'The quick brown foxes were jumping over lazy dogs number ' || g || ' ' || md5(g::text))
       || to_tsvector('simple', (SELECT w FROM words WHERE id = 1 + (g % 35)))
FROM generate_series(1, 200000) g;

CREATE INDEX t_txt ON t(txt);
CREATE INDEX t_txt_desc_nulls ON t(vc DESC NULLS LAST, txt);
CREATE INDEX t_low ON t(low);
CREATE INDEX t_upper_expr ON t(upper(txt));
CREATE INDEX t_n ON t(n);
CREATE INDEX t_f8 ON t(f8);
CREATE INDEX t_f4 ON t(f4);
CREATE INDEX t_tstz ON t(tstz);
CREATE INDEX t_ts_d ON t(ts, d, tm);
CREATE INDEX t_iv ON t(iv);
CREATE INDEX t_u ON t(u);
CREATE INDEX t_ch ON t(ch);
CREATE INDEX t_bt ON t(bt);
CREATE INDEX t_ip ON t(ip);
CREATE INDEX t_b_partial ON t(i4) WHERE b;
CREATE INDEX t_i8_incl ON t(i8) INCLUDE (txt);
CREATE INDEX t_hash_txt ON t USING hash(txt);
CREATE INDEX t_hash_u ON t USING hash(u);
CREATE INDEX t_j_gin ON t USING gin(j);
CREATE INDEX t_j_path ON t USING gin(j jsonb_path_ops);
CREATE INDEX t_arr_gin ON t USING gin(arr);
CREATE INDEX t_fts_gin ON t USING gin(fts);
CREATE INDEX t_trgm_gin ON t USING gin(txt gin_trgm_ops);
CREATE INDEX t_trgm_gist ON t USING gist(vc gist_trgm_ops);
CREATE INDEX t_r_gist ON t USING gist(r);
CREATE INDEX t_tsr_gist ON t USING gist(tsr);
CREATE INDEX t_ip_spgist ON t USING spgist(ip);
CREATE INDEX t_txt_spgist ON t USING spgist(vc);
CREATE INDEX t_d_brin ON t USING brin(d);
CREATE INDEX t_i8_brin ON t USING brin(i8);
ALTER TABLE t ADD CONSTRAINT t_no_overlap EXCLUDE USING gist (i4 WITH =, r WITH &&) WHERE (i4 = 0);

CREATE TABLE hp(id bigint, k text, v int, PRIMARY KEY(id, k)) PARTITION BY HASH(k);
CREATE TABLE hp0 PARTITION OF hp FOR VALUES WITH (MODULUS 4, REMAINDER 0);
CREATE TABLE hp1 PARTITION OF hp FOR VALUES WITH (MODULUS 4, REMAINDER 1);
CREATE TABLE hp2 PARTITION OF hp FOR VALUES WITH (MODULUS 4, REMAINDER 2);
CREATE TABLE hp3 PARTITION OF hp FOR VALUES WITH (MODULUS 4, REMAINDER 3);
INSERT INTO hp SELECT g, (SELECT w FROM words WHERE id = 1 + (g % 35)) || g, g FROM generate_series(1, 60000) g;

CREATE TABLE rp(id bigint, at timestamptz NOT NULL, v text) PARTITION BY RANGE(at);
CREATE TABLE rp_a PARTITION OF rp FOR VALUES FROM ('2020-01-01') TO ('2021-01-01');
CREATE TABLE rp_b PARTITION OF rp FOR VALUES FROM ('2021-01-01') TO ('2023-01-01');
INSERT INTO rp SELECT g, timestamptz '2020-01-01' + (g || ' minutes')::interval, md5(g::text) FROM generate_series(1, 500000, 9) g;
CREATE INDEX rp_at ON rp(at);

CREATE TABLE big(id int PRIMARY KEY, doc text, comp_lz4 text COMPRESSION lz4, raw bytea);
INSERT INTO big SELECT g, repeat(md5(g::text), 400), repeat((SELECT w FROM words WHERE id = 1 + (g % 35)), 3000), decode(repeat(md5(g::text), 300), 'hex') FROM generate_series(1, 800) g;

SELECT lo_from_bytea(424242, decode(repeat('c0ffee', 20000), 'hex'));

CREATE SEQUENCE s1 START 1000 INCREMENT 7; SELECT nextval('s1') FROM generate_series(1,100);
CREATE MATERIALIZED VIEW mv AS SELECT i4 % 100 AS bucket, count(*) c, sum(n) sn FROM t GROUP BY 1;
CREATE UNIQUE INDEX mv_b ON mv(bucket);
UPDATE t SET txt = txt || '!' WHERE id % 13 = 0;
DELETE FROM t WHERE id % 17 = 0;
VACUUM (ANALYZE);
CHECKPOINT;
