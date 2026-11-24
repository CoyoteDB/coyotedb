//go:build integration && unix

package integration

import (
	"context"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// rwWatch polls one node's Postgres on a long-lived connection and
// records when it answered "read-write" (pg_is_in_recovery() = false).
type rwWatch struct {
	mu          sync.Mutex
	first, last time.Time
}

func watchRW(ctx context.Context, cs string, w *rwWatch) {
	var c *pgx.Conn
	defer func() {
		if c != nil {
			_ = c.Close(context.Background())
		}
	}()
	for ctx.Err() == nil {
		if c == nil {
			cctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
			c, _ = pgx.Connect(cctx, cs)
			cancel()
			if c == nil {
				time.Sleep(20 * time.Millisecond) // polling interval
				continue
			}
		}
		qctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		var rec bool
		err := c.QueryRow(qctx, "SELECT pg_is_in_recovery()").Scan(&rec)
		cancel()
		if err != nil {
			_ = c.Close(context.Background())
			c = nil
			continue
		}
		if !rec {
			now := time.Now()
			w.mu.Lock()
			if w.first.IsZero() {
				w.first = now
			}
			w.last = now
			w.mu.Unlock()
		}
		time.Sleep(10 * time.Millisecond) // polling interval
	}
}

func (w *rwWatch) times() (time.Time, time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.first, w.last
}

// F6 (lease review): a planned switch whose old main node's agent is
// frozen (SIGSTOP: Postgres keeps running, the agent cannot stop it) must
// not promote the target until the old main node's lease has run out.
// Before the fix the target was read-write while the old Postgres still
// was too. A healthy switch stays fast.
func TestLeasePlannedSwitchWaitsForOldMain(t *testing.T) {
	root := t.TempDir()
	a, b := newCNode(t, root, "node-a"), newCNode(t, root, "node-b")
	w := newWitness(t, root, "wit")
	a.bootstrap()
	itemsOn(t, a.conn())
	b.join(a)
	w.join(a)
	syncFull(t, a, b)
	csA, csB := connString(t, a), connString(t, b)

	ctx, cancel := context.WithCancel(context.Background())
	var wa, wb rwWatch
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); watchRW(ctx, csA, &wa) }()
	go func() { defer wg.Done(); watchRW(ctx, csB, &wb) }()
	defer func() { cancel(); wg.Wait() }()
	waitFor(t, 10*time.Second, "node-a seen read-write", func() bool { f, _ := wa.times(); return !f.IsZero() })

	// node-a bootstrapped, so it leads Raft. The switch must run on a live
	// leader at once (the window is the frozen old main node's remaining
	// lease), so move leadership away: a 3s freeze (well inside the lease's
	// 6s fast-stop point) makes the others elect a new leader.
	pid := a.a.cmd.Process.Pid
	idA := idOf(t, a)
	for try := 0; b.clusterStatus().Cluster.LeaderID == idA; try++ {
		if try == 3 {
			t.Fatal("node-a stays the Raft leader")
		}
		_ = syscall.Kill(pid, syscall.SIGSTOP)
		time.Sleep(3 * time.Second) // the freeze itself
		_ = syscall.Kill(pid, syscall.SIGCONT)
		waitFor(t, 15*time.Second, "a Raft leader", func() bool { return b.clusterStatus().Cluster.LeaderID != "" })
	}
	syncFull(t, a, b)

	// Freeze node-a's agent only: its Postgres and watchdog keep running.
	if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	frozen := true
	defer func() {
		if frozen {
			_ = syscall.Kill(pid, syscall.SIGCONT)
		}
	}()
	start := time.Now()
	out, err := b.cli("switch", "--to", "node-b")
	took := time.Since(start)
	if err != nil || !strings.Contains(out, "node-b is now the main node") {
		t.Fatalf("switch with node-a frozen: %v\n%s", err, out)
	}
	waitFor(t, 30*time.Second, "node-b seen read-write", func() bool { f, _ := wb.times(); return !f.IsZero() })
	_, lastA := wa.times()
	firstB, _ := wb.times()
	t.Logf("switch with the old main node frozen took %s; node-a last read-write %s before node-b first was",
		took.Round(time.Millisecond), firstB.Sub(lastA).Round(time.Millisecond))
	if !lastA.Before(firstB) {
		t.Fatalf("TWO MAIN NODES: node-a was still read-write %s after node-b became read-write", lastA.Sub(firstB).Round(time.Millisecond))
	}

	// node-a wakes up, steps aside and rejoins as a copy.
	_ = syscall.Kill(pid, syscall.SIGCONT)
	frozen = false
	a.waitOut("Following node-b live.", 3*time.Minute)
	syncFull(t, b, a)

	// A healthy switch back: the old main node steps down at once, so the
	// switch does not wait out a lease.
	start = time.Now()
	b.mustCLI("switch", "--to", "node-a")
	if took := time.Since(start); took > 8*time.Second {
		t.Fatalf("a healthy planned switch took %s", took.Round(time.Millisecond))
	} else {
		t.Logf("healthy planned switch took %s", took.Round(time.Millisecond))
	}
	waitFor(t, 2*time.Minute, "node-a main, node-b following it", func() bool { return a.isPrimary() && b.isStreaming() })
	cancel()
	wg.Wait()
	stopAll(a, b, w)
}

// idOf is a node's member id.
func idOf(t *testing.T, n *cnode) string {
	t.Helper()
	s := n.clusterStatus()
	for _, m := range s.Nodes {
		if m.Name == n.name {
			return m.ID
		}
	}
	t.Fatalf("%s not in its own status", n.name)
	return ""
}
