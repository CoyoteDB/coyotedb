//go:build integration && unix

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	coyoteagent "coyotedb.com/coyote/internal/agent"
)

// serviceRun starts `coyote service run` (the background service's entry
// point) in the foreground, as the service manager would.
func (n *cnode) serviceRun() *agent {
	n.t.Helper()
	n.a = launchCoyoteDBEnv(n.t, n.env, "service", "run", "--data-dir", n.dir)
	return n.a
}

func (n *cnode) request(mode, code string) {
	n.t.Helper()
	r := coyoteagent.ServiceRequest{Mode: mode, Invite: code, Name: n.name,
		Listen: "127.0.0.1:" + strconv.Itoa(n.listen), ControlPort: n.ctl}
	if !n.witness {
		r.PGPort, r.ClientPort = n.port, n.client
	}
	b, _ := json.Marshal(r)
	if err := os.MkdirAll(n.dir, 0o700); err != nil {
		n.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(n.dir, coyoteagent.ServiceRequestFile), b, 0o600); err != nil {
		n.t.Fatal(err)
	}
}

// The service entry point never creates a cluster by itself: on a fresh
// data folder it waits for the request `up` / `join` leave, then does
// exactly that (and removes the request, which may hold an invite code).
// A restarted service just resumes.
func TestM5ServiceRunWaitsForRequest(t *testing.T) {
	root := t.TempDir()
	a, b := newCNode(t, root, "node-a"), newCNode(t, root, "node-b")
	a.serviceRun()
	a.waitOut("This machine is not in a cluster yet", 30*time.Second)
	time.Sleep(3 * time.Second)
	if _, err := os.Stat(filepath.Join(a.dir, "cluster", "identity.json")); err == nil {
		t.Fatal("the service created a cluster without being asked")
	}
	a.request("up", "")
	a.waitOut("Press Ctrl-C to stop.", 3*time.Minute)
	if _, err := os.Stat(filepath.Join(a.dir, coyoteagent.ServiceRequestFile)); !os.IsNotExist(err) {
		t.Fatalf("request not removed: %v", err)
	}
	itemsOn(t, a.conn())

	code := a.invite()
	b.serviceRun()
	b.waitOut("This machine is not in a cluster yet", 30*time.Second)
	b.request("join", code)
	waitFor(t, 3*time.Minute, "node-b streaming", b.isStreaming)
	insertRange(t, a.conn(), 1000, 1050, "svc-")
	waitSame(t, a.pgNode, b.pgNode)

	// A restart of the service resumes the member without a request.
	b.a.stop(t)
	waitNoNodeProcs(t, b.dir, 15*time.Second)
	b.serviceRun()
	waitFor(t, 2*time.Minute, "node-b streaming again", b.isStreaming)
	stopAll(a, b)
}
