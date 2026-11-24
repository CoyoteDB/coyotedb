//go:build chaos && worst && linux

// Linux-only worst cases. They need tools a container gives without root
// on the host (scripts/worst/run.sh): strace with CAP_SYS_PTRACE (the
// disk refusing writes, fsync stalling), tc with CAP_NET_ADMIN (real
// packet loss on the loopback device), prlimit (no privilege: own
// processes). WORST_SUDO=1 runs strace and tc through `sudo -n` (the
// container's tester may run only mount, tc, strace and kill as root).
package chaos

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func init() {
	for _, sc := range []Scenario{
		{Name: "worst-disk-readonly-main", Topo: Three, Body: func(r *Run) { diskReadOnly(r, true) }},
		{Name: "worst-disk-readonly-copy", Topo: Three, Body: func(r *Run) { diskReadOnly(r, false) }},
		{Name: "worst-fsync-stall-main-60s", Topo: Three, Body: func(r *Run) { fsyncStall(r, true, 60*time.Second) }},
		{Name: "worst-fsync-stall-copy-60s", Topo: Three, Body: func(r *Run) { fsyncStall(r, false, 60*time.Second) }},
		{Name: "worst-fd-exhaustion-main", Topo: Three, Body: fdExhaustionMain},
		{Name: "worst-netem-loss-30pct-60s", Topo: Three, Body: func(r *Run) { netemLoss(r, "loss 30% delay 2ms 1ms reorder 25% 50%", 60*time.Second) }},
		{Name: "worst-netem-loss-10pct-120s", Topo: Three, Body: func(r *Run) { netemLoss(r, "loss 10% delay 2ms 1ms reorder 10% 50%", 120*time.Second) }},
		{Name: "worst-netem-links-loss-30pct-60s", Topo: Three, Body: func(r *Run) { netemLinks(r, "loss 30% delay 2ms 1ms reorder 25% 50%", 60*time.Second) }},
		{Name: "worst-netem-links-loss-10pct-120s", Topo: Three, Body: func(r *Run) { netemLinks(r, "loss 10% delay 2ms 1ms reorder 10% 50%", 120*time.Second) }},
	} {
		scenarios = append(scenarios, worst(sc))
	}
	extraSoakFaults = append(extraSoakFaults, fault{"packet loss 20% on every connection (netem)", func(r *Run) func() {
		args := []string{"qdisc", "add", "dev", "lo", "root", "netem", "loss", "20%", "delay", "2ms", "1ms", "reorder", "10%", "50%"}
		if out, err := privileged("tc", args...).CombinedOutput(); err != nil {
			r.C.Fail("tc netem: %v %s", err, out)
		}
		return func() { _ = privileged("tc", "qdisc", "del", "dev", "lo", "root").Run() }
	}})
}

func privileged(name string, args ...string) *exec.Cmd {
	if os.Getenv("WORST_SUDO") == "1" {
		return exec.Command("sudo", append([]string{"-n", name}, args...)...)
	}
	return exec.Command(name, args...)
}

// writeCalls are the system calls that change a file or a folder. A
// read-only file system refuses all of them with EROFS. write(2) is left
// out on purpose: it also writes to sockets and pipes, which a read-only
// disk does not touch; PostgreSQL writes its files with pwrite/pwritev
// and syncs them, Go's raft log (bbolt) uses pwrite + fdatasync, and the
// agent's state files are fsynced and renamed, so every durable write of
// every process still fails.
const writeCalls = "pwrite64,pwritev,pwritev2,fsync,fdatasync,ftruncate,fallocate,rename,renameat,renameat2,unlink,unlinkat,mkdir,mkdirat,link,linkat,symlink,symlinkat"

// straceFault attaches strace to pids (following their new children)
// with the given -e inject spec and returns how to detach it.
func (r *Run) straceFault(what string, pids []int, trace, inject string) (stop func()) {
	args := []string{"-qq", "-f", "-o", "/dev/null", "-e", "trace=" + trace, "-e", "inject=" + inject}
	for _, p := range pids {
		args = append(args, "-p", strconv.Itoa(p))
	}
	cmd := privileged("strace", args...)
	errf, _ := os.Create(filepath.Join(r.C.Dir, "strace-"+strings.ReplaceAll(what, " ", "-")+".err"))
	cmd.Stderr = errf
	if err := cmd.Start(); err != nil {
		r.C.Fail("strace: %v", err)
	}
	r.C.Log.Event("strace %s: %d processes, inject %s", what, len(pids), inject)
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done); errf.Close() }()
	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		_ = cmd.Process.Signal(syscall.SIGTERM) // sudo passes it on; strace detaches
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			_ = privileged("kill", "-KILL", strconv.Itoa(cmd.Process.Pid)).Run()
			<-done
		}
		r.C.Log.Event("strace %s detached", what)
	}
	r.t.Cleanup(stop)
	// Attaching takes a moment; the fault starts once strace holds them.
	_ = poll(10*time.Second, func() bool { return tracedBy(pids[0]) })
	return stop
}

// tracedBy: pid is being traced (TracerPid in /proc).
func tracedBy(pid int) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return false
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "TracerPid:") {
			return strings.TrimSpace(strings.TrimPrefix(l, "TracerPid:")) != "0"
		}
	}
	return false
}

// nodePIDs is n's postmaster and children except the logger (its log
// stays readable so the run shows what Postgres said), plus, with agent,
// the agent and its watchdog.
func (r *Run) nodePIDs(n *Node, agent bool) []int {
	pm, kids := n.pgTree()
	if pm == 0 {
		r.C.Fail("no postmaster on %s", n.Name)
	}
	pids := []int{pm}
	for _, k := range kids {
		if !strings.Contains(k.cmd, "logger") {
			pids = append(pids, k.pid)
		}
	}
	if agent {
		pids = append(pids, n.AgentPID())
		if w := n.WatchdogPID(); w > 0 {
			pids = append(pids, w)
		}
	}
	return pids
}

// diskReadOnly: the disk of the main node (or a copy) turns read-only
// (what ext4 does after an I/O error with errors=remount-ro): every write,
// sync, create-by-rename and delete of the agent, its watchdog and every
// Postgres process fails with EROFS for 45 s. A real remount is refused
// while files are open for writing, and dm-flakey needs root, so the
// refusal is injected into the system calls (strace). Expected: the main
// node cannot write, another takes over within the budget; nothing lost;
// once writable again it comes back as a copy.
func diskReadOnly(r *Run, main bool) {
	p := r.Primary()
	n := p
	if !main {
		n = r.Standbys(p)[0]
	}
	stop := r.straceFault("readonly "+n.Name, r.nodePIDs(n, true), writeCalls, writeCalls+":error=EROFS")
	t0 := r.C.Log.Mark(n.Name + "'s disk refuses writes (EROFS) for 45 s")
	if main {
		r.expectWritesBack("main node's disk read-only", t0, ResumeBudget)
	}
	r.Let(time.Until(t0.Add(45 * time.Second)))
	gap := r.longestAckGap(t0, time.Now())
	r.C.Log.Event("while %s's disk was read-only: main node %q, term %d, longest pause between acknowledged writes %s",
		n.Name, r.C.PrimaryByCluster(), r.epochNow(), gap.Round(time.Millisecond))
	r.t.Logf("read-only %s: longest write pause %s, main node now %q", n.Name, gap.Round(time.Millisecond), r.C.PrimaryByCluster())
	if !main && gap > 15*time.Second {
		r.Problem("a copy's read-only disk stopped writes for %s", gap.Round(time.Millisecond))
	}
	if l := strings.ToLower(n.AllOutput()); !strings.Contains(l, "read-only") {
		r.Problem("%s's output never says its disk is read-only in plain words; last lines:\n%s", n.Name, lastLines(n.AllOutput(), 12))
	}
	stop()
	r.Let(5 * time.Second)
}

// fsyncStall: every fsync/fdatasync of the main node's (or a copy's)
// Postgres processes takes d (a disk whose flushes hang, e.g. a dying
// SSD or a saturated network disk), injected with strace delay_enter. The
// agent is not traced: it stays healthy. Expected for the main node:
// it cannot commit, so another node takes over within the
// budget; for a copy, writes go on through the other copy.
func fsyncStall(r *Run, main bool, d time.Duration) {
	p := r.Primary()
	n := p
	if !main {
		n = r.Standbys(p)[0]
	}
	us := strconv.FormatInt(d.Microseconds(), 10)
	stop := r.straceFault("fsync-stall "+n.Name, r.nodePIDs(n, false), "fsync,fdatasync", "fsync,fdatasync:delay_enter="+us)
	t0 := r.C.Log.Mark(fmt.Sprintf("every fsync of %s's Postgres takes %s", n.Name, d))
	if main {
		r.expectWritesBack("main node's fsync stall", t0, ResumeBudget)
	}
	r.Let(time.Until(t0.Add(d)))
	gap := r.longestAckGap(t0, time.Now())
	r.C.Log.Event("during the fsync stall on %s: main node %q, term %d, longest pause between acknowledged writes %s",
		n.Name, r.C.PrimaryByCluster(), r.epochNow(), gap.Round(time.Millisecond))
	r.t.Logf("fsync stall %s: longest write pause %s", n.Name, gap.Round(time.Millisecond))
	if !main && gap > 15*time.Second {
		r.Problem("a copy's stalled fsync stopped writes for %s", gap.Round(time.Millisecond))
	}
	stop()
	r.Let(10 * time.Second)
}

// fdExhaustionMain: the main node's agent, watchdog and every Postgres
// process run out of file descriptors (soft limit lowered to what each
// has open plus 2) for 40 s. Expected: nothing lost; writes keep going
// or another node takes over within the budget; whole after.
func fdExhaustionMain(r *Run) {
	p := r.Primary()
	pids := r.nodePIDs(p, true)
	type old struct{ pid, soft, hard int }
	var olds []old
	t0 := r.C.Log.Mark("file descriptors of " + p.Name + " exhausted for 40 s")
	for _, pid := range pids {
		soft, hard, ok := nofile(pid)
		if !ok {
			continue
		}
		ents, _ := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
		lim := len(ents) + 2
		if out, err := exec.Command("prlimit", "--pid", strconv.Itoa(pid), fmt.Sprintf("--nofile=%d:%d", lim, hard)).CombinedOutput(); err != nil {
			r.C.Log.Event("prlimit %d: %v %s", pid, err, out)
			continue
		}
		olds = append(olds, old{pid, soft, hard})
	}
	r.C.Log.Event("lowered the open-file limit of %d processes of %s", len(olds), p.Name)
	restore := func() {
		for _, o := range olds {
			_ = exec.Command("prlimit", "--pid", strconv.Itoa(o.pid), fmt.Sprintf("--nofile=%d:%d", o.soft, o.hard)).Run()
		}
	}
	defer restore()
	r.expectWritesBack("main node out of file descriptors", t0, ResumeBudget)
	r.Let(time.Until(t0.Add(40 * time.Second)))
	r.C.Log.Event("while out of file descriptors: main node %q, term %d, longest pause between acknowledged writes %s",
		r.C.PrimaryByCluster(), r.epochNow(), r.longestAckGap(t0, time.Now()).Round(time.Millisecond))
	restore()
	r.Let(10 * time.Second)
}

func nofile(pid int) (soft, hard int, ok bool) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/limits", pid))
	if err != nil {
		return 0, 0, false
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "Max open files") {
			f := strings.Fields(strings.TrimPrefix(l, "Max open files"))
			if len(f) >= 2 {
				s, e1 := strconv.Atoi(f[0])
				h, e2 := strconv.Atoi(f[1])
				return s, h, e1 == nil && e2 == nil
			}
		}
	}
	return 0, 0, false
}

// netemLoss: real packet loss (and reordering) on the loopback device of
// the container every node runs in, for d: every connection suffers it,
// node to node, agent to Postgres, and the test's own clients. Expected:
// nothing lost, never two main nodes, whole again after.
func netemLoss(r *Run, spec string, d time.Duration) {
	args := append([]string{"qdisc", "add", "dev", "lo", "root", "netem"}, strings.Fields(spec)...)
	if out, err := privileged("tc", args...).CombinedOutput(); err != nil {
		r.C.Fail("tc netem (needs CAP_NET_ADMIN; scripts/worst/run.sh): %v %s", err, out)
	}
	del := func() { _ = privileged("tc", "qdisc", "del", "dev", "lo", "root").Run() }
	r.t.Cleanup(del)
	t0 := r.C.Log.Mark("netem on lo: " + spec + " for " + d.String())
	e0 := r.epochNow()
	r.Let(d)
	del()
	r.afterLoss("netem " + spec)
	r.C.Log.Event("netem removed: term %d -> %d, longest pause between acknowledged writes %s", e0, r.epochNow(), r.longestAckGap(t0, time.Now()).Round(time.Millisecond))
	r.t.Logf("netem %s: term %d -> %d, longest write pause %s", spec, e0, r.epochNow(), r.longestAckGap(t0, time.Now()).Round(time.Millisecond))
}

// netemLinks: packet loss only between machines: every packet to or from
// a node's cluster port or one of the per-pair proxies goes through netem;
// each node's own traffic (agent to its Postgres, the test's clients to a
// node) does not. Expected: nothing lost, never two main nodes; writes go
// on (slower) or fail over; whole again after.
func netemLinks(r *Run, spec string, d time.Duration) {
	tc := func(args ...string) {
		if out, err := privileged("tc", args...).CombinedOutput(); err != nil {
			r.C.Fail("tc %v: %v %s", args, err, out)
		}
	}
	del := func() { _ = privileged("tc", "qdisc", "del", "dev", "lo", "root").Run() }
	r.t.Cleanup(del)
	tc("qdisc", "add", "dev", "lo", "root", "handle", "1:", "prio", "bands", "4", "priomap", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0")
	tc(append([]string{"qdisc", "add", "dev", "lo", "parent", "1:4", "handle", "40:", "netem"}, strings.Fields(spec)...)...)
	var ports []int
	for _, n := range r.C.Nodes {
		ports = append(ports, n.Listen)
	}
	for _, l := range r.C.Net.Links() {
		ports = append(ports, l.ln.Addr().(*net.TCPAddr).Port)
	}
	for _, p := range ports {
		for _, dir := range []string{"dport", "sport"} {
			tc("filter", "add", "dev", "lo", "parent", "1:", "protocol", "ip", "prio", "1", "u32", "match", "ip", dir, strconv.Itoa(p), "0xffff", "flowid", "1:4")
		}
	}
	t0 := r.C.Log.Mark(fmt.Sprintf("netem between machines (%d ports): %s for %s", len(ports), spec, d))
	e0 := r.epochNow()
	r.Let(d)
	del()
	gap := r.longestAckGap(t0, time.Now())
	r.afterLoss("netem between machines " + spec)
	var acks int
	for _, op := range r.W.H.snapshotAppends() {
		if op.Outcome == OK && op.End.After(t0) && op.End.Before(t0.Add(d)) {
			acks++
		}
	}
	r.C.Log.Event("netem between machines removed: term %d -> %d, %d appends acknowledged during it, longest pause %s", e0, r.epochNow(), acks, gap.Round(time.Millisecond))
	r.t.Logf("netem links %s: term %d -> %d, %d appends acknowledged during it, longest write pause %s", spec, e0, r.epochNow(), acks, gap.Round(time.Millisecond))
}

// afterLoss waits up to 3 minutes for the cluster to be whole after a
// network fault ended; if it is not, it saves what every socket and
// Postgres process is waiting on (diag-*.txt in the run folder) and
// records a problem. Heal then tries once more.
func (r *Run) afterLoss(what string) {
	t0 := time.Now()
	if poll(3*time.Minute, func() bool { _, ok := r.C.Healthy(); return ok }) == nil {
		r.C.Log.Event("%s: whole again %s after the fault ended", what, time.Since(t0).Round(100*time.Millisecond))
		r.t.Logf("%s: whole again %s after the fault ended", what, time.Since(t0).Round(100*time.Millisecond))
		return
	}
	dump := func(name string, cmd *exec.Cmd) {
		out, _ := cmd.CombinedOutput()
		_ = os.WriteFile(filepath.Join(r.C.Dir, "diag-"+name+".txt"), out, 0o644)
	}
	dump("ss", exec.Command("ss", "-tnpoi"))
	dump("ps", exec.Command("ps", "-eo", "pid,ppid,stat,wchan:32,etime,cmd"))
	for _, n := range r.C.Data() {
		var b strings.Builder
		for _, q := range []string{
			"SELECT string_agg(pid || ' ' || coalesce(backend_type,'') || ' ' || coalesce(wait_event_type,'') || '/' || coalesce(wait_event,'') || ' ' || coalesce(state,''), E'\\n') FROM pg_stat_activity",
			"SELECT coalesce(string_agg(pid || ' ' || status || ' ' || coalesce(flushed_lsn::text,'') || ' ' || coalesce(sender_host,'') || ':' || coalesce(sender_port::text,''), E'\\n'), 'no wal receiver') FROM pg_stat_wal_receiver",
			"SELECT coalesce(string_agg(application_name || ' ' || state || ' ' || sync_state, E'\\n'), 'no wal senders') FROM pg_stat_replication",
		} {
			s, err := r.queryText(n, q)
			fmt.Fprintf(&b, "-- %s\n%s %v\n", q, s, err)
		}
		_ = os.WriteFile(filepath.Join(r.C.Dir, "diag-pg-"+n.Name+".txt"), []byte(b.String()), 0o644)
	}
	r.Problem("%s: the cluster was not whole within 3 minutes after the fault ended (diag-*.txt in the run folder)", what)
}
