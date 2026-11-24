//go:build docker

// Package docker tests the CoyoteDB container image (docker/Dockerfile,
// D46) with real Docker, real containers and real PostgreSQL. No mocks.
//
//	go test -tags docker -count=1 -timeout 60m -v ./test/docker/
//
// It needs the `docker` command and a running engine (Linux, or Docker
// Desktop on macOS / Windows). Everything it creates is named
// coyote-test-dk-* (containers, volumes) and removed at the end; see
// docs/TESTING.md "Docker image".
//
// Knobs (all optional):
//
//	COYOTE_DOCKER_IMAGE   image to test (default coyote-test/coyote:dev)
//	COYOTE_DOCKER_BUILD   0 = use the image as it is, do not build it
//	COYOTE_DOCKER_SRC     repository root to build from (default: ../..)
//	COYOTE_DOCKER_BIND    host address ports are published on (default:
//	                      the bridge gateway on Linux, 127.0.0.1 elsewhere)
//	COYOTE_DOCKER_DIAL    address this test dials to reach those ports
//	                      (default: the bind address; when the test itself
//	                      runs in a container on Docker Desktop:
//	                      host.docker.internal)
//	COYOTE_DOCKER_KEEP    1 = leave containers and volumes for debugging
package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"coyotedb.com/coyote/test/testports"
)

const prefix = "coyote-test-dk-"

// ---- docker helpers ----

func docker(ctx context.Context, args ...string) (string, error) {
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("docker %s: %w: %s", strings.Join(redact(args), " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// redact keeps invite codes and passwords out of test logs.
func redact(args []string) []string {
	out := slices.Clone(args)
	for i, a := range out {
		if strings.HasPrefix(a, "coyote-") && len(a) > 40 {
			out[i] = "coyote-<invite>"
		}
		if strings.Contains(a, "password=") || strings.HasPrefix(a, "postgres") && strings.Contains(a, "@") {
			out[i] = "<conn>"
		}
	}
	return out
}

func dockerT(t *testing.T, timeout time.Duration, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := docker(ctx, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// image returns the image under test, building it first unless told not to.
func image(t *testing.T) string {
	t.Helper()
	img := env("COYOTE_DOCKER_IMAGE", "coyote-test/coyote:dev")
	if os.Getenv("COYOTE_DOCKER_BUILD") == "0" {
		dockerT(t, time.Minute, "image", "inspect", img)
		return img
	}
	src := env("COYOTE_DOCKER_SRC", filepath.Join("..", ".."))
	src, _ = filepath.Abs(src)
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "build", "-f", filepath.Join(src, "docker", "Dockerfile"), "-t", img, src)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker build: %v\n%s", err, tail(string(out), 40))
	}
	t.Logf("built %s in %s", img, time.Since(start).Round(time.Second))
	return img
}

func tail(s string, n int) string {
	l := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return strings.Join(l, "\n")
}

// bindAddr is the host address published ports listen on. Nodes reach
// each other through it (host.docker.internal), the way nodes on different
// machines reach each other through each host's published ports.
func bindAddr(t *testing.T) string {
	if v := os.Getenv("COYOTE_DOCKER_BIND"); v != "" {
		return v
	}
	if runtime.GOOS != "linux" {
		return "127.0.0.1" // Docker Desktop: host.docker.internal reaches it
	}
	gw := strings.TrimSpace(dockerT(t, time.Minute, "network", "inspect", "bridge", "--format", "{{(index .IPAM.Config 0).Gateway}}"))
	if net.ParseIP(gw) == nil {
		t.Fatalf("bridge gateway %q is not an address; set COYOTE_DOCKER_BIND", gw)
	}
	return gw
}

// ---- the cluster ----

type node struct {
	name        string // n1, n2, n3, w
	witness     bool
	clusterPort int
	clientPort  int
}

func (n node) container() string { return prefix + n.name }
func (n node) volume() string    { return prefix + n.name + "-data" }

type rig struct {
	t     *testing.T
	img   string
	bind  string
	nodes []node
}

func newRig(t *testing.T, img string) *rig {
	r := &rig{t: t, img: img, bind: bindAddr(t)}
	for _, nm := range []string{"n1", "n2", "n3", "w"} {
		n := node{name: nm, witness: nm == "w"}
		var err error
		if n.clusterPort, err = testports.Free(); err != nil {
			t.Fatal(err)
		}
		if !n.witness {
			if n.clientPort, err = testports.Free(); err != nil {
				t.Fatal(err)
			}
		}
		r.nodes = append(r.nodes, n)
	}
	r.removeAll() // leftovers of an earlier, interrupted run
	t.Cleanup(func() {
		if t.Failed() {
			r.dumpLogs()
		}
		if os.Getenv("COYOTE_DOCKER_KEEP") == "1" {
			t.Logf("COYOTE_DOCKER_KEEP=1: leaving %s* containers and volumes", prefix)
			return
		}
		r.removeAll()
		r.assertNothingLeft()
	})
	return r
}

func (r *rig) node(name string) node {
	for _, n := range r.nodes {
		if n.name == name {
			return n
		}
	}
	r.t.Fatalf("no node %q", name)
	return node{}
}

func (r *rig) data() []node {
	var out []node
	for _, n := range r.nodes {
		if !n.witness {
			out = append(out, n)
		}
	}
	return out
}

func (r *rig) removeAll() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	for _, n := range r.nodes {
		_, _ = docker(ctx, "rm", "-f", "-v", n.container())
		_, _ = docker(ctx, "volume", "rm", "-f", n.volume())
	}
}

func (r *rig) assertNothingLeft() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c, _ := docker(ctx, "ps", "-a", "--filter", "name="+prefix, "--format", "{{.Names}}")
	v, _ := docker(ctx, "volume", "ls", "--filter", "name="+prefix, "--format", "{{.Name}}")
	if strings.TrimSpace(c+v) != "" {
		r.t.Errorf("left behind after cleanup:\n%s%s", c, v)
	} else {
		r.t.Logf("cleanup: no %s* containers or volumes left", prefix)
	}
}

func (r *rig) dumpLogs() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, n := range r.nodes {
		out, _ := docker(ctx, "logs", "--tail", "40", n.container())
		r.t.Logf("---- docker logs %s ----\n%s", n.name, out)
		out, _ = docker(ctx, "run", "--rm", "-v", n.volume()+":/data", "--entrypoint", "tail", r.img, "-n", "60", "/data/coyote/logs/coyote.log")
		r.t.Logf("---- coyote.log %s ----\n%s", n.name, out)
	}
}

// start runs a node exactly as docker/README.md tells people to, except
// that the ports are this test's: published on the host, and the node
// advertises the host's address, so node-to-node traffic goes through the
// host's published ports as it does between machines.
func (r *rig) start(n node, mode, code string) {
	r.t.Helper()
	args := []string{"run", "-d", "--name", n.container(), "--hostname", n.name,
		"--stop-timeout", "120", "--shm-size", "256m",
		"--add-host", "host.docker.internal:host-gateway",
		"-p", fmt.Sprintf("%s:%d:%d", r.bind, n.clusterPort, n.clusterPort),
		"-v", n.volume() + ":/data"}
	if !n.witness {
		args = append(args, "-p", fmt.Sprintf("%s:%d:%d", r.bind, n.clientPort, n.clientPort))
	}
	args = append(args, r.img, mode)
	if code != "" {
		args = append(args, code)
	}
	args = append(args, "--name", n.name,
		"--listen", fmt.Sprintf("0.0.0.0:%d", n.clusterPort),
		"--advertise", fmt.Sprintf("host.docker.internal:%d", n.clusterPort))
	if !n.witness {
		args = append(args, "--client-port", strconv.Itoa(n.clientPort))
	}
	dockerT(r.t, 2*time.Minute, args...)
}

// ---- status ----

type status struct {
	Cluster struct {
		Epoch       uint64   `json:"epoch"`
		PrimaryName string   `json:"primary_name"`
		LeaderName  string   `json:"leader_name"`
		SyncSet     []string `json:"sync_set"`
	} `json:"cluster"`
	Nodes []struct {
		Name        string `json:"name"`
		Role        string `json:"role"`
		Reachable   bool   `json:"reachable"`
		Replication string `json:"replication"`
		LagBytes    int64  `json:"lag_bytes"`
		Report      *struct {
			State string `json:"state"`
			PG    *struct {
				Running   bool `json:"running"`
				Streaming bool `json:"streaming"`
			} `json:"postgres"`
		} `json:"report"`
	} `json:"nodes"`
}

func (r *rig) status(n node) (status, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := docker(ctx, "exec", n.container(), "coyote", "status", "--json")
	var s status
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		return s, fmt.Errorf("status json: %w", err)
	}
	return s, nil
}

func (s status) state(name string) (state string, streaming bool) {
	for _, m := range s.Nodes {
		if m.Name == name && m.Report != nil {
			return m.Report.State, m.Report.PG != nil && m.Report.PG.Streaming
		}
	}
	return "", false
}

// waitFor polls cond until it returns nil or the deadline passes.
func waitFor(t *testing.T, what string, d time.Duration, cond func() error) time.Duration {
	t.Helper()
	start := time.Now()
	var last error
	for time.Since(start) < d {
		if last = cond(); last == nil {
			return time.Since(start)
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s: %v", d, what, last)
	return 0
}

// waitHealthy waits until, seen from node `from`, main is the main node
// and every name in copies is a copy following live.
func (r *rig) waitHealthy(from node, main string, copies []string, d time.Duration) time.Duration {
	r.t.Helper()
	return waitFor(r.t, fmt.Sprintf("main %s with copies %v (seen from %s)", main, copies, from.name), d, func() error {
		s, err := r.status(from)
		if err != nil {
			return err
		}
		if s.Cluster.PrimaryName != main {
			return fmt.Errorf("main node is %q", s.Cluster.PrimaryName)
		}
		if st, _ := s.state(main); st != "primary" {
			return fmt.Errorf("%s is %q", main, st)
		}
		for _, c := range copies {
			if st, live := s.state(c); st != "standby" || !live {
				return fmt.Errorf("%s is %q, streaming=%v", c, st, live)
			}
		}
		return nil
	})
}

func (r *rig) invite(from node) string {
	r.t.Helper()
	var code string
	waitFor(r.t, "an invite code from "+from.name, 2*time.Minute, func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		out, err := docker(ctx, "exec", from.container(), "coyote", "invite", "--json")
		if err != nil {
			return err
		}
		var v struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal([]byte(out), &v); err != nil || v.Code == "" {
			return fmt.Errorf("invite json: %v", err)
		}
		code = v.Code
		return nil
	})
	return code
}

// admin runs SQL in the application database ("app") of a node's own
// Postgres as the superuser (inside the container, 127.0.0.1 only, as
// rule 11 says).
func (r *rig) admin(n node, sql string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cs, err := docker(ctx, "exec", n.container(), "coyote", "conninfo", "--admin")
	if err != nil {
		return "", err
	}
	cs = strings.TrimSpace(cs)
	if !strings.Contains(cs, "/postgres?") {
		return "", fmt.Errorf("admin connection string has an unexpected form")
	}
	cs = strings.Replace(cs, "/postgres?", "/app?", 1)
	out, err := docker(ctx, "exec", n.container(), "psql", cs, "-X", "-At", "-v", "ON_ERROR_STOP=1", "-c", sql)
	return strings.TrimSpace(out), err
}

// controlState reads pg_controldata of a stopped node's volume: "shut
// down" after a clean stop, "in production" after a crash.
func (r *rig) controlState(n node) string {
	r.t.Helper()
	out := dockerT(r.t, 2*time.Minute, "run", "--rm", "-v", n.volume()+":/data", "--entrypoint", "pg_controldata", r.img, "/data/coyote/pgdata")
	for l := range strings.SplitSeq(out, "\n") {
		if k, v, ok := strings.Cut(l, ":"); ok && strings.TrimSpace(k) == "Database cluster state" {
			return strings.TrimSpace(v)
		}
	}
	r.t.Fatalf("no cluster state in pg_controldata output:\n%s", out)
	return ""
}

// noZombies checks PID 1 is tini and no process in the container is a
// zombie (tini reaps orphans: Postgres children of a killed postmaster, a
// watchdog that outlived its agent).
func (r *rig) noZombies(n node) {
	r.t.Helper()
	comm := strings.TrimSpace(dockerT(r.t, time.Minute, "exec", n.container(), "cat", "/proc/1/comm"))
	if comm != "tini" {
		r.t.Errorf("%s: PID 1 is %q, want tini", n.name, comm)
	}
	out := dockerT(r.t, time.Minute, "exec", n.container(), "sh", "-c", "cat /proc/[0-9]*/stat 2>/dev/null")
	procs := 0
	for l := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		i := strings.LastIndexByte(l, ')')
		if i < 0 || i+2 >= len(l) {
			continue
		}
		procs++
		if l[i+2] == 'Z' {
			r.t.Errorf("%s: zombie process: %s", n.name, l[:i+1])
		}
	}
	r.t.Logf("%s: PID 1 is %s, %d processes, no zombies", n.name, comm, procs)
}

// ---- the writer ----

type writer struct {
	mu      sync.Mutex
	ok      map[int64]time.Time
	unknown map[int64]bool
	next    int64
	stop    chan struct{}
	done    chan struct{}
}

func (w *writer) okCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.ok)
}

// firstOKAfter is when the first write acknowledged after t was sent.
func (w *writer) firstOKAfter(t time.Time) (time.Time, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var best time.Time
	for _, at := range w.ok {
		if at.After(t) && (best.IsZero() || at.Before(best)) {
			best = at
		}
	}
	return best, !best.IsZero()
}

// startWriter inserts rows with unique ids through the client ports of
// all data nodes (multi-host connection string, as `coyote conninfo`
// prints). A row counts as acknowledged only when the commit returned
// without error; any error makes it "unknown" (may or may not be there).
func startWriter(t *testing.T, cfg *pgx.ConnConfig) *writer {
	w := &writer{ok: map[int64]time.Time{}, unknown: map[int64]bool{}, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		var c *pgx.Conn
		defer func() {
			if c != nil {
				c.Close(context.Background())
			}
		}()
		for {
			select {
			case <-w.stop:
				return
			default:
			}
			if c == nil {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				var err error
				c, err = pgx.ConnectConfig(ctx, cfg)
				cancel()
				if err != nil {
					c = nil
					time.Sleep(200 * time.Millisecond) // reconnect pacing, not a wait-for-condition
					continue
				}
			}
			w.mu.Lock()
			w.next++
			id := w.next
			w.mu.Unlock()
			sent := time.Now()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_, err := c.Exec(ctx, "INSERT INTO dk (id) VALUES ($1)", id)
			cancel()
			w.mu.Lock()
			if err == nil {
				w.ok[id] = sent
			} else {
				w.unknown[id] = true
			}
			w.mu.Unlock()
			if err != nil {
				c.Close(context.Background())
				c = nil
			}
		}
	}()
	return w
}

func (w *writer) halt() {
	close(w.stop)
	<-w.done
}

// ---- the test ----

// TestDockerFailover: 3 data nodes + a witness in containers on one host,
// talking through published ports. Rows are written all the time through
// the client ports. The main container is killed (docker kill: SIGKILL of
// the whole container, like pulling its plug); another node takes over,
// writes resume, the old main comes back and rejoins as a copy. Then the
// new main is stopped with docker stop and must shut Postgres down
// cleanly; it is started again and rejoins. At the end every acknowledged
// row is on the main node and all copies hold identical data.
func TestDockerFailover(t *testing.T) {
	img := image(t)
	r := newRig(t, img)
	n1, n2, n3, w := r.node("n1"), r.node("n2"), r.node("n3"), r.node("w")
	t.Logf("ports published on %s: n1 %d/%d, n2 %d/%d, n3 %d/%d, witness %d", r.bind,
		n1.clusterPort, n1.clientPort, n2.clusterPort, n2.clientPort, n3.clusterPort, n3.clientPort, w.clusterPort)

	// 1. Form the cluster: up, then join x2 and a witness, each with its
	// own one-time invite.
	r.start(n1, "up", "")
	d := r.waitHealthy(n1, "n1", nil, 5*time.Minute)
	t.Logf("n1 is the main node after %s", d.Round(time.Second))
	for _, j := range []node{n2, n3} {
		r.start(j, "join", r.invite(n1))
		d := r.waitHealthy(n1, "n1", []string{j.name}, 10*time.Minute)
		t.Logf("%s joined and follows live after %s", j.name, d.Round(time.Second))
	}
	r.start(w, "witness", r.invite(n1))
	waitFor(t, "witness in the cluster", 5*time.Minute, func() error {
		s, err := r.status(n1)
		if err != nil {
			return err
		}
		if st, _ := s.state("w"); st != "witness" {
			return fmt.Errorf("witness is %q", st)
		}
		return nil
	})
	r.waitHealthy(n1, "n1", []string{"n2", "n3"}, 5*time.Minute)

	// 2. The application side: password from conninfo, the CA copied out
	// of a node, TLS verify-full against it.
	ci := dockerT(t, time.Minute, "exec", n1.container(), "coyote", "conninfo", "--json")
	var info struct {
		User, Database, Password string
		Hosts                    []string
	}
	if err := json.Unmarshal([]byte(ci), &info); err != nil || info.Password == "" {
		t.Fatalf("conninfo: %v", err)
	}
	t.Logf("conninfo hosts (what applications on other machines use): %v", info.Hosts)
	tmp := t.TempDir()
	ca := filepath.Join(tmp, "ca.crt")
	dockerT(t, time.Minute, "cp", n1.container()+":/data/coyote/ca.crt", ca)
	var hosts []string
	for _, n := range r.data() {
		hosts = append(hosts, fmt.Sprintf("localhost:%d", n.clientPort))
	}
	cfg, err := pgx.ParseConfig(fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=verify-full&sslrootcert=%s&connect_timeout=5",
		info.User, info.Password, strings.Join(hosts, ","), info.Database, filepath.ToSlash(ca)))
	if err != nil {
		t.Fatal(err)
	}
	// The servers' certificates name "localhost"; the published ports
	// listen on the bind address. Dial that, verify the name "localhost".
	dial := env("COYOTE_DOCKER_DIAL", r.bind)
	if net.ParseIP(dial) == nil {
		addrs, err := net.DefaultResolver.LookupHost(context.Background(), dial)
		if err != nil || len(addrs) == 0 {
			t.Fatalf("COYOTE_DOCKER_DIAL %q: %v", dial, err)
		}
		dial = addrs[0]
	}
	cfg.LookupFunc = func(ctx context.Context, host string) ([]string, error) {
		if host == "localhost" {
			return []string{dial}, nil
		}
		return net.DefaultResolver.LookupHost(ctx, host)
	}
	{
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		c, err := pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			cancel()
			t.Fatalf("connect through the client ports: %v", err)
		}
		_, err = c.Exec(ctx, "CREATE TABLE dk (id bigint PRIMARY KEY, at timestamptz NOT NULL DEFAULT now())")
		c.Close(ctx)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
	}
	wr := startWriter(t, cfg)
	writerStopped := false
	defer func() {
		if !writerStopped {
			wr.halt()
		}
	}()
	waitFor(t, "300 acknowledged writes", 3*time.Minute, func() error {
		if n := wr.okCount(); n < 300 {
			return fmt.Errorf("%d so far", n)
		}
		return nil
	})

	// 3. PID 1: kill a copy's postmaster with SIGKILL. Its children lose
	// their parent and are reparented to PID 1 (tini), which must reap
	// them; the agent starts Postgres again and the copy follows live.
	pmpid := strings.TrimSpace(dockerT(t, time.Minute, "exec", n3.container(), "head", "-n", "1", "/data/coyote/pgdata/postmaster.pid"))
	if _, err := strconv.Atoi(pmpid); err != nil {
		t.Fatalf("postmaster.pid first line %q", pmpid)
	}
	dockerT(t, time.Minute, "exec", n3.container(), "sh", "-c", "kill -9 "+pmpid) // kill is a shell builtin here
	d = r.waitHealthy(n1, "n1", []string{"n2", "n3"}, 5*time.Minute)
	t.Logf("n3's postmaster (pid %s) killed: Postgres back and following live after %s", pmpid, d.Round(time.Second))
	waitFor(t, "n3 orphans reaped", time.Minute, func() error {
		out, err := docker(context.Background(), "exec", n3.container(), "sh", "-c", "grep -l '^[0-9]* (.*) Z' /proc/[0-9]*/stat 2>/dev/null || true")
		if err != nil {
			return err
		}
		if s := strings.TrimSpace(out); s != "" {
			return fmt.Errorf("zombies: %s", s)
		}
		return nil
	})
	r.noZombies(n3)

	// 4. docker kill the main container.
	before := wr.okCount()
	dockerT(t, time.Minute, "kill", n1.container())
	// docker kill returns once the signal is sent; time the outage from
	// when the container is known to be dead.
	waitFor(t, "n1 container dead", time.Minute, func() error {
		if st := strings.TrimSpace(dockerT(t, time.Minute, "inspect", "--format", "{{.State.Running}}", n1.container())); st != "false" {
			return fmt.Errorf("running=%s", st)
		}
		return nil
	})
	killed := time.Now()
	t.Logf("killed the main container n1 after %d acknowledged writes", before)
	var newMain string
	d = waitFor(t, "a new main node", 3*time.Minute, func() error {
		s, err := r.status(n2)
		if err != nil {
			return err
		}
		p := s.Cluster.PrimaryName
		if p == "" || p == "n1" {
			return fmt.Errorf("main node is %q", p)
		}
		if st, _ := s.state(p); st != "primary" {
			return fmt.Errorf("%s is %q", p, st)
		}
		newMain = p
		return nil
	})
	t.Logf("new main node %s after %s", newMain, d.Round(100*time.Millisecond))
	waitFor(t, "writes after the failover", 3*time.Minute, func() error {
		if n := wr.okCount(); n < before+200 {
			return fmt.Errorf("%d new so far", n-before)
		}
		return nil
	})
	if at, ok := wr.firstOKAfter(killed); ok {
		t.Logf("writes back %s after the main container died (first acknowledged write sent then)", at.Sub(killed).Round(100*time.Millisecond))
		if at.Sub(killed) > 90*time.Second {
			t.Errorf("writes took %s to come back (budget 90 s)", at.Sub(killed))
		}
	}
	// A killed container's Postgres did not shut down: the contrast for
	// the clean-stop check below.
	if st := r.controlState(n1); st != "in production" {
		t.Errorf("killed n1: pg_controldata says %q, want \"in production\"", st)
	}

	// 5. The old main comes back and rejoins as a copy.
	restarted := time.Now()
	dockerT(t, time.Minute, "start", n1.container())
	others := []string{"n1"}
	for _, n := range []string{"n2", "n3"} {
		if n != newMain {
			others = append(others, n)
		}
	}
	survivor := r.node(newMain)
	r.waitHealthy(survivor, newMain, others, 10*time.Minute)
	t.Logf("old main n1 back as a copy following live %s after docker start", time.Since(restarted).Round(time.Second))

	// 6. docker stop the main node: Postgres must shut down cleanly
	// within the stop timeout, and another node takes over.
	m := r.node(newMain)
	before = wr.okCount()
	stopAt := time.Now()
	dockerT(t, 3*time.Minute, "stop", m.container())
	took := time.Since(stopAt)
	code := strings.TrimSpace(dockerT(t, time.Minute, "inspect", "--format", "{{.State.ExitCode}}", m.container()))
	logs := dockerT(t, time.Minute, "logs", "--tail", "5", m.container())
	t.Logf("docker stop %s took %s, exit code %s", m.name, took.Round(100*time.Millisecond), code)
	if code != "0" {
		t.Errorf("exit code %s after docker stop, want 0 (137 means Docker had to SIGKILL it)", code)
	}
	if !strings.Contains(logs, "Stopped.") {
		t.Errorf("coyote did not report a clean stop; last output:\n%s", logs)
	}
	if st := r.controlState(m); st != "shut down" {
		t.Errorf("after docker stop, pg_controldata says %q, want \"shut down\"", st)
	} else {
		t.Logf("pg_controldata after docker stop: %q (clean shutdown)", st)
	}
	pglog := dockerT(t, time.Minute, "run", "--rm", "-v", m.volume()+":/data", "--entrypoint", "sh", r.img, "-c", "cat /data/coyote/logs/postgresql-*.log | tail -n 3")
	if !strings.Contains(pglog, "database system is shut down") {
		t.Errorf("Postgres log does not end with a clean shutdown:\n%s", pglog)
	} else {
		t.Logf("Postgres log ends: %s", tail(pglog, 1))
	}
	var third string
	d = waitFor(t, "a main node after docker stop", 3*time.Minute, func() error {
		s, err := r.status(n1)
		if err != nil {
			return err
		}
		p := s.Cluster.PrimaryName
		if p == "" || p == m.name {
			return fmt.Errorf("main node is %q", p)
		}
		if st, _ := s.state(p); st != "primary" {
			return fmt.Errorf("%s is %q", p, st)
		}
		third = p
		return nil
	})
	t.Logf("after docker stop of %s, %s is the main node after %s", m.name, third, d.Round(100*time.Millisecond))
	waitFor(t, "writes after the planned stop", 3*time.Minute, func() error {
		if n := wr.okCount(); n < before+200 {
			return fmt.Errorf("%d new so far", n-before)
		}
		return nil
	})
	dockerT(t, time.Minute, "start", m.container())
	var copies []string
	for _, n := range r.data() {
		if n.name != third {
			copies = append(copies, n.name)
		}
	}
	d = r.waitHealthy(r.node(third), third, copies, 10*time.Minute)
	t.Logf("%s back as a copy following live after %s", m.name, d.Round(time.Second))

	// 7. Stop writing and check every row.
	wr.halt()
	writerStopped = true
	wr.mu.Lock()
	okIDs, unknown, attempted := wr.ok, wr.unknown, wr.next
	wr.mu.Unlock()
	t.Logf("writes: %d acknowledged, %d unknown, %d attempted", len(okIDs), len(unknown), attempted)

	mainNode := r.node(third)
	var sum string
	waitFor(t, "all data nodes hold identical rows", 5*time.Minute, func() error {
		var sums []string
		for _, n := range r.data() {
			s, err := r.admin(n, "SELECT count(*) || ' ' || coalesce(md5(string_agg(id::text, ',' ORDER BY id)), '') FROM dk")
			if err != nil {
				return fmt.Errorf("%s: %w", n.name, err)
			}
			sums = append(sums, n.name+"="+s)
			if n.name == mainNode.name {
				sum = s
			}
		}
		for _, s := range sums {
			if _, v, _ := strings.Cut(s, "="); v != sum {
				return fmt.Errorf("differ: %v", sums)
			}
		}
		return nil
	})
	t.Logf("all three data nodes: %s rows/hash", sum)

	out, err := r.admin(mainNode, "SELECT id FROM dk ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	present := map[int64]bool{}
	for l := range strings.SplitSeq(out, "\n") {
		if l == "" {
			continue
		}
		id, err := strconv.ParseInt(l, 10, 64)
		if err != nil {
			t.Fatalf("bad id %q", l)
		}
		present[id] = true
	}
	lost := 0
	for id := range okIDs {
		if !present[id] {
			lost++
			if lost <= 10 {
				t.Errorf("acknowledged row %d is missing", id)
			}
		}
	}
	for id := range present {
		if _, ok := okIDs[id]; !ok && !unknown[id] {
			t.Errorf("row %d is present but was never written", id)
		}
	}
	if lost > 0 {
		t.Fatalf("%d acknowledged rows lost", lost)
	}
	t.Logf("no acknowledged row lost: all %d present on the main node %s", len(okIDs), mainNode.name)

	// 8. PID 1 on every node after all of it.
	for _, n := range r.nodes {
		r.noZombies(n)
	}
}

// TestDockerCompose brings up docker/docker-compose.yml (3 data nodes + a
// witness on one machine), checks that the cluster forms by itself and
// that a row written on the main node reaches every copy, then removes it
// all with `docker compose down -v`.
func TestDockerCompose(t *testing.T) {
	img := image(t)
	src, _ := filepath.Abs(env("COYOTE_DOCKER_SRC", filepath.Join("..", "..")))
	file := filepath.Join(src, "docker", "docker-compose.yml")
	project := "coyote-test-dk-compose"
	var ports []string
	for range 3 {
		p, err := testports.Free()
		if err != nil {
			t.Fatal(err)
		}
		ports = append(ports, strconv.Itoa(p))
	}
	compose := func(timeout time.Duration, args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, "docker", append([]string{"compose", "-p", project, "-f", file}, args...)...)
		cmd.Env = append(os.Environ(), "COYOTE_IMAGE="+img,
			"COYOTE_PORT_NODE1="+ports[0], "COYOTE_PORT_NODE2="+ports[1], "COYOTE_PORT_NODE3="+ports[2])
		out, err := cmd.CombinedOutput()
		if err != nil {
			return string(out), fmt.Errorf("docker compose %s: %w\n%s", strings.Join(args, " "), err, tail(string(out), 30))
		}
		return string(out), nil
	}
	if out, err := compose(time.Minute, "version"); err != nil {
		t.Fatalf("docker compose is not available here: %v %s", err, out)
	}
	_, _ = compose(3*time.Minute, "down", "-v", "--remove-orphans")
	t.Cleanup(func() {
		if t.Failed() {
			out, _ := compose(time.Minute, "logs", "--tail", "40")
			t.Logf("---- compose logs ----\n%s", out)
		}
		if os.Getenv("COYOTE_DOCKER_KEEP") == "1" {
			return
		}
		if _, err := compose(3*time.Minute, "down", "-v", "--remove-orphans"); err != nil {
			t.Error(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c, _ := docker(ctx, "ps", "-a", "--filter", "name="+project, "--format", "{{.Names}}")
		v, _ := docker(ctx, "volume", "ls", "--filter", "name="+project, "--format", "{{.Name}}")
		n, _ := docker(ctx, "network", "ls", "--filter", "name="+project, "--format", "{{.Name}}")
		if strings.TrimSpace(c+v+n) != "" {
			t.Errorf("left behind after compose down -v:\n%s%s%s", c, v, n)
		} else {
			t.Logf("cleanup: no %s containers, volumes or networks left", project)
		}
	})
	start := time.Now()
	if _, err := compose(15*time.Minute, "up", "-d", "--wait", "--wait-timeout", "600", "node1", "node2", "node3", "witness"); err != nil {
		t.Fatal(err)
	}
	t.Logf("compose up --wait finished after %s", time.Since(start).Round(time.Second))
	c1 := project + "-node1-1"
	exec1 := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		return docker(ctx, append([]string{"exec", c1}, args...)...)
	}
	var primary string
	waitFor(t, "3 data nodes and a witness, copies following live", 10*time.Minute, func() error {
		out, err := exec1("coyote", "status", "--json")
		if err != nil {
			return err
		}
		var s status
		if err := json.Unmarshal([]byte(out), &s); err != nil {
			return err
		}
		if s.Cluster.PrimaryName == "" {
			return errors.New("no main node")
		}
		want := map[string]bool{"node1": true, "node2": true, "node3": true, "witness": true}
		for _, m := range s.Nodes {
			delete(want, m.Name)
			if m.Name == s.Cluster.PrimaryName || m.Role == "witness" {
				continue
			}
			if st, live := s.state(m.Name); st != "standby" || !live {
				return fmt.Errorf("%s is %q streaming=%v", m.Name, st, live)
			}
		}
		if len(want) > 0 {
			return fmt.Errorf("missing %v", want)
		}
		primary = s.Cluster.PrimaryName
		return nil
	})
	sql := func(n, q string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		c := project + "-" + n + "-1"
		cs, err := docker(ctx, "exec", c, "coyote", "conninfo", "--admin")
		if err != nil {
			return "", err
		}
		out, err := docker(ctx, "exec", c, "psql", strings.TrimSpace(cs), "-X", "-At", "-v", "ON_ERROR_STOP=1", "-c", q)
		return strings.TrimSpace(out), err
	}
	if _, err := sql(primary, "CREATE TABLE ck(v text); INSERT INTO ck VALUES ('compose')"); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"node1", "node2", "node3"} {
		if n == primary {
			continue
		}
		waitFor(t, "the row on "+n, 2*time.Minute, func() error {
			out, err := sql(n, "SELECT v FROM ck")
			if err != nil {
				return err
			}
			if out != "compose" {
				return fmt.Errorf("got %q", out)
			}
			return nil
		})
	}
	t.Logf("row written on the main node %s is on both copies", primary)
}
