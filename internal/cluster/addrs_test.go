package cluster

import (
	"net"
	"slices"
	"sync"
	"testing"
	"time"
)

// relay forwards a listener to target until closed (an address that
// stops answering when closed).
type relay struct {
	ln    net.Listener
	mu    sync.Mutex
	conns []net.Conn
}

func newRelay(t *testing.T, target string) *relay {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &relay{ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			up, err := net.Dial("tcp", target)
			if err != nil {
				c.Close()
				continue
			}
			r.mu.Lock()
			r.conns = append(r.conns, c, up)
			r.mu.Unlock()
			go func() { buf := make([]byte, 32<<10); copyConn(up, c, buf) }()
			go func() { buf := make([]byte, 32<<10); copyConn(c, up, buf) }()
		}
	}()
	t.Cleanup(r.close)
	return r
}

func copyConn(dst, src net.Conn, buf []byte) {
	defer dst.Close()
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, err := dst.Write(buf[:n]); err != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (r *relay) addr() string { return r.ln.Addr().String() }

func (r *relay) close() {
	r.ln.Close()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.conns {
		c.Close()
	}
	r.conns = nil
}

// listenFixed binds the node's port up front, so it can advertise a relay
// in front of it.
func listenFixed(t *testing.T, cfg *Config) string {
	t.Helper()
	ln, err := Listen(cfg.Timing, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Listener = ln
	return ln.Addr().String()
}

// Addresses given explicitly replace a pinned record once; after that a
// pin made later (`node set-address`) is kept (D62, case 5a).
func TestExplicitAddressesDoNotUndoALaterPin(t *testing.T) {
	h := newClusterHarness(t)
	a := h.bootstrap(h.cfg("a", RoleData, macPlat))
	wcfg := h.cfg("w", RoleWitness, Platform{})
	direct := listenFixed(t, &wcfg)
	wcfg.AdvertiseAddrs, wcfg.AdvertiseExplicit = []string{direct}, true
	w := h.join(a, wcfg)
	waitUntil(t, "w recorded", func() bool { return slices.Equal(a.State().Members[w.ID()].Addresses, []string{direct}) })
	time.Sleep(3 * FastTiming().ReconcileInterval) // its record is its explicit value: done

	alt := "localhost" + direct[len("127.0.0.1"):]
	pinned := true
	if _, err := a.Apply(ctxT(t), &UpdateMember{ID: w.ID(), Addresses: []string{alt}, Pinned: &pinned}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * FastTiming().ReconcileInterval) // several maintenance rounds
	if got := a.State().Members[w.ID()].Addresses; !slices.Equal(got, []string{alt}) {
		t.Fatalf("pinned address undone by the node: %v", got)
	}
	if got := w.AdvertiseAddrs(); !slices.Equal(got, []string{alt}) {
		t.Fatalf("the node announces %v, not its pinned record", got)
	}
}

// set-address --local (D62): with no leader, a node told where another
// member is now dials it there first, and forgets it once the record
// holds it.
func TestLocalAddressesAreDialedFirstAndForgotten(t *testing.T) {
	h := newClusterHarness(t)
	a := h.bootstrap(h.cfg("a", RoleData, macPlat))
	b := h.join(a, h.cfg("b", RoleData, macPlat))
	if id, name, ok := a.FindPeer("B"); !ok || id != b.ID() || name != "b" {
		t.Fatalf("FindPeer: %q %q %v", id, name, ok)
	}
	if err := a.SetLocalAddrs(a.ID(), []string{"127.0.0.1:1"}); err == nil {
		t.Fatal("a node accepted --local addresses for itself")
	}
	if err := a.SetLocalAddrs(b.ID(), []string{"127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	if got := a.resolve(b.ID()); len(got) != 2 || got[0] != "127.0.0.1:1" || got[1] != b.Addr().String() {
		t.Fatalf("resolve: %v", got)
	}
	book := a.PeerBook()
	if len(book) != 1 || book[0].Name != "b" || !slices.Equal(book[0].Local, []string{"127.0.0.1:1"}) {
		t.Fatalf("PeerBook: %+v", book)
	}
	// Recorded addresses that include it: the local entry goes.
	if err := a.SetLocalAddrs(b.ID(), []string{b.Addr().String()}); err != nil {
		t.Fatal(err)
	}
	h.propose(a)
	waitUntil(t, "local entry forgotten", func() bool { return len(a.PeerBook()[0].Local) == 0 })
}

// LeaderContactAge: zero on the leader, small on a follower the leader
// reaches, growing on one it cannot reach (D62).
func TestLeaderContactAge(t *testing.T) {
	h := newClusterHarness(t)
	a := h.bootstrap(h.cfg("a", RoleData, macPlat))
	bcfg := h.cfg("b", RoleData, macPlat)
	direct := listenFixed(t, &bcfg)
	r := newRelay(t, direct)
	bcfg.AdvertiseAddrs = []string{r.addr()}
	b := h.join(a, bcfg)
	c := h.join(a, h.cfg("c", RoleData, macPlat))
	h.waitConfig(a, map[string]Suffrage{a.ID(): Voter, b.ID(): Voter, c.ID(): Voter})
	l := h.leader()
	if l.LeaderContactAge() != 0 {
		t.Fatal("leader has a contact age")
	}
	if l == b { // rare: a bootstraps and leads
		if err := b.TransferLeadership(ctxT(t)); err != nil {
			t.Fatal(err)
		}
		waitUntil(t, "another leader", func() bool { l = h.leader(); return l != nil && l != b })
	}
	waitUntil(t, "b hears from the leader", func() bool { return b.LeaderContactAge() < time.Second })
	r.close() // the leader cannot dial b any more; b can still dial out
	waitUntil(t, "b's leader contact goes stale", func() bool { return b.LeaderContactAge() > 3*time.Second })
}
