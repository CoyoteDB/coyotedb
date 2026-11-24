//go:build integration && unix

package integration

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestProxyHoldsAcrossCrashFailover: the main node's agent is killed, and
// applications open new connections through the surviving nodes' client
// ports 1 and 2 s later, with the default settings (10 s lease). Each one
// must be held until the new main node is ready and then land on it, not be
// closed shortly before (2026-11-24 real-app demos, finding 1: Umami's HTTP
// 500s and a git server's 401 at +10 s, when the hold was a fixed 10 s).
func TestProxyHoldsAcrossCrashFailover(t *testing.T) {
	root := t.TempDir()
	a, b, c := newCNode(t, root, "node-a"), newCNode(t, root, "node-b"), newCNode(t, root, "node-c")
	a.bootstrap()
	itemsOn(t, a.conn())
	b.join(a)
	c.join(a)
	syncFull(t, a, b, c)
	a.waitStatusSays(30*time.Second, "Main node lease: healthy")
	ci := a.connInfo()
	if p := primaryOf(t, a, b, c); p != a {
		t.Fatalf("main node is %v, want node-a", p)
	}

	type result struct {
		via     string
		after   time.Duration // connection opened this long after the kill
		took    time.Duration // until the connection was usable
		port    int
		inRecov bool
		err     error
	}
	// One host per connection and no connect timeout of the client's own,
	// like the demo apps (node-postgres and lib/pq have none by default;
	// `coyote conninfo` sets 10 s, which is per host).
	urls := map[*cnode]string{}
	for _, n := range []*cnode{b, c} {
		urls[n] = withHost(t, ci.URL, net.JoinHostPort("127.0.0.1", strconv.Itoa(n.client)), map[string]string{"connect_timeout": "0"})
	}
	open := func(n *cnode, after time.Duration, t0 time.Time, out chan<- result) {
		time.Sleep(time.Until(t0.Add(after))) // open at a set moment after the kill
		start := time.Now()
		r := result{via: n.name, after: after}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		conn, err := pgx.Connect(ctx, urls[n])
		if err != nil {
			r.err, r.took = err, time.Since(start)
			out <- r
			return
		}
		defer conn.Close(context.Background())
		r.err = conn.QueryRow(ctx, "SELECT current_setting('port')::int, pg_is_in_recovery()").Scan(&r.port, &r.inRecov)
		r.took = time.Since(start)
		out <- r
	}

	t0 := time.Now()
	_ = a.a.cmd.Process.Kill() // the main node's agent dies; its watchdog stops Postgres
	out := make(chan result, 4)
	go open(b, time.Second, t0, out)
	go open(c, time.Second, t0, out)
	go open(b, 2*time.Second, t0, out)
	go open(c, 2*time.Second, t0, out)
	<-a.a.exited
	waitNoNodeProcs(t, a.dir, 15*time.Second)

	var p *cnode
	waitFor(t, 60*time.Second, "a new main node", func() bool {
		p = primaryOf(t, b, c)
		return p != nil && p.clusterStatus().Cluster.PrimaryName == p.name
	})
	t.Logf("failover done by %s", time.Since(t0).Round(time.Millisecond))
	for range 4 {
		r := <-out
		t.Logf("via %s, opened at +%s: usable after %s (port %d, in recovery %v), err %v",
			r.via, r.after, r.took.Round(time.Millisecond), r.port, r.inRecov, r.err)
		if r.err != nil {
			t.Errorf("connection via %s opened %s after the crash failed: %v", r.via, r.after, r.err)
			continue
		}
		if r.port != p.port || r.inRecov {
			t.Errorf("connection via %s landed on port %d (in recovery %v), want the new main node %s (%d)",
				r.via, r.port, r.inRecov, p.name, p.port)
		}
	}
	stopAll(b, c)
}
