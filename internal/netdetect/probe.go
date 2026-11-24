package netdetect

import (
	"cmp"
	"context"
	"errors"
	"net"
	"slices"
	"sync"
	"time"
)

// DefaultProbeTimeout bounds one reachability attempt.
const DefaultProbeTimeout = 2 * time.Second

// ProbeResult is the outcome for one candidate host:port.
type ProbeResult struct {
	Addr string        `json:"addr"`
	OK   bool          `json:"ok"`
	RTT  time.Duration `json:"rtt,omitempty"` // time to connect (about one round trip)
	Err  error         `json:"-"`
}

// ProbeDialFunc opens a connection to addr. The default is a plain TCP
// connect; join may pass one that also completes the TLS handshake.
type ProbeDialFunc func(ctx context.Context, addr string) (net.Conn, error)

// Probe tries every candidate in parallel, each bounded by timeout (<= 0
// means DefaultProbeTimeout), and returns results in input order. dial nil
// means TCP. Duplicates are probed once.
func Probe(ctx context.Context, addrs []string, timeout time.Duration, dial ProbeDialFunc) []ProbeResult {
	if timeout <= 0 {
		timeout = DefaultProbeTimeout
	}
	if dial == nil {
		dial = func(ctx context.Context, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", addr)
		}
	}
	type res struct {
		ok  bool
		rtt time.Duration
		err error
	}
	uniq := map[string]*res{}
	var wg sync.WaitGroup
	for _, a := range addrs {
		if _, ok := uniq[a]; ok {
			continue
		}
		r := &res{}
		uniq[a] = r
		wg.Add(1)
		go func(a string, r *res) { // each goroutine writes only its own r
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			t0 := time.Now()
			c, err := dial(cctx, a)
			rtt := time.Since(t0)
			if err == nil {
				c.Close()
			} else if errors.Is(err, context.DeadlineExceeded) || errors.Is(cctx.Err(), context.DeadlineExceeded) {
				err = errors.New("no answer within " + timeout.String())
			}
			r.ok, r.rtt, r.err = err == nil, rtt, err
		}(a, r)
	}
	wg.Wait()
	out := make([]ProbeResult, len(addrs))
	for i, a := range addrs {
		r := uniq[a]
		out[i] = ProbeResult{Addr: a, OK: r.ok, Err: r.err}
		if r.ok {
			out[i].RTT = r.rtt
		}
	}
	return out
}

// Fastest returns the reachable result with the lowest round trip.
func Fastest(rs []ProbeResult) (ProbeResult, bool) {
	ok := slices.DeleteFunc(slices.Clone(rs), func(r ProbeResult) bool { return !r.OK })
	if len(ok) == 0 {
		return ProbeResult{}, false
	}
	return slices.MinFunc(ok, func(a, b ProbeResult) int { return cmp.Compare(a.RTT, b.RTT) }), true
}

// Reachable returns the reachable addresses, fastest first.
func Reachable(rs []ProbeResult) []string {
	ok := slices.DeleteFunc(slices.Clone(rs), func(r ProbeResult) bool { return !r.OK })
	slices.SortStableFunc(ok, func(a, b ProbeResult) int { return cmp.Compare(a.RTT, b.RTT) })
	out := make([]string, len(ok))
	for i, r := range ok {
		out[i] = r.Addr
	}
	return out
}
