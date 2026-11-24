-- Logical content + behaviour digests. Must print identical lines on every platform.
\pset tuples_only on
\pset format unaligned
SET bytea_output = hex; SET extra_float_digits = 3; SET timezone = 'UTC'; SET enable_indexscan = off; SET enable_bitmapscan = off; SET enable_indexonlyscan = off;
SELECT 't rows', count(*), md5(string_agg(md5(t::text), '' ORDER BY id)) FROM t;
SELECT 'hp', tableoid::regclass, count(*), md5(string_agg(md5(hp::text), '' ORDER BY id, k)) FROM hp GROUP BY 2 ORDER BY 2;
SELECT 'rp', count(*), md5(string_agg(md5(rp::text), '' ORDER BY id)) FROM rp;
SELECT 'big', count(*), md5(string_agg(md5(doc)||md5(comp_lz4)||md5(raw), '' ORDER BY id)) FROM big;
SELECT 'lo', md5(lo_get(424242));
SELECT 'seq', last_value FROM s1;
SELECT 'mv', md5(string_agg(mv::text, '' ORDER BY bucket)) FROM mv;
SELECT 'sort txt', md5(string_agg(txt, '|' ORDER BY txt, id)) FROM t;
SELECT 'sort words', string_agg(id::text, ',' ORDER BY w, id) FROM words;
SELECT 'sort lower', string_agg(id::text, ',' ORDER BY lower(w), id) FROM words;
SELECT 'case', md5(string_agg(upper(w)||'/'||lower(w)||'/'||initcap(w), '|' ORDER BY id)) FROM words;
SELECT 'regex', md5(string_agg((w ~* '^[[:alpha:]]+$')::text || (w ~ '\w+')::text || coalesce(substring(w from '[[:upper:]]+'),'-'), '|' ORDER BY id)) FROM words;
SELECT 'hash', md5(string_agg(hashtext(w)::text || ':' || hashtextextended(w, 7)::text, '|' ORDER BY id)) FROM words;
SELECT 'hash misc', md5(string_agg(hashfloat8(f8)::text||hash_numeric(n)::text||uuid_hash(u)::text||hashint8(i8)::text, '|' ORDER BY id)) FROM t WHERE id < 5000;
SELECT 'fts', md5(string_agg(to_tsvector('english', txt)::text || to_tsvector('simple', txt)::text, '|' ORDER BY id)) FROM t WHERE id < 5000;
SELECT 'trgm', md5(string_agg(show_trgm(w)::text, '|' ORDER BY id)) FROM words;
SELECT 'tz', md5(string_agg((tstz AT TIME ZONE 'America/New_York')::text || (tstz AT TIME ZONE 'Europe/Berlin')::text || (tstz AT TIME ZONE 'Asia/Kolkata')::text || (tstz AT TIME ZONE 'Australia/Lord_Howe')::text, '|' ORDER BY id)) FROM t WHERE id < 20000;
SELECT 'float text', md5(string_agg(f8::text || '/' || f4::text, '|' ORDER BY id)) FROM t WHERE id < 20000;
SELECT 'libm', md5(string_agg(exp(i4::float8/1e9)::text || ln(abs(i4)::float8+1)::text || power(1.0000001::float8, i4%1000)::text || sin(i4::float8)::text || sqrt(abs(f4)::float8)::text || cbrt(i4::float8)::text, '|' ORDER BY id)) FROM t WHERE id < 20000;
SELECT 'numeric', md5(string_agg((n*n)::text || (n/3)::text || sqrt(abs(n))::text || ln(abs(n)+1)::text, '|' ORDER BY id)) FROM t WHERE id < 20000;
