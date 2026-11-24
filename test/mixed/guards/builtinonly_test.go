//go:build integration

package guards

import (
	"strings"
	"testing"
	"time"

	"coyotedb.com/coyote/internal/pg"
)

// D48: PrepareBuiltinOnly makes every database (template0 included)
// builtin-only; the event triggers then refuse every way of bringing in
// ICU or operating-system sort rules that an event trigger can see, for
// a normal user too; the quick check sees what they cannot (CREATE
// DATABASE with ICU) and a moved DDL log; the guard comes off cleanly.
func TestBuiltinOnlyGuard(t *testing.T) {
	s := newServer(t)
	ctx := ctxT(t, 5*time.Minute)
	s.exec("postgres", "CREATE DATABASE app")

	// Before: not prepared, the quick check says so for every database.
	q, err := pg.CheckBuiltinOnly(ctx, s.connectFunc())
	if err != nil {
		t.Fatal(err)
	}
	if !q.Blocking() {
		t.Fatalf("an unprepared cluster must not pass: %+v", q)
	}

	res, err := pg.PrepareBuiltinOnly(ctx, s.connectFunc(), true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Dropped < 100 || len(res.Kept) != 0 {
		t.Fatalf("prepare: dropped %d, kept %v", res.Dropped, res.Kept)
	}
	if _, err := pg.PrepareBuiltinOnly(ctx, s.connectFunc(), true); err != nil {
		t.Fatalf("prepare is not idempotent: %v", err)
	}
	q, err = pg.CheckBuiltinOnly(ctx, s.connectFunc())
	if err != nil {
		t.Fatal(err)
	}
	if q.Blocking() {
		t.Fatalf("prepared cluster: %v", q.Findings)
	}
	var allow bool
	if err := s.connect("postgres").QueryRow(ctx, "SELECT datallowconn FROM pg_database WHERE datname = 'template0'").Scan(&allow); err != nil || allow {
		t.Fatalf("template0 must be closed again (allow=%v, err=%v)", allow, err)
	}

	// A normal user (the app's role) can do normal DDL with built-in
	// collations; every way in for ICU / OS sort rules is refused.
	s.exec("postgres", "CREATE ROLE appuser LOGIN PASSWORD 'x'", "GRANT ALL ON DATABASE app TO appuser")
	s.exec("app", "GRANT ALL ON SCHEMA public TO appuser")
	c := s.connect("app")
	mustOK := []string{
		"SET ROLE appuser",
		"CREATE TABLE ok (id int PRIMARY KEY, a text, b text COLLATE pg_c_utf8, c text COLLATE \"C\", d text COLLATE pg_unicode_fast)",
		"CREATE INDEX ON ok (lower(a))",
		"CREATE VIEW okv AS SELECT a COLLATE \"C\" AS a FROM ok",
		"CREATE TEMP TABLE tmp (s text)",
		"RESET ROLE",
	}
	for _, q := range mustOK {
		if _, err := c.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	refused := []string{
		"CREATE COLLATION de (provider = icu, locale = 'de')",
		"CREATE TABLE bad1 (s text COLLATE \"unicode\")",
		"CREATE INDEX bad2 ON ok ((a COLLATE \"unicode\"))",
		"ALTER TABLE ok ALTER COLUMN d TYPE text COLLATE \"unicode\"",
		"ALTER TABLE ok ADD CONSTRAINT bad3 CHECK ((a COLLATE \"unicode\") > 'a')",
		"CREATE VIEW bad4 AS SELECT a COLLATE \"unicode\" AS a FROM ok",
		"CREATE DOMAIN bad5 AS text COLLATE \"unicode\"",
		"CREATE TYPE bad6 AS (a text COLLATE \"unicode\")",
		"CREATE TYPE bad7 AS RANGE (subtype = text, collation = \"unicode\")",
		"CREATE TABLE bad8 (s text) PARTITION BY RANGE (s COLLATE \"unicode\")",
		"CREATE MATERIALIZED VIEW bad9 AS SELECT a FROM ok ORDER BY a COLLATE \"unicode\"",
		"CREATE FUNCTION bad10(t text) RETURNS bool LANGUAGE sql BEGIN ATOMIC SELECT t COLLATE \"unicode\" > 'a'; END",
		"ALTER TABLE ok ADD COLUMN g text GENERATED ALWAYS AS ((a COLLATE \"unicode\")) STORED",
		"CREATE STATISTICS bad11 ON (a COLLATE \"unicode\") FROM ok",
	}
	for _, as := range []string{"appuser", "postgres"} {
		if _, err := c.Exec(ctx, "SET ROLE "+as); err != nil {
			t.Fatal(err)
		}
		for _, q := range refused {
			_, err := c.Exec(ctx, q)
			if err == nil || !strings.Contains(err.Error(), "mixes kinds of machines") {
				if as == "appuser" && err != nil && strings.Contains(err.Error(), "permission denied") {
					continue // not allowed anyway
				}
				t.Errorf("as %s: %s: want a refusal, got %v", as, q, err)
			}
		}
		if _, err := c.Exec(ctx, "RESET ROLE"); err != nil {
			t.Fatal(err)
		}
	}
	// session_replication_role = replica does not turn it off (ENABLE ALWAYS).
	if _, err := c.Exec(ctx, "SET session_replication_role = replica"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, refused[1]); err == nil {
		t.Error("the guard must also fire with session_replication_role = replica")
	}
	if _, err := c.Exec(ctx, "RESET session_replication_role"); err != nil {
		t.Fatal(err)
	}

	// The DDL log moved for the app database (not for temp tables).
	q2, err := pg.CheckBuiltinOnly(ctx, s.connectFunc())
	if err != nil {
		t.Fatal(err)
	}
	if q2.SameDDL(q) || q2.DDL["app"] <= q.DDL["app"] || q2.DDL["postgres"] != q.DDL["postgres"] {
		t.Fatalf("DDL log: before %v, after %v", q.DDL, q2.DDL)
	}

	// A new database made from template0 inherits the guard.
	s.exec("postgres", "CREATE DATABASE fromt0 TEMPLATE template0")
	cc := s.connect("fromt0")
	if _, err := cc.Exec(ctx, "CREATE TABLE x (s text COLLATE \"unicode\")"); err == nil {
		t.Error("a database made from template0 must have the guard")
	}
	var leftover int
	if err := cc.QueryRow(ctx, "SELECT count(*) FROM pg_collation WHERE oid >= 12000 AND collprovider IN ('i', 'c') AND collencoding IN (-1, pg_char_to_encoding('UTF8'))").Scan(&leftover); err != nil || leftover != 0 {
		t.Errorf("template0 still has imported collations: %d (%v)", leftover, err)
	}

	// CREATE DATABASE with ICU cannot be seen by event triggers: the
	// quick check blocks on it.
	s.exec("postgres", "CREATE DATABASE icudb LOCALE_PROVIDER icu ICU_LOCALE 'de' TEMPLATE template0")
	q3, err := pg.CheckBuiltinOnly(ctx, s.connectFunc())
	if err != nil {
		t.Fatal(err)
	}
	if !q3.Blocking() || !strings.Contains(findings(q3.Findings), "database icudb") {
		t.Fatalf("an ICU database must block: %v", q3.Findings)
	}
	s.exec("postgres", "DROP DATABASE icudb")

	// Turning the guard off for a missing trigger is seen too.
	s.exec("app", "ALTER EVENT TRIGGER "+pg.GuardSchema+"_check_ddl DISABLE")
	q4, err := pg.CheckBuiltinOnly(ctx, s.connectFunc())
	if err != nil {
		t.Fatal(err)
	}
	if !q4.Blocking() {
		t.Fatal("a disabled trigger must block")
	}
	if _, err := pg.PrepareBuiltinOnly(ctx, s.connectFunc(), false); err != nil {
		t.Fatal(err)
	}
	if q5, err := pg.CheckBuiltinOnly(ctx, s.connectFunc()); err != nil || q5.Blocking() {
		t.Fatalf("prepare must repair it: %v %v", err, q5.Findings)
	}

	// Writes can be refused and allowed again (a drifted main node).
	adm := s.connect("app")
	if changed, err := pg.SetWritesRefused(ctx, adm, true); err != nil || !changed {
		t.Fatalf("refuse writes: %v %v", changed, err)
	}
	w := s.connect("app")
	if _, err := w.Exec(ctx, "INSERT INTO ok (id) VALUES (1)"); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("a write must be refused, got %v", err)
	}
	if err := pg.Housekeeping(ctx, adm); err != nil {
		t.Fatal(err)
	}
	if _, err := adm.Exec(ctx, "CREATE TABLE housekeeping_ok (id int)"); err != nil {
		t.Fatalf("housekeeping must still work: %v", err)
	}
	if changed, err := pg.SetWritesRefused(ctx, adm, false); err != nil || !changed {
		t.Fatalf("allow writes: %v %v", changed, err)
	}
	w2 := s.connect("app")
	if _, err := w2.Exec(ctx, "INSERT INTO ok (id) VALUES (2)"); err != nil {
		t.Fatalf("writes must work again: %v", err)
	}

	// Removing the guard gives back the freedom of a cluster that does
	// not mix machines.
	if err := pg.UnprepareBuiltinOnly(ctx, s.connectFunc()); err != nil {
		t.Fatal(err)
	}
	s.exec("app", "CREATE TABLE free (s text COLLATE \"unicode\")")
	for _, db := range []string{"app", "postgres", "template1", "fromt0"} {
		on, err := pg.BuiltinOnlyInstalled(ctx, s.connect(db))
		if err != nil || on {
			t.Errorf("%s: guard still installed (%v)", db, err)
		}
	}
}

// The quick check works on a standby (read-only) and sees the same as
// the primary, including the DDL log position after replay.
func TestBuiltinOnlyOnStandby(t *testing.T) {
	p := newServer(t)
	ctx := ctxT(t, 5*time.Minute)
	if _, err := pg.PrepareBuiltinOnly(ctx, p.connectFunc(), true); err != nil {
		t.Fatal(err)
	}
	sb := newStandby(t, p)
	p.exec("postgres", "CREATE TABLE after_clone (id int)")
	var lsn string
	if err := p.connect("postgres").QueryRow(ctx, "SELECT pg_current_wal_lsn()::text").Scan(&lsn); err != nil {
		t.Fatal(err)
	}
	l, err := pg.ParseLSN(lsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := sb.node.WaitReplayed(ctx, l, time.Minute); err != nil {
		t.Fatal(err)
	}
	qp, err := pg.CheckBuiltinOnly(ctx, p.connectFunc())
	if err != nil {
		t.Fatal(err)
	}
	qs, err := pg.CheckBuiltinOnly(ctx, sb.connectFunc())
	if err != nil {
		t.Fatal(err)
	}
	if qp.Blocking() || qs.Blocking() || !qp.SameDDL(qs) {
		t.Fatalf("primary %v %v, standby %v %v", qp.DDL, qp.Findings, qs.DDL, qs.Findings)
	}
	// The probe includes the built-in Unicode version, and primary and
	// standby give the same answers.
	pr, err := pg.RunProbe(ctx, p.connect("postgres"), pg.ProbeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sr, err := pg.RunProbe(ctx, sb.connect("postgres"), pg.ProbeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pr.Digest() != sr.Digest() {
		t.Fatal("probe differs between primary and standby")
	}
}

func findings(fs []pg.Finding) string {
	var b strings.Builder
	for _, f := range fs {
		b.WriteString(f.String() + "\n")
	}
	return b.String()
}

// D49: stored structures that recompute float or geometric maths are
// refused at DDL time; plain indexes and comparisons on floats are not.
// Objects that existed before the guard are found by the quick check.
func TestBuiltinOnlyFloatMath(t *testing.T) {
	s := newServer(t)
	ctx := ctxT(t, 5*time.Minute)
	s.exec("postgres", "CREATE DATABASE app")
	s.exec("app", "CREATE TABLE before_guard (x float8)", "CREATE INDEX before_guard_exp ON before_guard (exp(x))")
	if _, err := pg.PrepareBuiltinOnly(ctx, s.connectFunc(), true); err != nil {
		t.Fatal(err)
	}
	q, err := pg.CheckBuiltinOnly(ctx, s.connectFunc())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(findings(q.Findings), "index public.before_guard_exp") {
		t.Fatalf("an existing float-maths index must be found: %v", q.Findings)
	}
	s.exec("app", "DROP INDEX before_guard_exp")
	if q, err = pg.CheckBuiltinOnly(ctx, s.connectFunc()); err != nil || q.Blocking() {
		t.Fatalf("after the drop: %v %v", err, q.Findings)
	}

	c := s.connect("app")
	allowed := []string{
		"CREATE TABLE f (id int PRIMARY KEY, x float8, y float4, n numeric, p point, c circle, ts timestamptz)",
		"CREATE INDEX f_x ON f (x)",
		"CREATE INDEX f_xy ON f (x, y)",
		"CREATE INDEX f_x_hash ON f USING hash (x)",
		"CREATE INDEX f_x_brin ON f USING brin (x)",
		"CREATE INDEX f_x_part ON f (x) WHERE x > float8 '0.5'",
		"CREATE INDEX f_y_part ON f (y) WHERE y >= x",
		"CREATE INDEX f_n2 ON f ((n * 2))",
		"ALTER TABLE f ADD CONSTRAINT f_pos CHECK (x > float8 '0' AND y <> 0)",
		"ALTER TABLE f ADD COLUMN n2 numeric GENERATED ALWAYS AS (n * 2) STORED",
		"ALTER TABLE f ADD COLUMN xv float8 GENERATED ALWAYS AS (x * 2) VIRTUAL",
		"ALTER TABLE f ADD CONSTRAINT f_ex EXCLUDE USING btree (x WITH =)",
		"CREATE TABLE fp (x float8) PARTITION BY RANGE (x)",
		"CREATE TABLE fp1 PARTITION OF fp FOR VALUES FROM (0) TO (1.5)",
		"CREATE VIEW fv AS SELECT corr(x, y), exp(x) FROM f GROUP BY x",
		"INSERT INTO f (id, x, y, n, p, c) VALUES (1, 1.5, 2.5, 3, point(1, 2), circle '((0,0),1)')",
	}
	for _, q := range allowed {
		if _, err := c.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	refused := []string{
		"CREATE INDEX bad ON f ((x * 2))",
		"CREATE INDEX bad ON f (exp(x))",
		"CREATE INDEX bad ON f ((n::float8))",
		"CREATE INDEX bad ON f (x) WHERE x * 2 > 1",
		"CREATE INDEX bad ON f (x) WHERE x > 0.5", // a numeric literal: converted to float8 at every insert
		"CREATE INDEX bad ON f USING gist (p)",
		"CREATE INDEX bad ON f USING spgist (p)",
		"CREATE INDEX bad ON f USING gist (c)",
		"CREATE INDEX bad ON f ((p <-> point(0, 0)))",
		"ALTER TABLE f ADD CONSTRAINT bad CHECK (x * 2 > 1)",
		"ALTER TABLE f ADD CONSTRAINT bad CHECK (p <-> point(0, 0) < 10)",
		"ALTER TABLE f ADD COLUMN bad float8 GENERATED ALWAYS AS (x * 2) STORED",
		"ALTER TABLE f ADD CONSTRAINT bad EXCLUDE USING gist (c WITH &&)",
		"CREATE TABLE bad (x float8) PARTITION BY RANGE ((x * 2))",
		"CREATE DOMAIN bad AS float8 CHECK (VALUE * 2 > 1)",
		"CREATE TABLE bad (b box)", // allowed itself; the index on it is not:
	}
	for i, q := range refused {
		_, err := c.Exec(ctx, q)
		if i == len(refused)-1 {
			if err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			_, err = c.Exec(ctx, "CREATE INDEX bad_b ON bad USING brin (b)")
			q = "CREATE INDEX bad_b ON bad USING brin (b)"
		}
		if err == nil || !strings.Contains(err.Error(), "decimal (floating-point) or geometric maths") {
			t.Errorf("%s: want a refusal, got %v", q, err)
		}
	}
	// A session's temporary table is never replicated: allowed, and not
	// reported while the session lives.
	if _, err := c.Exec(ctx, "CREATE TEMP TABLE tmp (x float8)"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, "CREATE INDEX ON tmp (exp(x))"); err != nil {
		t.Fatalf("temporary float-maths index: %v", err)
	}
	if q, err := pg.CheckBuiltinOnly(ctx, s.connectFunc()); err != nil || q.Blocking() {
		t.Fatalf("the allowed objects must pass the quick check: %v %v", err, q.Findings)
	}
}
