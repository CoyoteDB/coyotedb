package transport

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/raft"
)

// RaftLayer implements raft.StreamLayer on top of the transport. Raft server
// addresses are node ids, not host:port: Dial resolves the node's current
// addresses from cluster state, so nodes can change address freely.
type RaftLayer struct {
	t      *Transport
	ch     chan net.Conn
	done   chan struct{}
	closer sync.Once
}

var _ raft.StreamLayer = (*RaftLayer)(nil)

func newRaftLayer(t *Transport) *RaftLayer {
	return &RaftLayer{t: t, ch: make(chan net.Conn), done: make(chan struct{})}
}

func (l *RaftLayer) handle(s *Stream) {
	select {
	case <-l.done:
		s.Reject("raft is not running")
		return
	default:
	}
	if err := s.Accept(); err != nil {
		return
	}
	s.Detach()
	select {
	case l.ch <- s.Conn:
	case <-l.done:
		s.Conn.Close()
	}
}

// Accept waits for the next inbound raft stream.
func (l *RaftLayer) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

// Close stops delivering raft streams. It does not close the transport.
func (l *RaftLayer) Close() error {
	l.closer.Do(func() { close(l.done) })
	return nil
}

// Addr returns this node's id as the raft address.
func (l *RaftLayer) Addr() net.Addr { return nodeAddr(l.t.NodeID()) }

// Dial opens a raft stream to the node whose id is address.
func (l *RaftLayer) Dial(address raft.ServerAddress, timeout time.Duration) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(l.t.ctx, timeout)
	defer cancel()
	return l.t.Dial(ctx, string(address), StreamRaft)
}

type nodeAddr string

func (a nodeAddr) Network() string { return "coyote" }
func (a nodeAddr) String() string  { return string(a) }
