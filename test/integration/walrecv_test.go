//go:build integration && unix

// A copy's WAL receiver stuck on a connection whose other end never
// answers (D57, finding N1): Postgres does not apply connect_timeout to
// it, so only a restart from the agent ends the wait. Real Postgres; the
// "network" is a relay between the copy and the main node that can turn
// into a black hole (accepts, never answers).
package integration

import (
	"context"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"coyotedb.com/coyote/internal/pg"
)

// relay forwards 127.0.0.1:<port> to target, or, in black-hole mode,
// accepts connections and never answers.
type relay struct {
	ln     net.Listener
	target string
	mu     sync.Mutex
	hole   bool
	conns  []net.Conn
	wg     sync.WaitGroup
}

func newRelay(t *testing.T, target string) *relay {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &relay{ln: ln, target: target}
	r.wg.Add(1)
	go r.accept()
	t.Cleanup(r.close)
	return r
}

func (r *relay) port() int { return r.ln.Addr().(*net.TCPAddr).Port }

func (r *relay) accept() {
	defer r.wg.Done()
	for {
		c, err := r.ln.Accept()
		if err != nil {
			return
		}
		r.mu.Lock()
		r.conns = append(r.conns, c)
		hole := r.hole
		r.mu.Unlock()
		if hole {
			continue // held open, never read, never answered
		}
		up, err := net.Dial("tcp", r.target)
		if err != nil {
			c.Close()
			continue
		}
		r.mu.Lock()
		r.conns = append(r.conns, up)
		r.mu.Unlock()
		go func() { _, _ = io.Copy(up, c); up.Close() }()
		go func() { _, _ = io.Copy(c, up); c.Close() }()
	}
}

// setHole switches mode for new connections. Turning into a black hole
// also drops every open connection (the network broke); turning back does
// not (a connection stuck in the hole stays stuck, as after real loss).
func (r *relay) setHole(hole bool) {
	r.mu.Lock()
	r.hole = hole
	r.mu.Unlock()
	if hole {
		r.dropAll()
	}
}

func (r *relay) dropAll() {
	r.mu.Lock()
	old := r.conns
	r.conns = nil
	r.mu.Unlock()
	for _, c := range old {
		c.Close()
	}
}

func (r *relay) close() {
	r.ln.Close()
	r.dropAll()
	r.wg.Wait()
}

func TestStuckWALReceiverIsRestarted(t *testing.T) {
	root := t.TempDir()
	a := newPrimary(t, root, "n1")
	b := newPGNode(t, root, "n2")
	b.cloneFrom(a)
	rl := newRelay(t, a.addr())
	b.dev("set-upstream", "--upstream", "127.0.0.1:"+strconv.Itoa(rl.port()))

	node, err := pg.NewNode(b.dir)
	if err != nil {
		t.Fatal(err)
	}
	state := func() pg.WALReceiver {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		r, err := node.WALReceiverState(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := state(); !r.Streaming || r.PID == 0 {
		t.Fatalf("before: %+v, want streaming", r)
	}

	// The relay becomes a black hole: the next receiver connects and then
	// waits for an answer that never comes.
	rl.setHole(true)
	var stuck pg.WALReceiver
	waitFor(t, 30*time.Second, "a receiver waiting in LibpqwalreceiverConnect", func() bool {
		stuck = state()
		return stuck.PID != 0 && !stuck.Streaming && stuck.WaitEvent == "LibpqwalreceiverConnect"
	})
	// It stays stuck by itself (well past connect_timeout=10 and
	// wal_retrieve_retry_interval=5s): this is what the agent's watch is for.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if r := state(); r.PID != stuck.PID {
			t.Fatalf("receiver %d ended by itself (now %+v); the test no longer shows the stuck case", stuck.PID, r)
		}
		time.Sleep(time.Second)
	}

	// The network is fine again, but the stuck receiver does not notice.
	rl.setHole(false)
	if r := state(); r.PID != stuck.PID || r.Streaming {
		t.Fatalf("after the relay recovered: %+v, want still stuck on %d", r, stuck.PID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := node.RestartWALReceiver(ctx, pg.Install{}, stuck.PID); err != nil {
		t.Fatalf("restart: %v", err)
	}
	waitFor(t, 30*time.Second, "streaming again through a new receiver", func() bool {
		r := state()
		return r.Streaming && r.PID != stuck.PID
	})
	// A pid that is no longer the receiver is left alone.
	if err := node.RestartWALReceiver(ctx, pg.Install{}, stuck.PID); !pg.IsNotReceiver(err) {
		t.Fatalf("restart of a gone receiver: %v, want IsNotReceiver", err)
	}
	insertRange(t, a.conn(), 101, 150, "after-")
	waitSame(t, a, b)
	b.stop()
	a.stop()
}
