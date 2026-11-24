package tunnel_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"coyotedb.com/coyote/internal/cluster/transport"
	"coyotedb.com/coyote/internal/pki/pkitest"
	"coyotedb.com/coyote/internal/tunnel"
)

type members struct {
	mu    sync.Mutex
	ok    map[string]bool
	addrs map[string][]string
}

func (m *members) set(id string, ok bool, addr string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ok[id] = ok
	if addr != "" {
		m.addrs[id] = []string{addr}
	}
}
func (m *members) authorize(id string) bool { m.mu.Lock(); defer m.mu.Unlock(); return m.ok[id] }
func (m *members) resolve(id string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.addrs[id]
}

// env is three nodes: "client" runs a Forwarder, "a" and "c" serve pg
// streams to their own fake Postgres.
type env struct {
	m       *members
	client  *transport.Transport
	servers map[string]*transport.Transport
	fw      *tunnel.Forwarder
}

func newEnv(t *testing.T, backends map[string]string) *env {
	t.Helper()
	ca := pkitest.CA(t)
	e := &env{m: &members{ok: map[string]bool{}, addrs: map[string][]string{}}, servers: map[string]*transport.Transport{}}
	mk := func(id string) *transport.Transport {
		tr, err := transport.Listen(transport.Config{
			ListenAddr: "127.0.0.1:0", TLS: pkitest.Node(t, ca, id),
			Authorize: e.m.authorize, Resolve: e.m.resolve,
		})
		if err != nil {
			t.Fatal(err)
		}
		e.m.set(id, true, tr.Addr().String())
		return tr
	}
	e.client = mk("client")
	for id, pg := range backends {
		tr := mk(id)
		pg := pg
		tr.Handle(transport.StreamPG, tunnel.Backend(func() string { return pg }, time.Second))
		e.servers[id] = tr
	}
	fw, err := tunnel.NewForwarder(tunnel.ForwarderConfig{
		ListenAddr: "127.0.0.1:0",
		Dial: func(ctx context.Context, id string) (net.Conn, error) {
			return e.client.Dial(ctx, id, transport.StreamPG)
		},
		Target: "a",
	})
	if err != nil {
		t.Fatal(err)
	}
	e.fw = fw
	return e
}

func (e *env) close() {
	e.fw.Close()
	e.client.Close()
	for _, s := range e.servers {
		s.Close()
	}
}

// fakePG is a TCP server; handle runs per connection.
type fakePG struct {
	ln net.Listener
	wg sync.WaitGroup
}

func newFakePG(t *testing.T, handle func(net.Conn)) *fakePG {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakePG{ln: ln}
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			f.wg.Add(1)
			go func() {
				defer f.wg.Done()
				defer c.Close()
				handle(c)
			}()
		}
	}()
	return f
}

func (f *fakePG) addr() string { return f.ln.Addr().String() }
func (f *fakePG) close()       { f.ln.Close(); f.wg.Wait() }

func echo(c net.Conn) { io.Copy(c, c) }

// hashReply reads to EOF (needs half-close to work end to end), then writes
// the sha256 and byte count back.
func hashReply(c net.Conn) {
	h := sha256.New()
	n, err := io.Copy(h, c)
	if err != nil {
		return
	}
	c.Write(h.Sum(nil))
	var nb [8]byte
	for i := range 8 {
		nb[i] = byte(n >> (8 * i))
	}
	c.Write(nb[:])
}

func waitGoroutines(t *testing.T, base int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for runtime.NumGoroutine() > base {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			buf = buf[:runtime.Stack(buf, true)]
			t.Fatalf("goroutine leak: %d > %d\n%s", runtime.NumGoroutine(), base, buf)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestEchoThroughTunnel(t *testing.T) {
	base := runtime.NumGoroutine()
	pg := newFakePG(t, echo)
	e := newEnv(t, map[string]string{"a": pg.addr()})
	c, err := net.Dial("tcp", e.fw.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	for i := range 100 {
		msg := []byte{byte(i), 'x', 'y'}
		c.Write(msg)
		got := make([]byte, 3)
		if _, err := io.ReadFull(c, got); err != nil || !bytes.Equal(got, msg) {
			t.Fatalf("round %d: %v", i, err)
		}
	}
	// Half-close from the client: the echo server sees EOF and closes,
	// and we see EOF.
	c.(*net.TCPConn).CloseWrite()
	if b, err := io.ReadAll(c); err != nil || len(b) != 0 {
		t.Fatalf("after half-close: %q %v", b, err)
	}
	c.Close()
	e.close()
	pg.close()
	waitGoroutines(t, base)
}

func TestLargeTransferWithBackpressure(t *testing.T) {
	const total = 256 << 20
	base := runtime.NumGoroutine()
	release := make(chan struct{})
	pg := newFakePG(t, func(c net.Conn) {
		<-release // do not read until told: the sender must block
		hashReply(c)
	})
	e := newEnv(t, map[string]string{"a": pg.addr()})
	c, err := net.Dial("tcp", e.fw.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	var sent atomic.Int64
	want := sha256.New()
	writeErr := make(chan error, 1)
	go func() {
		rng := rand.NewChaCha8([32]byte{7})
		buf := make([]byte, 256<<10)
		for sent.Load() < total {
			rng.Read(buf)
			want.Write(buf)
			n, err := c.Write(buf)
			sent.Add(int64(n))
			if err != nil {
				writeErr <- err
				return
			}
		}
		writeErr <- c.(*net.TCPConn).CloseWrite()
	}()

	// Backpressure: with the backend not reading, the sender stalls after
	// filling socket and copy buffers; nothing buffers without bound.
	var last int64 = -1
	stableSince := time.Now()
	deadline := time.Now().Add(30 * time.Second)
	for {
		cur := sent.Load()
		if cur != last {
			last, stableSince = cur, time.Now()
		} else if time.Since(stableSince) > 300*time.Millisecond {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sender never stalled (sent %d)", cur)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if last >= 64<<20 {
		t.Fatalf("sender pushed %d MB with backend not reading: unbounded buffering", last>>20)
	}
	t.Logf("sender stalled at %d KB while backend paused", last>>10)
	close(release)

	reply := make([]byte, 40)
	c.SetReadDeadline(time.Now().Add(5 * time.Minute))
	if _, err := io.ReadFull(c, reply); err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if err := <-writeErr; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(reply[:32], want.Sum(nil)) {
		t.Fatal("hash mismatch")
	}
	var n int64
	for i := range 8 {
		n |= int64(reply[32+i]) << (8 * i)
	}
	if n != total {
		t.Fatalf("backend got %d bytes, want %d", n, total)
	}
	c.Close()
	e.close()
	pg.close()
	waitGoroutines(t, base)
}

func TestDownloadDirection(t *testing.T) {
	const total = 64 << 20
	pg := newFakePG(t, func(c net.Conn) {
		rng := rand.NewChaCha8([32]byte{9})
		buf := make([]byte, 128<<10)
		for sent := 0; sent < total; sent += len(buf) {
			rng.Read(buf)
			if _, err := c.Write(buf); err != nil {
				return
			}
		}
	})
	e := newEnv(t, map[string]string{"a": pg.addr()})
	defer pg.close()
	defer e.close()
	c, err := net.Dial("tcp", e.fw.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	h := sha256.New()
	n, err := io.Copy(h, c)
	if err != nil || n != total {
		t.Fatalf("got %d bytes: %v", n, err)
	}
	want := sha256.New()
	rng := rand.NewChaCha8([32]byte{9})
	buf := make([]byte, 128<<10)
	for sent := 0; sent < total; sent += len(buf) {
		rng.Read(buf)
		want.Write(buf)
	}
	if !bytes.Equal(h.Sum(nil), want.Sum(nil)) {
		t.Fatal("hash mismatch")
	}
}

func TestRepointClosesOldConnections(t *testing.T) {
	base := runtime.NumGoroutine()
	hello := func(name string) func(net.Conn) {
		return func(c net.Conn) {
			c.Write([]byte(name))
			io.Copy(io.Discard, c)
		}
	}
	pgA := newFakePG(t, hello("A"))
	pgC := newFakePG(t, hello("C"))
	e := newEnv(t, map[string]string{"a": pgA.addr(), "c": pgC.addr()})

	read1 := func(c net.Conn) string {
		b := make([]byte, 1)
		c.SetReadDeadline(time.Now().Add(10 * time.Second))
		if _, err := io.ReadFull(c, b); err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	old, err := net.Dial("tcp", e.fw.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if got := read1(old); got != "A" {
		t.Fatalf("first target %q", got)
	}
	e.fw.SetTarget("c")
	old.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := old.Read(make([]byte, 1)); err == nil {
		t.Fatal("old connection still open after re-point")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("old connection not closed")
	}
	old.Close()
	nc, err := net.Dial("tcp", e.fw.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if got := read1(nc); got != "C" {
		t.Fatalf("after re-point got %q", got)
	}
	if e.fw.Target() != "c" {
		t.Fatal("target not updated")
	}
	// Empty target refuses.
	e.fw.SetTarget("")
	nc.Close()
	r, err := net.Dial("tcp", e.fw.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	r.SetReadDeadline(time.Now().Add(10 * time.Second))
	if n, err := r.Read(make([]byte, 1)); n != 0 || err == nil {
		t.Fatal("connection accepted with no target")
	}
	r.Close()
	e.close()
	pgA.close()
	pgC.close()
	waitGoroutines(t, base)
}

func TestBackendDownGivesCleanError(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := ln.Addr().String()
	ln.Close()
	e := newEnv(t, map[string]string{"a": dead})
	defer e.close()
	// Direct dial: refused with a message.
	_, err := e.client.Dial(context.Background(), "a", transport.StreamPG)
	var re *transport.RefusedError
	if !errors.As(err, &re) || re.Denied {
		t.Fatalf("want refusal, got %v", err)
	}
	t.Logf("error seen by dialer: %v", err)
	// Through the forwarder: the local client is closed promptly.
	c, err := net.Dial("tcp", e.fw.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("expected close")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("forwarder kept a dead tunnel open")
	}
}

func TestRemovedMemberCannotOpenTunnel(t *testing.T) {
	pg := newFakePG(t, echo)
	defer pg.close()
	e := newEnv(t, map[string]string{"a": pg.addr()})
	defer e.close()
	c, err := net.Dial("tcp", e.fw.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("x"))
	if _, err := io.ReadFull(c, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	// "client" is removed from membership: the live tunnel is cut and new
	// ones are refused by node a.
	e.m.mu.Lock()
	e.m.ok["client"] = false
	e.m.mu.Unlock()
	if e.servers["a"].DropUnauthorized() == 0 {
		t.Fatal("nothing dropped")
	}
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("tunnel of removed member still open")
	}
	_, err = e.client.DialAddr(context.Background(), e.servers["a"].Addr().String(), "a", transport.StreamPG)
	var re *transport.RefusedError
	if !errors.As(err, &re) || !re.Denied {
		t.Fatalf("want denied, got %v", err)
	}
}

// TestRefusedClientSeesOrderlyClose: a client that already sent its
// startup bytes when the forwarder gives up must see an orderly end (FIN,
// read as EOF), not a reset. A reset is sent once; if it is lost the
// client waits on a dead connection (D57, finding N1).
func TestRefusedClientSeesOrderlyClose(t *testing.T) {
	fw, err := tunnel.NewForwarder(tunnel.ForwarderConfig{
		ListenAddr: "127.0.0.1:0",
		Target:     "a",
		Dial: func(ctx context.Context, _ string) (net.Conn, error) {
			<-time.After(200 * time.Millisecond) // the client's bytes arrive first
			return nil, errors.New("main node unreachable")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer fw.Close()
	for _, target := range []string{"a", ""} {
		fw.SetTarget(target)
		c, err := net.Dial("tcp", fw.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Write(make([]byte, 88)); err != nil {
			t.Fatal(err)
		}
		c.SetReadDeadline(time.Now().Add(10 * time.Second))
		_, err = c.Read(make([]byte, 1))
		c.Close()
		if !errors.Is(err, io.EOF) {
			t.Fatalf("target %q: want an orderly close (EOF), got %v", target, err)
		}
	}
}
