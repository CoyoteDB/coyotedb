package transport

import (
	"context"
	"errors"
	"io"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"coyotedb.com/coyote/internal/pki"
	"coyotedb.com/coyote/internal/pki/pkitest"
)

// members is a mutable membership set + address book shared by test nodes.
type members struct {
	mu    sync.Mutex
	ok    map[string]bool
	addrs map[string][]string
}

func newMembers() *members {
	return &members{ok: map[string]bool{}, addrs: map[string][]string{}}
}

func (m *members) set(id string, ok bool, addrs ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ok[id] = ok
	if addrs != nil {
		m.addrs[id] = addrs
	}
}

func (m *members) authorize(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ok[id]
}

func (m *members) resolve(id string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.addrs[id]...)
}

func fastTiming() Timing {
	return Timing{DialTimeout: 2 * time.Second, HandshakeTimeout: 2 * time.Second}
}

func start(t *testing.T, id *pki.NodeTLS, m *members) *Transport {
	t.Helper()
	tr, err := Listen(Config{ListenAddr: "127.0.0.1:0", TLS: id, Authorize: m.authorize, Resolve: m.resolve, Timing: fastTiming()})
	if err != nil {
		t.Fatal(err)
	}
	m.set(id.NodeID, true, tr.Addr().String())
	t.Cleanup(func() { tr.Close() })
	return tr
}

func echoHandler(s *Stream) {
	if err := s.Accept(); err != nil {
		return
	}
	io.Copy(s, s)
}

func TestEchoAndPeerIdentity(t *testing.T) {
	ca := pkitest.CA(t)
	m := newMembers()
	a := start(t, pkitest.Node(t, ca, "a"), m)
	b := start(t, pkitest.Node(t, ca, "b"), m)
	var seen atomic.Value
	a.Handle(StreamRPC, func(s *Stream) {
		seen.Store(s.Peer)
		echoHandler(s)
	})
	ctx := context.Background()
	c, err := b.Dial(ctx, "a", StreamRPC)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "hello" {
		t.Fatalf("echo %q %v", buf, err)
	}
	if seen.Load() != "b" {
		t.Fatalf("peer = %v", seen.Load())
	}
}

func TestAuthorization(t *testing.T) {
	ca := pkitest.CA(t)
	m := newMembers()
	a := start(t, pkitest.Node(t, ca, "a"), m)
	b := start(t, pkitest.Node(t, ca, "b"), m)
	a.Handle(StreamRPC, echoHandler)
	a.Handle(StreamJoin, echoHandler)
	ctx := context.Background()

	// Unknown stream type.
	if _, err := b.Dial(ctx, "a", StreamType(99)); err == nil {
		t.Fatal("unknown stream accepted")
	}
	// Non-member refused, with a clear reason.
	m.set("b", false)
	_, err := b.DialAddr(ctx, a.Addr().String(), "a", StreamRPC)
	var re *RefusedError
	if !errors.As(err, &re) || !re.Denied {
		t.Fatalf("want denied, got %v", err)
	}
	// Dial to a non-member is refused locally.
	m.set("b", true)
	m.set("a", false)
	if _, err := b.Dial(ctx, "a", StreamRPC); !errors.Is(err, ErrNotMember) {
		t.Fatalf("want ErrNotMember, got %v", err)
	}
	m.set("a", true)

	// Anonymous (join) client: only StreamJoin.
	fp := ca.Fingerprint()
	c, err := DialJoin(ctx, a.Addr().String(), pkitest.ClusterID, fp, fastTiming())
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	anon, err := dialStream(ctx, a.Addr().String(), "", StreamRPC, pki.JoinClientConfig(pkitest.ClusterID, fp, nil), fastTiming().withDefaults())
	if err == nil {
		anon.Close()
		t.Fatal("anonymous rpc stream accepted")
	}
	// Wrong pin.
	if _, err := DialJoin(ctx, a.Addr().String(), pkitest.ClusterID, [32]byte{1}, fastTiming()); err == nil {
		t.Fatal("wrong CA pin accepted")
	}
	// Cert from another CA is refused at the TLS layer.
	evil := start(t, pkitest.Node(t, pkitest.CA(t), "evil"), m)
	if _, err := evil.DialAddr(ctx, a.Addr().String(), "a", StreamRPC); err == nil {
		t.Fatal("foreign CA accepted")
	}
}

func TestDialTriesAddressesInOrderAndChecksIdentity(t *testing.T) {
	ca := pkitest.CA(t)
	m := newMembers()
	a := start(t, pkitest.Node(t, ca, "a"), m)
	b := start(t, pkitest.Node(t, ca, "b"), m)
	c := start(t, pkitest.Node(t, ca, "c"), m)
	a.Handle(StreamRPC, echoHandler)
	c.Handle(StreamRPC, echoHandler)
	// A dead address first, then c's address (wrong node), then a's.
	dead, _ := net.Listen("tcp", "127.0.0.1:0")
	deadAddr := dead.Addr().String()
	dead.Close()
	m.set("a", true, deadAddr, c.Addr().String(), a.Addr().String())
	conn, err := b.Dial(context.Background(), "a", StreamRPC)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	// Only the wrong node: must fail, not silently talk to c.
	m.set("a", true, c.Addr().String())
	if _, err := b.Dial(context.Background(), "a", StreamRPC); err == nil {
		t.Fatal("reached the wrong node")
	}
	// No addresses.
	m.set("a", true)
	m.mu.Lock()
	m.addrs["a"] = nil
	m.mu.Unlock()
	if _, err := b.Dial(context.Background(), "a", StreamRPC); !errors.Is(err, ErrNoAddress) {
		t.Fatalf("want ErrNoAddress, got %v", err)
	}
}

func TestHandlerReject(t *testing.T) {
	ca := pkitest.CA(t)
	m := newMembers()
	a := start(t, pkitest.Node(t, ca, "a"), m)
	b := start(t, pkitest.Node(t, ca, "b"), m)
	a.Handle(StreamPG, func(s *Stream) { s.Reject("postgres is down") })
	_, err := b.Dial(context.Background(), "a", StreamPG)
	var re *RefusedError
	if !errors.As(err, &re) || re.Msg != "postgres is down" || re.Denied {
		t.Fatalf("got %v", err)
	}
}

func TestDropUnauthorizedClosesLiveStreams(t *testing.T) {
	ca := pkitest.CA(t)
	m := newMembers()
	a := start(t, pkitest.Node(t, ca, "a"), m)
	b := start(t, pkitest.Node(t, ca, "b"), m)
	a.Handle(StreamRPC, echoHandler)
	c, err := b.Dial(context.Background(), "a", StreamRPC)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	m.set("b", false)
	if n := a.DropUnauthorized(); n != 1 {
		t.Fatalf("dropped %d", n)
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("stream of removed member still open")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("stream not closed (timed out instead)")
	}
}

func TestRaftLayer(t *testing.T) {
	ca := pkitest.CA(t)
	m := newMembers()
	a := start(t, pkitest.Node(t, ca, "a"), m)
	b := start(t, pkitest.Node(t, ca, "b"), m)
	if a.RaftLayer().Addr().String() != "a" {
		t.Fatal("raft address must be the node id")
	}
	go func() {
		c, err := a.RaftLayer().Accept()
		if err != nil {
			return
		}
		io.Copy(c, c)
		c.Close()
	}()
	c, err := b.RaftLayer().Dial("a", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c.Write([]byte("ping"))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("%q %v", buf, err)
	}
	c.Close()
	a.RaftLayer().Close()
	if _, err := a.RaftLayer().Accept(); err == nil {
		t.Fatal("accept after close")
	}
	if _, err := b.RaftLayer().Dial("a", 2*time.Second); err == nil {
		t.Fatal("raft stream accepted after layer close")
	}
}

func TestCloseLeavesNoGoroutines(t *testing.T) {
	base := runtime.NumGoroutine()
	ca := pkitest.CA(t)
	m := newMembers()
	mk := func(id string) *Transport {
		tr, err := Listen(Config{ListenAddr: "127.0.0.1:0", TLS: pkitest.Node(t, ca, id), Authorize: m.authorize, Resolve: m.resolve, Timing: fastTiming()})
		if err != nil {
			t.Fatal(err)
		}
		m.set(id, true, tr.Addr().String())
		return tr
	}
	a, b := mk("a"), mk("b")
	block := make(chan struct{})
	a.Handle(StreamRPC, func(s *Stream) {
		s.Accept()
		select {
		case <-block:
		case <-time.After(30 * time.Second):
		}
		io.Copy(io.Discard, s)
	})
	var conns []net.Conn
	for range 5 {
		c, err := b.Dial(context.Background(), "a", StreamRPC)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	// A half-open client that never handshakes.
	raw, err := net.Dial("tcp", a.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	close(block)
	b.Close()
	a.Close()
	for _, c := range conns {
		c.Close()
	}
	waitGoroutines(t, base)
}

func waitGoroutines(t *testing.T, base int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		n := runtime.NumGoroutine()
		if n <= base {
			return
		}
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			buf = buf[:runtime.Stack(buf, true)]
			t.Fatalf("goroutines: %d > baseline %d\n%s", n, base, buf)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAnonymousJoinStreamsAreCapped(t *testing.T) {
	ca := pkitest.CA(t)
	m := newMembers()
	a, err := Listen(Config{ListenAddr: "127.0.0.1:0", TLS: pkitest.Node(t, ca, "a"), Authorize: m.authorize,
		Resolve: m.resolve, Timing: fastTiming(), MaxAnonymous: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	m.set("a", true, a.Addr().String())
	b := start(t, pkitest.Node(t, ca, "b"), m)
	release := make(chan struct{})
	a.Handle(StreamJoin, func(s *Stream) {
		s.Accept()
		<-release // a stranger that holds its stream open
	})
	a.Handle(StreamRPC, echoHandler)
	ctx := context.Background()
	fp := ca.Fingerprint()
	var held []net.Conn
	for range 2 {
		c, err := DialJoin(ctx, a.Addr().String(), pkitest.ClusterID, fp, fastTiming())
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
	}
	waitUntil(t, func() bool { return a.NumAnonymous() == 2 })
	// Saturated: the next stranger is refused as busy.
	_, err = DialJoin(ctx, a.Addr().String(), pkitest.ClusterID, fp, fastTiming())
	var re *RefusedError
	if !errors.As(err, &re) || re.Denied {
		t.Fatalf("want busy refusal, got %v", err)
	}
	// Member traffic still flows while the join path is saturated.
	for range 5 {
		c, err := b.Dial(ctx, "a", StreamRPC)
		if err != nil {
			t.Fatalf("member stream while saturated: %v", err)
		}
		c.Write([]byte("ok"))
		buf := make([]byte, 2)
		if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ok" {
			t.Fatalf("member echo: %q %v", buf, err)
		}
		c.Close()
	}
	if a.NumAnonymous() != 2 {
		t.Fatalf("member streams counted as anonymous: %d", a.NumAnonymous())
	}
	// Slots free up when strangers go away.
	close(release)
	for _, c := range held {
		c.Close()
	}
	waitUntil(t, func() bool { return a.NumAnonymous() == 0 })
	c, err := DialJoin(ctx, a.Addr().String(), pkitest.ClusterID, fp, fastTiming())
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	c.Close()
}

func TestHandshakeDeadline(t *testing.T) {
	ca := pkitest.CA(t)
	m := newMembers()
	a, err := Listen(Config{ListenAddr: "127.0.0.1:0", TLS: pkitest.Node(t, ca, "a"), Authorize: m.authorize,
		Resolve: m.resolve, Timing: Timing{HandshakeTimeout: 300 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	// A client that connects and says nothing is dropped after the
	// handshake deadline.
	raw, err := net.Dial("tcp", a.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	start := time.Now()
	raw.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, err = raw.Read(make([]byte, 1))
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("silent client was not dropped")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("dropped only after %v", d)
	}
	waitUntil(t, func() bool { return a.NumConns() == 0 })
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
