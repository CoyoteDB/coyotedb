// Package transport is the single mTLS port every node listens on (safety
// rule 11). After the TLS handshake the client sends a 4-byte header that
// selects a stream type, and the server answers with a status:
//
//	client → server:  'H' 'W' <version=1> <stream type>
//	server → client:  <status u8> <msg len u16 BE> <msg>
//	                  status 0 = accepted, anything else = refused (msg says why)
//
// After an accepted status the connection carries raw bytes for that stream
// type (Raft's own protocol, an RPC, or a Postgres byte stream).
//
// Authorization: a client that presents a certificate must be a current
// cluster member (Config.Authorize) for every stream type; membership is the
// revocation list. A client without a certificate may only open StreamJoin.
package transport

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"sync"
	"time"

	"coyotedb.com/coyote/internal/pki"
)

// StreamType selects what a connection carries.
type StreamType byte

// Stream types. Values are on the wire; never renumber.
const (
	StreamRaft StreamType = 1 // hashicorp/raft NetworkTransport
	StreamRPC  StreamType = 2 // member-to-member RPC (forward to leader, status)
	StreamPG   StreamType = 3 // byte tunnel to the node's local Postgres
	StreamJoin StreamType = 4 // join RPC; the only stream open to cert-less clients
	// StreamAgent carries agent-to-agent requests (role changes, switch
	// steps, status) between members' agents. Members only.
	StreamAgent StreamType = 5
)

func (s StreamType) String() string {
	switch s {
	case StreamRaft:
		return "raft"
	case StreamRPC:
		return "rpc"
	case StreamPG:
		return "pg"
	case StreamJoin:
		return "join"
	case StreamAgent:
		return "agent"
	}
	return fmt.Sprintf("stream(%d)", byte(s))
}

const (
	magic0, magic1 = 'H', 'W'
	wireVersion    = 1

	statusOK      = 0
	statusRefused = 1
	statusDenied  = 2
	statusUnknown = 3

	maxRejectMsg = 1024
)

var (
	// ErrClosed is returned after Close.
	ErrClosed = errors.New("transport closed")
	// ErrNoAddress means the node has no known address.
	ErrNoAddress = errors.New("no known address for node")
	// ErrNotMember means the target is not a current cluster member.
	ErrNotMember = errors.New("node is not a cluster member")
)

// RefusedError is returned by Dial when the remote node accepted the TLS
// connection but refused the stream.
type RefusedError struct {
	NodeID string
	Type   StreamType
	Denied bool // authorization failure, as opposed to a handler refusal
	Msg    string
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf("node %s refused %s stream: %s", e.NodeID, e.Type, e.Msg)
}

// Timing for connections. Zero fields take defaults.
type Timing struct {
	DialTimeout      time.Duration // per address attempt, TCP + TLS + header
	HandshakeTimeout time.Duration // server side: TLS + header
	KeepAliveIdle    time.Duration
	KeepAliveIntvl   time.Duration
	KeepAliveCount   int
	// Redirect, if set, maps the address about to be dialled to the address
	// actually dialled. Only the chaos tests use it: every node-to-node
	// connection then goes through an in-test proxy for that ordered pair
	// of nodes, so the test can cut, stall or delay single links. TLS still
	// checks the peer's identity, so a redirect cannot impersonate a node.
	Redirect func(addr string) string
}

// DefaultTiming suits LAN/VPN links.
func DefaultTiming() Timing {
	return Timing{
		DialTimeout:      5 * time.Second,
		HandshakeTimeout: 5 * time.Second,
		KeepAliveIdle:    15 * time.Second,
		KeepAliveIntvl:   5 * time.Second,
		KeepAliveCount:   3,
	}
}

func (t Timing) withDefaults() Timing {
	d := DefaultTiming()
	if t.DialTimeout <= 0 {
		t.DialTimeout = d.DialTimeout
	}
	if t.HandshakeTimeout <= 0 {
		t.HandshakeTimeout = d.HandshakeTimeout
	}
	if t.KeepAliveIdle <= 0 {
		t.KeepAliveIdle = d.KeepAliveIdle
	}
	if t.KeepAliveIntvl <= 0 {
		t.KeepAliveIntvl = d.KeepAliveIntvl
	}
	if t.KeepAliveCount <= 0 {
		t.KeepAliveCount = d.KeepAliveCount
	}
	return t
}

func (t Timing) keepAlive() net.KeepAliveConfig {
	return net.KeepAliveConfig{Enable: true, Idle: t.KeepAliveIdle, Interval: t.KeepAliveIntvl, Count: t.KeepAliveCount}
}

// Config configures a Transport.
type Config struct {
	// ListenAddr is host:port to listen on ("127.0.0.1:0" in tests).
	ListenAddr string
	// Listener, if set, is used instead of ListenAddr.
	Listener net.Listener
	// TLS is this node's identity.
	TLS *pki.NodeTLS
	// Authorize reports whether nodeID is a current member. Required.
	Authorize func(nodeID string) bool
	// Resolve returns the addresses to try for nodeID, in order. Required
	// for Dial.
	Resolve func(nodeID string) []string
	Timing  Timing
	Logger  *slog.Logger
	// MaxAnonymous caps concurrent streams from clients without a
	// certificate (join streams). Further ones are refused as busy until a
	// slot frees. Member streams are never counted. 0 means
	// DefaultMaxAnonymous.
	MaxAnonymous int
}

// DefaultMaxAnonymous is the default cap on concurrent anonymous streams.
const DefaultMaxAnonymous = 8

// Handler serves one accepted-by-type stream. It must call s.Accept or
// s.Reject. When it returns the stream is closed, unless it called s.Detach.
type Handler func(s *Stream)

// Transport is a node's listener plus its dialer.
type Transport struct {
	cfg    Config
	timing Timing
	ln     net.Listener
	log    *slog.Logger
	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	handlers map[StreamType]Handler
	conns    map[*Conn]struct{}
	closed   bool
	anon     int // anonymous streams being served
	// admitted: members that accepted a stream from this node since it
	// started (see Admitted).
	admitted map[string]struct{}

	wg   sync.WaitGroup
	raft *RaftLayer
}

// Listen starts the listener and the accept loop.
func Listen(cfg Config) (*Transport, error) {
	if cfg.TLS == nil || cfg.Authorize == nil {
		return nil, errors.New("transport: TLS and Authorize are required")
	}
	timing := cfg.Timing.withDefaults()
	ln := cfg.Listener
	if ln == nil {
		lc := net.ListenConfig{KeepAliveConfig: timing.keepAlive()}
		var err error
		ln, err = lc.Listen(context.Background(), "tcp", cfg.ListenAddr)
		if err != nil {
			return nil, fmt.Errorf("listen %s: %w", cfg.ListenAddr, err)
		}
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t := &Transport{
		cfg:      cfg,
		timing:   timing,
		ln:       ln,
		log:      log.With("node", cfg.TLS.NodeID),
		ctx:      ctx,
		cancel:   cancel,
		handlers: map[StreamType]Handler{},
		conns:    map[*Conn]struct{}{},
		admitted: map[string]struct{}{},
	}
	t.raft = newRaftLayer(t)
	t.handlers[StreamRaft] = t.raft.handle
	t.wg.Add(1)
	go t.acceptLoop()
	return t, nil
}

// Addr is the bound listen address.
func (t *Transport) Addr() net.Addr { return t.ln.Addr() }

// NodeID is this node's id.
func (t *Transport) NodeID() string { return t.cfg.TLS.NodeID }

// Handle registers h for stream type st, replacing any earlier handler.
// StreamRaft is served by RaftLayer and cannot be replaced.
func (t *Transport) Handle(st StreamType, h Handler) {
	if st == StreamRaft {
		panic("transport: StreamRaft is reserved for RaftLayer")
	}
	t.mu.Lock()
	t.handlers[st] = h
	t.mu.Unlock()
}

// RaftLayer returns the raft.StreamLayer view of this transport.
func (t *Transport) RaftLayer() *RaftLayer { return t.raft }

func (t *Transport) acceptLoop() {
	defer t.wg.Done()
	backoff := 5 * time.Millisecond
	for {
		c, err := t.ln.Accept()
		if err != nil {
			if t.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			t.log.Warn("accept failed", "err", err)
			select {
			case <-time.After(backoff):
			case <-t.ctx.Done():
				return
			}
			backoff = min(backoff*2, time.Second)
			continue
		}
		backoff = 5 * time.Millisecond
		t.wg.Add(1)
		go t.serveConn(c)
	}
}

func (t *Transport) serveConn(raw net.Conn) {
	defer t.wg.Done()
	tc := tls.Server(raw, t.cfg.TLS.ServerConfig())
	conn := t.track(tc, "", "")
	if conn == nil {
		return
	}
	deadline := time.Now().Add(t.timing.HandshakeTimeout)
	_ = tc.SetDeadline(deadline)
	hctx, cancel := context.WithDeadline(t.ctx, deadline)
	err := tc.HandshakeContext(hctx)
	cancel()
	if err != nil {
		t.log.Debug("tls handshake failed", "remote", raw.RemoteAddr(), "err", err)
		conn.Close()
		return
	}
	peer := pki.PeerNodeID(tc.ConnectionState())
	var hdr [4]byte
	if _, err := io.ReadFull(tc, hdr[:]); err != nil {
		conn.Close()
		return
	}
	if hdr[0] != magic0 || hdr[1] != magic1 || hdr[2] != wireVersion {
		conn.Close()
		return
	}
	st := StreamType(hdr[3])
	conn.setPeer(peer)
	s := &Stream{Conn: conn, Peer: peer, Type: st}
	if peer == "" && st != StreamJoin {
		s.reply(statusDenied, "a client certificate is required")
		conn.Close()
		return
	}
	if peer == "" {
		if !t.acquireAnon() {
			s.reply(statusRefused, "busy: too many joins in progress, try again shortly")
			conn.Close()
			return
		}
		defer t.releaseAnon()
	}
	if peer != "" && !t.cfg.Authorize(peer) {
		t.log.Info("refused stream from non-member", "peer", peer, "type", st)
		s.reply(statusDenied, "not a cluster member")
		conn.Close()
		return
	}
	t.mu.Lock()
	h := t.handlers[st]
	t.mu.Unlock()
	if h == nil {
		s.reply(statusUnknown, "unknown stream type")
		conn.Close()
		return
	}
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.log.Error("stream handler panic", "type", st, "panic", r)
			}
		}()
		h(s)
	}()
	s.mu.Lock()
	answered, detached := s.answered, s.detached
	s.mu.Unlock()
	if !answered {
		s.reply(statusRefused, "handler did not accept")
	}
	if !detached {
		conn.Close()
	}
}

// Dial opens a stream of type st to nodeID, trying its known addresses in
// order. The returned connection is verified to be nodeID and the stream has
// been accepted by the remote handler.
func (t *Transport) Dial(ctx context.Context, nodeID string, st StreamType) (net.Conn, error) {
	if t.isClosed() {
		return nil, ErrClosed
	}
	if !t.cfg.Authorize(nodeID) {
		return nil, fmt.Errorf("dial %s: %w", nodeID, ErrNotMember)
	}
	var addrs []string
	if t.cfg.Resolve != nil {
		addrs = t.cfg.Resolve(nodeID)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("dial %s: %w", nodeID, ErrNoAddress)
	}
	var errs []error
	for _, a := range addrs {
		c, err := t.DialAddr(ctx, a, nodeID, st)
		if err == nil {
			return c, nil
		}
		var re *RefusedError
		if errors.As(err, &re) || ctx.Err() != nil {
			return nil, err
		}
		errs = append(errs, err)
	}
	return nil, fmt.Errorf("dial %s: %w", nodeID, errors.Join(errs...))
}

// DialAddr dials one address and expects to reach nodeID.
func (t *Transport) DialAddr(ctx context.Context, addr, nodeID string, st StreamType) (net.Conn, error) {
	c, err := dialStream(ctx, addr, nodeID, st, t.cfg.TLS.ClientConfig(nodeID), t.timing)
	if err != nil {
		return nil, err
	}
	conn := t.track(c, nodeID, addr)
	if conn == nil {
		c.Close()
		return nil, ErrClosed
	}
	t.mu.Lock()
	t.admitted[nodeID] = struct{}{}
	t.mu.Unlock()
	return conn, nil
}

// Admitted reports whether nodeID accepted a stream from this node at some
// point since the transport started. A later certificate refusal
// (RefusedError.Denied) from such a member means that member's state no
// longer lists this node; on its own a refusal could also come from a
// member that has not yet learned that this node joined.
func (t *Transport) Admitted(nodeID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.admitted[nodeID]
	return ok
}

// DialJoin opens a join stream to addr as a node without a certificate. The
// server must present the CA whose fingerprint is caFingerprint and a leaf
// for clusterID.
func DialJoin(ctx context.Context, addr, clusterID string, caFingerprint [32]byte, timing Timing) (net.Conn, error) {
	return dialStream(ctx, addr, "", StreamJoin, pki.JoinClientConfig(clusterID, caFingerprint, nil), timing.withDefaults())
}

func dialStream(ctx context.Context, addr, nodeID string, st StreamType, cfg *tls.Config, timing Timing) (*tls.Conn, error) {
	actx, cancel := context.WithTimeout(ctx, timing.DialTimeout)
	defer cancel()
	d := net.Dialer{KeepAliveConfig: timing.keepAlive()}
	dialAddr := addr
	if timing.Redirect != nil {
		dialAddr = timing.Redirect(addr)
	}
	raw, err := d.DialContext(actx, "tcp", dialAddr)
	if err != nil {
		return nil, err
	}
	tc := tls.Client(raw, cfg)
	if dl, ok := actx.Deadline(); ok {
		_ = tc.SetDeadline(dl)
	}
	// Abort blocking I/O if ctx is cancelled mid-way.
	stop := context.AfterFunc(actx, func() { _ = raw.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	if err := tc.HandshakeContext(actx); err != nil {
		raw.Close()
		return nil, fmt.Errorf("tls handshake with %s: %w", addr, err)
	}
	if _, err := tc.Write([]byte{magic0, magic1, wireVersion, byte(st)}); err != nil {
		tc.Close()
		return nil, fmt.Errorf("send header to %s: %w", addr, err)
	}
	var resp [3]byte
	if _, err := io.ReadFull(tc, resp[:]); err != nil {
		tc.Close()
		return nil, fmt.Errorf("read reply from %s: %w", addr, err)
	}
	if resp[0] != statusOK {
		n := int(binary.BigEndian.Uint16(resp[1:]))
		msg := make([]byte, min(n, maxRejectMsg))
		_, _ = io.ReadFull(tc, msg)
		tc.Close()
		return nil, &RefusedError{NodeID: nodeID, Type: st, Denied: resp[0] == statusDenied, Msg: string(msg)}
	}
	if !stop() {
		tc.Close()
		return nil, ctx.Err()
	}
	_ = tc.SetDeadline(time.Time{})
	return tc, nil
}

// DropUnauthorized closes every tracked connection whose peer is no longer
// authorized. Call it after membership changes. It returns how many were
// closed.
func (t *Transport) DropUnauthorized() int {
	t.mu.Lock()
	var drop []*Conn
	for c := range t.conns {
		if p := c.PeerID(); p != "" && !t.cfg.Authorize(p) {
			drop = append(drop, c)
		}
	}
	t.mu.Unlock()
	for _, c := range drop {
		t.log.Info("closing connection of removed member", "peer", c.PeerID())
		c.Close()
	}
	return len(drop)
}

// DropStale closes every connection this node dialed to an address its
// peer no longer has (Resolve does not list it any more): the peer moved,
// and a connection to its old address can hang until keepalives give up.
// Whoever used it dials again, at the new address. It returns how many
// were closed.
func (t *Transport) DropStale() int {
	if t.cfg.Resolve == nil {
		return 0
	}
	t.mu.Lock()
	var check []*Conn
	for c := range t.conns {
		if c.dialed != "" && c.PeerID() != "" {
			check = append(check, c)
		}
	}
	t.mu.Unlock()
	n := 0
	for _, c := range check {
		addrs := t.cfg.Resolve(c.PeerID())
		if len(addrs) == 0 || slices.Contains(addrs, c.dialed) {
			continue
		}
		t.log.Info("closing connection to a member's old address", "peer", c.PeerID(), "addr", c.dialed)
		c.Close()
		n++
	}
	return n
}

// DropLocalGone closes every connection, dialed or accepted, whose local
// IP address present reports as gone from this computer (D62). Such a
// connection is dead: its packets can never arrive, and TCP takes many
// minutes to notice. Closing it makes Raft and the tunnels redial at once.
// It returns how many were closed.
func (t *Transport) DropLocalGone(present func(netip.Addr) bool) int {
	t.mu.Lock()
	conns := make([]*Conn, 0, len(t.conns))
	for c := range t.conns {
		conns = append(conns, c)
	}
	t.mu.Unlock()
	n := 0
	for _, c := range conns {
		ap, err := netip.ParseAddrPort(c.LocalAddr().String())
		if err != nil {
			continue
		}
		ip := ap.Addr().Unmap()
		if present(ip) {
			continue
		}
		t.log.Info("closing connection on an address this computer no longer has", "peer", c.PeerID(), "local", ip)
		c.Close()
		n++
	}
	return n
}

func (t *Transport) acquireAnon() bool {
	limit := t.cfg.MaxAnonymous
	if limit <= 0 {
		limit = DefaultMaxAnonymous
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.anon >= limit {
		return false
	}
	t.anon++
	return true
}

func (t *Transport) releaseAnon() {
	t.mu.Lock()
	t.anon--
	t.mu.Unlock()
}

// NumAnonymous returns the number of anonymous streams being served.
func (t *Transport) NumAnonymous() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.anon
}

// NumConns returns the number of open tracked connections (for tests).
func (t *Transport) NumConns() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.conns)
}

func (t *Transport) isClosed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closed
}

// Close stops listening, closes every connection (inbound, outbound,
// detached) and waits for the transport's goroutines to exit.
func (t *Transport) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	conns := make([]*Conn, 0, len(t.conns))
	for c := range t.conns {
		conns = append(conns, c)
	}
	t.mu.Unlock()
	t.cancel()
	err := t.ln.Close()
	t.raft.Close()
	for _, c := range conns {
		c.Close()
	}
	t.wg.Wait()
	return err
}

func (t *Transport) track(tc *tls.Conn, peer, dialed string) *Conn {
	c := &Conn{Conn: tc, t: t, peer: peer, dialed: dialed}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		tc.Close()
		return nil
	}
	t.conns[c] = struct{}{}
	return c
}

// Conn is a tracked TLS connection. Close is idempotent.
type Conn struct {
	*tls.Conn
	t *Transport
	// dialed is the address this node dialed ("" for inbound); set once
	// before the connection is handed out.
	dialed string
	mu     sync.Mutex
	peer   string
	once   sync.Once
	err    error
}

// PeerID is the verified node id of the other side ("" for anonymous).
func (c *Conn) PeerID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.peer
}

func (c *Conn) setPeer(p string) {
	c.mu.Lock()
	c.peer = p
	c.mu.Unlock()
}

// Close closes the connection and stops tracking it. The TLS close_notify
// is bounded by a short deadline so a stalled peer cannot block Close.
func (c *Conn) Close() error {
	c.once.Do(func() {
		c.t.mu.Lock()
		delete(c.t.conns, c)
		c.t.mu.Unlock()
		_ = c.Conn.SetWriteDeadline(time.Now().Add(time.Second))
		c.err = c.Conn.Close()
	})
	return c.err
}

// Stream is an incoming connection waiting for its handler's decision.
type Stream struct {
	*Conn
	Peer string // verified node id; "" only for StreamJoin
	Type StreamType

	mu       sync.Mutex
	answered bool
	detached bool
}

// Accept tells the dialer the stream is open; after it the connection
// carries raw bytes for this stream type.
func (s *Stream) Accept() error { return s.reply(statusOK, "") }

// Reject refuses the stream with a message the dialer will see.
func (s *Stream) Reject(msg string) { _ = s.reply(statusRefused, msg) }

// Detach transfers ownership: the transport no longer closes the connection
// when the handler returns (it still closes it on Transport.Close).
func (s *Stream) Detach() {
	s.mu.Lock()
	s.detached = true
	s.mu.Unlock()
}

func (s *Stream) reply(status byte, msg string) error {
	s.mu.Lock()
	if s.answered {
		s.mu.Unlock()
		return nil
	}
	s.answered = true
	s.mu.Unlock()
	if len(msg) > maxRejectMsg {
		msg = msg[:maxRejectMsg]
	}
	b := make([]byte, 3, 3+len(msg))
	b[0] = status
	binary.BigEndian.PutUint16(b[1:], uint16(len(msg)))
	b = append(b, msg...)
	_ = s.Conn.SetWriteDeadline(time.Now().Add(s.t.timing.HandshakeTimeout))
	_, err := s.Conn.Write(b)
	_ = s.Conn.SetDeadline(time.Time{})
	return err
}
