//go:build chaos && unix

package chaos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgconn/ctxwatch"

	coyoteagent "coyotedb.com/coyote/internal/agent"
)

// Proxy-in-path runs (Scenario.ViaProxy): the workload is an application.
// Every client connects with the connection string `conninfo` prints (the
// app role, TLS verified against the cluster CA, every data node's client
// port, target_session_attrs=read-write), with the hosts rotated so the
// clients spread over all the proxies. Each connection asks which Postgres
// it reached (inet_server_port), so every operation is attributed to the
// node that really served it.
//
// On top of the usual checks this asserts rule 10 from the clients' side
// (checkRule10) and that the proxies did their part (proxy counters).

// app holds what `conninfo --json` said.
type app struct {
	url   string
	hosts []string
}

func (c *Cluster) loadApp() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var ci coyoteagent.ConnInfo
	err := poll(time.Minute, func() bool {
		out, err := c.Nodes[0].CLI(ctx, "conninfo", "--json")
		return err == nil && json.Unmarshal([]byte(out), &ci) == nil && !ci.Pending
	})
	if err != nil {
		c.Fail("no application connection string: %v", err)
	}
	c.app = &app{url: ci.URL, hosts: ci.Hosts}
}

// appConfig is the application's connection, hosts rotated by i.
func (c *Cluster) appConfig(i int) *pgx.ConnConfig {
	u, err := url.Parse(c.app.url)
	if err != nil {
		panic(err)
	}
	hs := slices.Clone(c.app.hosts)
	k := i % len(hs)
	u.Host = strings.Join(append(hs[k:], hs[:k]...), ",")
	cfg, err := pgx.ParseConfig(u.String())
	if err != nil {
		panic(err)
	}
	cfg.ConnectTimeout = 3 * time.Second
	cfg.BuildContextWatcherHandler = func(pc *pgconn.PgConn) ctxwatch.Handler {
		return &pgconn.DeadlineContextWatcherHandler{Conn: pc.Conn()}
	}
	cfg.RuntimeParams["application_name"] = "chaos-app"
	return cfg
}

// AppConnect connects as the application and reports which node served.
func (c *Cluster) AppConnect(ctx context.Context, i int) (*pgx.Conn, *Node, error) {
	conn, err := pgx.ConnectConfig(ctx, c.appConfig(i))
	if err != nil {
		return nil, nil, err
	}
	var port int
	var rec bool
	if err := conn.QueryRow(ctx, "SELECT inet_server_port(), pg_is_in_recovery()").Scan(&port, &rec); err != nil {
		conn.Close(context.Background())
		return nil, nil, err
	}
	for _, n := range c.Data() {
		if n.PG == port {
			if rec {
				conn.Close(context.Background())
				c.Fail("rule 10: a read-write connection through the client port reached %s, a copy", n.Name)
			}
			return conn, n, nil
		}
	}
	conn.Close(context.Background())
	return nil, nil, fmt.Errorf("served by an unknown Postgres port %d", port)
}

// PrimaryChange is a main node the harness saw (At is when it saw it; the
// true change happened earlier).
type PrimaryChange struct {
	At   time.Time
	Node string
}

// rule10Grace: after the harness saw a new main node, an old one may still
// finish an operation for at most the lease (6 s, it is fenced by then)
// plus margin for the harness' own lag.
const rule10Grace = 9 * time.Second

// checkRule10: no operation acknowledged through the client ports was
// served by a node that had been replaced as the main node more than
// rule10Grace earlier (unless it had become the main node again).
func checkRule10(h *History) []string {
	changes := slices.Clone(h.Primaries)
	slices.SortFunc(changes, func(a, b PrimaryChange) int { return a.At.Compare(b.At) })
	replacedAt := func(node string, t time.Time) (time.Time, bool) {
		// Latest time before t at which node stopped being the recorded
		// main node, with no later return of node recorded up to t +
		// rule10Grace.
		var away time.Time
		for i := 1; i < len(changes); i++ {
			if changes[i-1].Node == node && changes[i].Node != node && changes[i].At.Before(t) {
				away = changes[i].At
			}
			if changes[i].Node == node && !away.IsZero() && changes[i].At.Before(t.Add(rule10Grace)) && changes[i].At.After(away) {
				away = time.Time{}
			}
		}
		return away, !away.IsZero()
	}
	var out []string
	bad := func(kind, node string, end time.Time) {
		if away, ok := replacedAt(node, end); ok && end.After(away.Add(rule10Grace)) {
			out = append(out, fmt.Sprintf("rule 10: %s acknowledged by %s at %s, %s after it was replaced as the main node",
				kind, node, end.Format("15:04:05.000"), end.Sub(away).Round(time.Millisecond)))
		}
	}
	for _, a := range h.Appends {
		if a.Outcome == OK {
			bad(fmt.Sprintf("append %d", a.ID), a.Node, a.End)
		}
	}
	for _, r := range h.Regs {
		if r.Outcome == OK {
			bad(fmt.Sprintf("register op on key %d", r.Key), r.Node, r.End)
		}
	}
	if len(out) > 5 {
		out = append(out[:5], fmt.Sprintf("… and %d more", len(out)-5))
	}
	return out
}

// ProxyTotals adds up the proxies' counters over the running data nodes.
func (c *Cluster) ProxyTotals() (coyoteagent.ProxyReport, map[string]coyoteagent.ProxyReport) {
	var sum coyoteagent.ProxyReport
	per := map[string]coyoteagent.ProxyReport{}
	for _, n := range c.Data() {
		if !n.Running() {
			continue
		}
		r, ok := n.Report()
		if !ok || r.Proxy == nil {
			continue
		}
		p := *r.Proxy
		per[n.Name] = p
		sum.Total += p.Total
		sum.Forwarded += p.Forwarded
		sum.DroppedOnEpoch += p.DroppedOnEpoch
		sum.DroppedNoView += p.DroppedNoView
		sum.RefusedNoView += p.RefusedNoView
		sum.RefusedPlain += p.RefusedPlain
		sum.WaitTimeouts += p.WaitTimeouts
		sum.Waited += p.Waited
	}
	return sum, per
}

// expectProxy checks the proxies' counters at the end of a run.
func (r *Run) expectProxy(what string, ok func(sum coyoteagent.ProxyReport, per map[string]coyoteagent.ProxyReport) bool) {
	sum, per := r.C.ProxyTotals()
	b, _ := json.Marshal(per)
	r.C.Log.Event("proxy counters: %s", b)
	if !ok(sum, per) {
		r.C.Fail("proxy: %s (counters %s)", what, b)
	}
}

var errNoApp = errors.New("no application connection string")

// appSchema creates the workload's tables as the application role.
func (c *Cluster) appSchema(ctx context.Context) (*pgx.Conn, error) {
	if c.app == nil {
		return nil, errNoApp
	}
	var last error
	for i := range 20 {
		conn, _, err := c.AppConnect(ctx, i)
		if err == nil {
			return conn, nil
		}
		last = err
		time.Sleep(250 * time.Millisecond)
	}
	return nil, last
}
