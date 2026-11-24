//go:build integration && unix && mixed

// Mixed operating systems, Step 2 phase 2 (docs/MIXED_OS.md, D37): real
// agents and real Postgres on one machine, with one node recording another
// operating system (COYOTE_TEST_FAKE_OS) so the cluster treats it as a
// different kind of machine. Its fingerprint and probe are real (same
// machine, so they match): this exercises every gate, refusal and guard
// path; the real cross-machine run is described in docs/MIXED_OS.md.
//
// Separate tag so `make itest` is unchanged:
//
//	go test -tags 'integration mixed' -count=1 -run TestMixed ./test/integration/
package integration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	coyoteagent "coyotedb.com/coyote/internal/agent"
	"coyotedb.com/coyote/internal/pg"
)

const fakeLinux = "COYOTE_TEST_FAKE_OS=linux"

// joinMixed joins n with --allow-mixed-os and waits until it follows.
func (n *cnode) joinMixed(via *cnode) {
	n.t.Helper()
	n.a = launchCoyoteDBEnv(n.t, n.env, append(n.args("join", via.invite()), "--allow-mixed-os")...)
	if !n.a.waitReady(n.t, 5*time.Minute) {
		n.t.Fatalf("%s exited before ready:\n%s", n.name, n.a.out.String())
	}
	n.waitOut("Following ", 2*time.Minute)
}

// joinRefused runs a join that must fail, and returns its output.
func (n *cnode) joinRefused(via *cnode, extra ...string) string {
	n.t.Helper()
	n.a = launchCoyoteDBEnv(n.t, n.env, append(n.args("join", via.invite()), extra...)...)
	select {
	case <-n.a.exited:
	case <-time.After(5 * time.Minute):
		n.t.Fatalf("%s: join did not end:\n%s", n.name, n.a.out.String())
	}
	if n.a.err == nil {
		n.t.Fatalf("%s: join should have been refused:\n%s", n.name, n.a.out.String())
	}
	return n.a.out.String()
}

func member(s coyoteagent.Status, name string) coyoteagent.MemberView {
	for _, m := range s.Nodes {
		if m.Name == name {
			return m
		}
	}
	return coyoteagent.MemberView{}
}

// waitMeasured waits until every data node has a fingerprint and a clean
// audit recorded.
func waitMeasured(t *testing.T, via *cnode, names ...string) {
	t.Helper()
	waitFor(t, 2*time.Minute, "fingerprints and audits recorded", func() bool {
		s := via.clusterStatus()
		for _, n := range names {
			m := member(s, n)
			if m.Fingerprint == "" || m.Guard == nil || m.Guard.AuditAt.IsZero() {
				return false
			}
		}
		return true
	})
}

// mixedPair: a (this OS, main, with aEnv), b (recorded as Linux, joined
// with --allow-mixed-os), witness w, all in sync.
func mixedPair(t *testing.T, aEnv ...string) (a, b, w *cnode) {
	root := t.TempDir()
	a, b, w = newCNode(t, root, "node-a"), newCNode(t, root, "node-b"), newWitness(t, root, "wit")
	a.env = aEnv
	b.env = []string{fakeLinux}
	a.bootstrap()
	if !strings.Contains(a.a.out.String(), "Linux is the recommended system") {
		t.Errorf("no Linux recommendation when creating a cluster on this OS:\n%s", a.a.out.String())
	}
	itemsOn(t, a.conn())
	waitMeasured(t, a, "node-a")
	w.join(a)
	// Without consent: refused with a plain message naming the flag.
	if out := b.joinRefused(a); !strings.Contains(out, "--allow-mixed-os") || !strings.Contains(out, "Linux is the recommended system") {
		t.Fatalf("mixed join without the flag:\n%s", out)
	}
	// D49: a structure that recomputes float maths refuses the join, by name.
	mustExec(t, a.conn(), "CREATE TABLE fm (x float8)")
	mustExec(t, a.conn(), "CREATE INDEX fm_exp ON fm (exp(x))")
	if out := b.joinRefused(a, "--allow-mixed-os"); !strings.Contains(out, "fm_exp") || !strings.Contains(out, "floating-point") {
		t.Fatalf("mixed join with a float-maths index:\n%s", out)
	}
	mustExec(t, a.conn(), "DROP INDEX fm_exp")
	mustExec(t, a.conn(), "CREATE INDEX fm_x ON fm (x)") // a plain index on a float column is fine
	b.joinMixed(a)
	syncFull(t, a, b)
	return a, b, w
}

// Join, failover both ways, planned switch both ways, status.
func TestMixedJoinFailoverSwitch(t *testing.T) {
	a, b, w := mixedPair(t)
	s := a.clusterStatus()
	if !s.Cluster.MixedOS || !strings.HasPrefix(member(s, "node-b").System, "Linux ") || member(s, "node-a").Fingerprint != member(s, "node-b").Fingerprint {
		t.Fatalf("status: mixed %v, systems %q / %q", s.Cluster.MixedOS, member(s, "node-a").System, member(s, "node-b").System)
	}
	a.waitStatusSays(time.Minute, "Mixed systems: on", "node-b: Linux")
	insertRange(t, a.conn(), 101, 200, "before-")

	// Failover onto the other kind.
	a.killAgent()
	waitFor(t, 90*time.Second, "node-b main", func() bool { return b.isPrimary() })
	if got := queryInt(t, b.conn(), "SELECT count(*) FROM items"); got != 200 {
		t.Fatalf("node-b has %d rows, want 200", got)
	}
	insertRange(t, b.conn(), 201, 250, "on-b-")
	a.restart()
	a.waitOut("Following node-b live.", 3*time.Minute)
	waitAllSame(t, b, a)
	syncFull(t, b, a)

	// Failover back.
	b.killAgent()
	waitFor(t, 90*time.Second, "node-a main", func() bool { return a.isPrimary() })
	if got := queryInt(t, a.conn(), "SELECT count(*) FROM items"); got != 250 {
		t.Fatalf("node-a has %d rows, want 250", got)
	}
	b.restart()
	b.waitOut("Following node-a live.", 3*time.Minute)
	syncFull(t, a, b)

	// Planned switches between kinds: allowed (fingerprints match, guards clean).
	if out := a.mustCLI("switch", "--to", "node-b"); !strings.Contains(out, "node-b is now the main node") {
		t.Fatalf("switch to node-b: %s", out)
	}
	a.waitOut("Following node-b live.", 3*time.Minute)
	syncFull(t, b, a)
	b.mustCLI("switch", "--to", "node-a")
	waitFor(t, 2*time.Minute, "node-a main again", func() bool { return a.isPrimary() && b.isStreaming() })
	waitAllSame(t, a, b)
	stopAll(a, b, w)
}

// A guard failing after failover (D37, D48, D49): after a clean failover
// onto the other kind, float maths in an index is refused at DDL time; a
// database made with ICU rules (which event triggers cannot see) makes the
// guards block. Status shows it, a planned switch back across kinds and a
// new mixed join are refused, and when the main node dies right away (no
// waiting for a guard tick) the failover onto the other kind does not
// happen: the copy checks at promotion time and refuses, the cluster
// waits, no row is lost. Once the database is gone, failover across kinds
// works again and the next joiner gets in.
func TestMixedGuardFailsAfterFailover(t *testing.T) {
	a, b, w := mixedPair(t)
	insertRange(t, a.conn(), 101, 150, "before-")
	a.killAgent()
	waitFor(t, 90*time.Second, "node-b main", func() bool { return b.isPrimary() })
	a.restart()
	a.waitOut("Following node-b live.", 3*time.Minute)
	syncFull(t, b, a)

	mustExec(t, b.conn(), "CREATE TABLE f (x float8)")
	if _, err := b.conn().Exec(context.Background(), "CREATE INDEX f_exp ON f (exp(x))"); err == nil || !strings.Contains(err.Error(), "floating-point") {
		t.Fatalf("a float-maths index must be refused at once in a mixed cluster, got %v", err)
	}
	mustExec(t, b.conn(), "CREATE DATABASE icudb LOCALE_PROVIDER icu ICU_LOCALE 'de' TEMPLATE template0")
	waitFor(t, 30*time.Second, "node-a has replayed the new database", func() bool {
		return queryInt(t, a.conn(), "SELECT count(*) FROM pg_database WHERE datname = 'icudb'") == 1
	})
	b.killAgent() // at once: the guards have not necessarily run yet
	for i := 0; i < 60; i++ {
		if a.isPrimary() {
			t.Fatal("failover onto the other kind happened with an ICU database")
		}
		time.Sleep(time.Second) // sampling the outcome over 60 s (several controller rounds)
	}
	if !strings.Contains(a.a.out.String()+readLog(t, a), "may not take over") {
		t.Errorf("node-a does not say why it refused:\n%s", a.a.out.String())
	}
	b.restart()
	waitFor(t, 3*time.Minute, "node-b main again", func() bool { return b.isPrimary() && a.isStreaming() })
	if got := queryInt(t, b.conn(), "SELECT count(*) FROM items"); got != 150 {
		t.Fatalf("node-b has %d rows, want 150", got)
	}
	out := b.waitStatusSays(2*time.Minute, "cannot be shared between kinds of machines", "icudb")
	t.Logf("status with the finding:\n%s", out)
	if out, err := b.cli("switch", "--to", "node-a"); err == nil || !strings.Contains(out, "reports a problem") {
		t.Fatalf("cross-kind switch with a blocked guard: %v\n%s", err, out)
	}
	c := newCNode(t, t.TempDir(), "node-c")
	c.env = []string{fakeLinux}
	if out := c.joinRefused(b, "--allow-mixed-os"); !strings.Contains(out, "cannot take a different kind of computer right now") && !strings.Contains(out, "icudb") {
		t.Fatalf("mixed join with a blocked guard:\n%s", out)
	}

	mustExec(t, b.conn(), "DROP DATABASE icudb")
	waitFor(t, 3*time.Minute, "guards clean again", func() bool {
		s := b.clusterStatus()
		for _, n := range []string{"node-a", "node-b"} {
			if g := member(s, n).Guard; g == nil || g.AuditBlocking {
				return false
			}
		}
		return true
	})
	insertRange(t, b.conn(), 151, 160, "after-")
	waitAllSame(t, b, a)
	b.killAgent()
	waitFor(t, 2*time.Minute, "node-a main", func() bool { return a.isPrimary() })
	if got := queryInt(t, a.conn(), "SELECT count(*) FROM items"); got != 160 {
		t.Fatalf("node-a has %d rows, want 160", got)
	}
	b.restart()
	b.waitOut("Following node-a live.", 3*time.Minute)
	d := newCNode(t, t.TempDir(), "node-d")
	d.env = []string{fakeLinux}
	d.joinMixed(a)
	waitAllSame(t, a, b, d)
	stopAll(d, a, b, w)
}

// A main node whose own measurement drifts (D48): it never keeps serving
// writes silently. With its only copy down it refuses writes at once (reads
// still work); when the copy is back it hands over by a planned switch, then
// stops as a drifted copy and waits for a fresh copy; no row is lost.
func TestMixedDriftedMainHandsOver(t *testing.T) {
	salt := filepath.Join(t.TempDir(), "salt")
	if err := os.WriteFile(salt, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	a, b, w := mixedPair(t, "COYOTE_TEST_FINGERPRINT_SALT_FILE="+salt)
	a.settings("lease_duration=30s") // the restart below must not race a failover
	insertRange(t, a.conn(), 101, 150, "before-")
	waitAllSame(t, a, b)
	b.a.stop(t)

	// The "OS update" on the main node, picked up when its Postgres restarts.
	if err := os.WriteFile(salt, []byte("os-update"), 0o600); err != nil {
		t.Fatal(err)
	}
	pf, err := pg.ReadPidFile(a.pgdata)
	if err != nil || pf == nil {
		t.Fatalf("no postmaster.pid: %v", err)
	}
	if err := syscall.Kill(pf.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	// Polled with SHOW, not a write: with the only copy down a write
	// could wait for it (rule 9) or, after a degrade, be kept.
	waitFor(t, 2*time.Minute, "writes refused on the drifted main node", func() bool {
		c, err := a.tryConn()
		if err != nil {
			return false
		}
		defer c.Close(context.Background())
		var ro string
		return c.QueryRow(context.Background(), "SHOW default_transaction_read_only").Scan(&ro) == nil && ro == "on"
	})
	{
		c, err := a.tryConn()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err = c.Exec(ctx, "INSERT INTO items (id, body) VALUES (999, 'x')")
		cancel()
		c.Close(context.Background())
		if err == nil || !strings.Contains(err.Error(), "read-only") {
			t.Fatalf("a write on the drifted main node must be refused, got %v", err)
		}
	}
	if !a.isPrimary() {
		t.Fatal("node-a should still be the main node (no failover raced the restart)")
	}
	if got := queryInt(t, a.conn(), "SELECT count(*) FROM items"); got != 150 {
		t.Fatalf("reads on the drifted main: %d rows, want 150", got)
	}
	a.waitStatusSays(time.Minute, "node-a: This machine's database programs or behaviour changed")

	// The copy comes back: the drifted main hands over to it.
	b.restart()
	waitFor(t, 5*time.Minute, "node-b main (hand-over)", func() bool { return b.isPrimary() })
	if got := queryInt(t, b.conn(), "SELECT count(*) FROM items"); got != 150 {
		t.Fatalf("node-b has %d rows, want 150", got)
	}
	waitFor(t, 3*time.Minute, "node-a marked for a fresh copy", func() bool {
		m := member(b.clusterStatus(), "node-a")
		return m.Status == "needs_reclone" && !m.InSyncSet
	})
	insertRange(t, b.conn(), 151, 160, "after-")
	if !strings.Contains(a.a.out.String()+readLog(t, a), "handing over") {
		t.Errorf("node-a did not log the hand-over")
	}
	// Undo the update: node-a copies the data again and follows.
	if err := os.WriteFile(salt, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	a.a.stop(t)
	a.restart()
	a.waitOut("Following node-b live.", 5*time.Minute)
	waitAllSame(t, b, a)
	stopAll(a, b, w)
}

// A fingerprint that changes mid-life (an OS update), on a copy of a main
// node of another kind: the cluster refuses its new record, the copy stops
// following, leaves the sync set the usual way and is marked for a fresh
// copy; it is then never promoted: the main node dies and the other copy
// takes over. Undoing the change brings it back (fresh copy).
func TestMixedDriftedCopyNeverPromoted(t *testing.T) {
	a, b, w := mixedPair(t)
	c := newCNode(t, t.TempDir(), "node-c")
	c.env = []string{fakeLinux}
	c.joinMixed(a)
	syncFull(t, a, b, c)
	insertRange(t, a.conn(), 101, 150, "before-")

	b.a.stop(t)
	b.env = []string{fakeLinux, "COYOTE_TEST_FINGERPRINT_SALT=os-update"}
	b.launch("up", "")
	b.waitOut("no longer matches", 3*time.Minute)
	waitFor(t, 2*time.Minute, "node-b out of the sync set and marked for a fresh copy", func() bool {
		s := a.clusterStatus()
		m := member(s, "node-b")
		return !m.InSyncSet && m.Status == "needs_reclone" && m.Guard != nil && m.Guard.Drift != ""
	})
	a.waitStatusSays(time.Minute, "node-b: This machine's database programs or behaviour changed")
	// New mixed joins are refused while a member is drifted.
	e := newCNode(t, t.TempDir(), "node-e")
	e.env = []string{fakeLinux}
	if out := e.joinRefused(a, "--allow-mixed-os"); !strings.Contains(out, "cannot take a different kind") {
		t.Fatalf("mixed join with a drifted member:\n%s", out)
	}
	syncFull(t, a, c)

	a.killAgent()
	waitFor(t, 90*time.Second, "node-c main", func() bool { return c.isPrimary() })
	for i := 0; i < 20; i++ { // node-b never becomes main, also later
		if b.isPrimary() || c.clusterStatus().Cluster.PrimaryName == "node-b" {
			t.Fatal("the drifted copy was promoted")
		}
		time.Sleep(500 * time.Millisecond) // sampling the outcome over 10 s
	}
	if got := queryInt(t, c.conn(), "SELECT count(*) FROM items"); got != 150 {
		t.Fatalf("node-c has %d rows, want 150", got)
	}
	// Undo the update: node-b copies the data again and follows.
	b.a.stop(t)
	b.env = []string{fakeLinux}
	b.restart()
	b.waitOut("Following node-c live.", 5*time.Minute)
	waitAllSame(t, c, b)
	waitFor(t, 2*time.Minute, "drift cleared", func() bool {
		m := member(c.clusterStatus(), "node-b")
		return m.Status == "active" && m.Guard != nil && m.Guard.Drift == ""
	})
	stopAll(a, b, c, w)
}

// A mixed join racing a failover: the main node is killed while a node of
// another kind joins. The join either completes against the new main node
// or is refused in a way that a retry fixes; never two main nodes, no row
// lost.
func TestMixedJoinRacingFailover(t *testing.T) {
	root := t.TempDir()
	a, b, w := newCNode(t, root, "node-a"), newCNode(t, root, "node-b"), newWitness(t, root, "wit")
	a.bootstrap()
	itemsOn(t, a.conn())
	b.join(a)
	w.join(a)
	syncFull(t, a, b)
	waitMeasured(t, a, "node-a", "node-b")
	c := newCNode(t, root, "node-c")
	c.env = []string{fakeLinux}
	code := a.invite()
	c.a = launchCoyoteDBEnv(t, c.env, append(c.args("join", code), "--allow-mixed-os")...)
	c.waitOut("Joining the cluster", 2*time.Minute)
	a.killAgent()
	waitFor(t, 90*time.Second, "node-b main", func() bool { return b.isPrimary() })
	joined := false
	deadline := time.Now().Add(5 * time.Minute)
	for !joined && time.Now().Before(deadline) {
		if strings.Contains(c.a.out.String(), "Following node-b live.") {
			joined = true
			break
		}
		select {
		case <-c.a.exited:
			t.Logf("join ended during the failover (retrying):\n%s", c.a.out.String())
			c.a = launchCoyoteDBEnv(t, c.env, append(c.args("join", code), "--allow-mixed-os")...)
		case <-time.After(200 * time.Millisecond):
		}
		if b.isPrimary() && c.isPrimary() {
			t.Fatal("two main nodes")
		}
	}
	if !joined {
		t.Fatalf("node-c never followed node-b:\n%s", c.a.out.String())
	}
	if got := queryInt(t, b.conn(), "SELECT count(*) FROM items"); got != 100 {
		t.Fatalf("rows %d, want 100", got)
	}
	waitAllSame(t, b, c)
	s := b.clusterStatus()
	if !s.Cluster.MixedOS || !slices.ContainsFunc(s.Nodes, func(m coyoteagent.MemberView) bool { return m.Name == "node-c" && strings.HasPrefix(m.System, "Linux") }) {
		t.Fatalf("status after racing join: %+v", s.Cluster)
	}
	stopAll(b, c, w)
}

// tryConn is conn without failing the test (Postgres may be restarting).
func (n *cnode) tryConn() (*pgx.Conn, error) {
	out, err := exec.Command(coyoteBin, "conninfo", "--admin", "--data-dir", n.dir).Output()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return pgx.Connect(ctx, strings.TrimSpace(string(out)))
}

// readLog returns n's agent log.
func readLog(t *testing.T, n *cnode) string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(n.dir, "logs", "coyote.log"))
	return string(b)
}
