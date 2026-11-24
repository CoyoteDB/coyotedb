//go:build chaos && unix

package chaos

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgconn/ctxwatch"

	coyoteagent "coyotedb.com/coyote/internal/agent"
	"coyotedb.com/coyote/internal/pg"
	"coyotedb.com/coyote/test/testports"
)

// Node is one agent under test: its own data dir and ports. Every
// connection it opens to another agent goes through that pair's proxy.
type Node struct {
	Name    string
	Dir     string
	Listen  int // cluster port (mTLS)
	PG      int // Postgres, 127.0.0.1
	Ctl     int // control channel
	Client  int // client port (proxy), data nodes
	Witness bool

	c       *Cluster
	mu      sync.Mutex
	cmd     *exec.Cmd
	exited  chan struct{}
	stopped bool  // SIGSTOPped agent (or watchdog)
	frozen  []int // other pids the harness SIGSTOPped (watchdog); Cont resumes them
	joined  bool  // launched at least once (part of the cluster)
	env     []string
	logOff  int64 // output offset at the current launch
}

// Cluster is a running cluster plus its network.
type Cluster struct {
	t     *testing.T
	Dir   string
	Nodes []*Node
	Net   *Net
	Log   *RunLog
	pw    string // superuser password (never logged)
	late  []string
	// FormEnv is given to every agent launched by Form (fault points).
	FormEnv []string
	// ViaProxy: the workload connects as the application through the
	// client ports (see viaproxy.go); db is then the app database.
	ViaProxy bool
	app      *app
	db       string
}

func freePort() int {
	p, err := testports.Free()
	if err != nil {
		panic(err)
	}
	return p
}

// Topology names the nodes: data nodes and witnesses.
type Topology struct {
	Data      []string
	Witnesses []string
	// Late data nodes get ports and proxies but join only when the
	// scenario says so (Cluster.Join).
	Late []string
}

func (t Topology) String() string {
	s := fmt.Sprintf("%d data", len(t.Data))
	if len(t.Witnesses) > 0 {
		s += fmt.Sprintf(" + %d witness", len(t.Witnesses))
	}
	return s
}

var (
	Three      = Topology{Data: []string{"a", "b", "c"}}
	TwoWitness = Topology{Data: []string{"a", "b"}, Witnesses: []string{"w"}}
	Five       = Topology{Data: []string{"a", "b", "c", "d", "e"}}
	Two        = Topology{Data: []string{"a", "b"}}
	// ThreeTwoWitness has five voters, so it keeps a quorum after losing
	// two data nodes.
	ThreeTwoWitness = Topology{Data: []string{"a", "b", "c"}, Witnesses: []string{"w", "x"}}
)

// NewCluster creates the nodes, their proxies and dial maps (nothing runs yet).
func NewCluster(t *testing.T, dir string, topo Topology, log *RunLog) *Cluster {
	c := &Cluster{t: t, Dir: dir, Net: NewNet(), Log: log}
	for _, n := range topo.Data {
		c.Nodes = append(c.Nodes, &Node{Name: n, c: c})
	}
	for _, n := range topo.Witnesses {
		c.Nodes = append(c.Nodes, &Node{Name: n, c: c, Witness: true})
	}
	for _, n := range topo.Late {
		c.Nodes = append(c.Nodes, &Node{Name: n, c: c})
	}
	c.late = topo.Late
	for _, n := range c.Nodes {
		n.Dir = filepath.Join(dir, n.Name)
		n.Listen, n.PG, n.Ctl, n.Client = freePort(), freePort(), freePort(), freePort()
	}
	for _, x := range c.Nodes {
		var m []string
		for _, y := range c.Nodes {
			if x == y {
				continue
			}
			real := "127.0.0.1:" + strconv.Itoa(y.Listen)
			p, err := c.Net.Add(x.Name, y.Name, real)
			if err != nil {
				t.Fatal(err)
			}
			m = append(m, real+"="+p)
		}
		x.env = []string{"COYOTE_TEST_DIAL_MAP=" + strings.Join(m, ",")}
	}
	return c
}

func (c *Cluster) Node(name string) *Node {
	for _, n := range c.Nodes {
		if n.Name == name {
			return n
		}
	}
	c.t.Fatalf("no node %q", name)
	return nil
}

// Data is the data nodes that are part of the cluster.
func (c *Cluster) Data() []*Node {
	var out []*Node
	for _, n := range c.Nodes {
		if !n.Witness && n.isJoined() {
			out = append(out, n)
		}
	}
	return out
}

// Members is every node that is part of the cluster.
func (c *Cluster) Members() []*Node {
	var out []*Node
	for _, n := range c.Nodes {
		if n.isJoined() {
			out = append(out, n)
		}
	}
	return out
}

func (n *Node) isJoined() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.joined
}

func (c *Cluster) Names() []string {
	var out []string
	for _, n := range c.Nodes {
		out = append(out, n.Name)
	}
	return out
}

func (n *Node) logPath() string { return filepath.Join(n.c.Dir, n.Name+".out") }

// args is the agent command line.
func (n *Node) args(cmd, code string) []string {
	a := []string{cmd}
	if code != "" {
		a = append(a, code)
	}
	a = append(a, "--data-dir", n.Dir, "--name", "node-"+n.Name, "--listen", "127.0.0.1:"+strconv.Itoa(n.Listen),
		"--control-port", strconv.Itoa(n.Ctl))
	if !n.Witness {
		a = append(a, "--pg-port", strconv.Itoa(n.PG), "--client-port", strconv.Itoa(n.Client))
	}
	return a
}

// Launch starts the agent (not waiting). extraEnv: fault points.
func (n *Node) Launch(cmd, code string, extraEnv ...string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	f, err := os.OpenFile(n.logPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		n.c.t.Fatal(err)
	}
	if st, err := f.Stat(); err == nil {
		n.logOff = st.Size()
	}
	fmt.Fprintf(f, "\n===== %s launch %s %s =====\n", time.Now().Format(time.RFC3339Nano), cmd, strings.Join(extraEnv, " "))
	x := exec.Command(coyoteBin, n.args(cmd, code)...)
	x.Env = append(append(append(os.Environ(), "COYOTE_PG_CACHE="+pgCache), n.env...), extraEnv...)
	x.Stdout, x.Stderr = f, f
	if err := x.Start(); err != nil {
		f.Close()
		n.c.t.Fatal(err)
	}
	ch := make(chan struct{})
	go func() { _ = x.Wait(); f.Close(); close(ch) }()
	n.cmd, n.exited, n.stopped, n.joined = x, ch, false, true
	n.c.Log.Event("launch %s (%s, pid %d)", n.Name, cmd, x.Process.Pid)
}

// Output is the agent's output since the current launch.
func (n *Node) Output() string {
	n.mu.Lock()
	off := n.logOff
	n.mu.Unlock()
	b, err := os.ReadFile(n.logPath())
	if err != nil || int64(len(b)) < off {
		return ""
	}
	return string(b[off:])
}

// AllOutput is everything the agent printed in this run.
func (n *Node) AllOutput() string {
	b, _ := os.ReadFile(n.logPath())
	return string(b)
}

func (n *Node) Running() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.cmd == nil {
		return false
	}
	select {
	case <-n.exited:
		return false
	default:
		return true
	}
}

func (n *Node) AgentPID() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.cmd == nil {
		return 0
	}
	return n.cmd.Process.Pid
}

// WaitOut waits until the output since launch contains s.
func (n *Node) WaitOut(s string, within time.Duration) {
	n.c.t.Helper()
	if err := n.waitOut(s, within); err != nil {
		n.c.Fail("%v", err)
	}
}

// waitOut is WaitOut returning the failure: no s within the time, or the
// agent exited first (at once, with its last output).
func (n *Node) waitOut(s string, within time.Duration) error {
	n.mu.Lock()
	exited := n.exited
	n.mu.Unlock()
	var gone bool
	err := poll(within, func() bool {
		if strings.Contains(n.Output(), s) {
			return true
		}
		select {
		case <-exited:
			gone = !strings.Contains(n.Output(), s)
			return gone
		default:
			return false
		}
	})
	switch {
	case gone:
		return fmt.Errorf("%s exited before %q; its last output:\n%s", n.Name, s, lastLines(n.Output(), 30))
	case err != nil:
		return fmt.Errorf("%s: no %q within %s; its last output:\n%s", n.Name, s, within, lastLines(n.Output(), 30))
	}
	return nil
}

func lastLines(s string, k int) string {
	ls := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(ls) > k {
		ls = ls[len(ls)-k:]
	}
	return strings.Join(ls, "\n")
}

// launchReady launches n and waits for its ready line. If the agent
// reports at start that a local port is taken (another program grabbed
// it after the harness picked it), the node gets new local ports and is
// launched again. The cluster port is wired into every node's dial map, so
// it is not changed: a taken cluster port fails the run with that message.
func (n *Node) launchReady(cmd, code, ready string, within time.Duration, env ...string) {
	n.c.t.Helper()
	for attempt := 1; ; attempt++ {
		n.Launch(cmd, code, env...)
		err := n.waitOut(ready, within)
		if err == nil {
			return
		}
		out := n.Output()
		if attempt < 4 && testports.InUse(out) && !strings.Contains(out, "the cluster port") && !n.Running() {
			n.c.Log.Event("%s: a local port was taken; new ports, launch again", n.Name)
			n.PG, n.Ctl, n.Client = freePort(), freePort(), freePort()
			continue
		}
		n.c.Fail("%v", err)
	}
}

// CLI runs a one-shot command against this node's agent.
func (n *Node) CLI(ctx context.Context, args ...string) (string, error) {
	x := exec.CommandContext(ctx, coyoteBin, append(args, "--data-dir", n.Dir)...)
	x.Env = append(os.Environ(), "COYOTE_PG_CACHE="+pgCache)
	out, err := x.CombinedOutput()
	return string(out), err
}

// Status is `status --json` on this node (ok false if it did not answer).
func (n *Node) Status() (coyoteagent.Status, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := n.CLI(ctx, "status", "--json")
	var s coyoteagent.Status
	if err != nil || json.Unmarshal([]byte(out), &s) != nil {
		return s, false
	}
	return s, true
}

// Report is this node's own report (`dev status`).
func (n *Node) Report() (coyoteagent.NodeReport, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := n.CLI(ctx, "dev", "status")
	var r coyoteagent.NodeReport
	if err != nil || json.Unmarshal([]byte(out), &r) != nil {
		return r, false
	}
	return r, true
}

// ---- process faults ----

func (n *Node) signal(sig syscall.Signal) {
	if pid := n.AgentPID(); pid > 0 && n.Running() {
		_ = syscall.Kill(pid, sig)
	}
}

// Kill9 kills the agent (its watchdog then stops Postgres).
func (n *Node) Kill9() {
	n.c.Log.Event("kill -9 agent %s", n.Name)
	n.signal(syscall.SIGKILL)
	n.waitExit(30 * time.Second)
}

// Stop SIGSTOPs the agent; withPostmaster stops the postmaster too.
func (n *Node) Stop(withPostmaster bool) {
	n.c.Log.Event("SIGSTOP agent %s (postmaster too: %v)", n.Name, withPostmaster)
	if withPostmaster {
		if p := n.PostmasterPID(); p > 0 {
			_ = syscall.Kill(p, syscall.SIGSTOP)
		}
	}
	n.signal(syscall.SIGSTOP)
	n.mu.Lock()
	n.stopped = true
	n.mu.Unlock()
}

// Cont resumes a stopped agent (and postmaster and watchdog, if stopped).
func (n *Node) Cont() {
	n.c.Log.Event("SIGCONT %s", n.Name)
	if p := n.PostmasterPID(); p > 0 {
		_ = syscall.Kill(p, syscall.SIGCONT)
	}
	n.signal(syscall.SIGCONT)
	n.mu.Lock()
	frozen := n.frozen
	n.stopped, n.frozen = false, nil
	n.mu.Unlock()
	for _, p := range frozen {
		_ = syscall.Kill(p, syscall.SIGCONT)
	}
}

// WatchdogPID is the pid of this node's watchdog (0 if none).
func (n *Node) WatchdogPID() int {
	for _, p := range procs() {
		if p.ppid == n.AgentPID() && strings.Contains(p.cmd, pg.WatchdogCommand) {
			return p.pid
		}
	}
	return 0
}

// StopWatchdog SIGSTOPs this node's watchdog (Cont resumes it).
func (n *Node) StopWatchdog() bool {
	p := n.WatchdogPID()
	if p <= 0 {
		return false
	}
	n.c.Log.Event("SIGSTOP watchdog of %s (pid %d)", n.Name, p)
	_ = syscall.Kill(p, syscall.SIGSTOP)
	n.mu.Lock()
	n.stopped = true
	n.frozen = append(n.frozen, p)
	n.mu.Unlock()
	return true
}

// ContWatchdog resumes a watchdog StopWatchdog stopped, and nothing else.
func (n *Node) ContWatchdog() {
	n.mu.Lock()
	frozen := n.frozen
	n.frozen = nil
	n.mu.Unlock()
	for _, p := range frozen {
		n.c.Log.Event("SIGCONT watchdog of %s (pid %d)", n.Name, p)
		_ = syscall.Kill(p, syscall.SIGCONT)
	}
}

// KillPostmaster SIGKILLs this node's postmaster.
func (n *Node) KillPostmaster() bool {
	p := n.PostmasterPID()
	if p <= 0 {
		return false
	}
	n.c.Log.Event("kill -9 postmaster of %s (pid %d)", n.Name, p)
	_ = syscall.Kill(p, syscall.SIGKILL)
	return true
}

// KillWatchdog SIGKILLs this node's watchdog.
func (n *Node) KillWatchdog() bool {
	for _, p := range procs() {
		if p.ppid == n.AgentPID() && strings.Contains(p.cmd, pg.WatchdogCommand) {
			n.c.Log.Event("kill -9 watchdog of %s (pid %d)", n.Name, p.pid)
			_ = syscall.Kill(p.pid, syscall.SIGKILL)
			return true
		}
	}
	return false
}

// Interrupt stops the agent cleanly.
func (n *Node) Interrupt() {
	n.c.Log.Event("stop agent %s (SIGINT)", n.Name)
	n.mu.Lock()
	stopped := n.stopped
	n.mu.Unlock()
	if stopped {
		n.Cont()
	}
	n.signal(syscall.SIGINT)
	n.waitExit(3 * time.Minute)
}

func (n *Node) waitExit(d time.Duration) {
	n.mu.Lock()
	ch := n.exited
	n.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case <-ch:
	case <-time.After(d):
		n.c.Log.Event("agent %s did not exit within %s; killing", n.Name, d)
		n.signal(syscall.SIGKILL)
		<-ch
	}
}

// Restart runs plain `up` again and waits for the ready banner.
func (n *Node) Restart(extraEnv ...string) {
	n.launchReady("up", "", "Press Ctrl-C to stop.", 5*time.Minute, extraEnv...)
}

// PostmasterPID reads postmaster.pid (0 if none or not alive).
func (n *Node) PostmasterPID() int {
	b, err := os.ReadFile(filepath.Join(n.Dir, "pgdata", "postmaster.pid"))
	if err != nil {
		return 0
	}
	line, _, _ := strings.Cut(string(b), "\n")
	p, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || p <= 1 || syscall.Kill(p, 0) != nil {
		return 0
	}
	return p
}

type proc struct {
	pid, ppid int
	cmd       string
}

func procs() []proc {
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,command=").Output()
	if err != nil {
		return nil
	}
	var ps []proc
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 3 {
			continue
		}
		pid, _ := strconv.Atoi(f[0])
		ppid, _ := strconv.Atoi(f[1])
		ps = append(ps, proc{pid: pid, ppid: ppid, cmd: strings.Join(f[2:], " ")})
	}
	return ps
}

// WaitProc waits (polling every 10ms) for a process whose command line
// contains both substrings; it returns its pid or 0.
func WaitProc(within time.Duration, a, b string) int {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		for _, p := range procs() {
			if strings.Contains(p.cmd, a) && strings.Contains(p.cmd, b) {
				return p.pid
			}
		}
		time.Sleep(10 * time.Millisecond) // polling interval
	}
	return 0
}

// Strays lists processes still running from this run's directory.
func (c *Cluster) Strays() []string {
	var out []string
	for _, p := range procs() {
		if strings.Contains(p.cmd, c.Dir) && !strings.HasPrefix(p.cmd, "ps ") {
			out = append(out, fmt.Sprintf("%d %s", p.pid, p.cmd))
		}
	}
	return out
}

// ---- forming the cluster ----

// Form bootstraps the first data node and joins the others, then waits
// until every copy is in the sync set and streaming.
func (c *Cluster) Form() {
	first := c.Nodes[0]
	first.launchReady("up", "", "Press Ctrl-C to stop.", 10*time.Minute, c.FormEnv...)
	s, err := pg.ReadSecretsFile(filepath.Join(first.Dir, "cluster.secrets"))
	if err != nil {
		c.t.Fatal(err)
	}
	c.pw = s.Superuser
	for _, n := range c.Nodes[1:] {
		if slices.Contains(c.late, n.Name) {
			continue
		}
		c.Join(n, c.FormEnv...)
	}
	c.WaitHealthy(3 * time.Minute)
	if c.ViaProxy {
		c.loadApp()
		c.db = pg.AppDatabase
	}
}

// Join adds n to the cluster (invite from a running member), waiting for
// the ready banner.
func (c *Cluster) Join(n *Node, env ...string) {
	cmd, code := c.joinArgs(n)
	n.launchReady(cmd, code, "Press Ctrl-C to stop.", 5*time.Minute, env...)
}

// LaunchJoin starts n's join without waiting for it (for scenarios that
// kill the joining node on purpose).
func (c *Cluster) LaunchJoin(n *Node, env ...string) {
	cmd, code := c.joinArgs(n)
	n.Launch(cmd, code, env...)
}

// joinArgs is the command (join or witness) and a fresh invite from a
// running member.
func (c *Cluster) joinArgs(n *Node) (string, string) {
	var via *Node
	for _, m := range c.Members() {
		if m.Running() && m != n {
			via = m
			break
		}
	}
	code := c.invite(via)
	if n.Witness {
		return "witness", code
	}
	return "join", code
}

func (c *Cluster) invite(n *Node) string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := n.CLI(ctx, "invite", "--json")
	if err != nil {
		c.Fail("invite on %s failed: %v", n.Name, err)
	}
	var r coyoteagent.InviteResponse
	if json.Unmarshal([]byte(out), &r) != nil || !strings.HasPrefix(r.Code, "coyote-") {
		c.Fail("invite on %s: no code", n.Name)
	}
	return r.Code
}

// Healthy reports (from the primary's own agent) whether the cluster is
// whole: one main node with a healthy lease, every other data node in the
// sync set and streaming synchronously, copies eligible.
func (c *Cluster) Healthy() (string, bool) {
	for _, n := range c.Data() {
		if !n.Running() {
			return "", false
		}
	}
	for _, n := range c.Data() {
		s, ok := n.Status()
		if !ok || s.Cluster.PrimaryName == "" || s.Cluster.PrimaryName != "node-"+n.Name {
			continue
		}
		if s.Controller == nil || !s.Controller.View.LeaseHealthy {
			return "", false
		}
		if len(c.Data()) > 1 && !s.Cluster.StandbyEligible {
			return "", false
		}
		for _, m := range s.Nodes {
			if m.Role != "data" || m.Primary {
				continue
			}
			if m.Replication != "streaming (sync)" || !m.InSyncSet || m.Status != "active" {
				return "", false
			}
		}
		return n.Name, true
	}
	return "", false
}

// WaitHealthy waits for Healthy.
func (c *Cluster) WaitHealthy(within time.Duration) string {
	var p string
	err := poll(within, func() bool {
		var ok bool
		p, ok = c.Healthy()
		return ok
	})
	if err != nil {
		c.Fail("the cluster did not become healthy within %s", within)
	}
	return p
}

// PrimaryByCluster is the node the cluster names main node (asked of any
// running node).
//
// Nodes cut off from the others keep their last (stale) copy of the state,
// so the answer with the highest epoch wins.
func (c *Cluster) PrimaryByCluster() string {
	var best string
	var epoch uint64
	for _, n := range c.Members() {
		if !n.Running() {
			continue
		}
		if s, ok := n.Status(); ok && s.Cluster.PrimaryName != "" && (best == "" || s.Cluster.Epoch > epoch) {
			best, epoch = strings.TrimPrefix(s.Cluster.PrimaryName, "node-"), s.Cluster.Epoch
		}
	}
	return best
}

// Epoch is the cluster epoch as some running node knows it.
func (c *Cluster) Epoch() uint64 {
	var e uint64
	for _, n := range c.Members() {
		if !n.Running() {
			continue
		}
		if s, ok := n.Status(); ok {
			e = max(e, s.Cluster.Epoch)
		}
	}
	return e
}

// Fail records a harness failure.
func (c *Cluster) Fail(format string, args ...any) {
	c.t.Helper()
	msg := fmt.Sprintf(format, args...)
	c.Log.Event("FAIL: %s", msg)
	c.t.Fatal(msg)
}

// Shutdown stops every agent, then checks no process of the run is left.
func (c *Cluster) Shutdown() []string {
	for _, n := range c.Nodes {
		if n.Running() {
			n.Interrupt()
		}
	}
	var strays []string
	_ = poll(30*time.Second, func() bool { strays = c.Strays(); return len(strays) == 0 })
	c.Net.Close()
	return strays
}

// ---- SQL ----

// connConfig is a connection to node n's own Postgres. Context
// cancellation closes the socket (no CancelRequest: cancelling a commit
// that waits for a copy would make Postgres report it as committed).
func (c *Cluster) connConfig(n *Node) *pgx.ConnConfig {
	cfg, err := pgx.ParseConfig(pg.ConnString(n.PG, c.pw, true))
	if err != nil {
		panic(err)
	}
	cfg.ConnectTimeout = 3 * time.Second
	cfg.BuildContextWatcherHandler = func(pc *pgconn.PgConn) ctxwatch.Handler {
		return &pgconn.DeadlineContextWatcherHandler{Conn: pc.Conn()}
	}
	cfg.RuntimeParams["application_name"] = "chaos"
	if c.db != "" {
		cfg.Database = c.db
	}
	return cfg
}

func (c *Cluster) Connect(ctx context.Context, n *Node) (*pgx.Conn, error) {
	return pgx.ConnectConfig(ctx, c.connConfig(n))
}

// FindPrimary returns a connection to a node whose Postgres says it is
// not in recovery (the clients' own discovery, no cluster help).
func (c *Cluster) FindPrimary(ctx context.Context, prefer string) (*pgx.Conn, *Node, error) {
	nodes := c.Data()
	if prefer != "" {
		slices.SortStableFunc(nodes, func(a, b *Node) int {
			if a.Name == prefer {
				return -1
			}
			if b.Name == prefer {
				return 1
			}
			return 0
		})
	}
	for _, n := range nodes {
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		conn, err := c.Connect(cctx, n)
		if err != nil {
			cancel()
			continue
		}
		var rec bool
		err = conn.QueryRow(cctx, "SELECT pg_is_in_recovery()").Scan(&rec)
		cancel()
		if err != nil || rec {
			conn.Close(context.Background())
			continue
		}
		return conn, n, nil
	}
	return nil, nil, errors.New("no read-write node found")
}

func poll(within time.Duration, cond func() bool) error {
	deadline := time.Now().Add(within)
	for {
		if cond() {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("timed out")
		}
		time.Sleep(100 * time.Millisecond) // polling interval
	}
}

var termRe = regexp.MustCompile(`now the main node \(term (\d+), timeline (\d+)\)`)

// PromotionTimelines maps timeline -> (node, epoch) from the agents'
// "now the main node" lines.
func (c *Cluster) PromotionTimelines() map[uint32][]string {
	out := map[uint32][]string{}
	for _, n := range c.Data() {
		for _, m := range termRe.FindAllStringSubmatch(n.AllOutput(), -1) {
			tl, _ := strconv.Atoi(m[2])
			out[uint32(tl)] = append(out[uint32(tl)], n.Name+"@term"+m[1])
		}
	}
	return out
}
