package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---- fakes ----

type fakeRouter struct {
	mu sync.Mutex
	v  View
	ch chan struct{}
}

func newRouter(epoch uint64, primary string) *fakeRouter {
	return &fakeRouter{v: View{Epoch: epoch, Primary: primary, LastContact: time.Now()}, ch: make(chan struct{}, 1)}
}

func (r *fakeRouter) View() View {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.v
}

func (r *fakeRouter) Changed() <-chan struct{} { return r.ch }

func (r *fakeRouter) set(f func(v *View)) {
	r.mu.Lock()
	f(&r.v)
	r.mu.Unlock()
	select {
	case r.ch <- struct{}{}:
	default:
	}
}

// keepContact refreshes LastContact until the test ends (the agent does
// this every renew interval).
func (r *fakeRouter) keepContact(t *testing.T, every time.Duration) {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		tk := time.NewTicker(every)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				r.mu.Lock()
				r.v.LastContact = time.Now()
				r.mu.Unlock()
			}
		}
	}()
	t.Cleanup(func() { close(stop); <-done })
}

// backend is a fake Postgres: a TCP server that greets with its name and
// then echoes. It counts and can close its live connections.
type backend struct {
	name string
	ln   net.Listener
	mu   sync.Mutex
	live map[net.Conn]struct{}
	n    atomic.Int64
	wg   sync.WaitGroup
	// mode: "echo" (default) or "halfclose" (read all, then reply and close)
	mode string
}

func newBackend(t *testing.T, name string) *backend { return newBackendMode(t, name, "echo") }

func newBackendMode(t *testing.T, name, mode string) *backend {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	b := &backend{name: name, ln: ln, live: map[net.Conn]struct{}{}, mode: mode}
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			b.n.Add(1)
			b.mu.Lock()
			b.live[c] = struct{}{}
			b.mu.Unlock()
			b.wg.Add(1)
			go func() {
				defer b.wg.Done()
				defer func() {
					b.mu.Lock()
					delete(b.live, c)
					b.mu.Unlock()
					c.Close()
				}()
				if _, err := io.WriteString(c, b.name+"\n"); err != nil {
					return
				}
				if b.mode == "halfclose" {
					data, err := io.ReadAll(c)
					if err != nil {
						return
					}
					sum := sha256.Sum256(data)
					fmt.Fprintf(c, "%x\n", sum)
					return
				}
				io.Copy(c, c)
			}()
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		b.mu.Lock()
		for c := range b.live {
			c.Close()
		}
		b.mu.Unlock()
		b.wg.Wait()
	})
	return b
}

func (b *backend) liveConns() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.live)
}

type fakeNet struct {
	mu       sync.Mutex
	backends map[string]*backend
	block    map[string]chan struct{} // dial waits for close
	fail     map[string]bool
	dials    atomic.Int64
}

func newNet(bs ...*backend) *fakeNet {
	n := &fakeNet{backends: map[string]*backend{}, block: map[string]chan struct{}{}, fail: map[string]bool{}}
	for _, b := range bs {
		n.backends[b.name] = b
	}
	return n
}

func (n *fakeNet) dial(ctx context.Context, node string) (net.Conn, error) {
	n.dials.Add(1)
	n.mu.Lock()
	b, blk, fail := n.backends[node], n.block[node], n.fail[node]
	n.mu.Unlock()
	if blk != nil {
		select {
		case <-blk:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if b == nil || fail {
		return nil, errors.New("no route to " + node)
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", b.ln.Addr().String())
}

// fakeVerifier answers from a table; it reads the greeting line the
// backend sends so it exercises the real connection.
type fakeVerifier struct {
	mu         sync.Mutex
	inRecovery map[string]bool
	calls      map[string]int
}

func newVerifier() *fakeVerifier {
	return &fakeVerifier{inRecovery: map[string]bool{}, calls: map[string]int{}}
}

func (v *fakeVerifier) InRecovery(ctx context.Context, c net.Conn) (bool, error) {
	line := make([]byte, 0, 32)
	var b [1]byte
	for {
		if _, err := c.Read(b[:]); err != nil {
			return false, err
		}
		if b[0] == '\n' {
			break
		}
		line = append(line, b[0])
	}
	name := string(line)
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls[name]++
	return v.inRecovery[name], nil
}

func (v *fakeVerifier) set(name string, inRecovery bool) {
	v.mu.Lock()
	v.inRecovery[name] = inRecovery
	v.mu.Unlock()
}

func (v *fakeVerifier) count(name string) int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.calls[name]
}

// ---- harness ----

type harness struct {
	t   *testing.T
	p   *Proxy
	r   *fakeRouter
	net *fakeNet
	v   *fakeVerifier
}

func start(t *testing.T, r *fakeRouter, n *fakeNet, v *fakeVerifier, mod func(*Config)) *harness {
	t.Helper()
	checkLeaks(t)
	cfg := Config{
		ListenAddrs:    []string{"127.0.0.1:0"},
		Router:         r,
		Dial:           n.dial,
		Verifier:       v,
		Lease:          time.Second,
		WaitForPrimary: 2 * time.Second,
		RetryInterval:  20 * time.Millisecond,
		CheckInterval:  10 * time.Millisecond,
	}
	if mod != nil {
		mod(&cfg)
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return &harness{t: t, p: p, r: r, net: n, v: v}
}

func (h *harness) addr() string { return h.p.Addrs()[0].String() }

// connect dials the proxy and returns the conn and the greeting (backend
// name), or "" with the error if the proxy closed the connection.
func (h *harness) connect() (net.Conn, string, error) {
	h.t.Helper()
	c, err := net.DialTimeout("tcp", h.addr(), time.Second)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { c.Close() })
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := readLine(c)
	_ = c.SetReadDeadline(time.Time{})
	return c, line, err
}

func (h *harness) mustConnect(want string) net.Conn {
	h.t.Helper()
	c, got, err := h.connect()
	if err != nil {
		h.t.Fatalf("connect: %v", err)
	}
	if got != want {
		h.t.Fatalf("landed on %q, want %q", got, want)
	}
	return c
}

func readLine(c net.Conn) (string, error) {
	var out []byte
	var b [1]byte
	for {
		n, err := c.Read(b[:])
		if n == 1 {
			if b[0] == '\n' {
				return string(out), nil
			}
			out = append(out, b[0])
		}
		if err != nil {
			return string(out), err
		}
	}
}

// waitClosed asserts that the peer closed c within d.
func waitClosed(t *testing.T, c net.Conn, d time.Duration) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(d))
	buf := make([]byte, 64)
	for {
		_, err := c.Read(buf)
		if err == nil {
			continue
		}
		if errors.Is(err, net.ErrClosed) {
			t.Fatal("test closed its own conn")
		}
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			t.Fatalf("connection still open after %v", d)
		}
		return // EOF or reset
	}
}

func eventually(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for: %s", d, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// checkLeaks fails the test if goroutines of this package outlive it. It
// is registered first, so it runs after every other cleanup.
func checkLeaks(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		deadline := time.Now().Add(5 * time.Second)
		for {
			leaked := proxyGoroutines()
			if leaked == "" {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("goroutines leaked:\n%s", leaked)
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
}

func proxyGoroutines() string {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	var out []string
	for _, g := range strings.Split(string(buf), "\n\n") {
		for _, fn := range []string{"proxy.(*Proxy)", "proxy.(*session)", "proxy.(*hold)"} {
			if strings.Contains(g, fn) {
				out = append(out, g)
				break
			}
		}
	}
	return strings.Join(out, "\n\n")
}

// ---- tests ----

func TestEchoAndLargeTransfer(t *testing.T) {
	a := newBackend(t, "A")
	h := start(t, newRouter(1, "A"), newNet(a), newVerifier(), nil)
	h.r.keepContact(t, 100*time.Millisecond)
	c := h.mustConnect("A")

	const size = 24 << 20
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(data)
	errc := make(chan error, 1)
	go func() {
		_, err := io.Copy(c, bytes.NewReader(data))
		errc <- err
	}()
	hasher := sha256.New()
	if _, err := io.CopyN(hasher, c, size); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if got := hasher.Sum(nil); !bytes.Equal(got, want[:]) {
		t.Fatal("data corrupted through the proxy")
	}
	st := h.p.Stats()
	if st.Active != 1 || st.Forwarded != 1 || st.Epoch != 1 || st.Primary != "A" || !st.Verified || !st.Trusted {
		t.Fatalf("stats: %+v", st)
	}
	c.Close()
	eventually(t, 2*time.Second, "session gone", func() bool { return h.p.Stats().Active == 0 && a.liveConns() == 0 })
}

func TestHalfClose(t *testing.T) {
	a := newBackendMode(t, "A", "halfclose")
	h := start(t, newRouter(1, "A"), newNet(a), newVerifier(), nil)
	h.r.keepContact(t, 100*time.Millisecond)
	// Verify reads the greeting and then closes; the backend's ReadAll
	// sees EOF and replies into a closed socket. Harmless.
	c := h.mustConnect("A")
	payload := bytes.Repeat([]byte("x"), 1<<20)
	if _, err := c.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := c.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := readLine(c)
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("%x", sha256.Sum256(payload)); line != want {
		t.Fatalf("reply after half-close = %q, want %q", line, want)
	}
	if _, err := c.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("want EOF after upstream closed, got %v", err)
	}
}

func TestEpochChangeDropsAllAndReroutes(t *testing.T) {
	a, b := newBackend(t, "A"), newBackend(t, "B")
	h := start(t, newRouter(5, "A"), newNet(a, b), newVerifier(), nil)
	h.r.keepContact(t, 100*time.Millisecond)

	const n = 20
	conns := make([]net.Conn, n)
	for i := range conns {
		conns[i] = h.mustConnect("A")
	}
	eventually(t, time.Second, "all piped", func() bool { return a.liveConns() == n })

	start := time.Now()
	h.r.set(func(v *View) { v.Epoch, v.Primary = 6, "B" })
	var wg sync.WaitGroup
	for _, c := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			waitClosed(t, c, 500*time.Millisecond)
		}()
	}
	wg.Wait()
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("drop took %v", d)
	}
	eventually(t, time.Second, "upstream side closed too", func() bool { return a.liveConns() == 0 })
	if st := h.p.Stats(); st.DroppedOnEpoch != n {
		t.Fatalf("DroppedOnEpoch = %d, want %d", st.DroppedOnEpoch, n)
	}
	h.mustConnect("B")
	if a.liveConns() != 0 {
		t.Fatal("a connection still reached the old primary")
	}
}

func TestSameEpochPrimaryChangeAlsoDrops(t *testing.T) {
	a, b := newBackend(t, "A"), newBackend(t, "B")
	h := start(t, newRouter(5, "A"), newNet(a, b), newVerifier(), nil)
	h.r.keepContact(t, 100*time.Millisecond)
	c := h.mustConnect("A")
	h.r.set(func(v *View) { v.Primary = "B" })
	waitClosed(t, c, 500*time.Millisecond)
	h.mustConnect("B")
}

func TestOlderEpochIgnored(t *testing.T) {
	a, b := newBackend(t, "A"), newBackend(t, "B")
	h := start(t, newRouter(5, "A"), newNet(a, b), newVerifier(), nil)
	h.r.keepContact(t, 100*time.Millisecond)
	h.r.set(func(v *View) { v.Epoch, v.Primary = 4, "B" })
	h.p.refresh()
	h.mustConnect("A")
	if st := h.p.Stats(); st.Epoch != 5 || st.Primary != "A" {
		t.Fatalf("stats after older epoch: %+v", st)
	}
}

func TestEpochChangeWhileDialing(t *testing.T) {
	a, b := newBackend(t, "A"), newBackend(t, "B")
	n := newNet(a, b)
	h := start(t, newRouter(1, "A"), n, newVerifier(), nil)
	h.r.keepContact(t, 100*time.Millisecond)
	h.mustConnect("A").Close() // A verified
	blk := make(chan struct{})
	n.mu.Lock()
	n.block["A"] = blk
	n.mu.Unlock()

	type res struct {
		name string
		err  error
	}
	out := make(chan res, 1)
	go func() {
		c, err := net.Dial("tcp", h.addr())
		if err != nil {
			out <- res{"", err}
			return
		}
		defer c.Close()
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		s, err := readLine(c)
		out <- res{s, err}
	}()
	eventually(t, time.Second, "dial to A in progress", func() bool { return n.dials.Load() >= 3 })
	h.r.set(func(v *View) { v.Epoch, v.Primary = 2, "B" })
	h.p.refresh()
	close(blk) // the A dial completes after the route moved
	r := <-out
	if r.err != nil || r.name != "B" {
		t.Fatalf("got %q, %v; want the client re-routed to B", r.name, r.err)
	}
	eventually(t, time.Second, "A has no connections", func() bool { return a.liveConns() == 0 })
}

func TestNoViewRefusesAndDrops(t *testing.T) {
	a := newBackend(t, "A")
	r := newRouter(1, "A")
	h := start(t, r, newNet(a), newVerifier(), func(c *Config) { c.Lease = 300 * time.Millisecond })
	c := h.mustConnect("A")

	// No contact renewal: the view ages out after the lease.
	waitClosed(t, c, 2*time.Second)
	if st := h.p.Stats(); st.DroppedNoView != 1 || st.Trusted {
		t.Fatalf("stats: %+v", st)
	}
	eventually(t, time.Second, "upstream closed", func() bool { return a.liveConns() == 0 })

	t0 := time.Now()
	_, _, err := h.connect()
	if err == nil {
		t.Fatal("a cut-off node accepted a client")
	}
	if d := time.Since(t0); d > 300*time.Millisecond {
		t.Fatalf("refusal took %v; must be prompt", d)
	}
	if st := h.p.Stats(); st.RefusedNoView != 1 {
		t.Fatalf("RefusedNoView = %d", st.RefusedNoView)
	}

	// Contact comes back: clients are served again.
	r.set(func(v *View) { v.LastContact = time.Now() })
	r.keepContact(t, 50*time.Millisecond)
	h.mustConnect("A")
}

func TestNeverContactedRefuses(t *testing.T) {
	a := newBackend(t, "A")
	r := newRouter(1, "A")
	r.v.LastContact = time.Time{}
	h := start(t, r, newNet(a), newVerifier(), nil)
	if _, _, err := h.connect(); err == nil {
		t.Fatal("served without ever having a view")
	}
	if n := a.n.Load(); n != 0 {
		t.Fatalf("backend saw %d connections", n)
	}
}

func TestViewLeaseOverride(t *testing.T) {
	a := newBackend(t, "A")
	r := newRouter(1, "A")
	r.v.LastContact = time.Now().Add(-2 * time.Second)
	r.v.Lease = 5 * time.Second // longer than Config.Lease (1 s)
	h := start(t, r, newNet(a), newVerifier(), nil)
	h.mustConnect("A")
}

func TestInRecoveryNotUsedUntilPromoted(t *testing.T) {
	a, b := newBackend(t, "A"), newBackend(t, "B")
	v := newVerifier()
	v.set("B", true)
	h := start(t, newRouter(1, "A"), newNet(a, b), v, nil)
	h.r.keepContact(t, 100*time.Millisecond)
	h.r.set(func(w *View) { w.Epoch, w.Primary = 2, "B" })

	got := make(chan string, 1)
	go func() {
		c, err := net.Dial("tcp", h.addr())
		if err != nil {
			got <- err.Error()
			return
		}
		defer c.Close()
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		s, err := readLine(c)
		if err != nil {
			s = "error: " + err.Error()
		}
		got <- s
	}()
	eventually(t, 2*time.Second, "client waiting and B checked twice", func() bool {
		return h.p.Stats().Waiting == 1 && v.count("B") >= 2
	})
	select {
	case s := <-got:
		t.Fatalf("client was served (%q) by a standby", s)
	default:
	}
	if b.n.Load() > int64(v.count("B"))+1 { // at most one check in flight
		t.Fatalf("B got %d connections but only %d checks: a client reached a standby", b.n.Load(), v.count("B"))
	}
	v.set("B", false) // promoted
	if s := <-got; s != "B" {
		t.Fatalf("after promotion got %q", s)
	}
	st := h.p.Stats()
	if st.Waited != 1 || st.VerifyFailures < 2 || !st.Verified {
		t.Fatalf("stats: %+v", st)
	}
}

func TestVerifyCachedPerEpoch(t *testing.T) {
	a := newBackend(t, "A")
	v := newVerifier()
	h := start(t, newRouter(1, "A"), newNet(a), v, nil)
	h.r.keepContact(t, 100*time.Millisecond)
	for range 10 {
		h.mustConnect("A").Close()
	}
	if n := v.count("A"); n != 1 {
		t.Fatalf("verified %d times in one epoch, want 1", n)
	}
	h.r.set(func(w *View) { w.Epoch = 2 })
	h.mustConnect("A")
	if n := v.count("A"); n != 2 {
		t.Fatalf("verified %d times over two epochs, want 2", n)
	}
}

func TestWaitForPrimary(t *testing.T) {
	a := newBackend(t, "A")
	h := start(t, newRouter(3, ""), newNet(a), newVerifier(), nil)
	h.r.keepContact(t, 50*time.Millisecond)
	got := make(chan string, 1)
	go func() {
		c, err := net.Dial("tcp", h.addr())
		if err != nil {
			got <- err.Error()
			return
		}
		defer c.Close()
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		s, _ := readLine(c)
		got <- s
	}()
	eventually(t, time.Second, "client waiting", func() bool { return h.p.Stats().Waiting == 1 })
	h.r.set(func(v *View) { v.Epoch, v.Primary = 4, "A" })
	if s := <-got; s != "A" {
		t.Fatalf("got %q", s)
	}
	if st := h.p.Stats(); st.Waited != 1 || st.Waiting != 0 || st.WaitTimeouts != 0 {
		t.Fatalf("stats: %+v", st)
	}
}

func TestWaitForPrimaryTimeout(t *testing.T) {
	h := start(t, newRouter(3, ""), newNet(), newVerifier(), func(c *Config) { c.WaitForPrimary = 200 * time.Millisecond })
	h.r.keepContact(t, 50*time.Millisecond)
	t0 := time.Now()
	_, _, err := h.connect()
	d := time.Since(t0)
	if err == nil {
		t.Fatal("served with no primary")
	}
	if d < 180*time.Millisecond || d > time.Second {
		t.Fatalf("closed after %v, want about 200ms", d)
	}
	if st := h.p.Stats(); st.WaitTimeouts != 1 || st.Waiting != 0 {
		t.Fatalf("stats: %+v", st)
	}
}

func TestPrimaryUnreachableWaitsThenTimesOut(t *testing.T) {
	a := newBackend(t, "A")
	n := newNet(a)
	h := start(t, newRouter(1, "A"), n, newVerifier(), func(c *Config) { c.WaitForPrimary = 300 * time.Millisecond })
	h.r.keepContact(t, 50*time.Millisecond)
	h.mustConnect("A").Close()
	n.mu.Lock()
	n.fail["A"] = true
	n.mu.Unlock()
	if _, _, err := h.connect(); err == nil {
		t.Fatal("served with the primary unreachable")
	}
	st := h.p.Stats()
	if st.UpstreamErrors < 2 || st.WaitTimeouts != 1 {
		t.Fatalf("stats: %+v", st)
	}
}

func TestHeldClientHangUpFreesSlot(t *testing.T) {
	h := start(t, newRouter(1, ""), newNet(), newVerifier(), func(c *Config) { c.WaitForPrimary = 10 * time.Second })
	h.r.keepContact(t, 50*time.Millisecond)
	c, err := net.Dial("tcp", h.addr())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("startup")); err != nil {
		t.Fatal(err)
	}
	eventually(t, time.Second, "waiting", func() bool { return h.p.Stats().Waiting == 1 })
	c.Close()
	eventually(t, time.Second, "slot freed after hang-up", func() bool { st := h.p.Stats(); return st.Waiting == 0 })
}

func TestHeldBytesReachUpstream(t *testing.T) {
	a := newBackend(t, "A")
	h := start(t, newRouter(1, ""), newNet(a), newVerifier(), nil)
	h.r.keepContact(t, 50*time.Millisecond)
	c, err := net.Dial("tcp", h.addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// The client speaks first (like a Postgres startup packet) while held.
	if _, err := c.Write([]byte("hello-1\n")); err != nil {
		t.Fatal(err)
	}
	eventually(t, time.Second, "waiting", func() bool { return h.p.Stats().Waiting == 1 })
	h.r.set(func(v *View) { v.Primary = "A" })
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	for _, want := range []string{"A", "hello-1"} {
		got, err := readLine(c)
		if err != nil || got != want {
			t.Fatalf("got %q, %v; want %q", got, err, want)
		}
	}
	if _, err := c.Write([]byte("hello-2\n")); err != nil {
		t.Fatal(err)
	}
	if got, err := readLine(c); err != nil || got != "hello-2" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestConnectionLimit(t *testing.T) {
	a := newBackend(t, "A")
	h := start(t, newRouter(1, "A"), newNet(a), newVerifier(), func(c *Config) { c.MaxConns = 2 })
	h.r.keepContact(t, 50*time.Millisecond)
	h.mustConnect("A")
	h.mustConnect("A")
	if _, _, err := h.connect(); err == nil {
		t.Fatal("third connection accepted over MaxConns=2")
	}
	if st := h.p.Stats(); st.RefusedLimit != 1 || st.Active != 2 {
		t.Fatalf("stats: %+v", st)
	}
}

func TestWaitQueueLimit(t *testing.T) {
	h := start(t, newRouter(1, ""), newNet(), newVerifier(), func(c *Config) { c.MaxWaiting = 1; c.WaitForPrimary = 5 * time.Second })
	h.r.keepContact(t, 50*time.Millisecond)
	c1, err := net.Dial("tcp", h.addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	eventually(t, time.Second, "first waiting", func() bool { return h.p.Stats().Waiting == 1 })
	t0 := time.Now()
	if _, _, err := h.connect(); err == nil {
		t.Fatal("second waiter accepted over MaxWaiting=1")
	}
	if d := time.Since(t0); d > time.Second {
		t.Fatalf("queue-full refusal took %v", d)
	}
	if st := h.p.Stats(); st.RefusedLimit != 1 || st.Waiting != 1 {
		t.Fatalf("stats: %+v", st)
	}
}

func TestShutdownGraceful(t *testing.T) {
	a := newBackend(t, "A")
	h := start(t, newRouter(1, "A"), newNet(a), newVerifier(), nil)
	h.r.keepContact(t, 50*time.Millisecond)
	c := h.mustConnect("A")
	// The client finishes on its own during the grace period.
	go func() {
		time.Sleep(50 * time.Millisecond)
		c.Close()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.p.Shutdown(ctx); err != nil {
		t.Fatalf("graceful shutdown: %v", err)
	}
	if _, err := net.DialTimeout("tcp", h.addr(), 200*time.Millisecond); err == nil {
		t.Fatal("still accepting after shutdown")
	}
	if err := h.p.Shutdown(ctx); !errors.Is(err, ErrClosed) {
		t.Fatalf("second shutdown: %v", err)
	}
}

func TestShutdownDeadline(t *testing.T) {
	a := newBackend(t, "A")
	h := start(t, newRouter(1, "A"), newNet(a), newVerifier(), nil)
	h.r.keepContact(t, 50*time.Millisecond)
	conns := []net.Conn{h.mustConnect("A"), h.mustConnect("A")}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	t0 := time.Now()
	err := h.p.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(t0); d > time.Second {
		t.Fatalf("shutdown took %v", d)
	}
	for _, c := range conns {
		waitClosed(t, c, time.Second)
	}
	eventually(t, time.Second, "upstreams closed", func() bool { return a.liveConns() == 0 })
}

func TestShutdownReleasesWaiters(t *testing.T) {
	h := start(t, newRouter(1, ""), newNet(), newVerifier(), func(c *Config) { c.WaitForPrimary = 30 * time.Second })
	h.r.keepContact(t, 50*time.Millisecond)
	c, err := net.Dial("tcp", h.addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	eventually(t, time.Second, "waiting", func() bool { return h.p.Stats().Waiting == 1 })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	t0 := time.Now()
	if err := h.p.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(t0); d > time.Second {
		t.Fatalf("shutdown with a waiter took %v", d)
	}
	waitClosed(t, c, time.Second)
}

func TestChurnNoLeaks(t *testing.T) {
	a, b := newBackend(t, "A"), newBackend(t, "B")
	h := start(t, newRouter(1, "A"), newNet(a, b), newVerifier(), func(c *Config) { c.WaitForPrimary = 500 * time.Millisecond })
	h.r.keepContact(t, 20*time.Millisecond)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Bounded: each connection leaves sockets in TIME_WAIT, and the
			// macOS ephemeral port range is small.
			for range 60 {
				select {
				case <-stop:
					return
				default:
				}
				c, err := net.Dial("tcp", h.addr())
				if err != nil {
					continue
				}
				_ = c.SetDeadline(time.Now().Add(2 * time.Second))
				if _, err := readLine(c); err == nil {
					io.WriteString(c, "ping\n")
					readLine(c)
				}
				c.Close()
			}
		}()
	}
	for i := range 20 {
		time.Sleep(15 * time.Millisecond)
		h.r.set(func(v *View) {
			v.Epoch++
			switch i % 3 {
			case 0:
				v.Primary = "B"
			case 1:
				v.Primary = ""
			default:
				v.Primary = "A"
			}
		})
	}
	close(stop)
	wg.Wait()
	if err := h.p.Close(); err != nil {
		t.Fatal(err)
	}
	if st := h.p.Stats(); st.Active != 0 || st.Waiting != 0 {
		t.Fatalf("after close: %+v", st)
	}
}

func TestUnderlyingTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	wrapped := wrapper{c}
	if underlyingTCP(wrapped) != c.(*net.TCPConn) {
		t.Fatal("did not unwrap NetConn()")
	}
	if underlyingTCP(noClose{c}) != nil {
		t.Fatal("unexpected unwrap")
	}
}

type wrapper struct{ net.Conn }

func (w wrapper) NetConn() net.Conn { return w.Conn }

func TestConfigValidation(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("empty config accepted")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, err = New(Config{ListenAddrs: []string{ln.Addr().String()}, Router: newRouter(1, ""), Dial: newNet().dial, Verifier: newVerifier()})
	if err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("busy port error = %v", err)
	}
}

// The hold for a new connection is derived from the lease (HoldLeases
// leases), not a fixed 10 s: a crash failover takes a full lease plus the
// election, the controller's margin and the promotion, and a connection
// opened just after the crash must still be held when the new primary is
// ready (2026-11-24 real-app demos, finding 1).
func TestHoldDerivedFromLease(t *testing.T) {
	const lease = 300 * time.Millisecond
	want := HoldLeases * lease
	for _, tc := range []struct {
		name string
		mod  func(*Config, *fakeRouter)
	}{
		{"config lease", func(c *Config, _ *fakeRouter) { c.Lease = lease }},
		{"view lease", func(c *Config, r *fakeRouter) { c.Lease = 5 * time.Second; r.v.Lease = lease }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRouter(3, "")
			h := start(t, r, newNet(), newVerifier(), func(c *Config) {
				c.WaitForPrimary = 0 // the default: derived
				tc.mod(c, r)
			})
			h.r.keepContact(t, 50*time.Millisecond)
			t0 := time.Now()
			_, _, err := h.connect()
			d := time.Since(t0)
			if err == nil {
				t.Fatal("served with no primary")
			}
			if d < want-20*time.Millisecond || d > want+time.Second {
				t.Fatalf("closed after %v, want about %v (%d leases)", d, want, HoldLeases)
			}
			if st := h.p.Stats(); st.WaitTimeouts != 1 || st.Waiting != 0 {
				t.Fatalf("stats: %+v", st)
			}
		})
	}
}

// A connection opened when the primary has just died is held past one full
// lease and released to the new primary when it appears (the crash
// failover shape: lease, then election and promotion).
func TestHeldAcrossCrashFailover(t *testing.T) {
	const lease = 300 * time.Millisecond
	a, b := newBackend(t, "A"), newBackend(t, "B")
	n := newNet(a, b)
	h := start(t, newRouter(1, "A"), n, newVerifier(), func(c *Config) {
		c.WaitForPrimary = 0
		c.Lease = lease
	})
	h.r.keepContact(t, 50*time.Millisecond)
	h.mustConnect("A").Close()
	n.mu.Lock()
	n.fail["A"] = true // A crashed; the cluster still names it until failover
	n.mu.Unlock()

	got := make(chan string, 1)
	go func() {
		c, err := net.Dial("tcp", h.addr())
		if err != nil {
			got <- err.Error()
			return
		}
		defer c.Close()
		_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
		s, _ := readLine(c)
		got <- s
	}()
	eventually(t, time.Second, "client waiting", func() bool { return h.p.Stats().Waiting == 1 })
	time.Sleep(2 * lease) // lease + election + promotion
	select {
	case s := <-got:
		t.Fatalf("client answered %q before the new primary existed", s)
	default:
	}
	h.r.set(func(v *View) { v.Epoch, v.Primary = 2, "B" })
	if s := <-got; s != "B" {
		t.Fatalf("got %q, want B", s)
	}
	if st := h.p.Stats(); st.WaitTimeouts != 0 || st.Waiting != 0 {
		t.Fatalf("stats: %+v", st)
	}
}

// Negative WaitForPrimary still disables holding.
func TestHoldDisabled(t *testing.T) {
	h := start(t, newRouter(3, ""), newNet(), newVerifier(), func(c *Config) { c.WaitForPrimary = -1 })
	h.r.keepContact(t, 50*time.Millisecond)
	t0 := time.Now()
	if _, _, err := h.connect(); err == nil {
		t.Fatal("served with no primary")
	}
	if d := time.Since(t0); d > 500*time.Millisecond {
		t.Fatalf("closed after %v, want at once", d)
	}
}
