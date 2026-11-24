//go:build integration

package guards

import (
	"context"
	"strings"
	"testing"
	"time"

	"coyotedb.com/coyote/internal/pg"
)

// A database made the way InitDB makes it passes the audit: nothing
// blocks and nothing warns.
func TestAuditCleanDatabase(t *testing.T) {
	s := newServer(t)
	ctx := ctxT(t, 5*time.Minute)
	s.exec("postgres", "CREATE DATABASE app", "CREATE EXTENSION amcheck", "CREATE EXTENSION pg_trgm",
		"CREATE TABLE t (id int PRIMARY KEY, s text, f float8, n numeric)",
		"CREATE INDEX ON t (lower(s))", "CREATE INDEX ON t (sqrt(f))", "CREATE INDEX ON t ((f * 2 + 1))",
		"CREATE INDEX ON t USING gin (s gin_trgm_ops)", "CREATE INDEX ON t ((s COLLATE pg_c_utf8))", `CREATE INDEX ON t ((s COLLATE "C"))`,
		"CREATE INDEX ON t (exp(n))", "CREATE TABLE g (a float8, b float8 GENERATED ALWAYS AS (a * a) STORED)")
	r, err := pg.AuditCluster(ctx, s.connectFunc(), pg.AuditOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Databases) != 3 {
		t.Errorf("databases %v, want app, postgres, template1", r.Databases)
	}
	for _, f := range r.Findings {
		t.Logf("%s", f)
		if f.Severity != pg.SeverityInfo {
			t.Errorf("clean database has a finding: %s", f)
		}
	}
	again, err := pg.AuditCluster(ctx, s.connectFunc(), pg.AuditOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if again.Digest() != r.Digest() {
		t.Error("audit digest is not stable across runs")
	}
}

// Dropping the imported collations leaves no operating-system or ICU
// collation except the one Postgres pins, in every database including a
// new one, and the database still works.
func TestDropImportedCollations(t *testing.T) {
	s := newServer(t)
	ctx := ctxT(t, 5*time.Minute)
	s.exec("postgres", "CREATE DATABASE app")
	// One imported collation is in use: it must be kept and reported.
	c := s.connect("app")
	var inUse string
	if err := c.QueryRow(ctx, `SELECT collname FROM pg_collation WHERE collprovider = 'i' AND oid >= 12000 ORDER BY collname LIMIT 1`).Scan(&inUse); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, `CREATE TABLE keep (s text COLLATE "`+inUse+`")`); err != nil {
		t.Fatal(err)
	}
	n, kept, err := pg.DropImportedCollationsEverywhere(ctx, s.connectFunc())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("dropped %d collations, kept %d", n, len(kept))
	if n < 100 || len(kept) != 1 || !strings.Contains(kept[0].Object, inUse) {
		t.Fatalf("dropped %d, kept %+v", n, kept)
	}
	if _, err := c.Exec(ctx, "DROP TABLE keep"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := pg.DropImportedCollationsEverywhere(ctx, s.connectFunc()); err != nil {
		t.Fatal(err)
	}
	s.exec("postgres", "CREATE DATABASE later")
	for _, db := range []string{"postgres", "template1", "app", "later"} {
		c := s.connect(db)
		var left []string
		// Collations for another encoding stay (no database here can use
		// them; Postgres hides them from UTF-8 databases).
		rows, err := c.Query(ctx, `SELECT collname || ':' || collprovider::text FROM pg_collation
			WHERE (collprovider = 'i' OR (collprovider = 'c' AND collname NOT IN ('C', 'POSIX')))
			  AND collencoding IN (-1, pg_char_to_encoding('UTF8')) ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var x string
			if err := rows.Scan(&x); err != nil {
				t.Fatal(err)
			}
			left = append(left, x)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if len(left) != 1 || left[0] != "unicode:i" {
			t.Errorf("%s: collations left %v, want only the pinned unicode:i", db, left)
		}
		// Still works: text, sorting, an index, case mapping, full-text.
		var got string
		err = c.QueryRow(ctx, `WITH x(s) AS (VALUES ('b'), ('Ä'), ('a')) SELECT string_agg(upper(s), ',' ORDER BY s) || to_tsvector('english', 'running dogs')::text FROM x`).Scan(&got)
		if err != nil || got != "A,B,Ä'dog':2 'run':1" {
			t.Errorf("%s: query after drop: %q %v", db, got, err)
		}
		c.Close(context.Background())
	}
	s.exec("app", "CREATE TABLE t (s text)", "INSERT INTO t SELECT md5(g::text) FROM generate_series(1, 1000) g", "CREATE INDEX ON t (s)")
	r, err := pg.AuditCluster(ctx, s.connectFunc(), pg.AuditOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range r.Findings {
		if f.Severity != pg.SeverityInfo || (f.Check == "collation" && f.Object == "system collations") {
			t.Errorf("after dropping: %s", f)
		}
	}
}

// The audit finds every forbidden thing, with the right severity.
func TestAuditFindsProblems(t *testing.T) {
	s := newServer(t)
	ctx := ctxT(t, 5*time.Minute)
	c := s.connect("postgres")
	var icu, libc string
	if err := c.QueryRow(ctx, `SELECT collname FROM pg_collation WHERE collprovider = 'i' AND oid >= 12000 ORDER BY collname LIMIT 1`).Scan(&icu); err != nil {
		t.Fatal(err)
	}
	if err := c.QueryRow(ctx, `SELECT collname FROM pg_collation WHERE collprovider = 'c' AND collname NOT IN ('C', 'POSIX') ORDER BY collname LIMIT 1`).Scan(&libc); err != nil {
		t.Skipf("this Postgres build imported no libc collations: %v", err) // never on our builds; see test log
	}
	tsDir := t.TempDir()
	s.exec("postgres",
		`CREATE TABLE icu_col (id int, s text COLLATE "`+icu+`")`,
		`CREATE TABLE libc_idx (id int, s text)`,
		`CREATE INDEX libc_idx_s ON libc_idx (s COLLATE "`+libc+`")`,
		`CREATE TABLE expr_ord (id int, s text)`,
		`CREATE INDEX expr_ord_i ON expr_ord ((s < ('x' COLLATE "`+icu+`")))`,
		`CREATE DOMAIN icu_dom AS text COLLATE "`+icu+`"`,
		`CREATE TABLE m (x float8, y float8, n numeric)`,
		`CREATE INDEX m_exp ON m (exp(x))`,
		`CREATE INDEX m_pow_op ON m ((x ^ 2.5))`,
		`CREATE INDEX m_partial ON m (y) WHERE sin(y) > 0`,
		`CREATE INDEX m_ok ON m (sqrt(x))`,
		`CREATE TABLE gen (x float8, lnx float8 GENERATED ALWAYS AS (ln(x)) STORED, v float8 GENERATED ALWAYS AS (cos(x)) VIRTUAL)`,
		`CREATE TABLE pt (x float8) PARTITION BY RANGE (power(x, 2.0))`,
		`CREATE TABLE chk (x float8 CHECK (cbrt(x) < 10))`,
		`CREATE FUNCTION wrap_atan(float8) RETURNS float8 LANGUAGE sql IMMUTABLE BEGIN ATOMIC SELECT atan($1) + 1; END`,
		`CREATE INDEX m_wrapped ON m (wrap_atan(y))`,
		`CREATE FUNCTION opaque(float8) RETURNS float8 LANGUAGE plpgsql IMMUTABLE AS $$ BEGIN RETURN $1; END $$`,
		`CREATE INDEX m_opaque ON m (opaque(x))`,
		`CREATE INDEX m_numeric_cast ON m (((x)::numeric))`,
		`CREATE EXTENSION seg`,
		`CREATE TABLESPACE elsewhere LOCATION '`+strings.ReplaceAll(tsDir, "'", "''")+`'`,
		`ALTER SYSTEM SET jit = on`,
		`ALTER SYSTEM SET effective_io_concurrency = 1`,
		`SELECT pg_reload_conf()`,
	)
	// pg_reload_conf is asynchronous.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var jit string
		if err := c.QueryRow(ctx, "SELECT setting FROM pg_settings WHERE name = 'jit'").Scan(&jit); err != nil {
			t.Fatal(err)
		}
		if jit == "on" || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	r, err := pg.AuditCluster(ctx, s.connectFunc(), pg.AuditOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range r.Findings {
		t.Logf("%s", f)
	}
	want := []struct {
		sev           pg.Severity
		check, object string
		problem       string
	}{
		{pg.SeverityBlock, "collation", "column public.icu_col.s", "ICU"},
		{pg.SeverityBlock, "collation", "index public.libc_idx_s", "operating system"},
		{pg.SeverityBlock, "collation", "index public.expr_ord_i", "index expression"},
		{pg.SeverityBlock, "collation", "domain public.icu_dom", "ICU"},
		{pg.SeverityBlock, "libm_expr", "index public.m_exp", "exp(double precision)"},
		{pg.SeverityBlock, "libm_expr", "index public.m_pow_op", "dpow"},
		{pg.SeverityBlock, "libm_expr", "index public.m_partial", "index condition uses sin"},
		{pg.SeverityBlock, "libm_expr", "column public.gen.lnx", "ln(double precision)"},
		{pg.SeverityBlock, "libm_expr", "column public.gen.v", "virtual generated column uses cos"},
		{pg.SeverityBlock, "libm_expr", "table public.pt", "partition key uses power"},
		{pg.SeverityBlock, "libm_expr", "index public.m_wrapped", "through public.wrap_atan(double precision)"},
		{pg.SeverityWarn, "libm_expr", "constraint chk_x_check on public.chk", "cbrt"},
		{pg.SeverityWarn, "opaque_function", "index public.m_opaque", "opaque(double precision)"},
		{pg.SeverityWarn, "float_text_expr", "index public.m_numeric_cast", "numeric(double precision)"},
		{pg.SeverityBlock, "extension", "extension seg", "not been reviewed"},
		{pg.SeverityBlock, "tablespace", "tablespace elsewhere", "folder"},
		{pg.SeverityBlock, "setting", "setting jit", "Just-in-time"},
		{pg.SeverityBlock, "setting", "setting effective_io_concurrency", "copied between machines"},
	}
	for _, w := range want {
		found := false
		for _, f := range r.Findings {
			if f.Severity == w.sev && f.Check == w.check && f.Object == w.object && strings.Contains(f.Problem, w.problem) {
				found = true
			}
		}
		if !found {
			t.Errorf("missing %s %s finding for %q (%q)", w.sev, w.check, w.object, w.problem)
		}
	}
	for _, f := range r.Findings {
		if f.Object == "index public.m_ok" {
			t.Errorf("sqrt is exact and must not be reported: %s", f)
		}
	}
	if !r.Blocking() {
		t.Error("report does not block")
	}
}
