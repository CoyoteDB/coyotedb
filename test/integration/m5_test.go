//go:build integration && unix

package integration

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	coyoteagent "coyotedb.com/coyote/internal/agent"
	"coyotedb.com/coyote/internal/pg"
)

// connInfo waits until the application role exists and returns it.
func (n *cnode) connInfo() coyoteagent.ConnInfo {
	n.t.Helper()
	var ci coyoteagent.ConnInfo
	waitFor(n.t, 60*time.Second, n.name+": application role ready", func() bool {
		n.cliJSON(&ci, "conninfo", "--json")
		return !ci.Pending
	})
	return ci
}

func tryConnect(url string) (*pgx.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return pgx.Connect(ctx, url)
}

// withHost rewrites an application URL to one host (and optionally other
// query parameters).
func withHost(t *testing.T, raw, host string, set map[string]string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	u.Host = host
	q := u.Query()
	for k, v := range set {
		q.Set(k, v)
	}
	u.RawQuery = strings.ReplaceAll(q.Encode(), "+", "%20")
	return u.String()
}

// The client path (rule 10 and client authentication): applications use
// the app role over TLS through any node's client port and always land on
// the main node; the internal roles are refused there; a plaintext client
// never reaches Postgres; the app role is refused on the plain local port;
// a switch drops the open client connections; a new password works and the
// old one stops working.
func TestM5ClientPath(t *testing.T) {
	root := t.TempDir()
	a, b := newCNode(t, root, "node-a"), newCNode(t, root, "node-b")
	w := newWitness(t, root, "wit")
	a.bootstrap()
	b.join(a)
	w.join(a)
	syncFull(t, a, b)

	ci := a.connInfo()
	hostA := net.JoinHostPort("127.0.0.1", strconv.Itoa(a.client))
	hostB := net.JoinHostPort("127.0.0.1", strconv.Itoa(b.client))
	if len(ci.Hosts) != 2 || ci.Hosts[0] != hostA || ci.Hosts[1] != hostB {
		t.Fatalf("hosts %v, want [%s %s]", ci.Hosts, hostA, hostB)
	}
	for _, want := range []string{"sslmode=verify-full", "target_session_attrs=read-write", "sslrootcert="} {
		if !strings.Contains(ci.URL, want) {
			t.Fatalf("conninfo URL lacks %s", want)
		}
	}
	if ci.User != pg.AppUser || ci.Database != pg.AppDatabase {
		t.Fatalf("user/db %s/%s", ci.User, ci.Database)
	}

	// The multi-host URL, and each node's port alone, all reach the main
	// node (read-write) as the app role over TLS.
	for name, u := range map[string]string{
		"multi-host": ci.URL,
		"via a":      withHost(t, ci.URL, hostA, nil),
		"via b":      withHost(t, ci.URL, hostB, map[string]string{"target_session_attrs": "any"}),
	} {
		c, err := tryConnect(u)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := queryText(t, c, "SELECT current_user"); got != pg.AppUser {
			t.Fatalf("%s: current_user %s", name, got)
		}
		if queryText(t, c, "SELECT pg_is_in_recovery()::text") != "false" {
			t.Fatalf("%s: landed on a copy", name)
		}
		if queryText(t, c, "SELECT ssl::text FROM pg_stat_ssl WHERE pid = pg_backend_pid()") != "true" {
			t.Fatalf("%s: not TLS end to end", name)
		}
		_ = c.Close(context.Background())
	}
	app, err := tryConnect(withHost(t, ci.URL, hostB, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close(context.Background())
	mustExec(t, app, "CREATE TABLE notes (id int PRIMARY KEY, body text)")
	mustExec(t, app, "INSERT INTO notes VALUES (1, 'through b')")

	// The superuser and the other internal roles are refused on the client
	// port even with the right password, before any password exchange.
	su := secretsOf(t, a.pgNode).Superuser
	for _, role := range []string{pg.Superuser, pg.ReplUser, pg.HealthUser} {
		u := withHost(t, ci.URL, hostA, nil)
		uu, _ := url.Parse(u)
		uu.User = url.UserPassword(role, su)
		uu.Path = "/postgres"
		if c, err := tryConnect(uu.String()); err == nil {
			c.Close(context.Background())
			t.Fatalf("%s logged in through the client port", role)
		} else if sqlState(err) != "28000" {
			t.Fatalf("%s: want 28000 (pg_hba reject), got %v", role, err)
		}
	}
	// No TLS: refused by the proxy with a message that says what to do.
	if c, err := tryConnect(withHost(t, ci.URL, hostA, map[string]string{"sslmode": "disable"})); err == nil {
		c.Close(context.Background())
		t.Fatal("a plaintext client got through")
	} else if !strings.Contains(err.Error(), "TLS") {
		t.Fatalf("plaintext refusal does not mention TLS: %v", err)
	}
	// The app role on the local Postgres port without TLS: rejected.
	plain := "postgresql://" + pg.AppUser + ":" + url.QueryEscape(ci.Password) + "@" + a.addr() + "/" + pg.AppDatabase + "?sslmode=disable"
	if c, err := tryConnect(plain); err == nil {
		c.Close(context.Background())
		t.Fatal("the app role logged in without TLS on the local port")
	} else if sqlState(err) != "28000" {
		t.Fatalf("app role on the plain port: %v", err)
	}

	// Status shows the proxy, with a verified main node.
	// (b: "via b" and the open app session; a: the plaintext refusal.)
	waitFor(t, 20*time.Second, "proxy stats in status", func() bool {
		ra, rb := a.report(), b.report()
		return ra.Proxy != nil && rb.Proxy != nil && rb.Proxy.Port == b.client && rb.Proxy.Verified &&
			rb.Proxy.Forwarded >= 2 && rb.Proxy.Active == 1 && ra.Proxy.RefusedPlain >= 1
	})

	// A planned switch closes the open client connection (rule 10); a new
	// one lands on the new main node.
	a.mustCLI("switch", "--to", "node-b")
	waitFor(t, 30*time.Second, "old client connection dropped", func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, err := app.Exec(ctx, "SELECT 1")
		return err != nil
	})
	c2, err := tryConnect(ci.URL)
	if err != nil {
		t.Fatal(err)
	}
	if queryText(t, c2, "SELECT pg_is_in_recovery()::text") != "false" || queryText(t, c2, "SELECT body FROM notes WHERE id = 1") != "through b" {
		t.Fatal("after the switch the client did not land on the new main node with the data")
	}
	mustExec(t, c2, "INSERT INTO notes VALUES (2, 'after switch')")
	_ = c2.Close(context.Background())

	// A new password: the old one stops working, the new one works, on
	// every node's port.
	var ni coyoteagent.ConnInfo
	b.cliJSON(&ni, "conninfo", "--json", "--new-password")
	if ni.Password == "" || ni.Password == ci.Password {
		t.Fatal("password not changed")
	}
	waitFor(t, 30*time.Second, "old password refused", func() bool {
		c, err := tryConnect(ci.URL)
		if err == nil {
			c.Close(context.Background())
			return false
		}
		var pe *pgconn.PgError
		return errors.As(err, &pe) && pe.Code == "28P01"
	})
	if c, err := tryConnect(ni.URL); err != nil {
		t.Fatalf("new password: %v", err)
	} else {
		c.Close(context.Background())
	}
	waitFor(t, 30*time.Second, "node-a's secrets file has the new password", func() bool {
		return secretsOf(t, a.pgNode).App == ni.Password
	})
	// Neither password reached a log (agent log or Postgres' own).
	for _, n := range []*cnode{a, b, w} {
		filepath.WalkDir(filepath.Join(n.dir, "logs"), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			body, _ := os.ReadFile(p)
			for _, pw := range []string{ci.Password, ni.Password} {
				if strings.Contains(string(body), pw) {
					t.Errorf("an application password appears in %s", p)
				}
			}
			return nil
		})
	}
	stopAll(a, b, w)
}

func memberAddrs(t *testing.T, n *cnode, name string) ([]string, bool) {
	t.Helper()
	for _, m := range n.clusterStatus().Nodes {
		if m.Name == name {
			return m.Addresses, m.AddressesPinned
		}
	}
	t.Fatalf("no member %s", name)
	return nil, false
}

// node set-address: every address is checked over the cluster's mutual
// TLS as that very node; a wrong one changes nothing; a right one is
// recorded, pinned, used by the cluster, and survives the node's restart.
func TestM5SetAddress(t *testing.T) {
	root := t.TempDir()
	a, b := newCNode(t, root, "node-a"), newCNode(t, root, "node-b")
	w := newWitness(t, root, "wit")
	a.bootstrap()
	itemsOn(t, a.conn())
	b.join(a)
	w.join(a)
	syncFull(t, a, b)
	orig, _ := memberAddrs(t, a, "node-b")

	// Nothing listens there.
	dead := net.JoinHostPort("127.0.0.1", strconv.Itoa(freePort(t)))
	if out, err := a.cli("node", "set-address", "node-b", dead); err == nil || !strings.Contains(out, "nothing was changed") {
		t.Fatalf("dead address: %v\n%s", err, out)
	}
	// node-a's own port: answers, but as node-a.
	aAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(a.listen))
	if out, err := a.cli("node", "set-address", "node-b", aAddr); err == nil || !strings.Contains(out, "is not") {
		t.Fatalf("other node's address: %v\n%s", err, out)
	}
	if got, pinned := memberAddrs(t, a, "node-b"); !slices.Equal(got, orig) || pinned {
		t.Fatalf("addresses changed by a refused set-address: %v %v", got, pinned)
	}

	// The same port under another name: accepted, pinned.
	alt := net.JoinHostPort("localhost", strconv.Itoa(b.listen))
	out := a.mustCLI("node", "set-address", "node-b", alt+","+dead)
	if !strings.Contains(out, "answers as node-b") || !strings.Contains(out, "now reach node-b") {
		t.Fatalf("set-address: %s", out)
	}
	want := []string{alt, dead}
	if got, pinned := memberAddrs(t, a, "node-b"); !slices.Equal(got, want) || !pinned {
		t.Fatalf("recorded %v pinned=%v", got, pinned)
	}
	// The cluster keeps working over it, and node-b keeps the pinned
	// addresses across a restart (it does not announce its own).
	b.a.stop(t)
	waitNoNodeProcs(t, b.dir, 15*time.Second)
	b.restart()
	waitFor(t, 60*time.Second, "node-b streaming again", b.isStreaming)
	insertRange(t, a.conn(), 500, 510, "after-set-address-")
	waitSame(t, a.pgNode, b.pgNode)
	if got, pinned := memberAddrs(t, b, "node-b"); !slices.Equal(got, want) || !pinned {
		t.Fatalf("after restart %v pinned=%v", got, pinned)
	}
	// --force records an address nobody can check.
	a.mustCLI("node", "set-address", "wit", dead+","+net.JoinHostPort("127.0.0.1", strconv.Itoa(w.listen)))
	if out, err := a.cli("node", "set-address", "wit", dead, "--force"); err != nil {
		t.Fatalf("--force: %v\n%s", err, out)
	}
	a.mustCLI("node", "set-address", "wit", net.JoinHostPort("127.0.0.1", strconv.Itoa(w.listen)))
	stopAll(a, b, w)
}
