package cluster

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"coyotedb.com/coyote/internal/cluster/transport"
	"coyotedb.com/coyote/internal/invite"
	"coyotedb.com/coyote/internal/tunnel"
)

const waitLong = 30 * time.Second

// clock is a wall clock with an adjustable offset (for leader-side expiry).
type clock struct{ off atomic.Int64 }

func (c *clock) Now() time.Time          { return time.Now().Add(time.Duration(c.off.Load())) }
func (c *clock) Advance(d time.Duration) { c.off.Add(int64(d)) }

func testLogger(t *testing.T, name string) *slog.Logger {
	if os.Getenv("COYOTE_TEST_LOG") == "" {
		return slog.New(slog.DiscardHandler)
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})).With("test", t.Name(), "n", name)
}

type harness struct {
	t     *testing.T
	root  string
	mu    sync.Mutex
	nodes map[string]*Node // by name
}

func newClusterHarness(t *testing.T) *harness {
	h := &harness{t: t, root: t.TempDir(), nodes: map[string]*Node{}}
	t.Cleanup(func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, n := range h.nodes {
			n.Shutdown()
		}
	})
	return h
}

func (h *harness) cfg(name string, role Role, p Platform) Config {
	return Config{
		Dir:        filepath.Join(h.root, name),
		ListenAddr: "127.0.0.1:0",
		Name:       name,
		Role:       role,
		Platform:   p,
		Timing:     FastTiming(),
		Logger:     testLogger(h.t, name),
	}
}

func (h *harness) track(n *Node) *Node {
	h.mu.Lock()
	h.nodes[n.Name()] = n
	h.mu.Unlock()
	return n
}

func (h *harness) stop(name string) {
	h.mu.Lock()
	n := h.nodes[name]
	delete(h.nodes, name)
	h.mu.Unlock()
	if err := n.Shutdown(); err != nil {
		h.t.Fatalf("shutdown %s: %v", name, err)
	}
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), waitLong)
	t.Cleanup(cancel)
	return ctx
}

func (h *harness) bootstrap(cfg Config) *Node {
	h.t.Helper()
	n, err := Bootstrap(ctxT(h.t), cfg)
	if err != nil {
		h.t.Fatalf("bootstrap %s: %v", cfg.Name, err)
	}
	return h.track(n)
}

func (h *harness) invite(via *Node) string {
	h.t.Helper()
	code, err := via.CreateInvite(ctxT(h.t), time.Minute, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	return code
}

func (h *harness) join(via *Node, cfg Config) *Node {
	h.t.Helper()
	n, err := Join(ctxT(h.t), cfg, h.invite(via))
	if err != nil {
		h.t.Fatalf("join %s: %v", cfg.Name, err)
	}
	return h.track(n)
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitLong)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (h *harness) live() []*Node {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Collect(maps.Values(h.nodes))
}

func (h *harness) leader() *Node {
	h.t.Helper()
	var l *Node
	waitUntil(h.t, "a leader", func() bool {
		for _, n := range h.live() {
			if n.IsLeader() {
				l = n
				return true
			}
		}
		return false
	})
	return l
}

// config returns id -> suffrage of n's Raft configuration.
func config(n *Node) map[string]Suffrage {
	cs, err := n.RaftConfiguration()
	if err != nil {
		return nil
	}
	out := map[string]Suffrage{}
	for _, s := range cs {
		out[s.ID] = s.Suffrage
	}
	return out
}

func (h *harness) waitConfig(n *Node, want map[string]Suffrage) {
	h.t.Helper()
	waitUntil(h.t, fmt.Sprintf("raft config %v", want), func() bool {
		l := h.leader()
		if !maps.Equal(config(l), want) {
			return false
		}
		// The state records the suffrage too.
		st := l.State()
		for id, s := range want {
			if st.Members[id].Suffrage != s {
				return false
			}
		}
		return true
	})
	_ = n
}

var apiKeySeq atomic.Int64

// propose adds an API key through n and waits until every live node has it.
func (h *harness) propose(n *Node) string {
	h.t.Helper()
	key := "k" + strconv.FormatInt(apiKeySeq.Add(1), 10)
	if _, err := n.Apply(ctxT(h.t), &AddAPIKey{Hash: key, Name: key, At: time.Now()}); err != nil {
		h.t.Fatalf("apply via %s: %v", n.Name(), err)
	}
	return key
}

func (h *harness) waitReplicated(key string, nodes ...*Node) {
	h.t.Helper()
	for _, n := range nodes {
		waitUntil(h.t, "replication of "+key+" to "+n.Name(), func() bool {
			_, ok := n.State().APIKeys[key]
			return ok
		})
	}
}

func TestBootstrapSingleNode(t *testing.T) {
	h := newClusterHarness(t)
	a := h.bootstrap(h.cfg("a", RoleData, macPlat))
	st := a.State()
	if !st.Bootstrapped() || st.Epoch != 1 || st.PrimaryID != a.ID() || len(st.Members) != 1 {
		t.Fatalf("state %+v", st)
	}
	if st.Members[a.ID()].Addresses[0] != a.Addr().String() {
		t.Fatalf("advertised %v, listening %v", st.Members[a.ID()].Addresses, a.Addr())
	}
	if !a.IsLeader() || a.LeaderID() != a.ID() {
		t.Fatal("not leader")
	}
	ls, err := a.LinearizableState(ctxT(t))
	if err != nil || ls.ClusterID != st.ClusterID {
		t.Fatalf("linearizable read: %v", err)
	}
	h.propose(a)
	if _, err := os.Stat(filepath.Join(a.cfg.Dir, filePending)); err == nil {
		waitUntil(t, "bootstrap.pending removed", func() bool {
			_, err := os.Stat(filepath.Join(a.cfg.Dir, filePending))
			return err != nil
		})
	}
	// Bootstrap refuses an existing state dir.
	if _, err := Bootstrap(ctxT(t), h.cfg("a", RoleData, macPlat)); err == nil {
		t.Fatal("bootstrap over existing state")
	}
	// Key files and the dirs holding Raft data (CA key) are private.
	if !isWindows() {
		for _, p := range []string{fileKey, ".", dirRaft} {
			fi, err := os.Stat(filepath.Join(a.cfg.Dir, p))
			if err != nil || fi.Mode().Perm()&0o077 != 0 {
				t.Fatalf("%s perms: %v %v", p, fi.Mode(), err)
			}
		}
	}
}

func isWindows() bool { return filepath.Separator == '\\' }

func TestJoinThreeReplicateAndFailover(t *testing.T) {
	h := newClusterHarness(t)
	a := h.bootstrap(h.cfg("a", RoleData, macPlat))
	b := h.join(a, h.cfg("b", RoleData, macPlat))
	// Two data nodes, no witness: b is a non-voter (D17).
	h.waitConfig(a, map[string]Suffrage{a.ID(): Voter, b.ID(): Nonvoter})
	c := h.join(a, h.cfg("c", RoleData, macPlat))
	h.waitConfig(a, map[string]Suffrage{a.ID(): Voter, b.ID(): Voter, c.ID(): Voter})

	// Replication, and proposals from followers are forwarded.
	k := h.propose(c)
	h.waitReplicated(k, a, b, c)
	if st := b.State(); len(st.Members) != 3 || st.ClusterID != a.ClusterID() {
		t.Fatalf("b state %+v", st)
	}
	// Followers cannot do linearizable reads.
	if _, err := b.LinearizableState(ctxT(t)); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("follower linearizable read: %v", err)
	}

	// Kill the leader: a new one is elected and accepts proposals.
	old := h.leader()
	oldID := old.ID()
	h.stop(old.Name())
	nl := h.leader()
	if nl.ID() == oldID {
		t.Fatal("old leader still leading")
	}
	survivors := h.live()
	k = h.propose(survivors[0])
	h.waitReplicated(k, survivors...)
}

func TestJoinRejections(t *testing.T) {
	h := newClusterHarness(t)
	clk := &clock{}
	acfg := h.cfg("a", RoleData, macPlat)
	acfg.Now = clk.Now
	a := h.bootstrap(acfg)
	ctx := ctxT(t)

	// Reused invite.
	code := h.invite(a)
	h.track(mustJoin(t, code, h.cfg("b", RoleData, macPlat)))
	if _, err := Join(ctx, h.cfg("c", RoleData, macPlat), code); !errors.Is(err, ErrSecretUsed) {
		t.Fatalf("reused invite: %v", err)
	}

	// Wrong secret (valid format, right CA pin).
	inv, _ := invite.Decode(h.invite(a))
	inv.Secret[0] ^= 1
	bad, _ := inv.Encode()
	if _, err := Join(ctx, h.cfg("c", RoleData, macPlat), bad); !errors.Is(err, ErrSecretUnknown) {
		t.Fatalf("wrong secret: %v", err)
	}

	// Expired: locally (before any network attempt)...
	code = h.invite(a)
	late := h.cfg("c", RoleData, macPlat)
	late.Now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if _, err := Join(ctx, late, code); !errors.Is(err, invite.ErrExpired) {
		t.Fatalf("locally expired: %v", err)
	}
	// ...and judged by the leader's clock.
	clk.Advance(2 * time.Minute)
	if _, err := Join(ctx, h.cfg("c", RoleData, macPlat), code); !errors.Is(err, ErrSecretExpired) {
		t.Fatalf("leader-expired: %v", err)
	}
	clk.Advance(-2 * time.Minute)

	// Platform mismatch: a clear error. The same code then still works for
	// a witness on that platform (the refused join did not burn it).
	code = h.invite(a)
	_, err := Join(ctx, h.cfg("lin", RoleData, linuxPlat), code)
	if !errors.Is(err, ErrMixedOff) || !strings.Contains(err.Error(), "linux/amd64") || !strings.Contains(err.Error(), "darwin/arm64") {
		t.Fatalf("platform mismatch: %v", err)
	}
	t.Logf("mismatch error shown to user: %v", err)
	h.track(mustJoin(t, code, h.cfg("lin", RoleWitness, linuxPlat)))

	// Duplicate name.
	dup := h.cfg("b", RoleData, macPlat)
	dup.Dir = filepath.Join(h.root, "b-again")
	if _, err := Join(ctx, dup, h.invite(a)); !errors.Is(err, ErrDuplicateName) {
		t.Fatalf("duplicate name: %v", err)
	}

	// Garbage code: friendly error, no network.
	if _, err := Join(ctx, h.cfg("z", RoleData, macPlat), "coyote-abc"); !errors.Is(err, invite.ErrCorrupt) {
		t.Fatalf("corrupt code: %v", err)
	}
	if n := len(a.State().Members); n != 3 {
		t.Fatalf("members %d, want 3", n)
	}
}

func mustJoin(t *testing.T, code string, cfg Config) *Node {
	t.Helper()
	n, err := Join(ctxT(t), cfg, code)
	if err != nil {
		t.Fatalf("join %s: %v", cfg.Name, err)
	}
	return n
}

func TestJoinViaFollowerIsForwarded(t *testing.T) {
	h := newClusterHarness(t)
	a := h.bootstrap(h.cfg("a", RoleData, macPlat))
	b := h.join(a, h.cfg("b", RoleData, macPlat))
	waitUntil(t, "b knows leader", func() bool { return b.LeaderID() == a.ID() })
	// Invite that only lists the follower's address.
	code, err := b.CreateInvite(ctxT(t), time.Minute, b.AdvertiseAddrs())
	if err != nil {
		t.Fatal(err)
	}
	inv, _ := invite.Decode(code)
	if !slices.Equal(inv.Addrs, b.AdvertiseAddrs()) {
		t.Fatalf("addrs %v", inv.Addrs)
	}
	c := h.track(mustJoin(t, code, h.cfg("c", RoleData, macPlat)))
	h.waitConfig(a, map[string]Suffrage{a.ID(): Voter, b.ID(): Voter, c.ID(): Voter})
}

func TestRestartFromDiskCatchesUpAtNewAddress(t *testing.T) {
	h := newClusterHarness(t)
	a := h.bootstrap(h.cfg("a", RoleData, macPlat))
	b := h.join(a, h.cfg("b", RoleData, macPlat))
	c := h.join(a, h.cfg("c", RoleData, macPlat))
	h.waitConfig(a, map[string]Suffrage{a.ID(): Voter, b.ID(): Voter, c.ID(): Voter})
	oldAddr := c.Addr().String()
	ccfg := c.cfg
	h.stop("c")

	var keys []string
	for range 5 {
		keys = append(keys, h.propose(a))
	}
	// Restart on a new port (address changes are normal: VPN, DHCP).
	ccfg.ListenAddr = "127.0.0.1:0"
	c2, err := Start(ctxT(t), ccfg)
	if err != nil {
		t.Fatal(err)
	}
	h.track(c2)
	if c2.Addr().String() == oldAddr {
		t.Log("same port reused by chance")
	}
	h.waitReplicated(keys[len(keys)-1], c2)
	waitUntil(t, "new address recorded", func() bool {
		return slices.Equal(a.State().Members[c2.ID()].Addresses, []string{c2.Addr().String()})
	})
	k := h.propose(c2)
	h.waitReplicated(k, a, b, c2)
}

func TestRemoveMemberRefusesItsConnections(t *testing.T) {
	h := newClusterHarness(t)
	// Fake Postgres on a: echo.
	pg, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	go func() {
		for {
			c, err := pg.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	acfg := h.cfg("a", RoleData, macPlat)
	acfg.PGAddr = func() string { return pg.Addr().String() }
	a := h.bootstrap(acfg)
	b := h.join(a, h.cfg("b", RoleData, macPlat))
	c := h.join(a, h.cfg("c", RoleData, macPlat))
	d := h.join(a, h.cfg("d", RoleWitness, linuxPlat))
	h.waitConfig(a, map[string]Suffrage{a.ID(): Voter, b.ID(): Voter, c.ID(): Voter, d.ID(): Voter})

	// c tunnels to a's Postgres.
	fw, err := tunnel.NewForwarder(tunnel.ForwarderConfig{ListenAddr: "127.0.0.1:0", Dial: c.PGDialer(), Target: a.ID()})
	if err != nil {
		t.Fatal(err)
	}
	defer fw.Close()
	conn, err := net.Dial("tcp", fw.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.Write([]byte("hi"))
	if _, err := io.ReadFull(conn, make([]byte, 2)); err != nil {
		t.Fatalf("tunnel before removal: %v", err)
	}

	if err := b.RemoveMember(ctxT(t), c.ID()); err != nil { // via a follower
		t.Fatal(err)
	}
	h.waitConfig(a, map[string]Suffrage{a.ID(): Voter, b.ID(): Voter, d.ID(): Voter})
	// Every remaining member has applied the removal (a follower may
	// apply it a moment after the leader's configuration changed).
	waitUntil(t, "removal applied everywhere", func() bool {
		for _, n := range []*Node{a, b, d} {
			if _, ok := n.State().Members[c.ID()]; ok {
				return false
			}
		}
		return true
	})
	// Live tunnel is cut.
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("removed member's tunnel still open")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("removed member's tunnel not closed")
	}
	// New streams from c are refused by every remaining member.
	for _, n := range []*Node{a, b, d} {
		_, err := c.tr.DialAddr(ctxT(t), n.Addr().String(), n.ID(), transport.StreamRPC)
		var re *transport.RefusedError
		if !errors.As(err, &re) || !re.Denied {
			t.Fatalf("%s accepted removed member: %v", n.Name(), err)
		}
	}
	_, err = c.tr.DialAddr(ctxT(t), a.Addr().String(), a.ID(), transport.StreamPG)
	if err == nil {
		t.Fatal("removed member opened a pg tunnel")
	}
	k := h.propose(b)
	h.waitReplicated(k, a, b, d)

	// Primary and last voter guards.
	if err := b.RemoveMember(ctxT(t), a.ID()); !errors.Is(err, ErrIsPrimary) {
		t.Fatalf("remove primary: %v", err)
	}
}

func TestTwoDataNodesAndWitness(t *testing.T) {
	h := newClusterHarness(t)
	a := h.bootstrap(h.cfg("a", RoleData, macPlat))
	b := h.join(a, h.cfg("b", RoleData, macPlat))
	h.waitConfig(a, map[string]Suffrage{a.ID(): Voter, b.ID(): Nonvoter})

	// The non-voter is stopped: a keeps quorum alone.
	bcfg := b.cfg
	h.stop("b")
	k := h.propose(a)
	h.waitReplicated(k, a)

	b2, err := Start(ctxT(t), bcfg)
	if err != nil {
		t.Fatal(err)
	}
	h.track(b2)
	h.waitReplicated(k, b2)

	// A witness (any platform) joins: b is promoted, all three vote.
	w := h.join(a, h.cfg("w", RoleWitness, linuxPlat))
	h.waitConfig(a, map[string]Suffrage{a.ID(): Voter, b2.ID(): Voter, w.ID(): Voter})

	// Witness leaves: back to one voter + one non-voter.
	if err := w.Leave(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	delete(h.nodes, "w")
	h.mu.Unlock()
	h.waitConfig(a, map[string]Suffrage{a.ID(): Voter, b2.ID(): Nonvoter})
	k = h.propose(b2)
	h.waitReplicated(k, a, b2)
}

// The reply to a node's own removal is lost after the removal committed
// (the leader cuts the removed node's connections when it applies the
// removal). Leave still learns that it succeeded, shuts the node down, and
// a second Leave succeeds too.
func TestLeaveReplyLost(t *testing.T) {
	h := newClusterHarness(t)
	a := h.bootstrap(h.cfg("a", RoleData, macPlat))
	b := h.join(a, h.cfg("b", RoleData, macPlat))
	w := h.join(a, h.cfg("w", RoleWitness, linuxPlat))
	h.waitConfig(a, map[string]Suffrage{a.ID(): Voter, b.ID(): Voter, w.ID(): Voter})
	l := h.leader()
	if l == w {
		t.Fatal("the witness leads; this test needs it to forward its removal")
	}

	l.dropApplyResponses.Store(1)
	if err := w.Leave(ctxT(t)); err != nil {
		t.Fatalf("leave with the reply lost: %v", err)
	}
	if l.dropApplyResponses.Load() != 0 {
		t.Fatal("the reply was not dropped: the test did not exercise the lost reply")
	}
	if w.ctx.Err() == nil {
		t.Fatal("leave returned without shutting the node down")
	}
	if err := w.Leave(ctxT(t)); err != nil {
		t.Fatalf("second leave: %v", err)
	}
	h.mu.Lock()
	delete(h.nodes, "w")
	h.mu.Unlock()
	h.waitConfig(a, map[string]Suffrage{a.ID(): Voter, b.ID(): Nonvoter})
	if _, ok := l.State().Members[w.ID()]; ok {
		t.Fatal("w is still a member")
	}
}

// The leader that took a node's removal stops between the commit and the
// reply. Leave keeps asking (the new leader, or the old one once back) and
// ends certain that the removal committed.
func TestLeaveLeaderStopsBeforeReply(t *testing.T) {
	h := newClusterHarness(t)
	a := h.bootstrap(h.cfg("a", RoleData, macPlat))
	b := h.join(a, h.cfg("b", RoleData, macPlat))
	c := h.join(a, h.cfg("c", RoleData, macPlat))
	d := h.join(a, h.cfg("d", RoleWitness, linuxPlat))
	h.waitConfig(a, map[string]Suffrage{a.ID(): Voter, b.ID(): Voter, c.ID(): Voter, d.ID(): Voter})
	l := h.leader()
	var leaver *Node
	for _, n := range []*Node{a, b, c} {
		if n != l {
			leaver = n
			break
		}
	}
	lcfg := l.cfg
	lcfg.ListenAddr = l.Addr().String() // it comes back where the others look for it

	l.dropApplyResponses.Store(1)
	done := make(chan error, 1)
	go func() { done <- leaver.Leave(ctxT(t)) }()
	waitUntil(t, "the removal processed and its reply dropped", func() bool { return l.dropApplyResponses.Load() == 0 })
	h.stop(l.Name())
	// It comes back (the rest may lack a quorum without it while the
	// leaver still counts as a Raft voter).
	l2, err := Start(ctxT(t), lcfg)
	if err != nil {
		t.Fatal(err)
	}
	h.track(l2)
	if err := <-done; err != nil {
		t.Fatalf("leave: %v", err)
	}
	if leaver.ctx.Err() == nil {
		t.Fatal("leave returned without shutting the node down")
	}
	h.mu.Lock()
	delete(h.nodes, leaver.Name())
	h.mu.Unlock()
	waitUntil(t, "the leaver gone from the cluster state", func() bool {
		_, still := h.leader().State().Members[leaver.ID()]
		return !still
	})
	h.propose(h.leader())
}

func TestSnapshotTruncationThenFreshJoin(t *testing.T) {
	h := newClusterHarness(t)
	acfg := h.cfg("a", RoleData, macPlat)
	acfg.Timing.TrailingLogs = 2
	a := h.bootstrap(acfg)
	var last string
	for range 30 {
		last = h.propose(a)
	}
	if err := a.Snapshot(); err != nil {
		t.Fatal(err)
	}
	first, err := a.store.FirstIndex()
	if err != nil || first <= 10 {
		t.Fatalf("log not truncated: first index %d (%v)", first, err)
	}
	b := h.join(a, h.cfg("b", RoleData, macPlat))
	h.waitReplicated(last, b)
	if b.raft.Stats()["last_snapshot_index"] == "0" {
		t.Fatal("b did not install a snapshot")
	}
	sa, sb := a.State(), b.State()
	sa.AppliedIndex, sb.AppliedIndex = 0, 0
	waitUntil(t, "b converges", func() bool {
		sa, sb = a.State(), b.State()
		return sa.AppliedIndex == sb.AppliedIndex
	})
	if len(sb.APIKeys) != 30 || len(sb.CA.KeyDER) == 0 || sb.Epoch != sa.Epoch {
		t.Fatalf("restored state incomplete: keys=%d", len(sb.APIKeys))
	}
}

func TestForceNewClusterOnLoneSurvivor(t *testing.T) {
	h := newClusterHarness(t)
	a := h.bootstrap(h.cfg("a", RoleData, macPlat))
	b := h.join(a, h.cfg("b", RoleData, macPlat))
	c := h.join(a, h.cfg("c", RoleData, macPlat))
	h.waitConfig(a, map[string]Suffrage{a.ID(): Voter, b.ID(): Voter, c.ID(): Voter})
	k := h.propose(a)
	h.waitReplicated(k, c)
	epoch := c.State().Epoch

	// a and b die for good; c alone cannot make progress.
	h.stop("a")
	h.stop("b")
	ccfg := c.cfg
	h.stop("c")

	if _, err := ForceNewCluster(ctxT(t), ccfg, "yes"); !errors.Is(err, ErrNotConfirmed) {
		t.Fatalf("unconfirmed force: %v", err)
	}
	c2, err := ForceNewCluster(ctxT(t), ccfg, ForceNewClusterConfirmation)
	if err != nil {
		t.Fatal(err)
	}
	h.track(c2)
	st := c2.State()
	if st.Epoch != epoch+1 || st.PrimaryID != c2.ID() || len(st.Members) != 1 || len(st.Forced) != 1 || st.Forced[0].NodeID != c2.ID() {
		t.Fatalf("after force: epoch %d (was %d), primary %s, members %d, forced %+v", st.Epoch, epoch, st.PrimaryID, len(st.Members), st.Forced)
	}
	if _, ok := st.APIKeys[k]; !ok {
		t.Fatal("state lost")
	}
	if cfg := config(c2); !maps.Equal(cfg, map[string]Suffrage{c2.ID(): Voter}) {
		t.Fatalf("raft config %v", cfg)
	}
	h.propose(c2)
	// The cluster can grow again.
	d := h.join(c2, h.cfg("d", RoleData, macPlat))
	k = h.propose(d)
	h.waitReplicated(k, c2, d)
}

func TestApplyCASThroughCluster(t *testing.T) {
	h := newClusterHarness(t)
	a := h.bootstrap(h.cfg("a", RoleData, macPlat))
	b := h.join(a, h.cfg("b", RoleData, macPlat))
	ctx := ctxT(t)
	// Two controllers race to set the primary from epoch 1: one wins.
	var wins, losses atomic.Int32
	var wg sync.WaitGroup
	for _, n := range []*Node{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := n.Apply(ctx, &SetPrimary{ExpectedEpoch: 1, NodeID: b.ID(), LeaseDuration: time.Second})
			switch {
			case err == nil:
				wins.Add(1)
			case errors.Is(err, ErrEpochMismatch):
				losses.Add(1)
			default:
				t.Errorf("unexpected: %v", err)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 || losses.Load() != 1 {
		t.Fatalf("wins %d losses %d", wins.Load(), losses.Load())
	}
	// Change notification on the follower.
	sub := b.Subscribe()
	defer sub.Close()
	if _, err := a.Apply(ctx, &RenewLease{NodeID: b.ID(), Epoch: 2}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "lease seq on b", func() bool {
		select {
		case <-sub.C:
		default:
		}
		return b.State().Lease.Seq == 1
	})
}

func TestShutdownLeavesNoGoroutines(t *testing.T) {
	base := runtime.NumGoroutine()
	h := newClusterHarness(t)
	a := h.bootstrap(h.cfg("a", RoleData, macPlat))
	b := h.join(a, h.cfg("b", RoleData, macPlat))
	w := h.join(a, h.cfg("w", RoleWitness, linuxPlat))
	h.waitConfig(a, map[string]Suffrage{a.ID(): Voter, b.ID(): Voter, w.ID(): Voter})
	h.propose(b)
	for _, name := range []string{"w", "b", "a"} {
		h.stop(name)
	}
	deadline := time.Now().Add(waitLong)
	for runtime.NumGoroutine() > base {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			t.Fatalf("goroutines %d > %d\n%s", runtime.NumGoroutine(), base, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSingleDataNodeWithWitness(t *testing.T) {
	h := newClusterHarness(t)
	a := h.bootstrap(h.cfg("a", RoleData, macPlat))
	w := h.join(a, h.cfg("w", RoleWitness, linuxPlat))
	// One data node: the witness must not vote.
	h.waitConfig(a, map[string]Suffrage{a.ID(): Voter, w.ID(): Nonvoter})
	// A dead witness does not stall the cluster.
	h.stop("w")
	k := h.propose(a)
	h.waitReplicated(k, a)
	// A second data node: 2 + witness, all three vote (witness is down,
	// so start it again first; promotion needs it reachable).
	w2, err := Start(ctxT(t), w.cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.track(w2)
	b := h.join(a, h.cfg("b", RoleData, macPlat))
	h.waitConfig(a, map[string]Suffrage{a.ID(): Voter, b.ID(): Voter, w2.ID(): Voter})
	// b leaves: back to 1 + witness, the witness is demoted.
	if err := b.Leave(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	delete(h.nodes, "b")
	h.mu.Unlock()
	h.waitConfig(a, map[string]Suffrage{a.ID(): Voter, w2.ID(): Nonvoter})
}

func TestJoinRetryAfterLostResponse(t *testing.T) {
	h := newClusterHarness(t)
	a := h.bootstrap(h.cfg("a", RoleData, macPlat))
	code := h.invite(a)
	// The leader commits the join, but its answer is lost on the way.
	a.dropJoinResponses.Store(1)
	b := h.track(mustJoin(t, code, h.cfg("b", RoleData, macPlat)))
	if a.dropJoinResponses.Load() != 0 {
		t.Fatal("fault was not injected")
	}
	st := a.State()
	named := 0
	for _, m := range st.Members {
		if m.Name == "b" {
			named++
		}
	}
	if len(st.Members) != 2 || named != 1 {
		t.Fatalf("members after retried join: %d (named b: %d)", len(st.Members), named)
	}
	if _, ok := st.Members[b.ID()]; !ok {
		t.Fatal("joined node id is not the member")
	}
	if _, err := os.Stat(filepath.Join(b.cfg.Dir, fileJoinPending)); err == nil {
		t.Fatal("join.pending.json left behind")
	}
	// Another node presenting the used invite is still refused.
	if _, err := Join(ctxT(t), h.cfg("c", RoleData, macPlat), code); !errors.Is(err, ErrSecretUsed) {
		t.Fatalf("other node with used invite: %v", err)
	}
	k := h.propose(b)
	h.waitReplicated(k, a, b)
}

func TestJoinResumesAfterRestart(t *testing.T) {
	h := newClusterHarness(t)
	a := h.bootstrap(h.cfg("a", RoleData, macPlat))
	code := h.invite(a)
	ccfg := h.cfg("c", RoleData, macPlat)
	// Every answer is lost: the joiner gives up, keeping its progress.
	a.dropJoinResponses.Store(1000)
	_, err := Join(ctxT(t), ccfg, code)
	if err == nil || definitive(err) || !strings.Contains(err.Error(), "resume") {
		t.Fatalf("want resumable failure, got %v", err)
	}
	p, err := loadJoinPending(ccfg.Dir)
	if err != nil || p == nil {
		t.Fatalf("no saved join state: %v", err)
	}
	if _, ok := a.State().Members[p.NodeID]; !ok {
		t.Fatal("first attempt should have been committed")
	}
	// "Process restart": a new Join call on the same dir resumes with the
	// saved id and key, and the leader answers again.
	a.dropJoinResponses.Store(0)
	c, err := Join(ctxT(t), ccfg, code)
	if err != nil {
		t.Fatal(err)
	}
	h.track(c)
	if c.ID() != p.NodeID {
		t.Fatalf("resumed as %s, saved id %s", c.ID(), p.NodeID)
	}
	if n := len(a.State().Members); n != 2 {
		t.Fatalf("members %d, want 2", n)
	}
	if p, _ := loadJoinPending(ccfg.Dir); p != nil {
		t.Fatal("join.pending.json left behind")
	}
	// It moved to its new port; the leader learns it and replicates.
	k := h.propose(a)
	h.waitReplicated(k, c)
}

func TestJoinStreamFirstRequestDeadline(t *testing.T) {
	h := newClusterHarness(t)
	a := h.bootstrap(h.cfg("a", RoleData, macPlat))
	st := a.State()
	c, err := transport.DialJoin(ctxT(t), a.Addr().String(), st.ClusterID, sha256Of(st.CA.CertDER), FastTiming().transport())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Say nothing: the node hangs up after JoinRequestTimeout.
	start := time.Now()
	c.SetReadDeadline(time.Now().Add(waitLong))
	_, err = c.Read(make([]byte, 1))
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("silent stranger was not dropped")
	}
	if d := time.Since(start); d > 2*FastTiming().JoinRequestTimeout+time.Second {
		t.Fatalf("dropped only after %v", d)
	}
}

func sha256Of(b []byte) [32]byte { return sha256.Sum256(b) }
