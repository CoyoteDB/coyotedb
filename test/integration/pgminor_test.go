//go:build integration

// PostgreSQL minor update (D70, safety rule 1): real agents and real
// PostgreSQL on one machine. The cluster starts on the earlier pinned
// release (COYOTE_TEST_PG_BUILD, as an older CoyoteDB release would run
// it) and is updated one computer at a time to the current one, in the
// documented order (ARCHITECTURE "Updating a cluster to a new release").
package integration

import (
	"context"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"coyotedb.com/coyote/internal/pg"
)

// earlierPG is the newest earlier pinned release of this platform.
func earlierPG(t *testing.T) string {
	t.Helper()
	cur, err := pg.CurrentBuild()
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range pg.KnownBuilds() {
		if b.Version != pg.Version && b.Target == cur.Target {
			mustRun(t, b)
			return b.Version
		}
	}
	t.Fatalf("no earlier pinned PostgreSQL release for %s", cur.Target)
	return ""
}

// missingLib finds "libxml2.so.2: cannot open shared object file" in a
// failed program's output.
var missingLib = regexp.MustCompile(`([^\s:]+\.so[.0-9]*): cannot open shared object file`)

// mustRun downloads build b into the test cache and checks that its
// postgres starts at all (`postgres -V`), so a build that needs a system
// library this machine lacks fails here, at once, with the library named.
// theseus-rs 18.6.0 for Linux is linked against the system's libxml2,
// libgssapi_krb5, libreadline, libxslt and others; 18.7.0 ships them in
// its own lib folder (docs/TESTING.md "PostgreSQL minor updates").
func mustRun(t *testing.T, b pg.Build) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	inst, err := pg.Ensure(ctx, pg.FetchOptions{CacheDir: pgCache, Build: b})
	if err != nil {
		t.Fatalf("could not get PostgreSQL %s (%s): %v", b.Version, b.Target, err)
	}
	vctx, vcancel := context.WithTimeout(ctx, 30*time.Second)
	defer vcancel()
	out, err := exec.CommandContext(vctx, inst.Bin("postgres"), "-V").CombinedOutput()
	if err == nil {
		return
	}
	why := strings.TrimSpace(string(out))
	if m := missingLib.FindStringSubmatch(why); m != nil {
		t.Fatalf("PostgreSQL %s (%s) does not run on this machine: it needs the system library %s, which is not installed (%s). "+
			"Install it, or run the same roll in containers: TestDockerUpdate with an image on %s (docs/TESTING.md)",
			b.Version, b.Target, m[1], why, b.Version)
	}
	t.Fatalf("PostgreSQL %s (%s) does not run on this machine: postgres -V: %v: %s", b.Version, b.Target, err, why)
}

func pgShort(v string) string { return v[:strings.LastIndexByte(v, '.')] }

// refusedJoin runs a join that must fail and returns its output.
func (n *cnode) refusedJoin(via *cnode) string {
	n.t.Helper()
	n.a = launchCoyoteDBEnv(n.t, n.env, n.args("join", via.invite())...)
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

// waitPG waits until every named data node's record says version and the
// cluster reports no warning.
func waitPG(t *testing.T, via *cnode, version string, names ...string) {
	t.Helper()
	waitFor(t, 3*time.Minute, "records on PostgreSQL "+version+" and no warnings", func() bool {
		s := via.clusterStatus()
		for _, n := range names {
			if memberNamed(s, n).PostgreSQL != version {
				return false
			}
		}
		return len(s.Warnings) == 0
	})
}

func TestPGMinorRollingUpdate(t *testing.T) {
	old := earlierPG(t)
	oldEnv := []string{"COYOTE_TEST_PG_BUILD=" + old}
	oldV, newV := pgShort(old), pgShort(pg.Version)
	root := t.TempDir()
	a, b, c := newCNode(t, root, "node-a"), newCNode(t, root, "node-b"), newCNode(t, root, "node-c")
	w := newWitness(t, root, "wit")
	for _, n := range []*cnode{a, b, c} {
		n.env = oldEnv
	}
	a.bootstrap()
	itemsOn(t, a.conn())
	b.join(a)
	c.join(a)
	w.join(a)
	syncFull(t, a, b, c)
	waitPG(t, a, oldV, "node-a", "node-b", "node-c")

	// 1. The copies, one at a time. Each one's record moves to the new
	// build (no "changed since it joined"), and it follows the old main
	// node (a newer copy may copy an older main node).
	update := func(n *cnode) {
		t.Helper()
		n.a.stop(t)
		n.env = nil
		n.restart()
		n.waitOut("Following node-a live.", 3*time.Minute)
	}
	update(b)
	insertRange(t, a.conn(), 101, 150, "b-updated-")
	waitPG(t, a, newV, "node-b")
	waitPG(t, a, oldV, "node-a", "node-c")
	syncFull(t, a, b, c)

	// Mid-roll: a switch to the updated copy is refused while node-c (not
	// the main node) still runs the older PostgreSQL: it could not copy it.
	if out, err := a.cli("switch", "--to", "node-b"); err == nil || !strings.Contains(out, "Update node-c first") {
		t.Fatalf("switch mid-roll: %v\n%s", err, out)
	}
	// A new computer on the older release is refused: a copy must not be
	// older than any node it could copy.
	late := newCNode(t, root, "node-late")
	late.env = oldEnv
	if out := late.refusedJoin(a); !strings.Contains(out, "a new data node must run the newest PostgreSQL in the cluster") {
		t.Fatalf("older joiner:\n%s", out)
	}

	update(c)
	insertRange(t, a.conn(), 151, 200, "c-updated-")
	waitPG(t, a, newV, "node-b", "node-c")
	syncFull(t, a, b, c)

	// 2. A planned switch to an updated copy. The old main node still
	// runs the older PostgreSQL: it does not copy the new main node, it
	// waits (database stopped) for its update.
	if out := a.mustCLI("switch", "--to", "node-b"); !strings.Contains(out, "node-b is now the main node") || !strings.Contains(out, "node-a runs an older PostgreSQL ("+oldV+")") {
		t.Fatalf("switch: %s", out)
	}
	waitFor(t, 2*time.Minute, "node-b main", func() bool { return b.isPrimary() })
	a.waitOut("A copy must run the same or a newer PostgreSQL than the main node", 2*time.Minute)
	if a.isStreaming() {
		t.Fatal("node-a (older PostgreSQL) copies the newer main node")
	}
	insertRange(t, b.conn(), 201, 250, "after-switch-")

	// 3. The old main node, updated: it rewinds and follows node-b.
	a.a.stop(t)
	a.env = nil
	a.restart()
	a.waitOut("Following node-b live.", 3*time.Minute)
	syncFull(t, b, a, c)
	waitPG(t, b, newV, "node-a", "node-b", "node-c")

	// 4. A new computer on the new release joins.
	d := newCNode(t, root, "node-d")
	d.join(b)
	syncFull(t, b, a, c, d)
	waitPG(t, b, newV, "node-a", "node-b", "node-c", "node-d")
	waitAllSame(t, b, a, c, d)
	if got := queryInt(t, b.conn(), "SELECT count(*) FROM items"); got != 250 {
		t.Fatalf("%d rows, want 250", got)
	}
	stopAll(a, b, c, d, w)
}
