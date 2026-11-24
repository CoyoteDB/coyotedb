//go:build integration

package guards

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"coyotedb.com/coyote/internal/pg"
)

// healthySchema has every index kind and both partition kinds, with text
// in many scripts (a small version of test/mixed/step0/workload.sql).
var healthySchema = []string{
	"CREATE EXTENSION IF NOT EXISTS amcheck",
	"CREATE EXTENSION IF NOT EXISTS pg_trgm",
	"CREATE EXTENSION IF NOT EXISTS btree_gist",
	`CREATE TABLE t (id bigint PRIMARY KEY, txt text NOT NULL, vc varchar(64), n numeric, f8 float8, u uuid UNIQUE, d date,
		j jsonb, arr int[], ip inet, r int4range, tsr tstzrange, fts tsvector, low text GENERATED ALWAYS AS (lower(txt)) STORED)`,
	`INSERT INTO t (id, txt, vc, n, f8, u, d, j, arr, ip, r, tsr, fts)
	 SELECT g, (ARRAY['apple','Äpfel','İstanbul','straße','ΑΘΗΝΑ','Москва','北京','서울','😀'])[1 + g % 9] || ' ' || md5(g::text),
	        left(md5((g * 7)::text), 1 + g % 30), g / 7.0, g::float8 / 3, md5(g::text)::uuid, date '1990-01-01' + g % 5000,
	        jsonb_build_object('k', g, 'a', jsonb_build_array(g % 7, g % 11)), ARRAY[g % 10, g % 100],
	        ('10.' || g % 256 || '.' || (g / 256) % 256 || '.1')::inet, int4range(g, g + 1 + g % 50),
	        tstzrange(timestamptz '2020-01-01 00:00+00' + g * interval '1 hour', timestamptz '2020-01-01 00:00+00' + (g + 5) * interval '1 hour'),
	        to_tsvector('english', 'quick brown foxes number ' || g)
	 FROM generate_series(1, 20000) g`,
	"CREATE INDEX t_txt ON t (txt)",
	"CREATE INDEX t_low ON t (low)",
	"CREATE INDEX t_upper ON t (upper(txt))",
	"CREATE INDEX t_n ON t (n)",
	"CREATE INDEX t_partial ON t (f8) WHERE id % 2 = 0",
	"CREATE INDEX t_hash_txt ON t USING hash (txt)",
	"CREATE INDEX t_hash_u ON t USING hash (u)",
	"CREATE INDEX t_j ON t USING gin (j)",
	"CREATE INDEX t_arr ON t USING gin (arr)",
	"CREATE INDEX t_fts ON t USING gin (fts)",
	"CREATE INDEX t_trgm_gin ON t USING gin (txt gin_trgm_ops)",
	"CREATE INDEX t_trgm_gist ON t USING gist (vc gist_trgm_ops)",
	"CREATE INDEX t_r ON t USING gist (r)",
	"CREATE INDEX t_tsr ON t USING gist (tsr)",
	"CREATE INDEX t_ip ON t USING spgist (ip)",
	"CREATE INDEX t_vc_sp ON t USING spgist (vc)",
	"CREATE INDEX t_d_brin ON t USING brin (d)",
	"CREATE INDEX t_id_brin ON t USING brin (id)",
	"ALTER TABLE t ADD CONSTRAINT t_excl EXCLUDE USING gist (id WITH =, r WITH &&)",
	"CREATE TABLE hp (id bigint, k text, PRIMARY KEY (id, k)) PARTITION BY HASH (k)",
	"CREATE TABLE hp0 PARTITION OF hp FOR VALUES WITH (MODULUS 3, REMAINDER 0)",
	"CREATE TABLE hp1 PARTITION OF hp FOR VALUES WITH (MODULUS 3, REMAINDER 1)",
	"CREATE TABLE hp2 PARTITION OF hp FOR VALUES WITH (MODULUS 3, REMAINDER 2)",
	"INSERT INTO hp SELECT g, md5(g::text) || 'ü' FROM generate_series(1, 6000) g",
	"CREATE TABLE rp (id bigint, at timestamptz NOT NULL) PARTITION BY RANGE (at)",
	"CREATE TABLE rp_a PARTITION OF rp FOR VALUES FROM ('2020-01-01 00:00+00') TO ('2021-01-01 00:00+00')",
	"CREATE TABLE rp_def PARTITION OF rp DEFAULT",
	"INSERT INTO rp SELECT g, timestamptz '2020-06-01 00:00+00' + g * interval '1 hour' FROM generate_series(1, 20000) g",
	"CREATE TABLE big (id int PRIMARY KEY, doc text)",
	"INSERT INTO big SELECT g, repeat(md5(g::text), 500) FROM generate_series(1, 200) g",
	"UPDATE t SET txt = txt || '!' WHERE id % 13 = 0",
	"DELETE FROM t WHERE id % 17 = 0",
	"VACUUM ANALYZE",
}

// A healthy database passes every level, and every index kind amcheck
// cannot verify is really compared with its table.
func TestVerifyHealthy(t *testing.T) {
	s := newServer(t)
	ctx := ctxT(t, 10*time.Minute)
	s.exec("postgres", healthySchema...)
	for _, o := range []pg.VerifyOptions{
		{Level: pg.VerifyQuick, InstallAmcheck: true, Seed: 7},
		{Level: pg.VerifyStandard, Pause: time.Millisecond, Seed: 7},
		{Level: pg.VerifyDeep, AllowBlocking: true, Seed: 7},
		{Level: pg.VerifyQuick}, // a new seed
	} {
		r, err := pg.VerifyCluster(ctx, s.connectFunc(), o)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: %v in %s", r.Level, r.Checked, r.Duration.Round(time.Millisecond))
		for _, f := range r.Findings {
			if f.Severity != pg.SeverityInfo {
				t.Errorf("%s: healthy database has finding: %s (%s)", r.Level, f, f.Detail)
			} else {
				t.Logf("%s: %s", r.Level, f)
			}
		}
		if r.Checked["index_scan"] < 9 {
			t.Errorf("%s: only %d indexes compared with their table, want all 9 gist/spgist/brin/hash ones", r.Level, r.Checked["index_scan"])
		}
		if r.Checked["gin"] < 4 || r.Checked["btree"] < 8 || r.Checked["table"] < 5 {
			t.Errorf("%s: too little checked: %v", r.Level, r.Checked)
		}
		if o.Level >= pg.VerifyStandard && r.Checked["partition"] != 5 {
			t.Errorf("%s: partitions checked %d, want 5", r.Level, r.Checked["partition"])
		}
	}
	// pg_amcheck agrees.
	fs, err := pg.RunPgAmcheck(ctx, pg.PgAmcheckOptions{Install: inst, Port: s.port, Passfile: s.passfile(), Deep: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) > 0 {
		t.Errorf("pg_amcheck on a healthy database: %v", fs)
	}
}

// The integrity check finds a B-tree whose sort order changed under it
// (exactly what different text-sorting rules on two machines would do),
// and indexes that miss rows the table has.
func TestVerifyFindsDamage(t *testing.T) {
	s := newServer(t)
	ctx := ctxT(t, 10*time.Minute)
	s.exec("postgres",
		"CREATE EXTENSION amcheck",
		`CREATE TABLE w (id int PRIMARY KEY, s text, r int4range, h int, b int)`,
		`INSERT INTO w SELECT g, (ARRAY['apple','Banana','cherry','Date','éclair','Zebra','zoo','Äpfel'])[1 + g % 8] || g, int4range(g, g + 10), g, g FROM generate_series(1, 3000) g`,
		`CREATE INDEX w_s_c ON w (s COLLATE "C")`,
		`CREATE INDEX w_r ON w USING gist (r)`,
		`CREATE INDEX w_h ON w USING hash (h)`,
		`CREATE INDEX w_h_bt ON w (b)`,
	)
	var icu string
	c := s.connect("postgres")
	if err := c.QueryRow(ctx, `SELECT oid::text FROM pg_collation WHERE collname = 'en-x-icu'`).Scan(&icu); err != nil {
		t.Fatal(err)
	}
	s.exec("postgres",
		// 1. The index was built with C (byte) order; tell Postgres it is
		//    in ICU English order instead. Its pages are unchanged, but
		//    every check now sees keys out of order: 'Banana' sorts before
		//    'apple' in C and after it in English.
		`UPDATE pg_index SET indcollation = '`+icu+`'::oidvector WHERE indexrelid = 'w_s_c'::regclass`,
		// 2. Make three indexes miss rows: while indisready is false,
		//    Postgres does not add new rows to them.
		`UPDATE pg_index SET indisready = false WHERE indexrelid IN ('w_r'::regclass, 'w_h'::regclass, 'w_h_bt'::regclass)`,
	)
	s.exec("postgres", `INSERT INTO w SELECT g, 'new' || g, int4range(g, g + 10), g, g FROM generate_series(3001, 9000) g`)
	s.exec("postgres", `UPDATE pg_index SET indisready = true WHERE indexrelid IN ('w_r'::regclass, 'w_h'::regclass, 'w_h_bt'::regclass)`)

	quick, err := pg.VerifyCluster(ctx, s.connectFunc(), pg.VerifyOptions{Level: pg.VerifyQuick, InstallAmcheck: true, Seed: 7})
	if err != nil {
		t.Fatal(err)
	}
	std, err := pg.VerifyCluster(ctx, s.connectFunc(), pg.VerifyOptions{Level: pg.VerifyStandard, Seed: 7})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("quick checked %v, standard checked %v", quick.Checked, std.Checked)
	for _, f := range std.Findings {
		t.Logf("%s | %s", f, f.Detail)
	}
	has := func(r pg.VerifyReport, check, object string) bool {
		for _, f := range r.Findings {
			if f.Severity == pg.SeverityBlock && f.Check == check && f.Object == object {
				return true
			}
		}
		return false
	}
	for _, r := range []pg.VerifyReport{quick, std} {
		if !has(r, "btree", "index public.w_s_c") {
			t.Errorf("%s: re-ordered B-tree not found", r.Level)
		}
		if !has(r, "index_scan", "index public.w_r") || !has(r, "index_scan", "index public.w_h") {
			t.Errorf("%s: GiST/hash index missing rows not found", r.Level)
		}
	}
	if !has(std, "btree", "index public.w_h_bt") {
		t.Error("standard: B-tree missing rows not found (heapallindexed)")
	}
	if has(quick, "btree", "index public.w_h_bt") {
		t.Error("quick: a B-tree missing rows is structurally fine; quick must not claim it")
	}
	for _, f := range std.Findings {
		if f.Severity == pg.SeverityBlock && f.Object == "index public.w_pkey" {
			t.Errorf("healthy index reported: %s", f)
		}
	}
	// pg_amcheck agrees about the B-trees.
	fs, err := pg.RunPgAmcheck(ctx, pg.PgAmcheckOptions{Install: inst, Port: s.port, Passfile: s.passfile()})
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, f := range fs {
		found[f.Object] = true
	}
	if !found["index public.w_s_c"] || !found["index public.w_h_bt"] {
		t.Errorf("pg_amcheck findings %v", fs)
	}
}

// A damaged page is found by pg_checksums with the server stopped, and by
// the online check after a start.
func TestVerifyChecksumDamage(t *testing.T) {
	s := newServer(t)
	ctx := ctxT(t, 10*time.Minute)
	s.exec("postgres", "CREATE EXTENSION amcheck", "CREATE TABLE c (id int, v text)", "INSERT INTO c SELECT g, md5(g::text) FROM generate_series(1, 2000) g", "CHECKPOINT")
	var rel string
	if err := s.connect("postgres").QueryRow(ctx, "SELECT pg_relation_filepath('c')").Scan(&rel); err != nil {
		t.Fatal(err)
	}
	s.stop()
	res, err := pg.CheckChecksumsOffline(ctx, inst, s.node.PGData())
	if err != nil || res.BadChecksums != 0 || res.BlocksScanned == 0 {
		t.Fatalf("healthy: %+v %v", res, err)
	}
	// Flip one byte in the middle of the first page's row data.
	p := filepath.Join(s.node.PGData(), filepath.FromSlash(rel))
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	b[6000] ^= 0x55
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	res, err = pg.CheckChecksumsOffline(ctx, inst, s.node.PGData())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("pg_checksums: %+v", res)
	if res.BadChecksums != 1 || len(res.Problems) != 1 || !strings.Contains(res.Problems[0], "block 0") {
		t.Errorf("damaged page not reported: %+v", res)
	}
	s.start()
	r, err := pg.VerifyCluster(ctx, s.connectFunc(), pg.VerifyOptions{Level: pg.VerifyQuick, InstallAmcheck: true})
	if err != nil {
		t.Fatal(err)
	}
	var heap bool
	for _, f := range r.Findings {
		t.Logf("%s | %s", f, f.Detail)
		if f.Severity == pg.SeverityBlock && f.Check == "heap" && f.Object == "table public.c" {
			heap = true
		}
	}
	if !heap {
		t.Error("online check did not report the damaged table")
	}
	// The damage was counted; a second run reports the counter too.
	r, err = pg.VerifyCluster(ctx, s.connectFunc(), pg.VerifyOptions{Level: pg.VerifyQuick, Databases: nil})
	if err != nil {
		t.Fatal(err)
	}
	var counter bool
	for _, f := range r.Findings {
		if f.Check == "checksum" && f.Severity == pg.SeverityBlock {
			counter = true
		}
	}
	if !counter {
		t.Error("checksum failure counter not reported")
	}
}

// passfile writes a libpq password file for the superuser.
func (s *server) passfile() string {
	p := filepath.Join(s.node.StateDir, "amcheck.pgpass")
	body := pg.RenderPassfile([]pg.PassEntry{{Host: "127.0.0.1", Port: strconv.Itoa(s.port), DB: "*", User: pg.Superuser, Password: s.pw}})
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		s.t.Fatal(err)
	}
	return p
}
