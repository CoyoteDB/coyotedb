//go:build integration && unix && ext

// Extensions (D53): pgvector built by scripts/extensions, installed by the
// agents themselves from COYOTE_EXTENSIONS_FROM into a private copy of the
// pinned PostgreSQL tree, then used for real: an HNSW index, nearest-
// neighbour queries on the main node and on its copies, after a failover,
// on the old main node after it rejoined, and `coyote migrate` moving a
// table with a vector column and an HNSW index into a cluster.
//
// Needs the archives (they are not published yet; see docs/DECISIONS.md D53):
//
//	COYOTE_TEST_EXT_DIR=<folder with pgvector-*.tar.gz> \
//	  go test -tags 'integration ext' -count=1 -run TestExt ./test/integration/
package integration

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"coyotedb.com/coyote/internal/pg"
)

// extSetup gives the test its own copy of the pinned PostgreSQL tree (the
// shared cache is never changed) and the environment that points agents
// at it and at the extension archives.
func extSetup(t *testing.T) (cache string, env []string, inst pg.Install, ext pg.Extension) {
	t.Helper()
	dir := os.Getenv("COYOTE_TEST_EXT_DIR")
	if dir == "" {
		t.Fatal("COYOTE_TEST_EXT_DIR must name the folder with the pgvector archives (scripts/extensions)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	shared, err := pg.Ensure(ctx, pg.FetchOptions{CacheDir: pgCache, SkipExtensions: true})
	if err != nil {
		t.Fatal(err)
	}
	cache = filepath.Join(t.TempDir(), "pg-cache")
	inst = pg.Install{Root: filepath.Join(cache, shared.Build.Name()), Build: shared.Build}
	copyTree(t, shared.Root, inst.Root)
	if err := pg.CheckInstall(inst, true); err != nil {
		t.Fatalf("copied tree: %v", err)
	}
	exts := pg.ExtensionsFor(inst.Build)
	if len(exts) != 1 || exts[0].Name != "vector" {
		t.Fatalf("pinned extensions for %s: %+v", inst.Build.Target, exts)
	}
	if err := pg.CheckExtension(inst, exts[0], false); err == nil {
		t.Fatal("the copied tree already has pgvector; the agents must install it")
	}
	return cache, []string{"COYOTE_PG_CACHE=" + cache, pg.ExtensionsFromEnv + "=" + dir}, inst, exts[0]
}

// copyTree copies a PostgreSQL tree (files, folders, symlinks) and nothing
// an extension installed into it.
func copyTree(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, p)
		if err != nil {
			return err
		}
		if strings.HasPrefix(rel, ".coyote-ext-") {
			return nil
		}
		dst := filepath.Join(to, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(dst, 0o755)
		case d.Type()&fs.ModeSymlink != 0:
			l, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(l, dst)
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, fi.Mode().Perm())
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
	if err != nil {
		t.Fatal(err)
	}
}

// vectors returns n deterministic vectors of dim float4 values, written
// exactly (shortest round-trip text of each float32).
func vectors(seed uint64, n, dim int) []string {
	r := rand.New(rand.NewPCG(seed, 7))
	out := make([]string, n)
	for i := range out {
		parts := make([]string, dim)
		for j := range parts {
			parts[j] = strconv.FormatFloat(float64(float32(r.Float64()*2-1)), 'g', -1, 32)
		}
		out[i] = "[" + strings.Join(parts, ",") + "]"
	}
	return out
}

func insertVectors(t *testing.T, c *pgx.Conn, table string, first int, vs []string) {
	t.Helper()
	b := &pgx.Batch{}
	for i, v := range vs {
		b.Queue("INSERT INTO "+table+" (id, emb) VALUES ($1, $2::vector)", first+i, v)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := c.SendBatch(ctx, b).Close(); err != nil {
		t.Fatalf("insert into %s: %v", table, err)
	}
}

// knn runs nearest-neighbour queries through the index (sequential scans
// off) and returns ids with their exact distances, one line per query.
// It also checks the plan uses the named index.
func knn(t *testing.T, c *pgx.Conn, table, op, index string, qs []string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := c.Exec(ctx, "SET enable_seqscan = off; SET hnsw.ef_search = 100"); err != nil {
		t.Fatal(err)
	}
	q := "SELECT id, (emb " + op + " $1::vector)::text FROM " + table + " ORDER BY emb " + op + " $1::vector LIMIT 10"
	var plan strings.Builder
	rows, err := c.Query(ctx, "EXPLAIN (COSTS OFF) "+q, qs[0])
	if err != nil {
		t.Fatal(err)
	}
	var line string
	if _, err := pgx.ForEachRow(rows, []any{&line}, func() error { plan.WriteString(line + "\n"); return nil }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.String(), "Index Scan using "+index) {
		t.Fatalf("the query does not use %s:\n%s", index, plan.String())
	}
	var out strings.Builder
	for _, v := range qs {
		rows, err := c.Query(ctx, q, v)
		if err != nil {
			t.Fatal(err)
		}
		var id int
		var d string
		n := 0
		if _, err := pgx.ForEachRow(rows, []any{&id, &d}, func() error {
			fmt.Fprintf(&out, "%d:%s ", id, d)
			n++
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if n != 10 {
			t.Fatalf("%d results, want 10", n)
		}
		out.WriteString("\n")
	}
	return out.String()
}

// sameKNN waits until n answers every query exactly like want.
func sameKNN(t *testing.T, n *cnode, want string, queries func(*pgx.Conn) string) {
	t.Helper()
	c := n.conn()
	var got string
	deadline := time.Now().Add(60 * time.Second)
	for {
		got = queries(c)
		if got == want || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond) // waiting for replay
	}
	if got != want {
		t.Fatalf("%s answers differently:\n got %s\nwant %s", n.name, got, want)
	}
}

func TestExtVectorReplicationAndFailover(t *testing.T) {
	_, env, inst, ext := extSetup(t)
	root := t.TempDir()
	a, b, c := newCNode(t, root, "node-a"), newCNode(t, root, "node-b"), newCNode(t, root, "node-c")
	for _, n := range []*cnode{a, b, c} {
		n.env = env
	}
	defer stopAll(a, b, c)
	a.bootstrap()
	if err := pg.CheckExtension(inst, ext, true); err != nil {
		t.Fatalf("the agent did not install pgvector: %v\n%s", err, a.a.out.String())
	}
	b.join(a)
	c.join(a)
	syncFull(t, a, b, c)

	// Every node offers it (the copies read the same tree; a copy answers
	// from its own Postgres with the library loaded there).
	for _, n := range []*cnode{a, b} {
		if got := queryText(t, n.conn(), "SELECT default_version FROM pg_available_extensions WHERE name = 'vector'"); got != ext.Version {
			t.Fatalf("%s offers vector %q, want %s", n.name, got, ext.Version)
		}
	}

	ca := a.conn()
	mustExec(t, ca, "CREATE EXTENSION vector")
	mustExec(t, ca, "CREATE TABLE docs (id int PRIMARY KEY, emb vector(16) NOT NULL)")
	insertVectors(t, ca, "docs", 1, vectors(1, 3000, 16))
	mustExec(t, ca, "CREATE INDEX docs_l2 ON docs USING hnsw (emb vector_l2_ops) WITH (m = 8, ef_construction = 32)")
	mustExec(t, ca, "CREATE INDEX docs_cos ON docs USING hnsw (emb vector_cosine_ops)")
	qs := vectors(99, 8, 16)
	queries := func(c *pgx.Conn) string {
		return knn(t, c, "docs", "<->", "docs_l2", qs) + knn(t, c, "docs", "<=>", "docs_cos", qs)
	}
	want := queries(ca)
	// Sanity: the index finds the true neighbours (exact search, no
	// index). HNSW is approximate and its graph is random per build, so
	// allow one miss of ten.
	mustExec(t, ca, "SET enable_indexscan = off; SET enable_seqscan = on; SET enable_bitmapscan = off")
	exact := strings.Split(queryText(t, ca, "SELECT string_agg(id::text, ',') FROM (SELECT id FROM docs ORDER BY emb <-> $1::vector LIMIT 10) s", qs[0]), ",")
	mustExec(t, ca, "RESET enable_indexscan; RESET enable_bitmapscan")
	found := 0
	for _, f := range strings.Fields(strings.SplitN(want, "\n", 2)[0]) {
		if slices.Contains(exact, strings.SplitN(f, ":", 2)[0]) {
			found++
		}
	}
	if found < 9 {
		t.Fatalf("HNSW found %d of the 10 true neighbours:\n%s\nexact %v", found, want, exact)
	}
	sameKNN(t, b, want, queries)
	sameKNN(t, c, want, queries)

	// Failover: the new main node answers the same, takes new rows into
	// the index, and its remaining copy follows.
	a.killAgent()
	var p *cnode
	waitFor(t, 60*time.Second, "a new main node", func() bool {
		p = primaryOf(t, b, c)
		return p != nil && p.clusterStatus().Cluster.PrimaryName == p.name
	})
	other := b
	if p == b {
		other = c
	}
	cp := p.conn()
	if got := queries(cp); got != want {
		t.Fatalf("after failover %s answers differently:\n got %s\nwant %s", p.name, got, want)
	}
	insertVectors(t, cp, "docs", 3001, vectors(2, 500, 16))
	want2 := queries(cp)
	if want2 == want {
		t.Fatal("500 new rows changed no answer; the queries do not reach the new rows")
	}
	sameKNN(t, other, want2, queries)

	// The old main node rejoins as a copy and answers the same.
	a.restart()
	a.waitOut("Following "+p.name+" live.", 2*time.Minute)
	sameKNN(t, a, want2, queries)
	stopAll(a, b, c)
}

// TestExtMigrateVector: a database with a vector column and an HNSW index
// moves in with `coyote migrate` (vector is no longer "not part of the
// build"), and the copy answers nearest-neighbour queries like the source
// on the main node and on its copy.
func TestExtMigrateVector(t *testing.T) {
	cache, env, inst, ext := extSetup(t)
	root := t.TempDir()
	a, b := newCNode(t, root, "node-a"), newCNode(t, root, "node-b")
	w := newWitness(t, root, "wit")
	for _, n := range []*cnode{a, b, w} {
		n.env = env
	}
	defer stopAll(a, b, w)
	a.bootstrap()
	b.join(a)
	w.join(a)
	syncFull(t, a, b)
	b.connInfo() // the app role exists
	if err := pg.CheckExtension(inst, ext, true); err != nil {
		t.Fatal(err)
	}

	src := startVectorSource(t, inst)
	mustExec(t, src.conn("postgres"), "CREATE DATABASE aiapp")
	sc := src.conn("aiapp")
	mustExec(t, sc, "CREATE EXTENSION vector")
	mustExec(t, sc, "CREATE TABLE chunks (id int PRIMARY KEY, body text, emb vector(8) NOT NULL)")
	insertVectors(t, sc, "chunks", 1, vectors(5, 2000, 8))
	mustExec(t, sc, "UPDATE chunks SET body = 'chunk ' || id")
	mustExec(t, sc, "CREATE INDEX chunks_emb ON chunks USING hnsw (emb vector_cosine_ops)")
	qs := vectors(77, 6, 8)
	want := knn(t, sc, "chunks", "<=>", "chunks_emb", qs)
	wantRows := fingerprint(t, sc, "chunks")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, coyoteBin, "migrate", "--data-dir", a.dir)
	cmd.Env = append(append(os.Environ(), env...), "COYOTE_PG_CACHE="+cache, "COYOTE_MIGRATE_SOURCE="+src.url("aiapp"))
	outb, err := cmd.CombinedOutput()
	out := string(outb)
	if strings.Contains(out, srcPassword) {
		t.Fatal("the source password appears in the command's output")
	}
	if err != nil || !strings.Contains(out, "Done. Database app on the cluster now holds the copy") || !strings.Contains(out, "vector") {
		t.Fatalf("migrate: %v\n%s", err, out)
	}
	t.Logf("migrate output:\n%s", out)

	for _, n := range []*cnode{a, b} {
		sameAsSourceIn(t, n, "app", map[string]string{"chunks": wantRows})
		c := nodeDB(t, n, "app")
		if def := queryText(t, c, "SELECT indexdef FROM pg_indexes WHERE indexname = 'chunks_emb'"); !strings.Contains(def, "USING hnsw (emb vector_cosine_ops)") {
			t.Fatalf("%s: index %q", n.name, def)
		}
		if got := knn(t, c, "chunks", "<=>", "chunks_emb", qs); got != want {
			t.Fatalf("%s answers differently from the source:\n got %s\nwant %s", n.name, got, want)
		}
	}
	// The app's own login can use it.
	app, err := tryConnect(appURLFrom(t, out))
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close(context.Background())
	if got := knn(t, app, "chunks", "<=>", "chunks_emb", qs); got != want {
		t.Fatalf("app session answers differently:\n got %s\nwant %s", got, want)
	}
	mustExec(t, app, "INSERT INTO chunks (id, body, emb) VALUES (5000, 'new', '[1,0,0,0,0,0,0,0]')")
	stopAll(a, b, w)
}

// startVectorSource starts a plain PostgreSQL (password login) from inst,
// which has pgvector installed.
func startVectorSource(t *testing.T, inst pg.Install) *srcPG {
	t.Helper()
	root := filepath.Join(t.TempDir(), "vector-src")
	s := &srcPG{t: t, inst: inst, data: filepath.Join(root, "data"), port: freePort(t)}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	pwfile := filepath.Join(root, "pw")
	if err := os.WriteFile(pwfile, []byte(srcPassword+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	initdb := exec.Command(inst.Bin("initdb"), "-D", s.data, "-U", "postgres", "--pwfile", pwfile,
		"--auth=scram-sha-256", "--encoding=UTF8", "--locale=C", "--no-sync")
	initdb.Env = withoutPGEnv()
	if out, err := initdb.CombinedOutput(); err != nil {
		t.Fatalf("initdb: %v\n%s", err, out)
	}
	f, err := os.OpenFile(filepath.Join(s.data, "postgresql.conf"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fmt.Fprintf(f, "\nport = %d\nlisten_addresses = '127.0.0.1'\nunix_socket_directories = ''\nfsync = off\n", s.port)
	f.Close()
	t.Cleanup(s.stop)
	start := exec.Command(inst.Bin("pg_ctl"), "-D", s.data, "-l", filepath.Join(root, "log"), "-w", "-t", "60", "start")
	start.Env = withoutPGEnv()
	if out, err := start.CombinedOutput(); err != nil {
		b, _ := os.ReadFile(filepath.Join(root, "log"))
		t.Fatalf("start source: %v\n%s\n%s", err, out, b)
	}
	return s
}

// TestExtVectorMixedGuard (D53 on top of D48/D49): pgvector is not allowed
// in a cluster that mixes kinds of machines: its distances are summed in a
// machine-dependent order (measured: most distances differ in the last
// digits between macOS arm64, macOS x86_64 and Linux x86_64). An HNSW index
// refuses the mixed join by name (the float-maths rule); without an index
// the extension itself refuses it (the audit's review list). In a cluster
// that is already mixed, creating a vector index is refused at once by the
// DDL guard, and the extension is reported as a blocking finding.
// One node records another operating system than the one the test runs
// on (COYOTE_TEST_FAKE_OS), as in mixed_test.go.
func TestExtVectorMixedGuard(t *testing.T) {
	_, env, _, _ := extSetup(t)
	root := t.TempDir()
	a, b, w := newCNode(t, root, "node-a"), newCNode(t, root, "node-b"), newWitness(t, root, "wit")
	a.env, w.env = env, env
	other := "linux"
	if runtime.GOOS == "linux" {
		other = "darwin"
	}
	b.env = append(slices.Clone(env), "COYOTE_TEST_FAKE_OS="+other)
	defer stopAll(a, b, w)
	a.bootstrap()
	waitFor(t, 2*time.Minute, "node-a's fingerprint and audit", func() bool {
		for _, m := range a.clusterStatus().Nodes {
			if m.Name == "node-a" && m.Fingerprint != "" && m.Guard != nil && !m.Guard.AuditAt.IsZero() {
				return true
			}
		}
		return false
	})
	w.join(a)
	ca := a.conn()
	mustExec(t, ca, "CREATE EXTENSION vector")
	mustExec(t, ca, "CREATE TABLE mx (id int PRIMARY KEY, emb vector(16) NOT NULL)")
	insertVectors(t, ca, "mx", 1, vectors(3, 200, 16))
	mustExec(t, ca, "CREATE INDEX mx_hnsw ON mx USING hnsw (emb vector_l2_ops)")

	joinRefused := func() string {
		t.Helper()
		b.a = launchCoyoteDBEnv(t, b.env, append(b.args("join", a.invite()), "--allow-mixed-os")...)
		select {
		case <-b.a.exited:
		case <-time.After(5 * time.Minute):
			t.Fatalf("join did not end:\n%s", b.a.out.String())
		}
		if b.a.err == nil {
			t.Fatalf("join should have been refused:\n%s", b.a.out.String())
		}
		return b.a.out.String()
	}
	if out := joinRefused(); !strings.Contains(out, "mx_hnsw") || !strings.Contains(out, "floating-point") {
		t.Fatalf("mixed join with an HNSW index:\n%s", out)
	}
	mustExec(t, ca, "DROP INDEX mx_hnsw")
	if out := joinRefused(); !strings.Contains(out, "extension vector") || !strings.Contains(out, "depends on the machine") {
		t.Fatalf("mixed join with pgvector installed:\n%s", out)
	}
	mustExec(t, ca, "DROP TABLE mx")
	mustExec(t, ca, "DROP EXTENSION vector")
	b.a = launchCoyoteDBEnv(t, b.env, append(b.args("join", a.invite()), "--allow-mixed-os")...)
	if !b.a.waitReady(t, 5*time.Minute) {
		t.Fatalf("mixed join exited before ready:\n%s", b.a.out.String())
	}
	b.waitOut("Following ", 2*time.Minute)
	syncFull(t, a, b)

	// Mixed now. CREATE EXTENSION is not DDL the trigger refuses (the
	// audit reports it, below), but every vector index is refused at once.
	// (A new session: the old one cached statements naming the dropped type.)
	ca = a.conn()
	mustExec(t, ca, "CREATE EXTENSION vector")
	mustExec(t, ca, "CREATE TABLE mx (id int PRIMARY KEY, emb vector(16) NOT NULL)")
	insertVectors(t, ca, "mx", 1, vectors(3, 200, 16))
	for _, ddl := range []string{
		"CREATE INDEX mx_hnsw ON mx USING hnsw (emb vector_l2_ops)",
		"CREATE INDEX mx_ivf ON mx USING ivfflat (emb vector_cosine_ops) WITH (lists = 4)",
		"CREATE INDEX mx_half ON mx USING hnsw ((emb::halfvec(16)) halfvec_l2_ops)",
	} {
		if _, err := ca.Exec(context.Background(), ddl); err == nil || !strings.Contains(err.Error(), "floating-point") {
			t.Fatalf("%s in a mixed cluster: %v", ddl, err)
		}
	}
	if n := queryInt(t, ca, "SELECT count(*) FROM pg_indexes WHERE tablename = 'mx' AND indexdef ~ 'USING (hnsw|ivfflat)'"); n != 0 {
		t.Fatalf("%d vector indexes exist", n)
	}
	a.waitStatusSays(3*time.Minute, "extension vector")
	mustExec(t, ca, "DROP TABLE mx")
	mustExec(t, ca, "DROP EXTENSION vector")
	stopAll(a, b, w)
}
