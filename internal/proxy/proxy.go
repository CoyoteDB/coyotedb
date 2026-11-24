// Package proxy is the client-facing Postgres port of every node (safety
// rule 10, D8). It is a plain TCP passthrough: each client connection is
// piped, byte for byte, to the current primary's Postgres. Postgres' own TLS
// passes through untouched; the proxy never parses the client's bytes.
//
// Rule 10 as built here:
//
//   - On any change of epoch (or of the primary) every piped client
//     connection is closed, both directions, at once.
//   - A node whose last quorum contact is older than the lease has no
//     trustworthy view: new connections are refused promptly (so a
//     multi-host connection string moves on) and existing ones are dropped.
//   - Before an upstream is used in an epoch, a Verifier checks over a fresh
//     connection that it really is a primary (pg_is_in_recovery() = false).
//     The result is cached for that (epoch, node).
//   - TCP keepalives on client and upstream sockets. There is no idle
//     timeout: idle pooled connections are legitimate, dead peers are found
//     by keepalive.
//
// While there is no usable primary (failover in progress, or the new primary
// is not promoted yet), new connections are held in a bounded queue and
// released to the new primary when it appears. The hold lasts HoldLeases
// leases by default (30 s with the 10 s lease), so it covers a whole crash
// failover: a full lease, then the election, the controller's margin and the
// promotion. Holding never relaxes the rules above: a held client is only
// ever attached to the primary of the current epoch, once verified.
package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// View is the router's current answer to "where is the primary, and can I
// trust that answer".
type View struct {
	// Epoch of the cluster state this view comes from. It only grows.
	Epoch uint64
	// Primary is the node id of the current primary; "" while there is none.
	Primary string
	// LastContact is when this node last confirmed its cluster state with
	// a quorum, as a time.Now() reading (monotonic). For safety it should
	// be the moment the confirming request was sent, not when the answer
	// came back. Zero means never.
	LastContact time.Time
	// Lease bounds how old LastContact may be (the cluster's primary
	// lease). Zero means Config.Lease.
	Lease time.Duration
}

// Router tells the proxy where the primary is.
type Router interface {
	// View returns the current view. It is called often and must be cheap
	// and safe for concurrent use.
	View() View
	// Changed returns a channel signalled (coalesced) whenever View may
	// have changed. It may return nil; the proxy also polls View every
	// Config.CheckInterval, which is what notices LastContact ageing.
	Changed() <-chan struct{}
}

// DialFunc opens a byte stream to node nodeID's Postgres (the local port
// when nodeID is this node, a tunnel pg stream otherwise).
type DialFunc func(ctx context.Context, nodeID string) (net.Conn, error)

// Verifier checks the Postgres at the other end of conn. It must run only
// the minimal exchange needed for SELECT pg_is_in_recovery() and must not
// close conn (the proxy does). PGVerifier is the production implementation.
type Verifier interface {
	InRecovery(ctx context.Context, conn net.Conn) (bool, error)
}

// Defaults.
const (
	DefaultListen         = ":5432"
	DefaultLease          = 10 * time.Second // the cluster's default primary lease (D33)
	DefaultMaxConns       = 2000
	DefaultMaxWaiting     = 512
	DefaultConnectTimeout = 5 * time.Second
	DefaultVerifyTimeout  = 5 * time.Second
	DefaultRetryInterval  = 250 * time.Millisecond
	DefaultCheckInterval  = 100 * time.Millisecond
)

// HoldLeases is how many leases a new connection is held, by default, while
// there is no usable primary. A crash failover takes one full lease (the
// controller waits it out, safety rule 4) plus a Raft election if the leader
// died too, the controller's margin, and the promotion; the cluster refuses a
// lease shorter than the Raft leader lease plus 1 s, so with the 10 s default
// lease the other two leases (20 s) leave room for a slow promotion. A client
// that gives up sooner (its own connect timeout) frees its slot at once.
const HoldLeases = 3

// DefaultKeepAlive is used on client and upstream sockets: a silent peer is
// declared dead after about 30 s + 3 × 10 s.
var DefaultKeepAlive = net.KeepAliveConfig{Enable: true, Idle: 30 * time.Second, Interval: 10 * time.Second, Count: 3}

// Config configures New. Router, Dial and Verifier are required.
type Config struct {
	// ListenAddrs to accept clients on. Default [DefaultListen]. Ignored
	// when Listeners is set.
	ListenAddrs []string
	// Listeners, if set, are used instead of ListenAddrs (tests, or a
	// caller that wants to open the sockets itself). The proxy closes them.
	Listeners []net.Listener

	Router   Router
	Dial     DialFunc
	Verifier Verifier
	Logger   *slog.Logger

	// Lease is used when View.Lease is zero. Default DefaultLease.
	Lease time.Duration
	// WaitForPrimary is how long a new connection is held while there is
	// no usable primary. Zero (the default) means HoldLeases times the
	// lease of the current view; negative disables waiting (connections are
	// closed at once).
	WaitForPrimary time.Duration
	// MaxConns caps client connections in any state. Default DefaultMaxConns.
	MaxConns int
	// MaxWaiting caps connections held waiting for a primary. Default
	// DefaultMaxWaiting.
	MaxWaiting int
	// ConnectTimeout bounds each upstream dial. Default DefaultConnectTimeout.
	ConnectTimeout time.Duration
	// VerifyTimeout bounds one primary check (dial + exchange). Default
	// DefaultVerifyTimeout.
	VerifyTimeout time.Duration
	// RetryInterval paces re-verification and re-dial while clients wait.
	// Default DefaultRetryInterval.
	RetryInterval time.Duration
	// CheckInterval is how often View is polled. Default DefaultCheckInterval.
	CheckInterval time.Duration
	// KeepAlive for client and upstream TCP sockets. Zero value means
	// DefaultKeepAlive.
	KeepAlive net.KeepAliveConfig
	// RequireTLS refuses clients that do not open with TLS (see gate).
	RequireTLS bool
}

func (c *Config) normalize() error {
	if c.Router == nil || c.Dial == nil || c.Verifier == nil {
		return errors.New("proxy: Router, Dial and Verifier are required")
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.DiscardHandler)
	}
	if len(c.ListenAddrs) == 0 {
		c.ListenAddrs = []string{DefaultListen}
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	def(&c.Lease, DefaultLease)
	def(&c.ConnectTimeout, DefaultConnectTimeout)
	def(&c.VerifyTimeout, DefaultVerifyTimeout)
	def(&c.RetryInterval, DefaultRetryInterval)
	def(&c.CheckInterval, DefaultCheckInterval)
	if c.MaxConns <= 0 {
		c.MaxConns = DefaultMaxConns
	}
	if c.MaxWaiting <= 0 {
		c.MaxWaiting = DefaultMaxWaiting
	}
	if c.KeepAlive == (net.KeepAliveConfig{}) {
		c.KeepAlive = DefaultKeepAlive
	}
	return nil
}

// Stats is a snapshot of the proxy's counters.
type Stats struct {
	Active         int    `json:"active"`          // connections piped to a primary now
	Waiting        int    `json:"waiting"`         // connections held for a primary now
	Total          uint64 `json:"total"`           // client connections accepted
	Forwarded      uint64 `json:"forwarded"`       // connections that reached a primary
	DroppedOnEpoch uint64 `json:"dropped_epoch"`   // closed because the epoch or primary changed
	DroppedNoView  uint64 `json:"dropped_no_view"` // closed because the node lost its view
	RefusedNoView  uint64 `json:"refused_no_view"` // refused at connect: node cut off
	RefusedLimit   uint64 `json:"refused_limit"`   // refused: MaxConns or MaxWaiting reached
	Waited         uint64 `json:"waited"`          // connections that had to wait for a primary
	WaitTimeouts   uint64 `json:"wait_timeouts"`   // closed because no primary appeared in time
	UpstreamErrors uint64 `json:"upstream_errors"` // failed upstream dials
	VerifyFailures uint64 `json:"verify_failures"` // primary checks that failed or found a standby
	RefusedPlain   uint64 `json:"refused_plain"`   // refused: the client did not start TLS (RequireTLS)

	Epoch    uint64 `json:"epoch"`
	Primary  string `json:"primary"`
	Verified bool   `json:"verified"` // the primary passed the check in this epoch
	Trusted  bool   `json:"trusted"`  // this node has a trustworthy view
}

type counters struct {
	total, forwarded, droppedEpoch, droppedNoView, refusedNoView,
	refusedLimit, waited, waitTimeouts, upstreamErrors, verifyFailures, refusedPlain atomic.Uint64
}

type verifyKey struct {
	epoch uint64
	node  string
}

type verifyState struct {
	ok       bool
	inflight bool
	tried    bool
	lastTry  time.Time
	warned   bool
}

// Proxy is a running client listener. Create with New; stop with Shutdown
// or Close.
type Proxy struct {
	cfg   Config
	log   *slog.Logger
	lns   []net.Listener
	ctx   context.Context // cancelled when the proxy is fully closing
	stop  context.CancelFunc
	cnt   counters
	refMu sync.Mutex // serialises refresh (View read + apply)

	mu       sync.Mutex
	view     View
	trusted  bool
	verify   map[verifyKey]*verifyState
	sessions map[*session]struct{}
	conns    int // client connections in any state
	waiting  int
	wake     chan struct{} // closed and replaced on every change
	closing  bool
	drained  chan struct{} // closed when closing and conns == 0

	lastTimeoutLog atomic.Int64 // unix nanos of the last wait-timeout warning

	wg sync.WaitGroup
}

// ErrClosed is returned by Shutdown on a proxy already shut down.
var ErrClosed = errors.New("proxy: closed")

// New starts listening and serving.
func New(cfg Config) (*Proxy, error) {
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	lns := cfg.Listeners
	if len(lns) == 0 {
		for _, a := range cfg.ListenAddrs {
			ln, err := net.Listen("tcp", a)
			if err != nil {
				for _, l := range lns {
					l.Close()
				}
				return nil, listenError(a, err)
			}
			lns = append(lns, ln)
		}
	}
	ctx, stop := context.WithCancel(context.Background())
	p := &Proxy{
		cfg: cfg, log: cfg.Logger, lns: lns, ctx: ctx, stop: stop,
		verify: map[verifyKey]*verifyState{}, sessions: map[*session]struct{}{},
		wake: make(chan struct{}), drained: make(chan struct{}),
	}
	p.refresh()
	p.wg.Add(1)
	go p.monitor()
	for _, ln := range lns {
		p.wg.Add(1)
		go p.acceptLoop(ln)
	}
	p.log.Info("proxy listening", "addrs", p.addrStrings())
	return p, nil
}

func listenError(addr string, err error) error {
	if errors.Is(err, syscall.EADDRINUSE) {
		return fmt.Errorf("proxy: cannot listen on %s: the port is already in use (is another PostgreSQL running on this machine?): %w", addr, err)
	}
	return fmt.Errorf("proxy: listen %s: %w", addr, err)
}

// Addrs returns the listen addresses.
func (p *Proxy) Addrs() []net.Addr {
	out := make([]net.Addr, len(p.lns))
	for i, ln := range p.lns {
		out[i] = ln.Addr()
	}
	return out
}

func (p *Proxy) addrStrings() []string {
	out := make([]string, len(p.lns))
	for i, ln := range p.lns {
		out[i] = ln.Addr().String()
	}
	return out
}

// Stats returns a snapshot of the counters.
func (p *Proxy) Stats() Stats {
	p.mu.Lock()
	s := Stats{
		Active: len(p.sessions), Waiting: p.waiting,
		Epoch: p.view.Epoch, Primary: p.view.Primary, Trusted: p.trusted,
	}
	if st := p.verify[verifyKey{p.view.Epoch, p.view.Primary}]; st != nil && p.view.Primary != "" {
		s.Verified = st.ok
	}
	p.mu.Unlock()
	c := &p.cnt
	s.Total, s.Forwarded = c.total.Load(), c.forwarded.Load()
	s.DroppedOnEpoch, s.DroppedNoView = c.droppedEpoch.Load(), c.droppedNoView.Load()
	s.RefusedNoView, s.RefusedLimit = c.refusedNoView.Load(), c.refusedLimit.Load()
	s.Waited, s.WaitTimeouts = c.waited.Load(), c.waitTimeouts.Load()
	s.UpstreamErrors, s.VerifyFailures = c.upstreamErrors.Load(), c.verifyFailures.Load()
	s.RefusedPlain = c.refusedPlain.Load()
	return s
}

// Shutdown stops accepting, closes connections still waiting for a
// primary, and waits for piped connections to end by themselves until ctx
// is done; then it closes whatever is left. It returns once every
// goroutine of the proxy has exited. The error is ctx.Err() if connections
// had to be cut, nil otherwise.
func (p *Proxy) Shutdown(ctx context.Context) error {
	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		return ErrClosed
	}
	p.closing = true
	if p.conns == 0 {
		close(p.drained)
	}
	p.wakeLocked()
	p.mu.Unlock()
	for _, ln := range p.lns {
		ln.Close()
	}
	var err error
	select {
	case <-p.drained:
	case <-ctx.Done():
		err = ctx.Err()
		n := p.dropAll()
		p.log.Info("proxy shutdown deadline reached; closed client connections", "count", n)
	}
	p.stop()
	p.wg.Wait()
	p.log.Info("proxy stopped")
	return err
}

// Close shuts down at once, cutting every connection.
func (p *Proxy) Close() error {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := p.Shutdown(ctx)
	if errors.Is(err, context.Canceled) {
		err = nil
	}
	return err
}

func (p *Proxy) dropAll() int {
	p.mu.Lock()
	ss := p.takeSessionsLocked()
	p.mu.Unlock()
	for _, s := range ss {
		s.close()
	}
	return len(ss)
}

func (p *Proxy) wakeLocked() {
	close(p.wake)
	p.wake = make(chan struct{})
}

func (p *Proxy) takeSessionsLocked() []*session {
	out := make([]*session, 0, len(p.sessions))
	for s := range p.sessions {
		out = append(out, s)
		delete(p.sessions, s)
	}
	return out
}

// ---- view tracking ----

func (p *Proxy) monitor() {
	defer p.wg.Done()
	t := time.NewTicker(p.cfg.CheckInterval)
	defer t.Stop()
	changed := p.cfg.Router.Changed()
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-changed:
		case <-t.C:
		}
		p.refresh()
	}
}

func (p *Proxy) leaseOf(v View) time.Duration {
	if v.Lease > 0 {
		return v.Lease
	}
	return p.cfg.Lease
}

// holdFor is how long a new connection may wait for a usable primary.
func (p *Proxy) holdFor() time.Duration {
	switch w := p.cfg.WaitForPrimary; {
	case w < 0:
		return 0
	case w > 0:
		return w
	}
	p.mu.Lock()
	v := p.view
	p.mu.Unlock()
	return HoldLeases * p.leaseOf(v)
}

// refresh reads the router's view and applies it: drops every piped
// connection on an epoch/primary change or when the view stops being
// trustworthy.
func (p *Proxy) refresh() {
	p.refMu.Lock()
	defer p.refMu.Unlock()
	v := p.cfg.Router.View()
	trusted := !v.LastContact.IsZero() && time.Since(v.LastContact) <= p.leaseOf(v)

	p.mu.Lock()
	old, wasTrusted := p.view, p.trusted
	if v.Epoch < old.Epoch {
		p.mu.Unlock()
		p.log.Warn("proxy: router reported an older epoch; ignored", "epoch", v.Epoch, "current", old.Epoch)
		return
	}
	routeChanged := v.Epoch != old.Epoch || v.Primary != old.Primary
	p.view, p.trusted = v, trusted
	var dropped []*session
	reason := ""
	switch {
	case routeChanged:
		dropped, reason = p.takeSessionsLocked(), "epoch"
		for k := range p.verify {
			if k.epoch < v.Epoch {
				delete(p.verify, k)
			}
		}
	case !trusted:
		dropped, reason = p.takeSessionsLocked(), "no_view"
	}
	if trusted && v.Primary != "" {
		// Check the new primary right away, before a client needs it.
		k := verifyKey{v.Epoch, v.Primary}
		if st := p.verifyStateLocked(k); !st.tried && !p.closing {
			p.startVerifyLocked(k, st)
		}
	}
	if routeChanged || trusted != wasTrusted {
		p.wakeLocked()
	}
	p.mu.Unlock()

	if routeChanged {
		p.log.Info("proxy: primary changed", "epoch", v.Epoch, "primary", v.Primary, "dropped", len(dropped))
	}
	if trusted != wasTrusted {
		if trusted {
			p.log.Info("proxy: cluster view is trustworthy again", "epoch", v.Epoch)
		} else if wasTrusted {
			p.log.Warn("proxy: no quorum contact within the lease; refusing clients", "epoch", v.Epoch, "dropped", len(dropped))
		}
	}
	switch reason {
	case "epoch":
		p.cnt.droppedEpoch.Add(uint64(len(dropped)))
	case "no_view":
		p.cnt.droppedNoView.Add(uint64(len(dropped)))
	}
	for _, s := range dropped {
		s.close()
	}
}

// ---- primary check ----

func (p *Proxy) verifyStateLocked(k verifyKey) *verifyState {
	st := p.verify[k]
	if st == nil {
		st = &verifyState{}
		p.verify[k] = st
	}
	return st
}

func (p *Proxy) startVerifyLocked(k verifyKey, st *verifyState) {
	st.inflight, st.tried, st.lastTry = true, true, time.Now()
	p.wg.Add(1)
	go p.runVerify(k, st)
}

func (p *Proxy) runVerify(k verifyKey, st *verifyState) {
	defer p.wg.Done()
	ctx, cancel := context.WithTimeout(p.ctx, p.cfg.VerifyTimeout)
	defer cancel()
	inRecovery, err := p.check(ctx, k.node)

	p.mu.Lock()
	st.inflight = false
	st.lastTry = time.Now()
	ok := err == nil && !inRecovery
	st.ok = ok
	first := !st.warned
	if !ok {
		st.warned = true
	}
	p.wakeLocked()
	p.mu.Unlock()

	switch {
	case ok:
		p.log.Info("proxy: primary checked", "epoch", k.epoch, "node", k.node)
	default:
		p.cnt.verifyFailures.Add(1)
		msg := "proxy: primary check failed"
		if err == nil {
			msg, err = "proxy: named primary is still in recovery; not using it yet", errors.New("pg_is_in_recovery() = true")
		}
		lvl := slog.LevelDebug
		if first && p.ctx.Err() == nil {
			lvl = slog.LevelWarn
		}
		p.log.Log(context.Background(), lvl, msg, "epoch", k.epoch, "node", k.node, "err", err)
	}
}

func (p *Proxy) check(ctx context.Context, node string) (bool, error) {
	c, err := p.dialUpstream(ctx, node)
	if err != nil {
		return false, err
	}
	defer c.Close()
	if d, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(d)
	}
	// The verifier may not watch ctx itself; closing the conn unblocks it.
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	return p.cfg.Verifier.InRecovery(ctx, c)
}

func (p *Proxy) dialUpstream(ctx context.Context, node string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.ConnectTimeout)
	defer cancel()
	c, err := p.cfg.Dial(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", node, err)
	}
	setKeepAlive(c, p.cfg.KeepAlive)
	return c, nil
}

// ---- clients ----

func (p *Proxy) acceptLoop(ln net.Listener) {
	defer p.wg.Done()
	var backoff time.Duration
	for {
		c, err := ln.Accept()
		if err != nil {
			if p.isClosing() || errors.Is(err, net.ErrClosed) {
				return
			}
			// Out of file descriptors and the like: back off, keep serving.
			backoff = min(max(2*backoff, 5*time.Millisecond), time.Second)
			p.log.Warn("proxy: accept failed", "err", err, "retry_in", backoff)
			select {
			case <-time.After(backoff):
			case <-p.ctx.Done():
				return
			}
			continue
		}
		backoff = 0
		p.cnt.total.Add(1)
		p.mu.Lock()
		if p.closing {
			p.mu.Unlock()
			c.Close()
			return
		}
		if p.conns >= p.cfg.MaxConns {
			p.mu.Unlock()
			p.cnt.refusedLimit.Add(1)
			p.log.Warn("proxy: connection limit reached; refusing client", "limit", p.cfg.MaxConns, "client", c.RemoteAddr().String())
			c.Close()
			continue
		}
		p.conns++
		p.mu.Unlock()
		p.wg.Add(1)
		go p.serve(c)
	}
}

func (p *Proxy) isClosing() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closing
}

func (p *Proxy) connDone() {
	p.mu.Lock()
	p.conns--
	if p.closing && p.conns == 0 {
		select {
		case <-p.drained:
		default:
			close(p.drained)
		}
	}
	p.mu.Unlock()
}

var (
	errNoView      = errors.New("this node has lost contact with the cluster")
	errWaitTimeout = errors.New("no primary became available in time")
	errQueueFull   = errors.New("too many connections waiting for a primary")
	errClosing     = errors.New("proxy is shutting down")
)

func (p *Proxy) serve(client net.Conn) {
	defer p.wg.Done()
	defer p.connDone()
	setKeepAlive(client, p.cfg.KeepAlive)
	log := p.log.With("client", client.RemoteAddr().String())
	if p.cfg.RequireTLS {
		gated, err := gate(client)
		if err != nil {
			client.Close()
			p.cnt.refusedPlain.Add(1)
			log.Debug("proxy: client refused", "reason", err)
			return
		}
		client = gated
	}

	s, err := p.connect(client)
	if err != nil {
		// Count before closing, so whoever sees the close also sees the count.
		switch {
		case errors.Is(err, errNoView):
			p.cnt.refusedNoView.Add(1)
		case errors.Is(err, errQueueFull):
			p.cnt.refusedLimit.Add(1)
		case errors.Is(err, errWaitTimeout):
			p.cnt.waitTimeouts.Add(1)
			p.warnWaitTimeout()
		}
		client.Close()
		log.Debug("proxy: client refused", "reason", err)
		return
	}
	p.cnt.forwarded.Add(1)
	log.Debug("proxy: client connected", "node", s.node, "epoch", s.epoch)
	s.pipe()
	p.mu.Lock()
	delete(p.sessions, s)
	p.mu.Unlock()
	s.close()
	log.Debug("proxy: client disconnected", "node", s.node, "epoch", s.epoch)
}

// warnWaitTimeout says, at most every 10 s, that clients were closed
// because no primary became usable within the hold. The client itself only
// sees its connection closed: it is waiting for the answer to its
// SSLRequest (or is mid TLS handshake), and an error sent before TLS is not
// shown by current drivers (CVE-2024-10977), so the reason is logged here.
func (p *Proxy) warnWaitTimeout() {
	now := time.Now().UnixNano()
	last := p.lastTimeoutLog.Load()
	if now-last < int64(10*time.Second) || !p.lastTimeoutLog.CompareAndSwap(last, now) {
		return
	}
	p.mu.Lock()
	v := p.view
	p.mu.Unlock()
	p.log.Warn("proxy: no usable main node within the hold; closed waiting clients",
		"hold", p.holdFor(), "epoch", v.Epoch, "primary", v.Primary, "wait_timeouts", p.cnt.waitTimeouts.Load())
}

// connect finds a verified primary, dials it and registers the session. It
// holds the client while there is no usable primary, up to holdFor.
func (p *Proxy) connect(client net.Conn) (*session, error) {
	deadline := time.Now().Add(p.holdFor())
	var h *hold // non-nil once the client is waiting
	defer func() {
		if h != nil {
			h.stop()
			p.mu.Lock()
			p.waiting--
			p.mu.Unlock()
		}
	}()
	startWaiting := func() error {
		if h != nil {
			h.resume()
			return nil
		}
		p.mu.Lock()
		if p.waiting >= p.cfg.MaxWaiting {
			p.mu.Unlock()
			return errQueueFull
		}
		p.waiting++
		p.mu.Unlock()
		p.cnt.waited.Add(1)
		h = newHold(client)
		return nil
	}

	for {
		p.refresh()
		p.mu.Lock()
		if p.closing {
			p.mu.Unlock()
			return nil, errClosing
		}
		if !p.trusted {
			p.mu.Unlock()
			return nil, errNoView
		}
		v := p.view
		ready := false
		if v.Primary != "" {
			k := verifyKey{v.Epoch, v.Primary}
			st := p.verifyStateLocked(k)
			switch {
			case st.ok:
				ready = true
			case !st.inflight && time.Since(st.lastTry) >= p.cfg.RetryInterval:
				p.startVerifyLocked(k, st)
			}
		}
		wake := p.wake
		p.mu.Unlock()

		if ready {
			s, err := p.attach(client, h, v)
			if s != nil || err != nil {
				return s, err
			}
			// Dial failed or the route moved meanwhile: wait and retry.
		}
		if err := startWaiting(); err != nil {
			return nil, err
		}

		left := time.Until(deadline)
		if left <= 0 {
			return nil, errWaitTimeout
		}
		t := time.NewTimer(min(left, p.cfg.RetryInterval))
		select {
		case <-wake:
		case <-t.C:
		case <-h.gone:
			t.Stop()
			return nil, errHungUp
		case <-p.ctx.Done():
			t.Stop()
			return nil, errClosing
		}
		t.Stop()
	}
}

// attach dials v.Primary and, if the route is still v, registers the
// session. (nil, nil) means "retry".
func (p *Proxy) attach(client net.Conn, h *hold, v View) (*session, error) {
	up, err := p.dialUpstream(p.ctx, v.Primary)
	if err != nil {
		p.cnt.upstreamErrors.Add(1)
		p.log.Debug("proxy: upstream dial failed", "node", v.Primary, "err", err)
		return nil, nil
	}
	var src io.Reader = client
	if h != nil {
		r, err := h.release()
		if err != nil {
			up.Close()
			return nil, err
		}
		src = r
	}
	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		up.Close()
		return nil, errClosing
	}
	if p.view.Epoch != v.Epoch || p.view.Primary != v.Primary || !p.trusted {
		// The route moved while dialing. Nothing from this client reached
		// the old upstream, so it may simply be routed again.
		p.mu.Unlock()
		up.Close()
		return nil, nil
	}
	s := &session{client: client, src: src, up: up, node: v.Primary, epoch: v.Epoch}
	if rc, ok := client.(*replayConn); ok && rc.sslRequest {
		s.wantTLS = true
	}
	p.sessions[s] = struct{}{}
	p.mu.Unlock()
	return s, nil
}

// ---- piping ----

type session struct {
	client net.Conn
	src    io.Reader // client bytes: client itself, or held bytes first
	up     net.Conn
	node   string
	epoch  uint64
	once   sync.Once
	// wantTLS: the client opened with SSLRequest, so Postgres' one-byte
	// answer must be 'S'. Anything else ('N': ssl is off) would let the
	// client go on in plaintext, so the session is closed instead.
	wantTLS bool
}

func (s *session) close() {
	s.once.Do(func() {
		s.client.Close()
		s.up.Close()
	})
}

var bufPool = sync.Pool{New: func() any { b := make([]byte, 32<<10); return &b }}

// pipe copies both ways until both directions are finished. A clean EOF in
// one direction is passed on as a half-close; any error closes everything.
// Backpressure is TCP's: a copy only reads when its last write finished, so
// at most one buffer per direction is in flight.
func (s *session) pipe() {
	done := make(chan struct{})
	go func() {
		defer close(done)
		var up io.Reader = s.up
		if s.wantTLS {
			up = &mustTLS{r: s.up}
		}
		s.copy(s.client, up)
	}()
	s.copy(s.up, s.src)
	<-done
}

func (s *session) copy(dst net.Conn, src io.Reader) {
	bp := bufPool.Get().(*[]byte)
	_, err := io.CopyBuffer(dst, src, *bp)
	bufPool.Put(bp)
	if err != nil {
		s.close()
		return
	}
	if cw, ok := dst.(interface{ CloseWrite() error }); ok {
		if cw.CloseWrite() == nil {
			return
		}
	}
	s.close()
}

// ---- holding a waiting client ----

// maxHeld bounds the bytes read from a held client. A Postgres client sends
// its startup (or SSLRequest) packet and then waits for an answer, so this
// is plenty; past it the proxy stops reading (backpressure).
const maxHeld = 16 << 10

var errHungUp = errors.New("client hung up while waiting")

// hold reads from a client that is waiting for a primary, so that a client
// giving up (closing its socket) frees its slot at once. Bytes read are kept
// and replayed to the upstream first; they are never inspected or logged.
type hold struct {
	conn     net.Conn
	buf      []byte
	gone     chan struct{} // closed when the client hung up
	done     chan struct{} // closed when the current reader exits
	stopping atomic.Bool
	hung     bool
}

func newHold(c net.Conn) *hold {
	h := &hold{conn: c, gone: make(chan struct{})}
	h.resume()
	return h
}

// resume starts reading (again, after release). A no-op while a reader runs.
func (h *hold) resume() {
	if h.done != nil || h.hung || len(h.buf) >= maxHeld {
		return
	}
	h.stopping.Store(false)
	_ = h.conn.SetReadDeadline(time.Time{})
	h.done = make(chan struct{})
	go h.read()
}

func (h *hold) read() {
	defer close(h.done)
	var b [4096]byte
	for len(h.buf) < maxHeld {
		n, err := h.conn.Read(b[:min(len(b), maxHeld-len(h.buf))])
		h.buf = append(h.buf, b[:n]...)
		if err != nil {
			if h.stopping.Load() && errors.Is(err, os.ErrDeadlineExceeded) {
				return
			}
			// EOF or reset: a Postgres client never half-closes before
			// it has its answer, so this is a hang-up.
			h.hung = true
			close(h.gone)
			return
		}
	}
}

// pause stops the reader and waits for it.
func (h *hold) pause() {
	if h.done == nil {
		return
	}
	h.stopping.Store(true)
	_ = h.conn.SetReadDeadline(time.Now())
	<-h.done
	h.done = nil
	_ = h.conn.SetReadDeadline(time.Time{})
}

// release stops reading and returns a reader yielding the held bytes and
// then the client's stream. The hold can be resumed if the session cannot
// be used after all.
func (h *hold) release() (io.Reader, error) {
	h.pause()
	if h.hung {
		return nil, errHungUp
	}
	return io.MultiReader(bytes.NewReader(slices.Clone(h.buf)), h.conn), nil
}

// stop ends the reader for good.
func (h *hold) stop() { h.pause() }

// ---- helpers ----

// setKeepAlive enables TCP keepalive on c or on the TCP connection under it
// (a TLS tunnel stream).
func setKeepAlive(c net.Conn, ka net.KeepAliveConfig) {
	if tc := underlyingTCP(c); tc != nil {
		_ = tc.SetKeepAliveConfig(ka)
	}
}

func underlyingTCP(c net.Conn) *net.TCPConn {
	for range 8 {
		switch v := c.(type) {
		case *net.TCPConn:
			return v
		case interface{ NetConn() net.Conn }:
			c = v.NetConn()
		default:
			return nil
		}
	}
	return nil
}
