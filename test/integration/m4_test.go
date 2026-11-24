//go:build integration

// Integration tests for milestone M4: the controller. Real agents and
// Postgres on localhost: automatic failover, planned switch, degrade and
// re-eligibility, strict mode, "needs re-clone", the two-node rule, and
// what `status` says in each state. The lease math itself is unit-tested
// with a fake clock (internal/controller).
package integration

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	coyoteagent "coyotedb.com/coyote/internal/agent"
)

// killAgent kill -9s only the agent: its watchdog must stop Postgres.
func (n *cnode) killAgent() {
	n.t.Helper()
	_ = n.a.cmd.Process.Kill()
	<-n.a.exited
	waitNoNodeProcs(n.t, n.dir, 15*time.Second)
}

func (n *cnode) settings(kv ...string) {
	n.t.Helper()
	n.mustCLI(append([]string{"settings", "set"}, kv...)...)
}

// waitStatusSays waits until `status` (human) on n contains every s.
func (n *cnode) waitStatusSays(within time.Duration, s ...string) string {
	n.t.Helper()
	var out string
	waitFor(n.t, within, "status to say "+strings.Join(s, " / "), func() bool {
		var err error
		out, err = n.cli("status")
		if err != nil {
			return false
		}
		for _, x := range s {
			if !strings.Contains(out, x) {
				return false
			}
		}
		return true
	})
	return out
}

// commitAsync runs one INSERT on n and reports when it finishes.
func commitAsync(t *testing.T, n *cnode, id int) <-chan error {
	t.Helper()
	cs := connString(t, n)
	done := make(chan error, 1)
	go func() {
		c, err := pgx.Connect(context.Background(), cs)
		if err != nil {
			done <- err
			return
		}
		defer c.Close(context.Background())
		_, err = c.Exec(context.Background(), "INSERT INTO items VALUES ($1, 'm4')", id)
		done <- err
	}()
	return done
}

// syncFull waits until every node in copies is a synchronous copy of p
// and copies are eligible.
func syncFull(t *testing.T, p *cnode, copies ...*cnode) {
	t.Helper()
	waitFor(t, 90*time.Second, "every copy in the sync set and eligible", func() bool {
		s := p.clusterStatus()
		if !s.Cluster.StandbyEligible || s.Controller == nil || !s.Controller.View.LeaseHealthy {
			return false
		}
		for _, c := range copies {
			ok := false
			for _, m := range s.Nodes {
				if m.Name == c.name && m.InSyncSet && m.Replication == "streaming (sync)" {
					ok = true
				}
			}
			if !ok {
				return false
			}
		}
		return true
	})
}

func primaryOf(t *testing.T, nodes ...*cnode) *cnode {
	t.Helper()
	for _, n := range nodes {
		if n.a != nil && n.isPrimary() {
			return n
		}
	}
	return nil
}

func TestM4AutomaticFailover(t *testing.T) {
	root := t.TempDir()
	a, b, c := newCNode(t, root, "node-a"), newCNode(t, root, "node-b"), newCNode(t, root, "node-c")
	a.bootstrap()
	itemsOn(t, a.conn())
	b.join(a)
	c.join(a)
	syncFull(t, a, b, c)
	a.waitStatusSays(30*time.Second, "Main node lease: healthy", "Writes wait for a copy on")
	insertRange(t, a.conn(), 101, 200, "before-")

	start := time.Now()
	a.killAgent()
	var p *cnode
	waitFor(t, 60*time.Second, "a new main node", func() bool {
		p = primaryOf(t, b, c)
		return p != nil && p.clusterStatus().Cluster.PrimaryName == p.name
	})
	took := time.Since(start)
	t.Logf("failover took %s", took.Round(time.Millisecond))
	if took > 30*time.Second {
		t.Fatalf("failover took %s (budget 30s)", took)
	}
	// Every acknowledged row is there, and writes work (waiting for the
	// remaining copy).
	cp := p.conn()
	if got := queryInt(t, cp, "SELECT count(*) FROM items"); got != 200 {
		t.Fatalf("new main node has %d rows, want 200", got)
	}
	insertRange(t, cp, 201, 250, "after-")

	// The old primary rejoins as a copy by itself; it never runs read-write.
	starts := primaryStarts(t, a.pgNode)
	a.restart()
	a.waitOut("Following "+p.name+" live.", 2*time.Minute)
	other := b
	if p == b {
		other = c
	}
	waitAllSame(t, p, a, other)
	if primaryStarts(t, a.pgNode) != starts {
		t.Fatal("the old main node started read-write")
	}
	if m := a.marker(); m != nil {
		t.Fatalf("marker still there after rejoining: %+v", m)
	}
	syncFull(t, p, a, other)
	var js coyoteagent.Status
	p.cliJSON(&js, "status", "--json")
	if js.Health.MainNode != p.name || js.Health.Lease != "renewed" || !js.Health.WritesWait || js.Health.OneCopy {
		t.Fatalf("status --json health: %+v", js.Health)
	}
	stopAll(a, b, c)
}

func TestM4PlannedSwitch(t *testing.T) {
	root := t.TempDir()
	a, b := newCNode(t, root, "node-a"), newCNode(t, root, "node-b")
	w := newWitness(t, root, "wit")
	a.bootstrap()
	itemsOn(t, a.conn())
	b.join(a)
	w.join(a)
	syncFull(t, a, b)

	if out, err := a.cli("switch", "--to", "wit"); err == nil || !strings.Contains(out, "witness") {
		t.Fatalf("switch to a witness: %v %s", err, out)
	}
	out := a.mustCLI("switch", "--to", "node-b")
	if !strings.Contains(out, "node-b is now the main node") {
		t.Fatalf("switch: %s", out)
	}
	if !b.isPrimary() {
		t.Fatal("node-b is not read-write")
	}
	// Writes wait for node-a (put back into the sync set) while it rejoins.
	a.waitOut("Following node-b live.", 2*time.Minute)
	insertRange(t, b.conn(), 101, 120, "after-switch-")
	waitSame(t, a.pgNode, b.pgNode)
	syncFull(t, b, a)
	// And back.
	b.mustCLI("switch", "--to", "node-a")
	waitFor(t, 2*time.Minute, "node-a main, node-b following it", func() bool { return a.isPrimary() && b.isStreaming() })
	waitSame(t, a.pgNode, b.pgNode)
	stopAll(a, b, w)
}

func TestM4DegradeThenEligibleAgain(t *testing.T) {
	root := t.TempDir()
	a, b := newCNode(t, root, "node-a"), newCNode(t, root, "node-b")
	w := newWitness(t, root, "wit")
	a.bootstrap()
	itemsOn(t, a.conn())
	b.join(a)
	w.join(a)
	syncFull(t, a, b)
	a.settings("degrade_after=4s")

	// Taken before the kill: the controller's degrade timer starts when
	// node-b's stream drops, which is after this and before killAgent
	// returns (it waits for every process of node-b to be gone).
	lost := time.Now()
	b.killAgent()
	done := commitAsync(t, a, 1001)
	select {
	case err := <-done:
		t.Fatalf("commit returned at once without the copy: %v", err)
	case <-time.After(2 * time.Second):
	}
	a.waitStatusSays(10*time.Second, "Writes are waiting")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("writes still wait long after degrade_after")
	}
	if d := time.Since(lost); d < 4*time.Second {
		t.Fatalf("degraded after %s, before degrade_after", d)
	}
	s := a.clusterStatus()
	if s.Cluster.StandbyEligible {
		t.Fatal("standby_eligible still true after degrading")
	}
	a.waitStatusSays(15*time.Second, "Running on one copy since")
	// While running on one copy, a planned switch is refused.
	if out, err := a.cli("switch", "--to", "node-b"); err == nil || !strings.Contains(out, "one copy") {
		t.Fatalf("switch while on one copy: %v %s", err, out)
	}

	// The copy comes back: eligible again only once caught up, and writes
	// wait for it again.
	b.restart()
	syncFull(t, a, b)
	waitSame(t, a.pgNode, b.pgNode)
	a.settings("degrade_after=1h")
	b.killAgent()
	done = commitAsync(t, a, 1002)
	select {
	case err := <-done:
		t.Fatalf("commit did not wait for the copy once eligible again: %v", err)
	case <-time.After(5 * time.Second):
	}
	b.restart()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	stopAll(a, b, w)
}

func TestM4StrictNeverDegrades(t *testing.T) {
	root := t.TempDir()
	a, b := newCNode(t, root, "node-a"), newCNode(t, root, "node-b")
	w := newWitness(t, root, "wit")
	a.bootstrap()
	itemsOn(t, a.conn())
	b.join(a)
	w.join(a)
	syncFull(t, a, b)
	a.settings("single_data_node_mode=strict", "degrade_after=2s")
	a.waitStatusSays(10*time.Second, "Mode: strict")

	b.killAgent()
	done := commitAsync(t, a, 1001)
	select {
	case err := <-done:
		t.Fatalf("strict: the commit returned without the copy: %v", err)
	case <-time.After(12 * time.Second):
	}
	if !a.clusterStatus().Cluster.StandbyEligible {
		t.Fatal("strict mode degraded")
	}
	a.waitStatusSays(10*time.Second, "Writes are waiting", "strict")
	b.restart()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	stopAll(a, b, w)
}

func TestM4NeedsReclone(t *testing.T) {
	root := t.TempDir()
	a, b, c := newCNode(t, root, "node-a"), newCNode(t, root, "node-b"), newCNode(t, root, "node-c")
	a.bootstrap()
	itemsOn(t, a.conn())
	b.join(a)
	c.join(a)
	syncFull(t, a, b, c)
	a.settings("gone_after=15s")

	c.a.stop(t)
	waitFor(t, 90*time.Second, "node-c marked needs re-clone and out of the sync set", func() bool {
		for _, m := range a.clusterStatus().Nodes {
			if m.Name == "node-c" {
				return m.Status == "needs_reclone" && !m.InSyncSet
			}
		}
		return false
	})
	ca := a.conn()
	waitFor(t, 30*time.Second, "node-c's slot dropped", func() bool {
		return queryInt(t, ca, "SELECT count(*) FROM pg_replication_slots WHERE slot_name = 'coyote_node_c'") == 0
	})
	insertRange(t, ca, 101, 150, "while-away-")
	a.settings("gone_after=30m")

	c.restart()
	c.waitOut("copying the data again", time.Minute)
	waitFor(t, 2*time.Minute, "node-c active again", func() bool {
		for _, m := range a.clusterStatus().Nodes {
			if m.Name == "node-c" {
				return m.Status == "active"
			}
		}
		return false
	})
	syncFull(t, a, b, c)
	waitAllSame(t, a, b, c)
	stopAll(a, b, c)
}

func TestM4TwoNodesNoWitnessNoFailover(t *testing.T) {
	root := t.TempDir()
	a, b := newCNode(t, root, "node-a"), newCNode(t, root, "node-b")
	a.bootstrap()
	itemsOn(t, a.conn())
	b.join(a)
	syncFull(t, a, b)
	out := a.waitStatusSays(20*time.Second, "no witness")
	if !strings.Contains(out, "Automatic failover:") {
		t.Fatalf("status:\n%s", out)
	}
	epoch := b.clusterStatus().Cluster.Epoch

	// node-a is the only voter (D17): with it gone nothing takes over.
	a.killAgent()
	// No failover can start without a leader. Wait until node-b has gone
	// without any quorum contact for twice what a failover needs (lease +
	// margin, 7s with the defaults), checking all along that it never
	// started read-write and its term never moved.
	const failoverNeeds = 7 * time.Second
	waitFor(t, 90*time.Second, "node-b without a quorum for twice a failover's wait", func() bool {
		r, ok := b.tryReport()
		if !ok {
			return false
		}
		if (r.PG != nil && r.PG.Role == "primary") || r.Epoch != epoch {
			t.Fatalf("node-b took over by itself: %+v", r)
		}
		return r.QuorumContactAgoMS != nil && time.Duration(*r.QuorumContactAgoMS)*time.Millisecond > 2*failoverNeeds
	})
	if b.isPrimary() {
		t.Fatal("node-b became the main node by itself")
	}
	a.restart()
	waitFor(t, 60*time.Second, "node-a main again, node-b following", func() bool { return a.isPrimary() && b.isStreaming() })
	if e := a.clusterStatus().Cluster.Epoch; e != epoch {
		t.Fatalf("term changed %d -> %d", epoch, e)
	}
	syncFull(t, a, b)
	stopAll(a, b)
}

// Per-machine settings (MIXED_OS.md A3): coyote.conf always sets
// wal_sync_method (fsync_writethrough on macOS, fsync on Windows: D44),
// dynamic_shared_memory_type and jit=off, and a value another node put in
// postgresql.auto.conf never carries over through a clone or a restart.
func TestM4PerMachineSettings(t *testing.T) {
	root := t.TempDir()
	a, b := newCNode(t, root, "node-a"), newCNode(t, root, "node-b")
	a.bootstrap()
	ca := a.conn()
	mustExec(t, ca, "ALTER SYSTEM SET wal_sync_method = 'open_datasync'")
	mustExec(t, ca, "ALTER SYSTEM SET jit = on")
	// A value from another OS ('sysv' is Unix only, so ALTER SYSTEM would
	// refuse it on Windows): written into the file as a copied data folder
	// would bring it.
	autoPath := filepath.Join(a.pgdata, "postgresql.auto.conf")
	autoB, err := os.ReadFile(autoPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(autoPath, append(autoB, "dynamic_shared_memory_type = 'sysv'\n"...), 0o600); err != nil {
		t.Fatal(err)
	}
	b.join(a)
	wantSync := map[string]string{"darwin": "fsync_writethrough", "windows": "fsync", "linux": "fdatasync"}[runtime.GOOS]
	wantDSM := map[string]string{"windows": "windows"}[runtime.GOOS]
	if wantDSM == "" {
		wantDSM = "posix"
	}
	check := func(n *cnode) {
		t.Helper()
		c := n.conn()
		for k, want := range map[string]string{"wal_sync_method": wantSync, "jit": "off", "dynamic_shared_memory_type": wantDSM} {
			if got := queryText(t, c, "SHOW "+k); got != want {
				t.Fatalf("%s: %s = %s, want %s", n.name, k, got, want)
			}
		}
		auto, err := os.ReadFile(filepath.Join(n.pgdata, "postgresql.auto.conf"))
		if err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"wal_sync_method", "jit", "dynamic_shared_memory_type"} {
			if strings.Contains(string(auto), k) {
				t.Fatalf("%s: postgresql.auto.conf still sets %s:\n%s", n.name, k, auto)
			}
		}
	}
	check(b)
	a.a.stop(t)
	waitNoNodeProcs(t, a.dir, 15*time.Second)
	a.restart()
	check(a)
	stopAll(b, a)
}
