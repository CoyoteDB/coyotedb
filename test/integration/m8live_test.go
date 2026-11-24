//go:build integration && unix

package integration

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// writer keeps inserting, updating and deleting on the source, the way a
// live app does, until stopped.
type writer struct {
	stop    chan struct{}
	done    sync.WaitGroup
	commits atomic.Int64
	err     atomic.Value
}

func startWriter(t *testing.T, src *srcPG) *writer {
	t.Helper()
	w := &writer{stop: make(chan struct{})}
	c, err := tryConnect(src.url("shop"))
	if err != nil {
		t.Fatal(err)
	}
	w.done.Add(1)
	go func() {
		defer w.done.Done()
		defer c.Close(context.Background())
		for i := 1; ; i++ {
			select {
			case <-w.stop:
				return
			default:
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_, err := c.Exec(ctx, `WITH t AS (INSERT INTO ticks (n, at) VALUES ($1, now()) RETURNING n)
			    UPDATE customers SET name = 'renamed ' || $1 WHERE id = 1 + $1 % 500`, i)
			if err == nil && i%10 == 0 {
				_, err = c.Exec(ctx, `DELETE FROM orders WHERE id = $1`, i)
			}
			cancel()
			if err != nil {
				w.err.Store(err)
				return
			}
			w.commits.Add(1)
			time.Sleep(20 * time.Millisecond)
		}
	}()
	return w
}

func (w *writer) halt(t *testing.T) {
	t.Helper()
	close(w.stop)
	w.done.Wait()
	if e := w.err.Load(); e != nil {
		t.Fatalf("writer on the source failed: %v", e)
	}
}

// count reads one number from database db on node n (superuser, local).
func countOn(n *cnode, db, q string) (int64, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := n.cli("conninfo", "--admin")
	if err != nil {
		return 0, false
	}
	u := strings.Replace(strings.TrimSpace(out), "/postgres?", "/"+db+"?", 1)
	c, err := pgx.Connect(ctx, u)
	if err != nil {
		return 0, false
	}
	defer c.Close(context.Background())
	var v int64
	if err := c.QueryRow(ctx, q).Scan(&v); err != nil {
		return 0, false
	}
	return v, true
}

// A live copy (logical replication) while the source keeps being written
// to; the coyote main node is killed while the copy follows changes. The
// subscription resumes from the new main node by itself, --status says so
// there, and --cutover on the new main node switches in a copy that matches
// the source exactly, with sequences copied.
func TestM8LiveFailoverDuringCatchUp(t *testing.T) {
	src := startSource(t)
	src.loadShop(200)
	sc := src.conn("shop")
	mustExec(t, sc, `CREATE TABLE ticks (id bigserial PRIMARY KEY, n int NOT NULL, at timestamptz NOT NULL)`)

	root := t.TempDir()
	a, b := newCNode(t, root, "node-a"), newCNode(t, root, "node-b")
	w := newWitness(t, root, "wit")
	defer stopAll(a, b, w)
	a.bootstrap()
	b.join(a)
	w.join(a)
	syncFull(t, a, b)
	a.connInfo()

	// Not on the main node's computer: refused, saying where to run it.
	out, err := migrateCmd(t, b, "--live", src.url("shop"))
	if err == nil || !strings.Contains(out, "Run this command on node-a") {
		t.Fatalf("--live on a copy: err=%v\n%s", err, out)
	}

	// Partitions without a primary key would make the source refuse the
	// app's updates and deletes once published: refused, with the fix.
	out, err = migrateCmd(t, a, "--live", src.url("shop"))
	if err == nil || !strings.Contains(out, "public.events_2025, public.events_2026 have no primary key") {
		t.Fatalf("--live with tables without a key: err=%v\n%s", err, out)
	}
	mustExec(t, sc, `ALTER TABLE events_2025 REPLICA IDENTITY FULL`)
	mustExec(t, sc, `ALTER TABLE events_2026 REPLICA IDENTITY FULL`)

	wr := startWriter(t, src)
	out, err = migrateCmd(t, a, "--live", src.url("shop"))
	if err != nil {
		t.Fatalf("--live: %v\n%s", err, out)
	}
	t.Logf("--live output:\n%s", out)
	if !strings.Contains(out, "now following changes") {
		t.Fatalf("--live output:\n%s", out)
	}
	// Changes flow while the app writes.
	waitFor(t, 60*time.Second, "new rows reaching the cluster", func() bool {
		n, ok := countOn(a, "app__migrating", "SELECT count(*) FROM ticks")
		return ok && n > 20
	})
	out, err = migrateCmd(t, a, "--status")
	if err != nil || !strings.Contains(out, "all copied; following changes") {
		t.Fatalf("--status: err=%v\n%s", err, out)
	}

	// Kill the main node while changes keep coming.
	before := wr.commits.Load()
	a.killAgent()
	waitFor(t, 90*time.Second, "node-b to become the main node", func() bool {
		return b.isPrimary() && b.clusterStatus().Cluster.PrimaryName == b.name
	})
	// The subscription resumes on b: rows written after the kill arrive.
	waitFor(t, 3*time.Minute, "changes after the failover reaching node-b", func() bool {
		var srcN int64
		_ = sc.QueryRow(context.Background(), "SELECT count(*) FROM ticks").Scan(&srcN)
		n, ok := countOn(b, "app__migrating", "SELECT count(*) FROM ticks")
		return ok && wr.commits.Load() > before+50 && n > before+50 && n <= srcN
	})
	// --status on the witness says where to look; on b it reports running.
	out, err = migrateCmd(t, w, "--status")
	if err == nil || !strings.Contains(out, "the main node is node-b") {
		t.Fatalf("--status on the witness: err=%v\n%s", err, out)
	}
	waitFor(t, 2*time.Minute, "the subscription worker running on node-b", func() bool {
		out, err = migrateCmd(t, b, "--status")
		return err == nil && strings.Contains(out, "Worker:        running")
	})
	t.Logf("--status on node-b after the failover:\n%s", out)

	// Cutover: stop the app, then switch.
	wr.halt(t)
	out, err = migrateCmd(t, b, "--cutover", "--yes")
	if err != nil {
		t.Fatalf("--cutover: %v\n%s", err, out)
	}
	t.Logf("--cutover output:\n%s", out)
	if !strings.Contains(out, "6 tables match the source") || !strings.Contains(out, "sequences copied from the source") {
		t.Fatalf("--cutover output:\n%s", out)
	}
	want := map[string]string{}
	for _, tbl := range append([]string{"ticks"}, shopTables...) {
		want[tbl] = fingerprint(t, sc, tbl)
	}
	sameAsSource(t, b, want)
	app, err := tryConnect(appURLFrom(t, out))
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close(context.Background())
	maxID := queryInt(t, sc, "SELECT max(id) FROM ticks")
	if id := queryInt(t, app, "INSERT INTO ticks (n, at) VALUES (0, now()) RETURNING id"); id <= maxID {
		t.Fatalf("new ticks id %d is not past the source's %d", id, maxID)
	}
	// Nothing left behind on either side.
	if n := queryInt(t, sc, "SELECT count(*) FROM pg_publication"); n != 0 {
		t.Fatalf("%d publications left on the source", n)
	}
	if n := queryInt(t, sc, "SELECT count(*) FROM pg_replication_slots"); n != 0 {
		t.Fatalf("%d replication slots left on the source", n)
	}
	if n := queryInt(t, app, "SELECT count(*) FROM pg_database WHERE datname LIKE 'app\\_\\_%'"); n != 0 {
		t.Fatalf("%d staging databases left", n)
	}
}
