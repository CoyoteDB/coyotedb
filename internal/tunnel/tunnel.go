// Package tunnel carries Postgres byte streams between nodes over the
// agent's mTLS port (safety rule 11: Postgres listens on 127.0.0.1 only).
//
// The client side is a Forwarder: it listens on 127.0.0.1:<port> and pipes
// each accepted connection over a pg stream to a target node. The server side
// is Backend: a transport handler that dials the node's own local Postgres.
// Bytes pass through untouched; replication, pg_basebackup and pg_rewind run
// through it, so half-close and backpressure are handled and nothing is
// buffered beyond the copy buffers.
package tunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"coyotedb.com/coyote/internal/cluster/transport"
)

// DialFunc opens a pg stream to nodeID (normally Transport.Dial with
// transport.StreamPG).
type DialFunc func(ctx context.Context, nodeID string) (net.Conn, error)

// ErrNoTarget is logged when a connection arrives while no target is set.
var ErrNoTarget = errors.New("tunnel has no target node")

const copyBuf = 64 << 10

// Forwarder listens locally and pipes connections to the current target.
type Forwarder struct {
	ln          net.Listener
	dial        DialFunc
	dialTimeout time.Duration
	log         *slog.Logger
	ctx         context.Context
	cancel      context.CancelFunc

	mu     sync.Mutex
	target string
	gen    uint64
	active map[*pair]struct{}
	closed bool

	wg sync.WaitGroup
}

// ForwarderConfig configures NewForwarder.
type ForwarderConfig struct {
	ListenAddr  string // normally "127.0.0.1:<port>"; ":0" picks a port
	Dial        DialFunc
	Target      string // initial target node id; "" refuses connections
	DialTimeout time.Duration
	Logger      *slog.Logger
}

// NewForwarder starts listening.
func NewForwarder(cfg ForwarderConfig) (*Forwarder, error) {
	if cfg.Dial == nil {
		return nil, errors.New("tunnel: Dial is required")
	}
	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("tunnel listen %s: %w", cfg.ListenAddr, err)
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 10 * time.Second
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	ctx, cancel := context.WithCancel(context.Background())
	f := &Forwarder{
		ln: ln, dial: cfg.Dial, dialTimeout: cfg.DialTimeout, log: log,
		ctx: ctx, cancel: cancel, target: cfg.Target, active: map[*pair]struct{}{},
	}
	f.wg.Add(1)
	go f.acceptLoop()
	return f, nil
}

// Addr is the local listen address.
func (f *Forwarder) Addr() *net.TCPAddr { return f.ln.Addr().(*net.TCPAddr) }

// Target returns the current target node id.
func (f *Forwarder) Target() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.target
}

// SetTarget re-points the forwarder. Every connection piped to the previous
// target is closed (even if the target is unchanged, so callers can force a
// reconnect); new connections go to nodeID. "" refuses new connections.
func (f *Forwarder) SetTarget(nodeID string) {
	f.mu.Lock()
	f.target = nodeID
	f.gen++
	old := f.takeActiveLocked()
	f.mu.Unlock()
	for _, p := range old {
		p.close()
	}
}

// Active returns the number of piped connections (for tests and status).
func (f *Forwarder) Active() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.active)
}

func (f *Forwarder) takeActiveLocked() []*pair {
	out := make([]*pair, 0, len(f.active))
	for p := range f.active {
		out = append(out, p)
	}
	clear(f.active)
	return out
}

// Close stops listening, closes every piped connection and waits for all
// goroutines.
func (f *Forwarder) Close() error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	f.closed = true
	old := f.takeActiveLocked()
	f.mu.Unlock()
	f.cancel()
	err := f.ln.Close()
	for _, p := range old {
		p.close()
	}
	f.wg.Wait()
	return err
}

func (f *Forwarder) acceptLoop() {
	defer f.wg.Done()
	for {
		c, err := f.ln.Accept()
		if err != nil {
			if f.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			f.log.Warn("tunnel accept failed", "err", err)
			select {
			case <-time.After(50 * time.Millisecond):
			case <-f.ctx.Done():
				return
			}
			continue
		}
		setKeepAlive(c)
		f.wg.Add(1)
		go f.serve(c)
	}
}

func (f *Forwarder) serve(client net.Conn) {
	defer f.wg.Done()
	f.mu.Lock()
	target, gen := f.target, f.gen
	f.mu.Unlock()
	if target == "" {
		f.log.Debug("tunnel refused connection", "err", ErrNoTarget)
		refuse(client)
		return
	}
	ctx, cancel := context.WithTimeout(f.ctx, f.dialTimeout)
	up, err := f.dial(ctx, target)
	cancel()
	if err != nil {
		f.log.Warn("tunnel dial failed", "target", target, "err", err)
		refuse(client)
		return
	}
	p := &pair{a: client, b: up}
	f.mu.Lock()
	if f.closed || f.gen != gen {
		// Re-pointed or closed while dialing: this connection belongs to
		// the old target.
		f.mu.Unlock()
		p.close()
		return
	}
	f.active[p] = struct{}{}
	f.mu.Unlock()
	Pipe(client, up)
	f.mu.Lock()
	delete(f.active, p)
	f.mu.Unlock()
}

// refuseLinger bounds how long refuse waits for the client to close.
const refuseLinger = 2 * time.Second

// refuse ends a local connection that cannot be served so that the client
// surely sees the end (D57). A plain Close of a socket holding unread bytes
// (the client's startup packet) sends a reset, which is sent once: if it is
// lost, the client waits for a reply on a connection whose other end is
// gone. A FIN, sent first, is retransmitted until acknowledged; the unread
// bytes are then read and dropped so the final close is orderly.
func refuse(c net.Conn) {
	if cw, ok := c.(closeWriter); ok && cw.CloseWrite() == nil {
		_ = c.SetReadDeadline(time.Now().Add(refuseLinger))
		_, _ = io.Copy(io.Discard, c)
	}
	c.Close()
}

type pair struct {
	a, b net.Conn
	once sync.Once
}

func (p *pair) close() {
	p.once.Do(func() {
		p.a.Close()
		p.b.Close()
	})
}

type closeWriter interface{ CloseWrite() error }

// halfCloseLinger bounds how long Pipe keeps one direction open after the
// other ended cleanly. Postgres' protocol never half-closes on purpose: a
// side that sent EOF is done, and its peer closes at once. Without a bound
// a peer that went silent (its machine gone, the close lost) would hold the
// pipe until the TCP keepalive gave up.
const halfCloseLinger = 30 * time.Second

// Pipe copies bytes both ways between a and b until both directions are
// done, then closes both. When one side finishes sending (EOF) the other
// side's write half is closed, so half-closed protocols work, for at most
// halfCloseLinger. An error in either direction closes both connections at
// once.
func Pipe(a, b net.Conn) {
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			a.Close()
			b.Close()
		})
	}
	var wg sync.WaitGroup
	wg.Add(2)
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		buf := make([]byte, copyBuf)
		_, err := io.CopyBuffer(dst, src, buf)
		if err != nil {
			closeBoth()
			return
		}
		// Clean EOF from src: pass the half-close on, and give the other
		// direction (reading from dst) a bounded time to finish.
		if cw, ok := dst.(closeWriter); ok {
			if cw.CloseWrite() == nil {
				_ = dst.SetReadDeadline(time.Now().Add(halfCloseLinger))
				return
			}
		}
		closeBoth()
	}
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
	closeBoth()
}

// Backend returns the transport handler for StreamPG: it dials pgAddr (the
// node's own 127.0.0.1 Postgres) and pipes the stream to it. If Postgres
// cannot be reached the stream is refused, so the dialer gets a clean error.
func Backend(pgAddr func() string, dialTimeout time.Duration) transport.Handler {
	if dialTimeout <= 0 {
		dialTimeout = 5 * time.Second
	}
	return func(s *transport.Stream) {
		addr := pgAddr()
		if addr == "" {
			s.Reject("postgres is not running on this node")
			return
		}
		d := net.Dialer{Timeout: dialTimeout, KeepAliveConfig: keepAliveCfg}
		pg, err := d.Dial("tcp", addr)
		if err != nil {
			s.Reject(fmt.Sprintf("postgres on this node is not reachable: %v", err))
			return
		}
		if err := s.Accept(); err != nil {
			pg.Close()
			return
		}
		Pipe(s, pg)
	}
}

var keepAliveCfg = net.KeepAliveConfig{Enable: true, Idle: 15 * time.Second, Interval: 5 * time.Second, Count: 3}

func setKeepAlive(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetKeepAliveConfig(keepAliveCfg)
	}
}
