//go:build integration && unix

package integration

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// occupy holds a TCP port on 127.0.0.1 until the test ends.
func occupy(t *testing.T) (int, net.Listener) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln.Addr().(*net.TCPAddr).Port, ln
}

// A local port that is taken is found before the cluster is contacted:
// the person reads which port and what to do, the command exits non-zero
// at once, nothing joined (the invite still works), nothing is left on
// disk that would make the next attempt behave differently.
func TestM5JoinWithTakenPortJoinsNothing(t *testing.T) {
	root := t.TempDir()
	a := newCNode(t, root, "node-a")
	a.bootstrap()
	itemsOn(t, a.conn())
	code := a.invite()

	for _, which := range []string{"control", "client", "cluster", "postgres"} {
		b := newCNode(t, root, "node-b-"+which)
		port, _ := occupy(t)
		switch which {
		case "control":
			b.ctl = port
		case "client":
			b.client = port
		case "cluster":
			b.listen = port
		case "postgres":
			b.port = port
		}
		start := time.Now()
		out, err := runCoyoteDB(b.args("join", code)...)
		took := time.Since(start)
		if err == nil || !strings.Contains(out, strconv.Itoa(port)) || !strings.Contains(strings.ToLower(out), "in use") {
			t.Fatalf("%s port taken: %v after %s\n%s", which, err, took, out)
		}
		if took > 20*time.Second {
			t.Fatalf("%s port taken: took %s to fail", which, took)
		}
		if _, err := os.Stat(filepath.Join(b.dir, "cluster", "identity.json")); err == nil {
			t.Fatalf("%s port taken: the node joined anyway", which)
		}
		if n := len(a.clusterStatus().Nodes); n != 1 {
			t.Fatalf("%s port taken: the cluster has %d members", which, n)
		}
	}
	// The invite was not used up; a free set of ports joins.
	b := newCNode(t, root, "node-b")
	b.launch("join", code)
	b.waitOut("Following ", 3*time.Minute)
	stopAll(b, a)
}

// A failure after the node became a member (here: the agent is killed
// right after the join, before Postgres is copied) resumes cleanly with
// the same command.
func TestM5JoinResumesAfterFailureAsMember(t *testing.T) {
	root := t.TempDir()
	a := newCNode(t, root, "node-a")
	a.bootstrap()
	itemsOn(t, a.conn())
	code := a.invite()
	b := newCNode(t, root, "node-b")
	b.launch("join", code)
	b.waitOut("Joined.", 2*time.Minute)
	b.killAgent()
	waitNoNodeProcs(t, b.dir, 15*time.Second)
	if _, err := os.Stat(filepath.Join(b.dir, "cluster", "identity.json")); err != nil {
		t.Fatalf("not a member after the join: %v", err)
	}
	b.launch("join", code)
	b.waitOut("Following ", 3*time.Minute)
	insertRange(t, a.conn(), 3000, 3010, "resumed-")
	waitSame(t, a.pgNode, b.pgNode)
	stopAll(b, a)
}
