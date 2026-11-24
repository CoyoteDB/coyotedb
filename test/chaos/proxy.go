//go:build chaos && unix

package chaos

import (
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Mode is what a link does to the bytes flowing one way.
type Mode int32

const (
	Pass      Mode = iota // forward
	Blackhole             // read and discard (the sender sees no error, nothing arrives)
	Drop                  // close every connection, refuse new ones
)

func (m Mode) String() string { return [...]string{"pass", "blackhole", "drop"}[m] }

// blackholeKill: a connection that has carried nothing for this long while
// blackholed is closed, as TCP keepalive would eventually.
const blackholeKill = 30 * time.Second

// Link is the proxy for connections that node From opens to node To. Up is
// the From->To direction of those connections, Down the To->From one.
type Link struct {
	From, To string
	ln       net.Listener
	target   string
	up, down atomic.Int32 // Mode
	delay    atomic.Int64 // ns per chunk, each direction
	// spikes: this percentage of chunks is held up by a random time up
	// to spikeMax (what TCP retransmissions after packet loss look like
	// to the program: the bytes arrive intact and in order, but late).
	spikePct atomic.Int32
	spikeMax atomic.Int64
	bytes    atomic.Int64
	conns    atomic.Int64

	mu   sync.Mutex
	open map[net.Conn]struct{}
}

// Net is one proxy per ordered pair of nodes.
type Net struct {
	mu    sync.Mutex
	links map[[2]string]*Link
	wg    sync.WaitGroup
}

func NewNet() *Net { return &Net{links: map[[2]string]*Link{}} }

// Add creates the proxy for connections from -> to (to listens on target)
// and returns its address.
func (n *Net) Add(from, to, target string) (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	l := &Link{From: from, To: to, ln: ln, target: target, open: map[net.Conn]struct{}{}}
	n.mu.Lock()
	n.links[[2]string{from, to}] = l
	n.mu.Unlock()
	n.wg.Add(1)
	go func() { defer n.wg.Done(); l.serve() }()
	return ln.Addr().String(), nil
}

func (n *Net) link(from, to string) *Link {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.links[[2]string{from, to}]
}

// Links returns all links.
func (n *Net) Links() []*Link {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]*Link, 0, len(n.links))
	for _, l := range n.links {
		out = append(out, l)
	}
	return out
}

// Close stops every proxy.
func (n *Net) Close() {
	for _, l := range n.Links() {
		l.ln.Close()
		l.closeAll()
	}
	n.wg.Wait()
}

// SetFlow sets what happens to bytes flowing from a to b, on every
// connection between them (whoever opened it).
func (n *Net) SetFlow(a, b string, m Mode) {
	if l := n.link(a, b); l != nil {
		l.up.Store(int32(m))
		if m == Drop {
			l.closeAll()
		}
	}
	if l := n.link(b, a); l != nil {
		l.down.Store(int32(m))
		if m == Drop {
			l.closeAll()
		}
	}
}

// Cut makes a and b unable to talk in either direction.
func (n *Net) Cut(a, b string, m Mode) { n.SetFlow(a, b, m); n.SetFlow(b, a, m) }

// Unreachable makes every connection another node opens to x fail (open
// ones are closed, new ones refused) while the connections x opens pass:
// what a computer whose address changed, or whose firewall drops incoming
// connections, looks like to the others (D62).
func (n *Net) Unreachable(x string, others []string) {
	for _, o := range others {
		if o == x {
			continue
		}
		if l := n.link(o, x); l != nil {
			l.up.Store(int32(Drop))
			l.down.Store(int32(Drop))
			l.closeAll()
		}
	}
}

// Isolate cuts node x off from every other node.
func (n *Net) Isolate(x string, others []string, m Mode) {
	for _, o := range others {
		if o != x {
			n.Cut(x, o, m)
		}
	}
}

// Partition splits the nodes into groups that cannot talk to each other.
func (n *Net) Partition(groups [][]string, m Mode) {
	for i, g := range groups {
		for j, h := range groups {
			if i == j {
				continue
			}
			for _, a := range g {
				for _, b := range h {
					n.SetFlow(a, b, m)
				}
			}
		}
	}
}

// Delay adds d to every chunk on every link.
func (n *Net) Delay(d time.Duration) {
	for _, l := range n.Links() {
		l.delay.Store(int64(d))
	}
}

// Spikes holds up pct percent of the chunks on every link by a random
// time up to max (0 turns it off).
func (n *Net) Spikes(pct int, max time.Duration) {
	for _, l := range n.Links() {
		l.spikePct.Store(int32(pct))
		l.spikeMax.Store(int64(max))
	}
}

// Heal passes everything again.
func (n *Net) Heal() {
	for _, l := range n.Links() {
		l.up.Store(int32(Pass))
		l.down.Store(int32(Pass))
		l.delay.Store(0)
		l.spikePct.Store(0)
		l.spikeMax.Store(0)
	}
}

// Describe is the current state (for the run log).
func (n *Net) Describe() string {
	s := ""
	for _, l := range n.Links() {
		if Mode(l.up.Load()) != Pass || Mode(l.down.Load()) != Pass || l.delay.Load() != 0 || l.spikePct.Load() != 0 {
			s += fmt.Sprintf("%s->%s up=%s down=%s delay=%s spikes=%d%%/%s; ", l.From, l.To, Mode(l.up.Load()), Mode(l.down.Load()),
				time.Duration(l.delay.Load()), l.spikePct.Load(), time.Duration(l.spikeMax.Load()))
		}
	}
	if s == "" {
		return "all links pass"
	}
	return s
}

func (l *Link) track(c net.Conn, add bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if add {
		l.open[c] = struct{}{}
	} else {
		delete(l.open, c)
	}
}

func (l *Link) closeAll() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for c := range l.open {
		c.Close()
	}
}

func (l *Link) serve() {
	for {
		c, err := l.ln.Accept()
		if err != nil {
			return
		}
		if Mode(l.up.Load()) == Drop || Mode(l.down.Load()) == Drop {
			c.Close()
			continue
		}
		go l.handle(c)
	}
}

func (l *Link) handle(c net.Conn) {
	l.conns.Add(1)
	s, err := net.DialTimeout("tcp", l.target, 5*time.Second)
	if err != nil {
		c.Close()
		return
	}
	l.track(c, true)
	l.track(s, true)
	defer func() {
		c.Close()
		s.Close()
		l.track(c, false)
		l.track(s, false)
	}()
	var lastData atomic.Int64
	lastData.Store(time.Now().UnixNano())
	done := make(chan struct{}, 2)
	go func() { l.pump(s, c, &l.up, &lastData); done <- struct{}{} }()
	go func() { l.pump(c, s, &l.down, &lastData); done <- struct{}{} }()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-done:
			return
		case <-tick.C:
			bh := Mode(l.up.Load()) == Blackhole || Mode(l.down.Load()) == Blackhole
			if bh && time.Since(time.Unix(0, lastData.Load())) > blackholeKill {
				return // like a TCP keepalive giving up
			}
		}
	}
}

// pump copies src to dst subject to the direction's mode.
func (l *Link) pump(dst, src net.Conn, mode *atomic.Int32, lastData *atomic.Int64) {
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if d := time.Duration(l.delay.Load()); d > 0 {
				time.Sleep(d) // the injected latency itself
			}
			if p, m := l.spikePct.Load(), l.spikeMax.Load(); p > 0 && m > 0 && rand.IntN(100) < int(p) {
				time.Sleep(time.Duration(rand.Int64N(m))) // the injected spike itself
			}
			switch Mode(mode.Load()) {
			case Pass:
				lastData.Store(time.Now().UnixNano())
				l.bytes.Add(int64(n))
				if _, werr := dst.Write(buf[:n]); werr != nil {
					return
				}
			case Blackhole:
				// discarded
			case Drop:
				return
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return
			}
			if tc, ok := dst.(*net.TCPConn); ok {
				_ = tc.CloseWrite()
			}
			return
		}
	}
}
