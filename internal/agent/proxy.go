package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync/atomic"
	"time"

	"coyotedb.com/coyote/internal/brand"
	"coyotedb.com/coyote/internal/cluster"
	"coyotedb.com/coyote/internal/pg"
	"coyotedb.com/coyote/internal/proxy"
)

// DefaultClientPort is where clients connect (the proxy, safety rule 10).
const DefaultClientPort = 5432

// Witnesses do not run the proxy. A witness has no Postgres, it is often a
// small machine whose only job is to vote, and it is not in any connection
// string (those list the data nodes); carrying client traffic through its
// tunnel would add a hop and a failure point for no gain.

// clusterRouter is the proxy's view of the cluster: epoch and primary from
// the replicated state (refreshed on every change), last quorum contact
// from Raft (cheap: no I/O).
type clusterRouter struct {
	cn    *cluster.Node
	cur   atomic.Pointer[routeView]
	ch    chan struct{}
	lease time.Duration
}

type routeView struct {
	epoch   uint64
	primary string
	lease   time.Duration
}

func newClusterRouter(cn *cluster.Node) *clusterRouter {
	r := &clusterRouter{cn: cn, ch: make(chan struct{}, 1)}
	r.update(cn.State())
	return r
}

func (r *clusterRouter) update(st cluster.State) {
	lease := st.Settings.LeaseDuration
	if st.Lease.Duration > 0 {
		lease = st.Lease.Duration
	}
	nv := &routeView{epoch: st.Epoch, primary: st.PrimaryID, lease: lease}
	if old := r.cur.Load(); old != nil && *old == *nv {
		return
	}
	r.cur.Store(nv)
	select {
	case r.ch <- struct{}{}:
	default:
	}
}

// run follows state changes until ctx ends.
func (r *clusterRouter) run(ctx context.Context) {
	sub := r.cn.Subscribe()
	defer sub.Close()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sub.C:
			r.update(r.cn.State())
		}
	}
}

// View implements proxy.Router. LastContact is conservative (see
// cluster.Node.QuorumContact): it is never later than the true moment the
// leader last held a quorum.
func (r *clusterRouter) View() proxy.View {
	v := r.cur.Load()
	return proxy.View{Epoch: v.epoch, Primary: v.primary, LastContact: r.cn.QuorumContact(), Lease: v.lease}
}

// Changed implements proxy.Router.
func (r *clusterRouter) Changed() <-chan struct{} { return r.ch }

// healthVerifier is proxy.PGVerifier with the health role's password taken
// from the cluster state at each check (it exists only once the first
// primary has published the secrets).
type healthVerifier struct{ cn *cluster.Node }

func (h healthVerifier) InRecovery(ctx context.Context, conn net.Conn) (bool, error) {
	s := h.cn.State().PGSecrets
	if s == nil {
		return false, errors.New("the cluster has no database passwords yet")
	}
	v := proxy.PGVerifier{User: pg.HealthUser, Password: pg.HealthPassword(s.Superuser)}
	return v.InRecovery(ctx, conn)
}

// dialPG opens a byte stream to node id's Postgres: the local port for this
// node, the mTLS tunnel otherwise.
func (a *Agent) dialPG(ctx context.Context, id string) (net.Conn, error) {
	if id == a.cn.ID() {
		addr := a.localPGAddr()
		if addr == "" {
			return nil, errors.New("postgres is not running on this node")
		}
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}
	return a.cn.PGDialer()(ctx, id)
}

// proxyListen is the proxy's listen address: the cluster listener's host
// with the client port.
func (a *Agent) proxyListen() string {
	host, _, err := net.SplitHostPort(a.listen)
	if err != nil {
		host = ""
	}
	return net.JoinHostPort(host, strconv.Itoa(a.clientPort))
}

// startProxy opens the client port. It runs after the tunnel exists and is
// shut down before Postgres stops (see Run).
func (a *Agent) startProxy(ctx context.Context, ln net.Listener) error {
	r := newClusterRouter(a.cn)
	go r.run(ctx)
	p, err := proxy.New(proxy.Config{
		Listeners:  []net.Listener{ln},
		Router:     r,
		Dial:       a.dialPG,
		Verifier:   healthVerifier{a.cn},
		Logger:     a.log.With("component", "proxy"),
		RequireTLS: true,
	})
	if err != nil {
		if addrInUse(err) {
			return &ExitError{Code: 1, Msg: portInUseMessage(a.clientPort)}
		}
		return fmt.Errorf("open the client port: %w", err)
	}
	a.proxy.Store(p)
	return nil
}

// stopProxy closes the client port: waiting clients are released, piped
// ones get a short grace to finish, then everything is closed.
func (a *Agent) stopProxy() {
	p := a.proxy.Swap(nil)
	if p == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := p.Shutdown(ctx); err != nil && !errors.Is(err, proxy.ErrClosed) {
		a.log.Info("proxy shutdown cut connections", "err", err)
	}
}

// portInUseMessage explains a busy client port and offers the next free
// one.
func portInUseMessage(port int) string {
	msg := fmt.Sprintf("Port %d is already in use on this machine (often another PostgreSQL). Clients connect to %s on this port, so it must be free.", port, brand.Product)
	if next := nextFreePort(port+1, 50); next > 0 {
		msg += fmt.Sprintf("\nTo use the next free port instead, start again with: --client-port %d", next)
	}
	return msg
}

func nextFreePort(from, tries int) int {
	for p := from; p < from+tries && p <= 65535; p++ {
		ln, err := net.Listen("tcp", net.JoinHostPort("", strconv.Itoa(p)))
		if err == nil {
			ln.Close()
			return p
		}
	}
	return 0
}
