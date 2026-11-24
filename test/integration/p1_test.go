//go:build integration && unix

// Finding P1 (D56): the new main node loses power while it promotes, after
// Postgres removed standby.signal and before it wrote its end-of-recovery
// record. Real Postgres; the state Postgres leaves in that window is made
// by hand (power cut of the copy, then standby.signal removed, as
// Postgres does before it writes the new timeline's history file), so it
// is reached every run.
package integration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"coyotedb.com/coyote/internal/pg"
)

// p1State: n1 main, n2 its copy; n2's stream is cut, n1 writes rows n2
// never gets, n1 dies, then n2 is power-cut "mid-promotion".
func p1State(t *testing.T) (a, b *pgNode) {
	t.Helper()
	root := t.TempDir()
	a = newPrimary(t, root, "n1")
	b = newPGNode(t, root, "n2")
	b.cloneFrom(a)
	waitSame(t, a, b)
	b.dev("cut-stream")
	insertRange(t, a.conn(), 1000, 1049, "unreplicated-")
	a.crash()
	b.crash()
	if err := os.Remove(filepath.Join(b.pgdata, "standby.signal")); err != nil {
		t.Fatal(err)
	}
	if cs := controlState(t, b.pgdata); cs.Timeline != 1 || cs.CleanlyShutDown() {
		t.Fatalf("set-up: want n2 crashed on timeline 1, got %+v", cs)
	}
	return a, b
}

// The fix: the interrupted promotion is resumed as a standby and promoted
// again, onto a new timeline; the old main node is then really rewound
// (its unreplicated rows are gone) and follows.
func TestP1ResumeInterruptedPromotion(t *testing.T) {
	a, b := p1State(t)
	node, err := pg.NewNode(b.dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.ResumeStandby(); err != nil {
		t.Fatal(err)
	}
	b.up()
	if r := queryText(t, b.conn(), "SELECT pg_is_in_recovery()::text"); r != "true" {
		t.Fatalf("resumed n2 is not a standby (in recovery: %s)", r)
	}
	b.dev("promote", "--slots-for", "n1")
	if tl := queryInt(t, b.conn(), "SELECT timeline_id FROM pg_control_checkpoint()"); tl < 2 {
		t.Fatalf("n2 promoted again but is on timeline %d", tl)
	}
	insertRange(t, b.conn(), 2000, 2099, "new-primary-")

	a.dev("demote")
	out := a.dev("rewind", "--from", b.addr())
	if !strings.Contains(out, "Rewound.") {
		t.Fatalf("old main not rewound:\n%s", out)
	}
	log, _ := os.ReadFile(filepath.Join(a.dir, "logs", "rewind.log"))
	if strings.Contains(string(log), "no rewind required") {
		t.Fatalf("pg_rewind found nothing to rewind; the histories were not told apart:\n%s", log)
	}
	a.up()
	waitSame(t, a, b)
	if k := queryInt(t, a.conn(), "SELECT count(*) FROM items WHERE id BETWEEN 1000 AND 1049"); k != 0 {
		t.Fatalf("old main still holds %d rows the new main never had", k)
	}
	a.stop()
	b.stop()
}

// Without the fix the copy comes back as a primary on timeline 1 (the
// mechanism of P1). Defence in depth: the old main node's rewind against
// it is refused (same timeline) and it is re-cloned instead of looping;
// and a copy started on that diverged data is told apart by
// SameTimelineCheck.
func TestP1SameTimelineDivergence(t *testing.T) {
	a, b := p1State(t)
	b.up()
	if r := queryText(t, b.conn(), "SELECT pg_is_in_recovery()::text"); r != "false" {
		t.Fatalf("set-up: n2 started as is should be a primary (in recovery: %s)", r)
	}
	if tl := queryInt(t, b.conn(), "SELECT timeline_id FROM pg_control_checkpoint()"); tl != 1 {
		t.Fatalf("set-up: n2 started as is should be on timeline 1, got %d", tl)
	}
	insertRange(t, b.conn(), 2000, 2099, "same-timeline-")

	// A copy on the diverged data (what "no rewind required" left before
	// the fix): standby.signal on n1's crashed data, pointed at n2.
	if err := os.WriteFile(filepath.Join(a.pgdata, "standby.signal"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// No upstream: the check reads the WAL n1 replayed from its own disk.
	a.up()
	insertRange(t, b.conn(), 3000, 3099, "more-")
	node, err := pg.NewNode(a.dir)
	if err != nil {
		t.Fatal(err)
	}
	var v pg.SameTimelineVerdict
	waitFor(t, 30*time.Second, "a known same-timeline verdict on n1", func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		v, err = node.SameTimelineCheck(ctx, pg.Upstream{Host: "127.0.0.1", Port: b.port}, secretsOf(t, a))
		if err != nil {
			t.Logf("check: %v", err)
		}
		return err == nil && v.Known
	})
	if !v.Diverged {
		t.Fatalf("same-timeline check says n1 follows n2 (replay %s, timeline %d); their WAL differs", v.Replay, v.TLI)
	}
	a.crash() // as in P1: the old main node's power comes and goes

	a.dev("demote")
	out := a.dev("rewind", "--from", b.addr())
	if !strings.Contains(out, "re-cloned instead") {
		t.Fatalf("old main on the same timeline was not re-cloned:\n%s", out)
	}
	a.up()
	waitSame(t, a, b)
	if k := queryInt(t, a.conn(), "SELECT count(*) FROM items WHERE id BETWEEN 1000 AND 1049"); k != 0 {
		t.Fatalf("old main still holds %d rows the main never had", k)
	}

	// A healthy copy: equal bytes whenever the check can tell.
	next := 4000
	waitFor(t, 30*time.Second, "a known verdict on the re-cloned copy", func() bool {
		insertRange(t, b.conn(), next, next, "healthy-")
		next++
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		v, err = node.SameTimelineCheck(ctx, pg.Upstream{Host: "127.0.0.1", Port: b.port}, secretsOf(t, a))
		return err == nil && v.Known
	})
	if v.Diverged {
		t.Fatalf("same-timeline check says a fresh copy diverged (replay %s)", v.Replay)
	}
	a.stop()
	b.stop()
}
