//go:build integration && unix

// A node's address changes (D62).
// "Address" here is what other nodes dial: each node under test is given
// a relay on 127.0.0.1 as its address, and the relay is closed to make
// that address stop answering, the way a computer's old address stops
// answering after the router hands it a new one. What the node detects
// as its own address comes from a file (COYOTE_TEST_ADDR_FILE), read
// again at every detection; everything else is the real program with
// real Postgres.
package integration

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	coyoteagent "coyotedb.com/coyote/internal/agent"
)

// direct is the node's own cluster port (no relay).
func (n *cnode) direct() string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(n.listen)) }

// detects makes the node detect addr as its own address, now and at every
// later detection (rewrite with setDetected).
func (n *cnode) detects(addr string) {
	n.t.Helper()
	p := filepath.Join(filepath.Dir(n.dir), n.name+".addr")
	if err := os.WriteFile(p, []byte(addr), 0o600); err != nil {
		n.t.Fatal(err)
	}
	n.env = append(n.env, "COYOTE_TEST_ADDR_FILE="+p)
}

func (n *cnode) setDetected(addr string) {
	n.t.Helper()
	if err := os.WriteFile(filepath.Join(filepath.Dir(n.dir), n.name+".addr"), []byte(addr), 0o600); err != nil {
		n.t.Fatal(err)
	}
}

// relayed gives n a relay as its address and returns it.
func relayed(t *testing.T, n *cnode) *relay {
	t.Helper()
	r := newRelay(t, n.direct())
	n.detects(net.JoinHostPort("127.0.0.1", strconv.Itoa(r.port())))
	return r
}

// seenAs: viewer's status lists n at exactly addr and reaches it.
func seenAs(viewer, n *cnode, addr string) bool {
	out, err := viewer.cli("status", "--json")
	if err != nil {
		return false
	}
	var s struct {
		Nodes []struct {
			Name      string   `json:"name"`
			Addresses []string `json:"addresses"`
			Reachable bool     `json:"reachable"`
		} `json:"nodes"`
	}
	if json.Unmarshal([]byte(out), &s) != nil {
		return false
	}
	for _, m := range s.Nodes {
		if m.Name == n.name {
			return m.Reachable && slices.Equal(m.Addresses, []string{addr})
		}
	}
	return false
}

// addrProblem waits until viewer's status shows code for node n with
// recorded as the previous address and now as the new one, and returns
// the fix command it names.
func addrProblem(t *testing.T, viewer, n *cnode, code, recorded, now string, within time.Duration) string {
	t.Helper()
	var fix string
	waitFor(t, within, viewer.name+"'s status showing "+code+" for "+n.name, func() bool {
		out, err := viewer.cli("status", "--json")
		if err != nil {
			return false
		}
		var s coyoteagent.Status
		if json.Unmarshal([]byte(out), &s) != nil {
			return false
		}
		for _, p := range s.Problems {
			if p.Code != code || p.Node != n.name || p.Address == nil || !slices.Equal(p.Address.Recorded, []string{recorded}) {
				continue
			}
			if now != "" && !slices.Equal(p.Address.Suggested, []string{now}) {
				continue
			}
			if len(p.Commands) > 0 {
				fix = p.Commands[0]
				return true
			}
		}
		return false
	})
	return fix
}

// noAddrProblem: viewer's status shows no address problem any more.
func noAddrProblem(viewer *cnode) bool {
	out, err := viewer.cli("status", "--json")
	if err != nil {
		return false
	}
	var s coyoteagent.Status
	if json.Unmarshal([]byte(out), &s) != nil {
		return false
	}
	for _, p := range s.Problems {
		if strings.HasPrefix(p.Code, "address_") {
			return false
		}
	}
	return true
}

// runFix runs a fix command status printed ("coyote node set-address ...")
// on n.
func runFix(t *testing.T, n *cnode, fix string) string {
	t.Helper()
	f := strings.Fields(fix)
	if len(f) < 4 || f[1] != "node" || f[2] != "set-address" {
		t.Fatalf("unexpected fix command %q", fix)
	}
	return n.mustCLI(f[1:]...)
}

// D66: a running node whose address changes is not followed by itself.
// Its status, and the others' (from the report it can still send out),
// show address_gone with the previous and the new address and the exact
// command; running that command fixes it. A main node whose address
// changes gives up its lease and another node takes over, never two main
// nodes at once; nothing acknowledged is lost.
func TestAddressGoneToldAndFixed(t *testing.T) {
	root := t.TempDir()
	a, b, c := newCNode(t, root, "node-a"), newCNode(t, root, "node-b"), newCNode(t, root, "node-c")
	ra, rb, rc := relayed(t, a), relayed(t, b), relayed(t, c)
	addrOf := func(r *relay) string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(r.port())) }
	a.bootstrap()
	itemsOn(t, a.conn())
	b.join(a)
	c.join(a)
	syncFull(t, a, b, c)
	if !seenAs(b, c, addrOf(rc)) {
		t.Fatalf("node-c is not recorded at its relay:\n%s", b.mustCLI("status"))
	}

	// A copy's address changes while it runs.
	rc.close()
	c.setDetected(c.direct())
	t0 := time.Now()
	fix := addrProblem(t, c, c, "address_gone", addrOf(rc), c.direct(), 30*time.Second)
	t.Logf("node-c reports address_gone %s after the change", time.Since(t0).Round(100*time.Millisecond))
	if want := "coyote node set-address node-c " + c.direct(); fix != want {
		t.Fatalf("fix command %q, want %q", fix, want)
	}
	// The leader shows it too (node-c told it), and nothing changed by itself.
	addrProblem(t, a, c, "address_gone", addrOf(rc), c.direct(), 30*time.Second)
	if seenAs(a, c, c.direct()) {
		t.Fatal("node-c's address changed in the record by itself")
	}
	text := a.mustCLI("status")
	if !strings.Contains(text, "node-c's address changed: it was "+addrOf(rc)+", it is now "+c.direct()) || !strings.Contains(text, "[address_gone]") || !strings.Contains(text, "Fix: "+fix) {
		t.Fatalf("status text does not tell:\n%s", text)
	}
	if d := a.mustCLI("status", "--details"); !strings.Contains(d, "Evidence:") || !strings.Contains(d, addrOf(rc)) {
		t.Fatalf("status --details:\n%s", d)
	}
	if out := runFix(t, a, fix); !strings.Contains(out, "answers as node-c") {
		t.Fatalf("fix:\n%s", out)
	}
	waitFor(t, 30*time.Second, "node-c reached at its new address and no address problem left", func() bool {
		return seenAs(a, c, c.direct()) && seenAs(b, c, c.direct()) && noAddrProblem(a) && noAddrProblem(c)
	})
	syncFull(t, a, b, c)
	insertRange(t, a.conn(), 1000, 1099, "after-copy-moved-")
	waitAllSame(t, a, b, c)

	// The main node's address changes while it runs. node-b is made the
	// main node while node-a leads Raft (as in TestMainUnreachableInbound).
	a.mustCLI("switch", "--to", "node-b")
	waitFor(t, 90*time.Second, "node-b main with node-a leading Raft", func() bool {
		s := a.clusterStatus()
		return b.isPrimary() && s.Cluster.PrimaryName == "node-b" && s.Cluster.LeaderName == "node-a"
	})
	syncFull(t, b, a, c)
	insertRange(t, b.conn(), 2000, 2099, "on-b-")
	want := queryText(t, b.conn(), tableHash)
	rb.close()
	b.setDetected(b.direct())
	t1 := time.Now()
	var main *cnode
	waitFor(t, 90*time.Second, "another node made the main node", func() bool {
		mains := 0
		for _, n := range []*cnode{a, b, c} {
			if n.isPrimary() {
				mains++
			}
		}
		if mains > 1 {
			t.Fatalf("%d main nodes at once", mains)
		}
		main = primaryOf(t, a, c)
		return main != nil && !b.isPrimary()
	})
	t.Logf("failed over to %s %s after node-b's address changed", main.name, time.Since(t1).Round(100*time.Millisecond))
	if got := queryText(t, main.conn(), tableHash); got != want {
		t.Fatalf("data on the new main node %s differs: %s, want %s", main.name, got, want)
	}
	fix = addrProblem(t, main, b, "address_gone", addrOf(rb), b.direct(), 30*time.Second)
	if out := runFix(t, main, fix); !strings.Contains(out, "answers as node-b") {
		t.Fatalf("fix:\n%s", out)
	}
	waitFor(t, 120*time.Second, "node-b following as a copy again, no address problem", func() bool {
		return b.isStreaming() && noAddrProblem(main) && noAddrProblem(b)
	})
	insertRange(t, main.conn(), 3000, 3099, "after-main-moved-")
	waitAllSame(t, main, a, b, c)
	_ = ra
	stopAll(a, b, c)
}

// Case 2's root: the main node can connect out but no other node can
// connect in (its address changed and nobody learned the new one, or a
// firewall drops incoming connections). Its lease renewals still reach the
// leader, so before D62 it kept the lease while every write waited for a
// copy that could not connect, forever. Now it stops renewing once no
// leader reaches it, and the cluster fails over the normal way.
//
// Built only from the dial map (COYOTE_TEST_DIAL_MAP), so it runs the
// same against a build without D62.
func TestMainUnreachableInboundFailsOver(t *testing.T) {
	root := t.TempDir()
	a, b, c := newCNode(t, root, "node-a"), newCNode(t, root, "node-b"), newCNode(t, root, "node-c")
	// node-b will be the main node. node-a and node-c reach it only through
	// a relay; node-b reaches them directly.
	rb := newRelay(t, b.direct())
	toB := "COYOTE_TEST_DIAL_MAP=" + b.direct() + "=127.0.0.1:" + strconv.Itoa(rb.port())
	a.env = append(a.env, toB)
	c.env = append(c.env, toB)
	a.bootstrap()
	itemsOn(t, a.conn())
	b.join(a)
	c.join(a)
	syncFull(t, a, b, c)
	// node-b main, node-a (still) the Raft leader: renewals go from node-b
	// to node-a, and node-a must dial node-b to reach it.
	a.mustCLI("switch", "--to", "node-b")
	waitFor(t, 90*time.Second, "node-b main with node-a leading Raft", func() bool {
		s := a.clusterStatus()
		return b.isPrimary() && s.Cluster.PrimaryName == "node-b" && s.Cluster.LeaderName == "node-a"
	})
	syncFull(t, b, a, c)
	insertRange(t, b.conn(), 1000, 1099, "on-b-")
	want := queryText(t, b.conn(), tableHash)

	rb.close() // nobody can connect to node-b any more; it can connect out
	t0 := time.Now()
	var main *cnode
	waitFor(t, 90*time.Second, "another node made the main node", func() bool {
		main = primaryOf(t, a, c)
		return main != nil
	})
	t.Logf("failed over to %s %s after node-b became unreachable", main.name, time.Since(t0).Round(100*time.Millisecond))
	if b.isPrimary() {
		t.Fatalf("node-b still runs as a main node after %s took over", main.name)
	}
	if got := queryText(t, main.conn(), tableHash); got != want {
		t.Fatalf("data on the new main node %s differs: %s, want %s", main.name, got, want)
	}
	insertRange(t, main.conn(), 2000, 2099, "after-failover-")

	// node-b reachable again: it rejoins as a copy.
	rb2 := newRelayAt(t, rb.port(), b.direct())
	defer rb2.close()
	waitFor(t, 120*time.Second, "node-b following as a copy again", b.isStreaming)
	waitAllSame(t, main, a, b, c)
	stopAll(a, b, c)
}

// newRelayAt is newRelay on a given port (the same address coming back).
func newRelayAt(t *testing.T, port int, target string) *relay {
	t.Helper()
	var ln net.Listener
	var err error
	waitFor(t, 10*time.Second, "the relay's port free again", func() bool {
		ln, err = net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		return err == nil
	})
	r := &relay{ln: ln, target: target}
	r.wg.Add(1)
	go r.accept()
	t.Cleanup(r.close)
	return r
}

// Case 4: every address changed while the cluster was off. No node can
// reach another, so no leader is elected and nothing can be recorded.
// Status says what to do; `node set-address --local` on each computer
// tells it where the others are now, without a leader; the cluster runs
// again, each node shows address_gone with its new address (D66: nothing
// is recorded by itself), and the commands it names record them. Also:
// `join` without an invite code on a folder that already joined just
// starts it (case 5a).
func TestAllAddressesChangedWhileOff(t *testing.T) {
	root := t.TempDir()
	a, b, c := newCNode(t, root, "node-a"), newCNode(t, root, "node-b"), newCNode(t, root, "node-c")
	ra, rb, rc := relayed(t, a), relayed(t, b), relayed(t, c)
	a.bootstrap()
	itemsOn(t, a.conn())
	b.join(a)
	c.join(a)
	syncFull(t, a, b, c)
	want := queryText(t, a.conn(), tableHash)
	stopAll(c, b, a)

	for _, r := range []*relay{ra, rb, rc} {
		r.close()
	}
	for _, n := range []*cnode{a, b, c} {
		n.setDetected(n.direct())
	}
	for _, n := range []*cnode{a, b} {
		n.launch("up", "")
	}
	// `join` with no code on a folder that already joined: starts it.
	c.launch("join", "")
	c.waitOut("already joined the cluster; starting it", 30*time.Second)

	out := a.waitStatusSays(90*time.Second, "Waiting to reach the other nodes", "set-address --local")
	if strings.Contains(out, "Cluster ,") || strings.Contains(out, "saved on none") {
		t.Fatalf("status shows empty-state text:\n%s", out)
	}
	for _, n := range []*cnode{a, b, c} {
		for _, o := range []*cnode{a, b, c} {
			if o == n {
				continue
			}
			got := n.mustCLI("node", "set-address", "--local", o.name, o.direct())
			if !strings.Contains(got, "answers as "+o.name) {
				t.Fatalf("set-address --local on %s for %s:\n%s", n.name, o.name, got)
			}
		}
	}
	t0 := time.Now()
	waitFor(t, 3*time.Minute, "a main node", func() bool { return primaryOf(t, a, b, c) != nil })
	relays := map[*cnode]*relay{a: ra, b: rb, c: rc}
	for _, n := range []*cnode{a, b, c} {
		fix := addrProblem(t, a, n, "address_gone", net.JoinHostPort("127.0.0.1", strconv.Itoa(relays[n].port())), n.direct(), time.Minute)
		if out := runFix(t, a, fix); !strings.Contains(out, "answers as "+n.name) {
			t.Fatalf("fix for %s:\n%s", n.name, out)
		}
	}
	waitFor(t, time.Minute, "every node recorded at its new address", func() bool {
		return seenAs(a, b, b.direct()) && seenAs(b, c, c.direct()) && seenAs(c, a, a.direct()) && noAddrProblem(a)
	})
	t.Logf("cluster back %s after the addresses were given", time.Since(t0).Round(100*time.Millisecond))
	main := primaryOf(t, a, b, c)
	if got := queryText(t, main.conn(), tableHash); got != want {
		t.Fatalf("data differs after the restart: %s, want %s", got, want)
	}
	syncFull(t, main, slices.DeleteFunc([]*cnode{a, b, c}, func(x *cnode) bool { return x == main })...)
	// The --local addresses are forgotten once recorded.
	waitFor(t, 30*time.Second, "--local addresses forgotten", func() bool {
		for _, n := range []*cnode{a, b, c} {
			raw, err := os.ReadFile(filepath.Join(n.dir, "cluster", "peers.json"))
			if err != nil || strings.Contains(string(raw), `"local"`) {
				return false
			}
		}
		return true
	})
	stopAll(a, b, c)
}

// Case 5a: an address given with --advertise (COYOTE_ADVERTISE in a
// container) is recorded once. A later `node set-address` is kept while
// the node runs and when it starts again with the same --advertise; a
// different --advertise replaces it.
func TestAdvertiseDoesNotUndoSetAddress(t *testing.T) {
	root := t.TempDir()
	a := newCNode(t, root, "node-a")
	w := newWitness(t, root, "wit")
	a.bootstrap()
	adv := []string{"--advertise", w.direct()}
	w.launchReady("witness", a.invite(), 3*time.Minute)
	w.a.stop(t)
	waitNoNodeProcs(t, w.dir, 15*time.Second)
	launchWith := func(extra ...string) {
		t.Helper()
		w.a = launchCoyoteDBEnv(t, w.env, append(w.args("up", ""), extra...)...)
		if !w.a.waitReady(t, 3*time.Minute) {
			t.Fatalf("witness not ready:\n%s", w.a.out.String())
		}
	}
	launchWith(adv...)
	waitFor(t, 30*time.Second, "the witness recorded at its --advertise", func() bool { return seenAs(a, w, w.direct()) })

	alt := net.JoinHostPort("localhost", strconv.Itoa(w.listen))
	a.mustCLI("node", "set-address", "wit", alt)
	// Before D62 the witness put its --advertise back within one
	// maintenance round (5 s).
	time.Sleep(12 * time.Second) // several maintenance rounds; nothing may change
	if !seenAs(a, w, alt) {
		t.Fatalf("set-address undone while the node runs:\n%s", a.mustCLI("status"))
	}
	w.a.stop(t)
	waitNoNodeProcs(t, w.dir, 15*time.Second)
	launchWith(adv...)
	time.Sleep(12 * time.Second) // as above, after a start with the same --advertise
	if !seenAs(a, w, alt) {
		t.Fatalf("set-address undone by a restart with the same --advertise:\n%s", a.mustCLI("status"))
	}
	// A new value does replace it.
	w.a.stop(t)
	waitNoNodeProcs(t, w.dir, 15*time.Second)
	r := newRelay(t, w.direct())
	moved := net.JoinHostPort("127.0.0.1", strconv.Itoa(r.port()))
	launchWith("--advertise", moved)
	waitFor(t, 30*time.Second, "the new --advertise recorded", func() bool { return seenAs(a, w, moved) })
	stopAll(a, w)
}
