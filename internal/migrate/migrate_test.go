package migrate

import (
	"slices"
	"strings"
	"testing"
)

func TestParseConnStringURI(t *testing.T) {
	p, err := parseConnString("postgresql://user%40x:p%40ss%2Fw@db.example.com:6543/my%20db?sslmode=require&application_name=web")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"user": "user@x", "host": "db.example.com", "port": "6543", "dbname": "my db", "sslmode": "require", "application_name": "web"}
	for k, v := range want {
		if p.get(k) != v {
			t.Errorf("%s = %q, want %q", k, p.get(k), v)
		}
	}
	if p.password != "p@ss/w" {
		t.Errorf("password %q", p.password)
	}
	ci := p.conninfo()
	if strings.Contains(ci, "p@ss") || strings.Contains(ci, "password") {
		t.Fatalf("conninfo holds the password: %s", ci)
	}
	if !strings.Contains(p.withPassword(), `password='p@ss/w'`) {
		t.Fatalf("withPassword: %s", p.withPassword())
	}
}

func TestParseConnStringMultiHostAndIPv6(t *testing.T) {
	p, err := parseConnString("postgres://u@h1:5432,h2:5433,[::1]:5434/d")
	if err != nil {
		t.Fatal(err)
	}
	if p.get("host") != "h1,h2,::1" || p.get("port") != "5432,5433,5434" {
		t.Fatalf("host %q port %q", p.get("host"), p.get("port"))
	}
	p, err = parseConnString("postgres://u@h1:5432,h2:5432/d")
	if err != nil {
		t.Fatal(err)
	}
	if p.get("port") != "5432" {
		t.Fatalf("port %q", p.get("port"))
	}
	if p.password != "" {
		t.Fatal("no password expected")
	}
}

func TestParseConnStringKeywords(t *testing.T) {
	p, err := parseConnString(`host=localhost  port = 5432 dbname='my db' password='it\'s secret' sslmode=disable`)
	if err != nil {
		t.Fatal(err)
	}
	if p.get("dbname") != "my db" || p.password != "it's secret" || p.get("port") != "5432" {
		t.Fatalf("%+v %q", p.vals, p.password)
	}
	if got := p.conninfo(); got != `host='localhost' port='5432' dbname='my db' sslmode='disable'` {
		t.Fatalf("conninfo %s", got)
	}
	for _, bad := range []string{"", "just some text", "host='unclosed", "=x"} {
		if _, err := parseConnString(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
	// An error never echoes a whole string (it may hold a password).
	_, err = parseConnString("hostlocalhost password secretsecret")
	if err == nil || strings.Contains(err.Error(), "secretsecret") {
		t.Fatalf("error %v", err)
	}
}

func TestQuoteValueRoundTrip(t *testing.T) {
	for _, v := range []string{`a b`, `it's`, `back\slash`, `'`, ``} {
		var p connParams
		if err := parseKeywords("k="+quoteValue(v), &p); err != nil {
			t.Fatal(err)
		}
		if p.get("k") != v {
			t.Fatalf("%q -> %q", v, p.get("k"))
		}
	}
}

const sampleTOC = `;
; Archive created at 2026-11-21 20:23:46 CST
;
3; 3079 16423 EXTENSION - pg_stat_statements
3921; 0 0 COMMENT - EXTENSION pg_stat_statements
2; 3079 16385 EXTENSION - pgcrypto
3922; 0 0 COMMENT - EXTENSION pgcrypto
6; 2615 16400 SCHEMA - extensions postgres
3930; 0 0 COMMENT - SCHEMA extensions postgres
279; 1255 16494 FUNCTION public f(integer) postgres
224; 1259 16468 TABLE public items postgres
3923; 0 0 COMMENT public TABLE items postgres
3912; 0 16468 TABLE DATA public items postgres
3914; 0 16479 TABLE DATA public orders postgres
3925; 0 0 SEQUENCE SET public items_id_seq postgres
3757; 2606 16477 CONSTRAINT public items items_pkey postgres
3760; 2606 16485 FK CONSTRAINT public orders orders_item_fkey postgres
3800; 1259 16600 MATERIALIZED VIEW DATA public mv postgres
3801; 3466 16601 EVENT TRIGGER - audit_ddl postgres
3802; 3256 16610 POLICY public items own rows postgres
3803; 3256 16611 POLICY public items everyone postgres
`

func TestTOCPlan(t *testing.T) {
	entries, err := parseTOC(strings.NewReader(sampleTOC))
	if err != nil {
		t.Fatal(err)
	}
	tp := planTOC(entries, map[string]bool{"public\x00items own rows": true})
	if len(tp.schemas) != 1 || tp.schemas[0].Name != "extensions" {
		t.Fatalf("schemas %+v", tp.schemas)
	}
	var ids []int
	for _, e := range tp.rest {
		ids = append(ids, e.ID)
	}
	want := []int{3, 2, 3930, 279, 224, 3923, 3912, 3914, 3925, 3757, 3760, 3800, 3803}
	if !slices.Equal(ids, want) {
		t.Fatalf("rest %v, want %v", ids, want)
	}
	if e := tp.dataByID[3912]; e.Schema != "public" || e.Name != "items" {
		t.Fatalf("data entry %+v", e)
	}
	if len(tp.dataByID) != 3 {
		t.Fatalf("data entries %d", len(tp.dataByID))
	}
	if len(tp.leftOut) != 1 || !strings.Contains(tp.leftOut[0], "audit_ddl") {
		t.Fatalf("left out %v", tp.leftOut)
	}
	// The list file keeps the lines exactly as pg_restore wrote them.
	if !strings.Contains(listFile(tp.rest), "3912; 0 16468 TABLE DATA public items postgres\n") {
		t.Fatal("list file")
	}
}

func TestFinishedItemAndRestoreLog(t *testing.T) {
	id, desc, ok := finishedItem("pg_restore: finished item 3912 TABLE DATA items")
	if !ok || id != 3912 || desc != "TABLE DATA items" {
		t.Fatal(id, desc, ok)
	}
	if _, _, ok := finishedItem("pg_restore: processing data for table \"public.items\""); ok {
		t.Fatal("not a finished line")
	}
	var r restoreLog
	for _, l := range []string{
		"pg_restore: from TOC entry 3; 3079 16423 EXTENSION pg_stat_statements (no owner)",
		"pg_restore: error: could not execute query: ERROR:  permission denied to create extension \"pg_stat_statements\"",
		"HINT:  Must be superuser to create this extension.",
		"pg_restore: from TOC entry 4100; 3256 16700 POLICY items own_rows postgres",
		"pg_restore: error: could not execute query: ERROR:  role \"authenticated\" does not exist",
	} {
		r.line(l)
	}
	if len(r.errs) != 2 || r.lost() {
		t.Fatalf("%q lost=%v", r.errs, r.lost())
	}
	if r.errs[0] != `extension pg_stat_statements: permission denied to create extension "pg_stat_statements"` {
		t.Errorf("err 0: %q", r.errs[0])
	}
	if !strings.HasPrefix(r.errs[1], "policy items own_rows: ") || !strings.HasSuffix(r.errs[1], `role "authenticated" does not exist`) {
		t.Errorf("err 1: %q", r.errs[1])
	}
	r.line("pg_restore: error: could not execute query: server closed the connection unexpectedly")
	if !r.lost() || len(r.errs) != 2 {
		t.Fatal("a lost connection is not an object error")
	}
}

func TestStatsQueries(t *testing.T) {
	small := table{Schema: "public", Name: "Items", Size: 1 << 20, Columns: []column{{Name: "id"}, {Name: "Pay", Money: true}}, PK: []string{"id"}, PKPlain: true}
	q := statsQuery(small)
	if !strings.Contains(q, `FROM ONLY "public"."Items" t`) || !strings.Contains(q, `ROW(t."id", t."Pay"::numeric)::text`) {
		t.Fatal(q)
	}
	big := small
	big.Size = 10 << 30
	if checkMode(big) != "first and last 5000 rows" {
		t.Fatal(checkMode(big))
	}
	q = statsQuery(big)
	if !strings.Contains(q, `ORDER BY t."id" DESC LIMIT 5000`) || !strings.Contains(q, `(SELECT count(*) FROM ONLY "public"."Items")`) {
		t.Fatal(q)
	}
	big.PKPlain = false // a text key sorts differently under another collation
	if checkMode(big) != "row count" {
		t.Fatal(checkMode(big))
	}
	if !strings.HasPrefix(statsQuery(big), "SELECT count(*), ''") {
		t.Fatal(statsQuery(big))
	}
}

func basePlanInput() planInput {
	src := &source{
		params:     connParams{keys: []string{"host", "dbname"}, vals: map[string]string{"host": "db.example.com", "dbname": "shop"}},
		versionNum: 160004, version: "16.4", database: "shop", user: "postgres", size: 1 << 30,
		encoding: "UTF8", collate: "en_US.UTF-8", provider: "libc",
		schemas:    []string{"public"},
		extensions: []extension{{"pgcrypto", "1.3", "public"}, {"pg_stat_statements", "1.10", "public"}},
		tables:     []table{{Schema: "public", Name: "items", Rows: 1000}},
	}
	tgt := &targetInfo{user: "app", exists: true, owner: "app",
		available:  map[string]availExt{"pgcrypto": {Trusted: true}, "pg_stat_statements": {Superuser: true}, "postgres_fdw": {Superuser: true}},
		collations: map[string]bool{"C": true, "POSIX": true, "ucs_basic": true}}
	return planInput{src: src, tgt: tgt, database: "app", mainName: "node-a", freeBytes: -1}
}

func TestPlanReady(t *testing.T) {
	p := buildPlan(basePlanInput())
	if len(p.blockers) != 0 {
		t.Fatalf("blockers %v", p.blockers)
	}
	if !slices.Equal(p.skipExtensions, []string{"pg_stat_statements"}) {
		t.Fatalf("skip %v", p.skipExtensions)
	}
	if len(p.notes) != 1 || !strings.Contains(p.notes[0], "en_US.UTF-8") {
		t.Fatalf("notes %v", p.notes)
	}
}

func TestPlanBlockers(t *testing.T) {
	cases := []struct {
		name string
		edit func(*planInput)
		want string
	}{
		{"old version", func(in *planInput) { in.src.versionNum, in.src.version = 110022, "11.22" }, "PostgreSQL 12 or newer"},
		{"newer version", func(in *planInput) { in.src.versionNum, in.src.version = 190001, "19.1" }, "newer than CoyoteDB"},
		{"missing extension", func(in *planInput) {
			in.src.extensions = append(in.src.extensions, extension{"postgis", "3.5.2", "public"})
		}, "Extension postgis (version 3.5.2) is not part of CoyoteDB's PostgreSQL build"},
		{"pinned extension not installed", func(in *planInput) {
			in.src.extensions = append(in.src.extensions, extension{"vector", "0.8.0", "public"})
		}, "Extension vector (version 0.8.0) comes with CoyoteDB but is not installed on the cluster's computers yet"},
		{"admin extension elsewhere", func(in *planInput) {
			in.src.extensions = append(in.src.extensions, extension{"postgres_fdw", "1.1", "public"})
		}, "Run this command on node-a"},
		{"not empty", func(in *planInput) { in.tgt.contents = []string{"table public.users"} }, "is not empty (table public.users)"},
		{"other owner", func(in *planInput) { in.tgt.owner = "postgres" }, "belongs to postgres"},
		{"unreadable", func(in *planInput) { in.src.unreadable = []string{"public.secret"} }, "cannot read public.secret"},
		{"rls", func(in *planInput) { in.src.rlsBlocked = []string{"public.items"} }, "Row-level security hides rows of public.items"},
		{"collation", func(in *planInput) {
			in.src.collations = []collationUse{{"en-x-icu", "pg_catalog", "public.items.name"}}
		}, `Column public.items.name uses the sort rule "en-x-icu"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := basePlanInput()
			c.edit(&in)
			p := buildPlan(in)
			if len(p.blockers) != 1 || !strings.Contains(p.blockers[0], c.want) {
				t.Fatalf("blockers %q, want one with %q", p.blockers, c.want)
			}
		})
	}
}

// pgvector installed on the cluster (D53): a source that uses it moves in;
// only a superuser may create it, so the administrator does, on the main
// node's machine, and elsewhere the preflight says where to run.
func TestPlanVectorAvailable(t *testing.T) {
	in := basePlanInput()
	in.src.extensions = append(in.src.extensions, extension{"vector", "0.8.0", "public"})
	in.tgt.available["vector"] = availExt{Superuser: true}
	in.adminReady = true
	p := buildPlan(in)
	if len(p.blockers) != 0 {
		t.Fatalf("blockers %v", p.blockers)
	}
	if len(p.adminExtensions) != 1 || p.adminExtensions[0].Name != "vector" {
		t.Fatalf("admin %v", p.adminExtensions)
	}
	if slices.Contains(p.skipExtensions, "vector") {
		t.Fatalf("vector skipped: %v", p.skipExtensions)
	}
	in.adminReady = false
	p = buildPlan(in)
	if len(p.blockers) != 1 || !strings.Contains(p.blockers[0], "Extension vector can only be created by the database administrator") {
		t.Fatalf("blockers %v", p.blockers)
	}
}

func TestPlanOptions(t *testing.T) {
	in := basePlanInput()
	in.src.extensions = append(in.src.extensions, extension{"vector", "0.8.0", "public"}, extension{"postgres_fdw", "1.1", "public"})
	in.userSkip = []string{"vector", "nope"}
	in.adminReady = true
	in.tgt.contents = []string{"table public.users"}
	in.replace = true
	p := buildPlan(in)
	if len(p.blockers) != 0 {
		t.Fatalf("blockers %v", p.blockers)
	}
	if !p.replacing {
		t.Fatal("not replacing")
	}
	if len(p.adminExtensions) != 1 || p.adminExtensions[0].Name != "postgres_fdw" {
		t.Fatalf("admin %v", p.adminExtensions)
	}
	if !slices.Contains(p.skipExtensions, "vector") {
		t.Fatalf("skip %v", p.skipExtensions)
	}
	if !slices.ContainsFunc(p.notes, func(n string) bool { return strings.Contains(n, `no extension "nope"`) }) {
		t.Fatalf("notes %v", p.notes)
	}
}

func TestSupabaseSchemasLeftOut(t *testing.T) {
	in := basePlanInput()
	in.src.schemas = []string{"auth", "extensions", "graphql", "public", "storage"}
	in.src.host = detectHost(in.src.schemas, []string{"supabase_admin"})
	if in.src.host != "Supabase" {
		t.Fatal(in.src.host)
	}
	in.src.extensions = append(in.src.extensions, extension{"pg_graphql", "1.5", "graphql"})
	p := buildPlan(in)
	if len(p.blockers) != 0 {
		t.Fatalf("blockers %v", p.blockers)
	}
	// auth and storage hold the app's users and file records: copied.
	if !slices.Equal(p.excludedSchemas, []string{"graphql"}) {
		t.Fatalf("excluded %v", p.excludedSchemas)
	}
	if !slices.Contains(p.skipExtensions, "pg_graphql") {
		t.Fatalf("skip %v", p.skipExtensions)
	}
	// Policies for roles the cluster lacks are left out, and said so.
	in.src.policies = []policy{{"public", "notes", "own notes", []string{"authenticated"}}, {"public", "notes", "all read", nil}}
	p = buildPlan(in)
	if !p.dropPolicies["public\x00notes own notes"] || len(p.dropPolicies) != 1 {
		t.Fatalf("drop %v", p.dropPolicies)
	}
	if !strings.Contains(strings.Join(p.leftOut, "\n"), `policy "own notes" on public.notes (for role authenticated, which exists only on the source`) {
		t.Fatalf("left out %v", p.leftOut)
	}
	if detectHost([]string{"auth"}, nil) != "" {
		t.Fatal("a schema named auth alone is not Supabase")
	}
}

func TestDBURL(t *testing.T) {
	tg := Target{AppURL: "postgresql://app:s3cret@127.0.0.1:1,127.0.0.1:2/app?sslmode=verify-full&sslrootcert=%2Fa%20b%2Fca.crt&target_session_attrs=read-write"}
	u, err := tg.dbURL("app__migrating")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(u, "s3cret") || !strings.Contains(u, "@127.0.0.1:1,127.0.0.1:2/app__migrating?") || !strings.Contains(u, "sslrootcert=%2Fa%20b%2Fca.crt") {
		t.Fatal(u)
	}
	if a := tg.AppURLFor("shop"); !strings.Contains(a, "app:s3cret@") || !strings.Contains(a, "/shop?") {
		t.Fatal(a)
	}
	if pattern(`we"ird`) != `"we""ird"` {
		t.Fatal(pattern(`we"ird`))
	}
}

func livePlanInput() planInput {
	in := basePlanInput()
	in.live, in.adminReady = true, true
	in.src.live = liveInfo{walLevel: "logical", replication: true, freeSlots: 3}
	return in
}

func TestPlanLive(t *testing.T) {
	if p := buildPlan(livePlanInput()); len(p.blockers) != 0 {
		t.Fatalf("blockers %v", p.blockers)
	}
	cases := []struct {
		name string
		edit func(*planInput)
		want string
	}{
		{"wal level", func(in *planInput) { in.src.live.walLevel = "replica" }, "wal_level = logical; it is replica"},
		{"no replication right", func(in *planInput) { in.src.live.replication = false }, "may not stream changes"},
		{"no slot", func(in *planInput) { in.src.live.freeSlots = 0 }, "no free replication slot"},
		{"no key", func(in *planInput) { in.src.live.noIdentity = []string{"public.log"} }, "Table public.log has no primary key"},
		{"not owner", func(in *planInput) { in.src.live.notOwned = []string{"public.items"} }, "does not own public.items"},
		{"large objects", func(in *planInput) { in.src.largeObjs = 2 }, "2 large objects"},
		{"not on main", func(in *planInput) { in.adminReady = false }, "Run this command on node-a"},
		{"already running", func(in *planInput) {
			in.tgt.staging, in.tgt.stagingNote = true, liveComment+" from x"
		}, "A live copy into database app is running"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := livePlanInput()
			c.edit(&in)
			p := buildPlan(in)
			if len(p.blockers) != 1 || !strings.Contains(p.blockers[0], c.want) {
				t.Fatalf("blockers %q, want one with %q", p.blockers, c.want)
			}
		})
	}
}

func TestLiveName(t *testing.T) {
	if got := liveName("My-App"); got != "coyote_migrate_my_app" {
		t.Fatal(got)
	}
	if got := liveName(strings.Repeat("a", 80)); len(got) != 63 {
		t.Fatal(len(got))
	}
}
