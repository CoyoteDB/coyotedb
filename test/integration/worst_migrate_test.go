//go:build integration && unix

package integration

import (
	"bufio"
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Worst case: `coyote migrate
// --cutover` of a live copy is killed (kill -9, like a closed laptop lid
// or a dropped SSH session taking the process with it) at each step it
// prints. Then the person runs `--cutover` again. Expected: it finishes,
// the cluster's database matches the source exactly, nothing is left on
// either side; at no point is a half-finished copy switched in.
func TestWorstMigrateCutoverKilled(t *testing.T) {
	for _, step := range []string{
		"Waiting for the last changes to arrive",
		"Checking the copy against the source",
		"the source;", // "N tables match the source; M rows."
		"copied from the source;",
		"Removing the subscription and the publication",
	} {
		t.Run(strings.Fields(step)[0]+"-"+strings.Fields(step)[1], func(t *testing.T) { cutoverKilledAt(t, step, 0, thenCutover) })
	}
	// The last step drops the subscription, then switches the copy in; a
	// kill between the two is reached only after the line, so kill later.
	for _, d := range []time.Duration{20 * time.Millisecond, 60 * time.Millisecond, 150 * time.Millisecond, 400 * time.Millisecond, time.Second} {
		t.Run("Removing+"+d.String(), func(t *testing.T) {
			cutoverKilledAt(t, "Removing the subscription and the publication", d, thenCutover)
		})
	}
	// The same kill, then the person gives up instead: --cancel must
	// leave the cluster's database as it was and nothing on the source.
	t.Run("Removing+20ms-then-cancel", func(t *testing.T) {
		cutoverKilledAt(t, "Removing the subscription and the publication", 20*time.Millisecond, thenCancel)
	})
	// A kill between the switch's two renames (the cluster's database
	// already moved aside, the copy not yet renamed in) is a window of
	// microseconds; it is made exactly by doing the first rename by hand
	// after a kill. --cutover must finish the switch; --cancel must put
	// the database back.
	t.Run("Removing+20ms-renamed-then-cutover", func(t *testing.T) {
		cutoverKilledAt(t, "Removing the subscription and the publication", 20*time.Millisecond, thenCutover|renamedAside)
	})
	t.Run("Removing+20ms-renamed-then-cancel", func(t *testing.T) {
		cutoverKilledAt(t, "Removing the subscription and the publication", 20*time.Millisecond, thenCancel|renamedAside)
	})
}

// What cutoverKilledAt does after the kill.
const (
	thenCutover  = 1 << iota // run --cutover again
	thenCancel               // run --cancel
	renamedAside             // first rename database app to app__replaced, as a kill between the switch's renames leaves it
)

func cutoverKilledAt(t *testing.T, step string, after time.Duration, then int) {
	src := startSource(t)
	src.loadShop(200)
	sc := src.conn("shop")
	mustExec(t, sc, `CREATE TABLE ticks (id bigserial PRIMARY KEY, n int NOT NULL, at timestamptz NOT NULL)`)
	mustExec(t, sc, `ALTER TABLE events_2025 REPLICA IDENTITY FULL`)
	mustExec(t, sc, `ALTER TABLE events_2026 REPLICA IDENTITY FULL`)

	root := t.TempDir()
	a, b := newCNode(t, root, "node-a"), newCNode(t, root, "node-b")
	w := newWitness(t, root, "wit")
	defer stopAll(a, b, w)
	a.bootstrap()
	b.join(a)
	w.join(a)
	syncFull(t, a, b)
	a.connInfo()

	wr := startWriter(t, src)
	if out, err := migrateCmd(t, a, "--live", src.url("shop")); err != nil {
		t.Fatalf("--live: %v\n%s", err, out)
	}
	waitFor(t, 60*time.Second, "new rows reaching the cluster", func() bool {
		n, ok := countOn(a, "app__migrating", "SELECT count(*) FROM ticks")
		return ok && n > 20
	})
	wr.halt(t)

	out, killed := cutoverKill(t, a, step, after)
	t.Logf("first --cutover, killed at %q (%v):\n%s", step, killed, out)
	if !killed {
		t.Fatalf("--cutover never printed %q", step)
	}
	// Whatever the kill left, the old database must still be usable and
	// unchanged by the cluster: the writer's rows are all there.
	if strings.Contains(out, "Done. Database") {
		if then&(thenCancel|renamedAside) != 0 {
			t.Fatalf("the first --cutover finished before the kill (%s after %q); this case needs it stopped", after, step)
		}
		t.Logf("the first --cutover finished before the kill (%s after %q)", after, step)
		return
	}
	if then&renamedAside != 0 {
		if !hasDB(t, a, "app__migrating") {
			t.Fatalf("the copy was already renamed in; cannot make the between-renames state")
		}
		waitFor(t, 60*time.Second, "database app moved aside", func() bool {
			c, err := tryConnect(withDB(t, a, "postgres"))
			if err != nil {
				return false
			}
			defer c.Close(context.Background())
			_, err = c.Exec(context.Background(), "ALTER DATABASE app RENAME TO app__replaced")
			return err == nil
		})
		// The session is closed at once: an open one would keep the
		// database from being renamed back.
		kc := nodeDB(t, a, "app__replaced")
		mustExec(t, kc, "CREATE TABLE keep_me (x int)")
		_ = kc.Close(context.Background())
	}
	// The source's rows as the app left them: the cluster must end with
	// exactly these (after --cutover), and the source must keep them.
	want := map[string]string{}
	for _, tbl := range append([]string{"ticks"}, shopTables...) {
		want[tbl] = fingerprint(t, sc, tbl)
	}
	if then&thenCancel != 0 {
		out, err := migrateCmd(t, a, "--cancel")
		t.Logf("--cancel:\n%s", out)
		if err != nil {
			t.Fatalf("--cancel failed after a kill at %q: %v\n%s", step, err, out)
		}
		if hasDB(t, a, "app__migrating") || hasDB(t, a, "app__replaced") || !hasDB(t, a, "app") {
			t.Fatalf("--cancel left the databases wrong: app__migrating %v, app__replaced %v, app %v",
				hasDB(t, a, "app__migrating"), hasDB(t, a, "app__replaced"), hasDB(t, a, "app"))
		}
		wantTables := 0
		if then&renamedAside != 0 {
			wantTables = 1 // keep_me: the database that was put back
		}
		if n := userTables(t, a, "app"); n != wantTables {
			t.Fatalf("database app has %d tables after --cancel, want %d (it must be as it was)", n, wantTables)
		}
		for tbl, w := range want {
			if got := fingerprint(t, sc, tbl); got != w {
				t.Fatalf("source table %s changed during --cancel", tbl)
			}
		}
		noneLeftOnSource(t, sc)
		out, err = migrateCmd(t, a, "--status")
		if err != nil || !strings.Contains(out, "No live copy is running") {
			t.Fatalf("--status after --cancel: %v\n%s", err, out)
		}
		return
	}
	out, err := migrateCmd(t, a, "--cutover", "--yes", "--replace")
	t.Logf("second --cutover:\n%s", out)
	if err != nil {
		t.Fatalf("CUTOVER NOT FINISHABLE after a kill at %q: %v\n%s", step, err, out)
	}
	sameAsSource(t, a, want)
	noneLeftOnSource(t, sc)
	if hasDB(t, a, "app__migrating") || hasDB(t, a, "app__replaced") {
		t.Fatalf("databases left behind: app__migrating %v, app__replaced %v", hasDB(t, a, "app__migrating"), hasDB(t, a, "app__replaced"))
	}
	// Once more: the move is done, and it says so plainly.
	out, err = migrateCmd(t, a, "--cutover", "--yes")
	t.Logf("third --cutover:\n%s", out)
	if err != nil || !strings.Contains(out, "The move is done") {
		t.Fatalf("--cutover after the move finished: %v\n%s", err, out)
	}
	out, err = migrateCmd(t, a, "--cancel")
	if err != nil || !strings.Contains(out, "already done") {
		t.Fatalf("--cancel after the move finished: %v\n%s", err, out)
	}
	sameAsSource(t, a, want)
}

func noneLeftOnSource(t *testing.T, sc *pgx.Conn) {
	t.Helper()
	if n := queryInt(t, sc, "SELECT count(*) FROM pg_publication"); n != 0 {
		t.Fatalf("%d publications left on the source", n)
	}
	if n := queryInt(t, sc, "SELECT count(*) FROM pg_replication_slots"); n != 0 {
		t.Fatalf("%d replication slots left on the source", n)
	}
}

func hasDB(t *testing.T, n *cnode, db string) bool {
	t.Helper()
	c := nodeDB(t, n, "postgres")
	defer c.Close(context.Background())
	return queryInt(t, c, "SELECT count(*) FROM pg_database WHERE datname = $1", db) == 1
}

// cutoverKill runs --cutover and kill -9s it as soon as its output has a
// line containing step. It returns the output and whether it killed it.
func cutoverKill(t *testing.T, n *cnode, step string, after time.Duration) (string, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, coyoteBin, "migrate", "--data-dir", n.dir, "--cutover", "--yes")
	cmd.Env = append(os.Environ(), "COYOTE_PG_CACHE="+pgCache)
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); pw.Close(); close(done) }()
	var b strings.Builder
	killed := false
	sc := bufio.NewScanner(pr)
	for sc.Scan() {
		b.WriteString(sc.Text() + "\n")
		if !killed && strings.Contains(sc.Text(), step) {
			killed = true
			p := cmd.Process
			time.AfterFunc(after, func() { _ = p.Signal(syscall.SIGKILL) })
		}
	}
	<-done
	return b.String(), killed
}
