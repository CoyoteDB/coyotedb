//go:build integration && unix

// Follow check (pg.Node.FollowCheck): can a running standby stream from
// the current primary without a rewind? Real Postgres, states set up by
// hand with the M2 dev tools so each one is reached every run.
package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"coyotedb.com/coyote/internal/pg"
)

func followCheck(t *testing.T, n, primary *pgNode) bool {
	t.Helper()
	node, err := pg.NewNode(n.dir)
	if err != nil {
		t.Fatal(err)
	}
	s, err := pg.ReadSecretsFile(filepath.Join(n.dir, "cluster.secrets"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ok, err := node.FollowCheck(ctx, pg.Upstream{Host: "127.0.0.1", Port: primary.port}, s)
	if err != nil {
		t.Fatalf("follow check on %s: %v", n.name, err)
	}
	return ok
}

func lsnOf(t *testing.T, n *pgNode, sql string) pg.LSN {
	t.Helper()
	l, err := pg.ParseLSN(queryText(t, n.conn(), sql))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// switchPoint reads where the primary's history left timeline tli.
func switchPoint(t *testing.T, primary *pgNode, tli uint32) pg.LSN {
	t.Helper()
	var b []byte
	name := fmt.Sprintf("%08X.history", tli+1)
	if err := primary.conn().QueryRow(context.Background(), "SELECT pg_read_binary_file($1)", "pg_wal/"+name).Scan(&b); err != nil {
		t.Fatal(err)
	}
	h, err := pg.ParseHistory(string(b))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range h {
		if e.TLI == tli {
			return e.SwitchLSN
		}
	}
	t.Fatalf("timeline %d not in %s: %q", tli, name, b)
	return 0
}

// A former primary rewound from the new one replays the new timeline
// from the WAL pg_rewind copied, before its WAL receiver ever reports the
// new timeline. Its last restartpoint is still on the old timeline. It
// holds nothing the primary lacks, so the check must say "can follow".
// (The bug seen in TestM4PlannedSwitch: the timeline was read from the
// old restartpoint and the position from the new timeline.)
func TestFollowCheckRewoundOldPrimary(t *testing.T) {
	root := t.TempDir()
	a := newPrimary(t, root, "n1")
	b := newPGNode(t, root, "n2")
	b.cloneFrom(a)
	waitSame(t, a, b)

	b.dev("cut-stream")
	insertRange(t, a.conn(), 1000, 1049, "unreplicated-")
	a.crash()
	b.dev("promote", "--slots-for", "n1")
	insertRange(t, b.conn(), 2000, 2099, "new-primary-")
	sw := switchPoint(t, b, 1)

	a.dev("demote")
	a.dev("rewind", "--from", b.addr())
	a.up()
	// No WAL receiver from here on: what the copy holds is fixed once
	// replay has reached the end of what it has.
	a.dev("cut-stream")
	var replay pg.LSN
	waitFor(t, 30*time.Second, "n1 replays past the switch point", func() bool {
		replay = lsnOf(t, a, "SELECT pg_last_wal_replay_lsn()::text")
		return replay > sw
	})
	if k := queryInt(t, a.conn(), "SELECT count(*) FROM pg_stat_wal_receiver"); k != 0 {
		t.Fatalf("n1 still has a WAL receiver")
	}
	cpTLI := queryInt(t, a.conn(), "SELECT timeline_id FROM pg_control_checkpoint()")
	t.Logf("switch 1->2 at %s; n1 replay %s, restartpoint timeline %d, minimum recovery point %s", sw, replay, cpTLI,
		queryText(t, a.conn(), "SELECT min_recovery_end_timeline || ' ' || min_recovery_end_lsn FROM pg_control_recovery()"))
	if cpTLI != 1 {
		t.Fatalf("set-up: want n1's restartpoint still on timeline 1, got %d", cpTLI)
	}

	if !followCheck(t, a, b) {
		t.Fatal("follow check says the rewound copy diverged; it holds only WAL the primary has")
	}
	a.stop()
	b.stop()
}

// A copy that received more from the old primary than the promoted one
// had really holds WAL the primary never had: the check must say so,
// whether its WAL receiver is gone or pointed at the new primary.
func TestFollowCheckCopyAheadOfPromoted(t *testing.T) {
	root := t.TempDir()
	a := newPrimary(t, root, "n1")
	b := newPGNode(t, root, "n2")
	b.cloneFrom(a)
	c := newPGNode(t, root, "n3")
	c.cloneFrom(a)
	d := newPGNode(t, root, "n4")
	d.cloneFrom(a)
	waitSame(t, a, b)
	waitSame(t, a, c)
	waitSame(t, a, d)

	// n4 must hold no more than n2's switch point. Equal table contents
	// or even equal receive positions are not enough: a receive position
	// can end inside a record (at a WAL page boundary, say), and
	// promotion ends the old timeline at the end of the last complete
	// record. So cut n4, write one more row, and cut n2 only once it has
	// replayed past everything n4 received.
	recv := func(n *pgNode) pg.LSN { return lsnOf(t, n, "SELECT pg_last_wal_receive_lsn()::text") }
	d.dev("cut-stream")
	l4 := recv(d)
	insertRange(t, a.conn(), 900, 900, "n2-not-n4-")
	waitFor(t, 30*time.Second, "n2 replays past what n4 received", func() bool {
		return lsnOf(t, b, "SELECT pg_last_wal_replay_lsn()::text") > l4
	})
	b.dev("cut-stream")
	insertRange(t, a.conn(), 1000, 1049, "only-old-primary-and-n3-")
	waitSame(t, a, c)
	// A clean stop, not a crash: n1 is never started again, and a killed
	// postmaster leaves its System V shared memory segment behind (on
	// macOS only 32 may exist).
	a.stop()
	b.dev("promote", "--slots-for", "n1,n3,n4")
	insertRange(t, b.conn(), 2000, 2009, "new-primary-")
	sw := switchPoint(t, b, 1)
	r3 := lsnOf(t, c, "SELECT pg_last_wal_replay_lsn()::text")
	t.Logf("switch 1->2 at %s; n3 replayed %s; n4 received %s", sw, r3, l4)
	if r3 <= sw || l4 > sw {
		t.Fatalf("set-up: want n4 (%s) <= switch point %s < n3 (%s)", l4, sw, r3)
	}

	// n3's stream to the dead old primary is gone.
	c.dev("cut-stream")
	if followCheck(t, c, b) {
		t.Fatal("follow check says n3 can follow, but it holds WAL past the switch point on timeline 1")
	}

	// Point n3 at the new primary. Its WAL receiver can only stream bits
	// of timeline 1 (up to the switch point) before Postgres refuses
	// timeline 2, so `dev set-upstream`, which waits for "streaming",
	// is not used: wait for Postgres' own refusal in n3's server log.
	logOff := serverLogSize(t, c)
	n3, err := pg.NewNode(c.dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- n3.SetUpstream(ctx, pg.Upstream{Host: "127.0.0.1", Port: b.port}, 2*time.Minute) }()
	const refusal = "forked off current database system timeline 1 before current recovery point"
	waitFor(t, 60*time.Second, "n3's Postgres refuses timeline 2", func() bool {
		return strings.Contains(serverLogFrom(t, c, logOff), refusal)
	})
	cancel()
	<-done
	for range 5 {
		if followCheck(t, c, b) {
			t.Fatal("follow check says n3 can follow after being pointed at n2; it diverged")
		}
	}

	// Control: n4 holds a prefix of what n2 had and follows.
	d.dev("set-upstream", "--upstream", b.addr())
	waitSame(t, b, d)
	if !followCheck(t, d, b) {
		t.Fatal("follow check says n4 diverged; it holds only WAL n2 has")
	}
	for _, n := range []*pgNode{c, d, b} {
		n.stop()
	}
}

func serverLogs(t *testing.T, n *pgNode) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(n.dir, "logs", "postgresql-*.log"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(m)
	return m
}

// serverLogSize is the total size of n's Postgres server logs now.
func serverLogSize(t *testing.T, n *pgNode) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, f := range serverLogs(t, n) {
		if fi, err := os.Stat(f); err == nil {
			out[f] = fi.Size()
		}
	}
	return out
}

// serverLogFrom returns what n's Postgres wrote to its server logs since off.
func serverLogFrom(t *testing.T, n *pgNode, off map[string]int64) string {
	t.Helper()
	var sb strings.Builder
	for _, f := range serverLogs(t, n) {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if o := off[f]; o <= int64(len(b)) {
			sb.Write(b[o:])
		}
	}
	return sb.String()
}
