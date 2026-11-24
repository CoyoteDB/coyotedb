//go:build chaos && unix && worst

// Worst cases (docs/TESTING.md "Worst cases"): disks that fill up, go
// read-only or stall; flipped bytes on a copy; machines that die in the
// middle of a join, a switch or a promotion; a backend killed by the
// out-of-memory killer; ports taken; lossy, flapping and changing
// networks. Each is a named chaos scenario judged by the run's final
// Check (no acknowledged write lost, no failed write present, all nodes
// identical, bank total, linearizable register, never two main nodes),
// plus what the scenario itself expects (writes back in time, a plain
// message), reported as problems so the final Check still runs.
//
// Build tag `worst`: `go test -tags 'chaos worst' ./test/chaos/`. Some
// need a small volume (WORST_DISKS on Linux; a disk image on macOS) or
// Linux tools (worst_linux_test.go); scripts/worst/run.sh runs them all
// in a container on Linux.
//
// Harness mutation (WORST_MUTATE=lose-ack): at the end of the scenario's
// fault story one acknowledged row is deleted on the main node, so the
// final Check must report a lost write. It proves the check has teeth in
// that scenario's flow; it says nothing about the product.
package chaos

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"coyotedb.com/coyote/internal/pg"
)

func init() {
	for _, sc := range worstScenarios {
		scenarios = append(scenarios, worst(sc))
	}
}

// worstScenarios: portable worst cases (Linux-only ones are added by
// worst_linux_test.go).
var worstScenarios = []Scenario{
	// ---- disks ----
	{Name: "worst-disk-full-main", Topo: Three, Prepare: onSmallVolume("a", 1024), Body: diskFullMain},
	{Name: "worst-disk-full-copy", Topo: Three, Prepare: onSmallVolume("b", 1024), Body: diskFullCopy},
	{Name: "worst-pg-frozen-main-60s", Topo: Three, Body: pgFrozenMain},
	{Name: "worst-pg-frozen-copy-60s", Topo: Three, Body: pgFrozenCopy},
	// ---- corruption ----
	{Name: "worst-corrupt-copy-heap-page", Topo: TwoWitness, Body: corruptCopyHeapPage},
	{Name: "worst-corrupt-copy-main-dies-at-detection", Topo: TwoWitness, Body: corruptCopyMainDiesAtDetection},
	{Name: "worst-corrupt-copy-wal", Topo: TwoWitness, Body: corruptCopyWAL},
	// ---- mid-operation ----
	joinKill("worst-power-cut-join-contacting", "Joining the cluster as"),
	joinKill("worst-power-cut-join-joined", "Joined."),
	joinKill("worst-power-cut-join-cloning", "  50%"),
	joinKill("worst-power-cut-join-copied", "Copy complete."),
	joinKill("worst-power-cut-join-following", "live."),
	switchKill("worst-kill-switch-before-cut", "switch-before-cut"),
	switchKill("worst-kill-switch-after-cut", "switch-after-cut"),
	switchKill("worst-kill-switch-after-cas", "switch-after-cas"),
	{Name: "worst-power-cut-new-main-mid-promotion", Topo: Three, Body: func(r *Run) { midPromotion(r, false) }},
	{Name: "worst-power-cut-new-main-mid-switch-promotion", Topo: Three, Body: func(r *Run) { midPromotion(r, true) }},
	// D56: the cut lands right after Postgres removed standby.signal,
	// before its end-of-recovery record (the P1 window), every run.
	{Name: "worst-power-cut-new-main-signal-gone", Topo: Three, Body: func(r *Run) { promotionCutAt(r, false, signalGoneCut) }},
	{Name: "worst-power-cut-new-main-signal-gone-switch", Topo: Three, Body: func(r *Run) { promotionCutAt(r, true, signalGoneCut) }},
	// ---- resources ----
	{Name: "worst-oom-kill-backends-main", Topo: Three, Body: oomKillMain},
	{Name: "worst-oom-kill-startup-copy", Topo: Three, Body: oomKillCopy},
	{Name: "worst-port-taken-on-restart", Topo: Three, Body: portTakenOnRestart},
	// ---- network ----
	{Name: "worst-latency-spikes-30pct", Topo: Three, Body: latencySpikes},
	{Name: "worst-asymmetric-flap-3m", Topo: Three, Body: func(r *Run) { asymmetricFlap(r, 3*time.Minute) }},
	{Name: "worst-asymmetric-flap-3m-five", Topo: Five, Body: func(r *Run) { asymmetricFlap(r, 3*time.Minute) }},
	{Name: "worst-main-address-change", Topo: Three, Body: mainAddressChange},
}

// worst adds the harness mutation to a scenario.
func worst(sc Scenario) Scenario {
	body := sc.Body
	sc.Body = func(r *Run) {
		body(r)
		if os.Getenv("WORST_MUTATE") == "lose-ack" {
			r.loseOneAck()
		}
	}
	return sc
}

// loseOneAck deletes one acknowledged append on the main node (harness
// mutation, see the package comment).
func (r *Run) loseOneAck() {
	var id int64 = -1
	for _, op := range r.W.H.snapshotAppends() {
		if op.Outcome == OK {
			id = op.ID
		}
	}
	if id < 0 {
		r.C.Fail("mutation: no acknowledged append to delete")
	}
	err := poll(3*time.Minute, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		conn, _, err := r.C.FindPrimary(ctx, "")
		if err != nil {
			return false
		}
		defer conn.Close(context.Background())
		_, err = conn.Exec(ctx, "DELETE FROM chaos_append WHERE id = $1", id)
		return err == nil
	})
	if err != nil {
		r.C.Fail("mutation: could not delete append %d", id)
	}
	r.C.Log.Event("MUTATION: deleted acknowledged append %d on the main node; the check must report it lost", id)
}

// ---- helpers ----

// pgTree is n's postmaster and its child processes (backends, walwriter,
// checkpointer, logger, walsenders, startup ...).
func (n *Node) pgTree() (pm int, kids []proc) {
	pm = n.PostmasterPID()
	if pm == 0 {
		return 0, nil
	}
	for _, p := range procs() {
		if p.ppid == pm {
			kids = append(kids, p)
		}
	}
	return pm, kids
}

// freezePG SIGSTOPs every Postgres process of n (not its agent or
// watchdog): a database whose every disk access hangs. It returns how to
// wake them.
func (r *Run) freezePG(n *Node) (thaw func()) {
	pm, kids := n.pgTree()
	if pm == 0 {
		r.C.Fail("no postmaster on %s", n.Name)
	}
	pids := []int{pm}
	for _, k := range kids {
		pids = append(pids, k.pid)
	}
	for _, p := range pids {
		_ = syscall.Kill(p, syscall.SIGSTOP)
	}
	r.C.Log.Event("SIGSTOP every Postgres process of %s (%d processes; agent and watchdog still run)", n.Name, len(pids))
	var once atomic.Bool
	return func() {
		if !once.CompareAndSwap(false, true) {
			return
		}
		// Children forked just before the stop are caught by a second pass.
		_, kids := n.pgTree()
		for _, k := range kids {
			pids = append(pids, k.pid)
		}
		for _, p := range pids {
			_ = syscall.Kill(p, syscall.SIGCONT)
		}
		r.C.Log.Event("SIGCONT every Postgres process of %s", n.Name)
	}
}

// logTail reads what n's Postgres logs gained since the last call.
type logTail struct {
	n   *Node
	off map[string]int64
}

func newLogTail(n *Node) *logTail {
	t := &logTail{n: n, off: map[string]int64{}}
	t.next()
	return t
}

func (t *logTail) next() string {
	fs, _ := filepath.Glob(filepath.Join(t.n.Dir, "logs", "postgresql-*.log"))
	var b strings.Builder
	for _, f := range fs {
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		if st, err := fh.Stat(); err == nil && st.Size() > t.off[f] {
			buf := make([]byte, st.Size()-t.off[f])
			k, _ := fh.ReadAt(buf, t.off[f])
			t.off[f] += int64(k)
			b.Write(buf[:k])
		}
		fh.Close()
	}
	return b.String()
}

// pgLogs is everything n's Postgres wrote to its log files.
func (n *Node) pgLogs() string {
	fs, _ := filepath.Glob(filepath.Join(n.Dir, "logs", "postgresql-*.log"))
	slices.Sort(fs)
	var b strings.Builder
	for _, f := range fs {
		x, _ := os.ReadFile(f)
		b.Write(x)
	}
	return b.String()
}

// sql runs one statement on n's own Postgres.
func (r *Run) sql(n *Node, q string, args ...any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	conn, err := r.C.Connect(ctx, n)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	_, err = conn.Exec(ctx, q, args...)
	return err
}

// mustSQL is sql that fails the run.
func (r *Run) mustSQL(n *Node, q string, args ...any) {
	if err := r.sql(n, q, args...); err != nil {
		r.C.Fail("%s on %s: %v", q, n.Name, err)
	}
}

// queryText reads one text value on n.
func (r *Run) queryText(n *Node, q string, args ...any) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := r.C.Connect(ctx, n)
	if err != nil {
		return "", err
	}
	defer conn.Close(context.Background())
	var s string
	err = conn.QueryRow(ctx, q, args...).Scan(&s)
	return s, err
}

// expectWritesBack: some write that started after t was acknowledged
// within d of t. A miss is a problem (the run goes on to the Check).
func (r *Run) expectWritesBack(what string, t time.Time, d time.Duration) time.Duration {
	var got time.Duration = -1
	_ = poll(time.Until(t.Add(d))+time.Second, func() bool {
		got = ResumeAfter(r.W.H, t)
		return got >= 0
	})
	if got < 0 || got > d {
		r.Problem("%s: writes were not acknowledged again within %s (first after: %s)", what, d, fmtDur(ResumeAfter(r.W.H, t)))
	} else {
		r.C.Log.Event("%s: writes acknowledged again %s after it", what, got.Round(time.Millisecond))
		r.t.Logf("%s: writes acknowledged again %s after it", what, got.Round(time.Millisecond))
	}
	return got
}

func fmtDur(d time.Duration) string {
	if d < 0 {
		return "none yet"
	}
	return d.Round(time.Millisecond).String()
}

// longestAckGap is the longest time between two consecutive write
// acknowledgements in [from, to].
func (r *Run) longestAckGap(from, to time.Time) time.Duration {
	var ends []time.Time
	for _, op := range r.W.H.snapshotAppends() {
		if op.Outcome == OK && !op.End.Before(from) && !op.End.After(to) {
			ends = append(ends, op.End)
		}
	}
	slices.SortFunc(ends, func(a, b time.Time) int { return a.Compare(b) })
	prev, gap := from, time.Duration(0)
	for _, e := range ends {
		gap = max(gap, e.Sub(prev))
		prev = e
	}
	return max(gap, to.Sub(prev))
}

// epochNow is the newest epoch any running node reports.
func (r *Run) epochNow() uint64 { return r.C.Epoch() }

// ---- small volumes (disk full) ----

// onSmallVolume puts node name's folder on a volume of about mb
// megabytes: WORST_DISKS/<name> (a tmpfs of that size made by
// scripts/worst/run.sh) on Linux, a disk image attached by this test on
// macOS.
func onSmallVolume(name string, mb int) func(c *Cluster) {
	return func(c *Cluster) {
		n := c.Node(name)
		var root string
		switch d := os.Getenv("WORST_DISKS"); {
		case d != "":
			root = filepath.Join(d, name)
			ents, err := os.ReadDir(root)
			if err != nil {
				c.Fail("small volume %s: %v (scripts/worst/run.sh mounts one per node)", root, err)
			}
			for _, e := range ents {
				_ = os.RemoveAll(filepath.Join(root, e.Name()))
			}
		case runtime.GOOS == "darwin":
			img := filepath.Join(c.Dir, "vol-"+name+".dmg")
			root = filepath.Join(c.Dir, "vol-"+name)
			if out, err := exec.Command("hdiutil", "create", "-quiet", "-size", strconv.Itoa(mb)+"m", "-fs", "HFS+", "-volname", "cw"+name, img).CombinedOutput(); err != nil {
				c.Fail("hdiutil create: %v %s", err, out)
			}
			if out, err := exec.Command("hdiutil", "attach", "-quiet", "-nobrowse", "-noverify", "-mountpoint", root, img).CombinedOutput(); err != nil {
				c.Fail("hdiutil attach: %v %s", err, out)
			}
			c.t.Cleanup(func() {
				c.Shutdown() // nothing of the run may hold the volume
				_ = exec.Command("hdiutil", "detach", "-force", root).Run()
			})
		default:
			c.Fail("a small volume is needed: set WORST_DISKS (scripts/worst/run.sh does) or run on macOS")
		}
		n.Dir = filepath.Join(root, "node")
		free, total := volumeSpace(root)
		c.Log.Event("%s's folder is on a small volume %s: %d MB of %d MB free", name, root, free>>20, total>>20)
		c.t.Cleanup(func() {
			// Keep the Postgres logs with the run's artifacts.
			_ = copyDir(filepath.Join(n.Dir, "logs"), filepath.Join(c.Dir, name+"-pglogs"))
		})
	}
}

func volumeSpace(path string) (free, total uint64) {
	var st syscall.Statfs_t
	if syscall.Statfs(path, &st) != nil {
		return 0, 0
	}
	return uint64(st.Bavail) * uint64(st.Bsize), uint64(st.Blocks) * uint64(st.Bsize)
}

func copyDir(from, to string) error {
	ents, err := os.ReadDir(from)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(to, 0o755); err != nil {
		return err
	}
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(from, e.Name()))
		if err == nil {
			_ = os.WriteFile(filepath.Join(to, e.Name()), b, 0o644)
		}
	}
	return nil
}

// fillVolume writes a filler file next to n's folder until the volume is
// full, then gives back leave bytes (another program filled the disk).
func (r *Run) fillVolume(n *Node, leave int64) string {
	f := filepath.Join(filepath.Dir(n.Dir), "filler")
	fh, err := os.OpenFile(f, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		r.C.Fail("filler: %v", err)
	}
	buf := make([]byte, 4<<20)
	var size int64
	for {
		k, err := fh.Write(buf)
		size += int64(k)
		if err != nil {
			break
		}
	}
	_ = fh.Sync()
	if err := fh.Truncate(max(size-leave, 0)); err != nil {
		r.C.Fail("filler truncate: %v", err)
	}
	fh.Close()
	free, _ := volumeSpace(filepath.Dir(n.Dir))
	r.C.Log.Event("filled %s's volume (%d MB filler), %d MB left", n.Name, (size-leave)>>20, free>>20)
	return f
}

// fillDB inserts about 1 MB per statement on p until a statement fails
// (the disk is full) or within passes; it returns when the first failure
// happened (zero if none).
func (r *Run) fillDB(p *Node, within time.Duration, maxMB ...int) time.Time {
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	conn, err := r.C.Connect(ctx, p)
	if err != nil {
		r.C.Fail("fill: %v", err)
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS worst_fill (id bigserial, pad text)`); err != nil {
		r.C.Fail("fill: %v", err)
	}
	for i := 0; ctx.Err() == nil; i++ {
		if len(maxMB) > 0 && i >= maxMB[0] {
			r.C.Log.Event("fill on %s stopped at its cap of %d MB", p.Name, i)
			return time.Time{}
		}
		_, err := conn.Exec(ctx, `INSERT INTO worst_fill (pad) SELECT repeat(md5(random()::text), 32) FROM generate_series(1, 1000)`)
		if err != nil {
			at := time.Now()
			r.C.Log.Event("fill on %s stopped after %d MB: %v", p.Name, i, err)
			return at
		}
	}
	return time.Time{}
}

// plainDiskMessage: n said in plain words that its disk is full.
func plainDiskMessage(out string) bool {
	l := strings.ToLower(out)
	return strings.Contains(l, "disk is full") || strings.Contains(l, "no space left")
}

// ---- disk scenarios ----

// diskFullMain: the main node's disk fills up (another program took the
// space, then the database's own writes take the rest). Expected: the
// main node cannot write, so another node takes over within the resume
// budget; no acknowledged write is lost; the full node says so in plain
// words; once space is freed it comes back as a copy.
func diskFullMain(r *Run) {
	p := r.Primary()
	if p.Name != "a" {
		r.C.Fail("precondition: the main node is %s, not a (the one on the small volume)", p.Name)
	}
	filler := r.fillVolume(p, 48<<20)
	full := r.fillDB(p, 2*time.Minute)
	if full.IsZero() {
		r.C.Fail("precondition: 2 minutes of bulk writes never filled %s's disk", p.Name)
	}
	t0 := r.C.Log.Mark("disk of " + p.Name + " full")
	e0 := r.epochNow()
	r.Let(60 * time.Second)
	var okAfter, failedAfter int
	for _, op := range r.W.H.snapshotAppends() {
		if op.Start.After(t0) {
			switch op.Outcome {
			case OK:
				okAfter++
			case Failed:
				failedAfter++
			}
		}
	}
	who := r.C.PrimaryByCluster()
	msg := fmt.Sprintf("60 s after %s's disk filled: main node %q, term %d -> %d; new appends: %d acknowledged, %d refused with a server error; longest pause between acknowledged appends %s",
		p.Name, who, e0, r.epochNow(), okAfter, failedAfter, r.longestAckGap(t0, time.Now()).Round(time.Millisecond))
	r.C.Log.Event("%s", msg)
	r.t.Logf("%s", msg)
	if who == p.Name && failedAfter > 0 && r.longestAckGap(t0, time.Now()) > ResumeBudget {
		r.Problem("FULL DISK ON THE MAIN NODE: %s", msg)
	}
	if !plainDiskMessage(p.AllOutput()) {
		r.Problem("%s's output never says its disk is full in plain words; last lines:\n%s", p.Name, lastLines(p.AllOutput(), 15))
	}
	_ = os.Remove(filler)
	r.C.Log.Event("freed the filler on %s", p.Name)
	r.Let(5 * time.Second)
}

// diskFullCopy: a copy's disk fills up while it receives the stream.
// Expected: writes go on (k = 1 of two copies), the full copy says so,
// and once space is freed it catches up.
func diskFullCopy(r *Run) {
	p := r.Primary()
	b := r.C.Node("b")
	if p == b {
		r.C.Fail("precondition: b (on the small volume) is the main node")
	}
	filler := r.fillVolume(b, 32<<20)
	t0 := r.C.Log.Mark("fill the main node until " + b.Name + "'s disk is full")
	// 150 MB: more than the 32 MB left on b, little enough that b holds
	// it (WAL + table) once the filler is gone.
	r.fillDB(p, 40*time.Second, 150)
	err := poll(2*time.Minute, func() bool { return strings.Contains(strings.ToLower(b.pgLogs()), "no space left") })
	if err != nil {
		r.C.Fail("precondition: %s's Postgres never reported a full disk", b.Name)
	}
	t1 := r.C.Log.Mark(b.Name + "'s Postgres hit the full disk")
	r.Let(20 * time.Second)
	gap := r.longestAckGap(t1, time.Now())
	r.C.Log.Event("longest pause between acknowledged writes while %s's disk was full: %s", b.Name, gap.Round(time.Millisecond))
	r.t.Logf("longest pause between acknowledged writes while %s's disk was full: %s", b.Name, gap.Round(time.Millisecond))
	if gap > 15*time.Second {
		r.Problem("a copy's full disk stopped writes for %s", gap.Round(time.Millisecond))
	}
	_ = t0
	if !plainDiskMessage(b.AllOutput()) {
		r.Problem("%s's output never says its disk is full in plain words; last lines:\n%s", b.Name, lastLines(b.AllOutput(), 15))
	}
	_ = os.Remove(filler)
	r.C.Log.Event("freed the filler on %s", b.Name)
	t2 := r.C.Log.Mark("space freed on " + b.Name)
	r.waitBack(b, r.Primary(), t2, 4*time.Minute)
}

// pgFrozenMain: every Postgres process of the main node hangs for 60 s
// (what a disk whose every access blocks looks like: the processes exist
// and hold their sockets, nothing they do completes) while its agent and
// watchdog keep running. Expected: the main node cannot
// write, so another node takes over within the resume budget; nothing
// acknowledged is lost.
func pgFrozenMain(r *Run) {
	p := r.Primary()
	r.C.Log.Event("freeze every Postgres process of %s for 60 s", p.Name)
	thaw := r.freezePG(p)
	defer thaw()
	// Marked after every process is stopped: a write already on its way
	// back when the freeze began does not count as "writes are back".
	t0 := r.C.Log.Mark(p.Name + " frozen")
	got := r.expectWritesBack("main node's database frozen", t0, ResumeBudget)
	r.Let(time.Until(t0.Add(60 * time.Second)))
	r.C.Log.Event("main node during the freeze: %q, term %d (writes back after %s)", r.C.PrimaryByCluster(), r.epochNow(), fmtDur(got))
	thaw()
	r.Let(10 * time.Second)
}

// pgFrozenCopy: the same on one copy. Expected: writes go on through the
// other copy.
func pgFrozenCopy(r *Run) {
	p := r.Primary()
	s := r.Standbys(p)[0]
	t0 := r.C.Log.Mark("freeze every Postgres process of copy " + s.Name + " for 60 s")
	thaw := r.freezePG(s)
	defer thaw()
	r.Let(60 * time.Second)
	gap := r.longestAckGap(t0, time.Now())
	r.C.Log.Event("longest pause between acknowledged writes while %s's database was frozen: %s", s.Name, gap.Round(time.Millisecond))
	r.t.Logf("longest pause between acknowledged writes while %s's database was frozen: %s", s.Name, gap.Round(time.Millisecond))
	if gap > 15*time.Second {
		r.Problem("a copy's frozen database stopped writes for %s", gap.Round(time.Millisecond))
	}
	thaw()
	r.Let(10 * time.Second)
}

// ---- corruption ----

// corruptCopyHeapPage: one byte flipped in an old, frozen page of a
// copy's table file (bit rot, a bad sector), while the copy is off. Page
// checksums are on (initdb --data-checksums). The page is never written
// again (insert-only table, vacuumed and frozen first), so replay never
// overwrites it. Then the main node dies and that copy is the only one
// left. Expected: the damage is detected and the copy is copied
// again before it may serve; never promoted with bad data, never spread.
// WORST_NO_FLIP=1 runs the same story without the flip (control).
func corruptCopyHeapPage(r *Run) {
	p, b, t1 := flipCopyPage(r)
	r.waitBack(b, p, t1, 3*time.Minute)
	r.C.Log.Event("%s is back in the sync set", b.Name)
	// D55: the copy reads every page after each start (the page check).
	// Wait until it has, on the data it now runs on: a damaged copy finds
	// the page, leaves the sync set and is copied fresh from the main
	// node first. (Before D55 no status says so; the wait then times out
	// and the story goes on as it did.)
	r.waitPagesChecked(b, p, t1, os.Getenv("WORST_NO_FLIP") == "", 3*time.Minute)
	t2 := r.HardKill(p)
	n := r.WaitNewPrimary(p, t2, time.Minute)
	r.C.Log.Event("new main node: %s", n.Name)
	r.Let(5 * time.Second)
	if s, err := r.queryText(n, "SELECT coalesce(sum(checksum_failures), 0)::text FROM pg_stat_database"); err == nil {
		r.C.Log.Event("checksum failures counted on %s so far: %s", n.Name, s)
	}
	if _, err := r.queryText(n, "SELECT count(*)::text FROM chaos_append"); err != nil {
		r.Problem("CORRUPT MAIN NODE: %s became the main node and cannot read its table: %v", n.Name, err)
	}
}

// corruptCopyMainDiesAtDetection (D55): as corruptCopyHeapPage, but the
// main node dies the moment the copy reports the damage, before it can be
// copied fresh. The only other data node is damaged, so the expected
// outcome is: nothing is promoted (unavailable beats unreadable), status
// says why, and when the main node comes back it serves again with every
// acknowledged write, and the copy is copied fresh from it.
func corruptCopyMainDiesAtDetection(r *Run) {
	p, b, t1 := flipCopyPage(r)
	err := poll(3*time.Minute, func() bool {
		rep, ok := b.Report()
		return ok && rep.Pages != nil && rep.Pages.Damaged
	})
	if err != nil {
		r.C.Fail("%s never reported its damaged page", b.Name)
	}
	r.C.Log.Event("%s reports damage %s after its restart", b.Name, time.Since(t1).Round(10*time.Millisecond))
	t2 := r.C.Log.Mark("kill -9 main node " + p.Name + " (not counted for the resume budget: nothing may take over)")
	p.Kill9()
	// A failover would land about 14 s after the kill; give it 40 s.
	var why string
	end := time.Now().Add(40 * time.Second)
	for time.Now().Before(end) {
		if rep, ok := b.Report(); ok && rep.PG != nil && rep.PG.Role == "primary" {
			r.Problem("%s (damaged) became the main node %s after the kill", b.Name, time.Since(t2).Round(time.Millisecond))
			break
		}
		if s, ok := b.Status(); ok && s.Health.Failover != "" {
			why = s.Health.Failover
		}
		time.Sleep(500 * time.Millisecond)
	}
	r.C.Log.Event("after 40 s: no main node; status says: %s", why)
	if why == "" {
		r.Problem("status never said why there is no main node")
	}
	t3 := r.C.Log.Mark("restart main node " + p.Name)
	p.Restart()
	r.waitResume(t3, 2*time.Minute)
	r.waitPagesChecked(b, p, t1, os.Getenv("WORST_NO_FLIP") == "", 4*time.Minute)
	if got := r.Primary(); got != p {
		r.Problem("%s is the main node, want %s", got.Name, p.Name)
	}
}

// flipCopyPage: rows into chaos_append, frozen and on disk on both nodes;
// power-cut the copy, flip one byte in block 1 of the table's file on it
// (unless WORST_NO_FLIP), start it again. Returns the main node, the copy
// and when the copy was started.
func flipCopyPage(r *Run) (p, b *Node, t1 time.Time) {
	p = r.Primary()
	b = r.Standbys(p)[0]
	r.Let(10 * time.Second) // rows enough for several pages
	r.mustSQL(p, "VACUUM (FREEZE) chaos_append")
	r.mustSQL(p, "CHECKPOINT")
	rel, err := r.queryText(p, "SELECT pg_relation_filepath('chaos_append')")
	if err != nil {
		r.C.Fail("relation path: %v", err)
	}
	pages, err := r.queryText(p, "SELECT (pg_relation_size('chaos_append') / 8192)::text")
	if err != nil || pages == "0" || pages == "1" || pages == "2" {
		r.C.Fail("precondition: chaos_append has %s pages (%v)", pages, err)
	}
	want := r.flushLSN(p)
	if err := poll(time.Minute, func() bool {
		s, err := r.queryText(b, "SELECT pg_last_wal_replay_lsn()::text")
		l, _ := pg.ParseLSN(s)
		return err == nil && l >= want
	}); err != nil {
		r.C.Fail("%s did not replay the checkpoint", b.Name)
	}
	r.mustSQL(b, "CHECKPOINT") // a restartpoint: the frozen pages are on b's disk
	r.C.Log.Mark("power cut " + b.Name)
	r.C.PowerCut(b)
	file := filepath.Join(b.Dir, "pgdata", rel)
	const block, off = 1, 5000
	if os.Getenv("WORST_NO_FLIP") == "" {
		flipByte(r, file, block*8192+off)
		r.C.Log.Event("flipped one byte in block %d of %s on %s (%s pages)", block, rel, b.Name, pages)
	} else {
		r.C.Log.Event("control run: no byte flipped")
	}
	t1 = r.C.Log.Mark("power back " + b.Name)
	b.Restart()
	return p, b, t1
}

// corruptCopyWAL: one byte flipped in a copy's newest change-log (WAL)
// segment, a little before the end it had received, while the copy is
// off. Expected: on restart its recovery stops at the damaged record
// (the record checksum), it streams the rest again from the main node,
// and it holds every acknowledged write; then it serves as the main node.
func corruptCopyWAL(r *Run) {
	p := r.Primary()
	b := r.Standbys(p)[0]
	r.Let(8 * time.Second)
	recv := r.receiveLSN(b)
	tli, err := r.queryText(b, "SELECT received_tli::text FROM pg_stat_wal_receiver")
	if err != nil {
		r.C.Fail("timeline of %s: %v", b.Name, err)
	}
	r.C.Log.Mark("power cut " + b.Name)
	r.C.PowerCut(b)
	at := uint64(recv) - 2048
	seg := at / (16 << 20)
	t, _ := strconv.Atoi(tli)
	name := fmt.Sprintf("%08X%08X%08X", t, seg/0x100, seg%0x100)
	file := filepath.Join(b.Dir, "pgdata", "pg_wal", name)
	if os.Getenv("WORST_NO_FLIP") == "" {
		flipByte(r, file, int64(at%(16<<20)))
		r.C.Log.Event("flipped one byte at %X/%X in %s on %s (received up to %s)", at>>32, uint32(at), name, b.Name, recv)
	}
	t1 := r.C.Log.Mark("power back " + b.Name)
	b.Restart()
	r.waitBack(b, p, t1, 3*time.Minute)
	logs := b.pgLogs()
	for _, l := range strings.Split(logs, "\n") {
		if strings.Contains(l, "checksum in record") || strings.Contains(l, "invalid record") || strings.Contains(l, "incorrect") {
			r.C.Log.Event("%s's Postgres: %s", b.Name, strings.TrimSpace(l))
		}
	}
	t2 := r.HardKill(p)
	r.WaitNewPrimary(p, t2, time.Minute)
	r.Let(8 * time.Second)
}

// waitPagesChecked waits until n's page check (D55) has read every page
// of the data it runs on and found no damage, and n is a full copy in the
// sync set again. With flipped, n must have reported the damage first.
func (r *Run) waitPagesChecked(n, p *Node, t0 time.Time, flipped bool, within time.Duration) {
	var seen string
	err := poll(within, func() bool {
		rep, ok := n.Report()
		if !ok || rep.Pages == nil {
			return false
		}
		if rep.Pages.Damaged && seen == "" {
			seen = strings.Join(rep.Pages.Problems, "; ")
			r.C.Log.Event("%s's page check: damaged: %s (%s)", n.Name, seen, rep.Pages.Action)
		}
		return rep.Pages.CheckedSinceStart && !rep.Pages.Damaged
	})
	if err != nil {
		r.C.Log.Event("%s's page check did not finish clean within %s", n.Name, within)
		return
	}
	if seen == "" && strings.Contains(n.Output(), "damaged pages") {
		seen = "(reported in its output)"
	}
	switch {
	case flipped && seen == "":
		r.Problem("%s's page check never reported the flipped byte", n.Name)
	case !flipped && seen != "":
		r.Problem("%s's page check reported damage with no byte flipped: %s", n.Name, seen)
	}
	r.waitBack(n, p, t0, within)
	r.C.Log.Event("%s read every page of its data, no damage (after %s)", n.Name, time.Since(t0).Round(100*time.Millisecond))
}

func flipByte(r *Run, file string, off int64) {
	f, err := os.OpenFile(file, os.O_RDWR, 0)
	if err != nil {
		r.C.Fail("open %s: %v", file, err)
	}
	defer f.Close()
	b := make([]byte, 1)
	if _, err := f.ReadAt(b, off); err != nil {
		r.C.Fail("read %s@%d: %v", file, off, err)
	}
	b[0] ^= 0x5a
	if _, err := f.WriteAt(b, off); err != nil {
		r.C.Fail("write %s@%d: %v", file, off, err)
	}
	if err := f.Sync(); err != nil {
		r.C.Fail("sync %s: %v", file, err)
	}
}

// ---- mid-operation ----

// joinKill: a fourth node joins under load and loses power the moment it
// prints line (one step of the join), then starts again with the same
// command. Expected: it finishes the join, catches up, and the cluster
// is whole; the join never harms the others.
func joinKill(name, line string) Scenario {
	return Scenario{Name: name, Topo: Topology{Data: []string{"a", "b", "c"}, Late: []string{"d"}}, Body: func(r *Run) {
		seen := func() bool { return strings.Contains(d0(r).Output(), line) }
		if line == "  50%" {
			// A clone long enough to be cut in the middle; progress is
			// printed every few seconds, so any step between 0 and 100 %.
			r.bulk(400)
			mid := regexp.MustCompile(`(?m)^\s+([1-9]|[1-9][0-9])%`)
			seen = func() bool { return mid.MatchString(d0(r).Output()) }
		}
		d := r.C.Node("d")
		cmd, code := r.C.joinArgs(d)
		d.Launch(cmd, code)
		if err := poll(5*time.Minute, func() bool { return seen() || !d.Running() }); err != nil || !d.Running() {
			r.C.Fail("the join never printed %q; its output:\n%s", line, lastLines(d.Output(), 30))
		}
		r.C.Log.Mark(fmt.Sprintf("power cut %s at %q", d.Name, line))
		r.C.PowerCut(d)
		if p := WaitProc(3*time.Second, d.Dir, "pg_basebackup"); p != 0 {
			r.C.Fail("pg_basebackup (pid %d) outlived the power cut", p)
		}
		r.Let(2 * time.Second)
		t1 := r.C.Log.Mark("same join command again on " + d.Name)
		d.launchReady(cmd, code, "Press Ctrl-C to stop.", 5*time.Minute)
		r.C.Log.Event("%s finished the join after the cut: %s", d.Name, time.Since(t1).Round(time.Millisecond))
		r.waitBack(d, r.Primary(), t1, 4*time.Minute)
		r.expectSameData(d, r.Primary())
	}}
}

func d0(r *Run) *Node { return r.C.Node("d") }

// switchKill: a planned switch under load; the agent running it (the Raft
// leader) is killed at point, and comes back without the fault.
// Expected: the cluster ends with one main node, whole; nothing lost.
func switchKill(name, point string) Scenario {
	return Scenario{Name: name, Topo: Three, Env: []string{faultEnv + point}, Body: func(r *Run) {
		p := r.Primary()
		to := r.Standbys(p)[r.Rng.IntN(2)]
		done := make(chan string, 1)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			out, err := p.CLI(ctx, "switch", "--to", "node-"+to.Name)
			done <- fmt.Sprintf("%v %s", err, strings.TrimSpace(out))
		}()
		// No early "done": after switch-after-cas the term has already
		// moved when the agent pauses, which would end the wait unhit.
		if r.killAtFault(point, func() bool { return false }) == 0 {
			r.C.Fail("no agent reached %s", point)
		}
		r.C.Log.Event("switch command returned: %s", <-done)
		r.C.WaitHealthy(3 * time.Minute)
		r.C.Log.Event("after the interrupted switch the main node is %s (was %s, target %s)", r.C.PrimaryByCluster(), p.Name, to.Name)
	}}
}

// midPromotion: the node chosen to take over (after the main node is
// killed, or by a planned switch) loses power right after its Postgres
// switched to a new timeline, before its agent made the copies' slots and
// recorded that it serves (D54's window). Its agent is frozen the moment
// the Postgres log shows the new timeline, so the power cut lands inside
// the window; pg_replslot is checked to prove it. Expected: no write
// lost, never two main nodes; once it has power again the cluster is
// whole (a failover to the third node in between is allowed only if rule
// 2 permits it).
func midPromotion(r *Run, planned bool) { promotionCutAt(r, planned, "") }

// signalGoneCut: after "selected new timeline ID", wait until Postgres
// has removed standby.signal (it does that after copying the last WAL
// segment and before it writes the history file and the end-of-recovery
// record, each with an fsync), then freeze the startup process at once
// and cut the power: the P1 window, every run.
const signalGoneCut = "selected new timeline ID (then standby.signal gone)"

var startupPIDRe = regexp.MustCompile(`\[(\d+)\] LOG:  selected new timeline ID`)

func promotionCutAt(r *Run, planned bool, line string) {
	p := r.Primary()
	cands := r.Standbys(p)
	tails := map[*Node]*logTail{}
	for _, n := range cands {
		tails[n] = newLogTail(n)
	}
	hit := make(chan *Node, 1)
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			for _, n := range cands {
				if txt := tails[n].next(); strings.Contains(txt, "selected new timeline ID") {
					if m := startupPIDRe.FindStringSubmatch(txt); m != nil && line == signalGoneCut {
						sig := filepath.Join(n.Dir, "pgdata", "standby.signal")
						for end := time.Now().Add(time.Minute); time.Now().Before(end); {
							if _, err := os.Stat(sig); err != nil {
								break
							}
							time.Sleep(100 * time.Microsecond)
						}
						if pid, err := strconv.Atoi(m[1]); err == nil {
							_ = syscall.Kill(pid, syscall.SIGSTOP)
						}
					}
					n.signal(syscall.SIGSTOP)
					hit <- n
					return
				}
			}
			time.Sleep(2 * time.Millisecond) // polling interval of the log watch
		}
	}()
	var t0 time.Time
	if planned {
		to := cands[r.Rng.IntN(2)]
		t0 = r.C.Log.Mark("planned switch to " + to.Name)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			out, err := p.CLI(ctx, "switch", "--to", "node-"+to.Name)
			r.C.Log.Event("switch returned: %v %s", err, strings.TrimSpace(out))
		}()
	} else {
		t0 = r.HardKill(p)
	}
	var n *Node
	select {
	case n = <-hit:
	case <-time.After(2 * time.Minute):
		close(stop)
		r.C.Fail("no copy switched timeline within 2 minutes")
	}
	close(stop)
	n.mu.Lock()
	n.stopped = true
	n.mu.Unlock()
	slots, _ := filepath.Glob(filepath.Join(n.Dir, "pgdata", "pg_replslot", "coyote_*"))
	r.C.Log.Event("%s switched timeline %s after the fault; agent frozen; slots on disk: %d %v", n.Name, time.Since(t0).Round(time.Millisecond), len(slots), slots)
	r.t.Logf("%s frozen right after its timeline switch; slots on disk: %d", n.Name, len(slots))
	r.C.Log.Mark("power cut " + n.Name + " mid-promotion")
	r.C.PowerCut(n)
	if !planned {
		// Two of three data nodes are now down for 25 s: no majority, so
		// no failover is possible and the 30 s resume budget of the hard
		// kill does not apply. Writes come back after power returns
		// (logged at the end).
		r.hardKills = slices.DeleteFunc(r.hardKills, func(k time.Time) bool { return k.Equal(t0) })
	}
	r.Let(25 * time.Second)
	r.C.Log.Event("25 s after the cut: main node %q, term %d", r.C.PrimaryByCluster(), r.epochNow())
	t1 := r.C.Log.Mark("power back " + n.Name)
	n.Launch("up", "")
	if !planned {
		p.Launch("up", "")
	}
	// The node recorded as the new main serves on timeline 1 again if the
	// cut landed before Postgres wrote the new timeline's history file:
	// crash recovery then goes on along timeline 1, the old main node's
	// timeline, and the old main node's own unacknowledged WAL past the
	// fork point conflicts with it under the same timeline ID, which
	// pg_rewind does not see ("no rewind required").
	var tl string
	_ = poll(time.Minute, func() bool {
		if rep, ok := n.Report(); !ok || rep.PG == nil || rep.PG.Role != "primary" {
			return false
		}
		// The timeline WAL is written on now; pg_control_checkpoint() still
		// shows the old one until the checkpoint after promotion is done.
		s, err := r.queryText(n, "SELECT (('x' || substr(pg_walfile_name(pg_current_wal_lsn()), 1, 8))::bit(32)::int)::text")
		tl = s
		return err == nil
	})
	hist, _ := filepath.Glob(filepath.Join(n.Dir, "pgdata", "pg_wal", "*.history"))
	r.C.Log.Event("%s after power back: serving on timeline %q; history files %v", n.Name, tl, hist)
	if tl == "1" {
		r.Problem("SAME TIMELINE AFTER FAILOVER: %s was recorded as the new main node, its promotion was cut (history files on disk: %v), and it serves again on timeline 1, the old main node's timeline", n.Name, hist)
	}
	r.C.WaitHealthy(4 * time.Minute)
	r.C.Log.Event("whole again %s after %s had power again; main node %s", time.Since(t1).Round(time.Millisecond), n.Name, r.C.PrimaryByCluster())
	r.t.Logf("whole again %s after %s had power again", time.Since(t1).Round(time.Millisecond), n.Name)
}

// ---- resources ----

// pgChild finds a child of n's postmaster whose title contains s.
func (n *Node) pgChild(s string) int {
	_, kids := n.pgTree()
	for _, k := range kids {
		if strings.Contains(k.cmd, s) {
			return k.pid
		}
	}
	return 0
}

// oomKillMain: the out-of-memory killer takes one Postgres process of the
// main node: a client backend, then the checkpointer, then the WAL
// writer. Each makes Postgres end every backend and recover in place.
// Expected: writes back within the budget each time (same main node or a
// failover), nothing lost.
func oomKillMain(r *Run) {
	for _, what := range []string{"127.0.0.1(", "checkpointer", "walwriter"} {
		p := r.Primary()
		pid := 0
		_ = poll(10*time.Second, func() bool { pid = p.pgChild(what); return pid != 0 })
		if pid == 0 {
			r.C.Fail("no %q process under %s's postmaster", what, p.Name)
		}
		t0 := r.C.Log.Mark(fmt.Sprintf("OOM kill: kill -9 %q (pid %d) on %s", what, pid, p.Name))
		_ = syscall.Kill(pid, syscall.SIGKILL)
		r.expectWritesBack("OOM kill of "+what, t0, ResumeBudget)
		r.Let(10 * time.Second)
		r.C.Log.Event("main node after the OOM kill of %q: %q, term %d", what, r.C.PrimaryByCluster(), r.epochNow())
	}
}

// oomKillCopy: the OOM killer takes a copy's startup (replay) process;
// Postgres on a copy cannot go on without it and stops. Expected: writes
// go on, the agent starts it again, the copy catches up.
func oomKillCopy(r *Run) {
	p := r.Primary()
	s := r.Standbys(p)[0]
	pid := s.pgChild("startup")
	if pid == 0 {
		r.C.Fail("no startup process on %s", s.Name)
	}
	t0 := r.C.Log.Mark(fmt.Sprintf("OOM kill: kill -9 startup (pid %d) on copy %s", pid, s.Name))
	_ = syscall.Kill(pid, syscall.SIGKILL)
	r.Let(15 * time.Second)
	gap := r.longestAckGap(t0, time.Now())
	r.C.Log.Event("longest pause between acknowledged writes: %s", gap.Round(time.Millisecond))
	if gap > 15*time.Second {
		r.Problem("a copy's OOM kill stopped writes for %s", gap.Round(time.Millisecond))
	}
	r.waitBack(s, p, t0, 3*time.Minute)
}

// holdPort listens on 127.0.0.1:port (another program took it).
func (r *Run) holdPort(port int) net.Listener {
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		r.C.Fail("take port %d: %v", port, err)
	}
	return ln
}

// portTakenOnRestart: the old main node restarts after a failover and
// finds one of its ports taken by another program: its database port,
// then its client port, then its cluster port. Expected each time: it
// stops at once with a message naming the port, without starting
// Postgres or touching the cluster; once the port is free it rejoins.
func portTakenOnRestart(r *Run) {
	p := r.Primary()
	t0 := r.HardKill(p)
	r.WaitNewPrimary(p, t0, time.Minute)
	for _, x := range []struct {
		what string
		port int
	}{{"database port", p.PG}, {"client port", p.Client}, {"cluster port", p.Listen}} {
		ln := r.holdPort(x.port)
		r.C.Log.Mark(fmt.Sprintf("%s's %s %d taken; restart it", p.Name, x.what, x.port))
		p.Launch("up", "")
		err := poll(time.Minute, func() bool { return !p.Running() })
		out := p.Output()
		if err != nil {
			r.Problem("%s kept running with its %s %d taken:\n%s", p.Name, x.what, x.port, lastLines(out, 15))
			p.Kill9()
		} else if !strings.Contains(out, strconv.Itoa(x.port)) || !testportsInUse(out) {
			r.Problem("%s stopped with its %s %d taken, but its message does not name the port as taken:\n%s", p.Name, x.what, x.port, lastLines(out, 15))
		} else {
			r.C.Log.Event("%s stopped with a plain message: %s", p.Name, strings.TrimSpace(lastLines(out, 3)))
			r.t.Logf("%s taken: %s", x.what, strings.TrimSpace(lastLines(out, 2)))
		}
		if pm := p.PostmasterPID(); pm != 0 {
			r.Problem("a postmaster (pid %d) runs in %s's folder although its %s was taken", pm, p.Name, x.what)
		}
		ln.Close()
	}
	t1 := r.C.Log.Mark("ports free again; restart " + p.Name)
	p.Restart()
	r.waitBack(p, r.Primary(), t1, 3*time.Minute)
}

func testportsInUse(out string) bool {
	return strings.Contains(out, "already in use") || strings.Contains(out, "in use by another program")
}

// ---- network ----

// latencySpikes: 30% of every link's chunks held up by up to 800 ms for
// 60 s: what heavy packet loss looks like above TCP (bytes intact and in
// order, but late; retransmission timeouts). tc netem needs root; on Linux
// the netem scenarios (worst_linux_test.go) drop real packets.
func latencySpikes(r *Run) {
	t0 := r.C.Log.Mark("latency spikes: 30% of chunks up to 800 ms, 60 s")
	r.C.Net.Spikes(30, 800*time.Millisecond)
	r.Let(60 * time.Second)
	r.C.Net.Heal()
	r.C.Log.Event("longest pause between acknowledged writes under the spikes: %s", r.longestAckGap(t0, time.Now()).Round(time.Millisecond))
	r.t.Logf("longest pause between acknowledged writes under the spikes: %s", r.longestAckGap(t0, time.Now()).Round(time.Millisecond))
}

// asymmetricFlap: for d, every 2-5 s the network changes to a new random
// set of one-way cuts (A hears B, B does not hear A), usually touching
// the main node. Expected: never two main nodes, nothing lost, whole
// again after.
func asymmetricFlap(r *Run, d time.Duration) {
	names := r.C.Names()
	end := time.Now().Add(d)
	t0 := r.C.Log.Mark("asymmetric flapping for " + d.String())
	e0 := r.epochNow()
	changes := 0
	for time.Now().Before(end) {
		r.C.Net.Heal()
		main := r.C.PrimaryByCluster()
		for range 1 + r.Rng.IntN(3) {
			from := names[r.Rng.IntN(len(names))]
			if main != "" && r.Rng.IntN(3) > 0 {
				from = main
			}
			to := names[r.Rng.IntN(len(names))]
			if to == from {
				continue
			}
			if r.Rng.IntN(2) == 0 {
				from, to = to, from
			}
			m := Blackhole
			if r.Rng.IntN(3) == 0 {
				m = Drop
			}
			r.C.Net.SetFlow(from, to, m)
		}
		changes++
		r.C.Log.Event("flap %d: %s", changes, r.C.Net.Describe())
		time.Sleep(time.Duration(2000+r.Rng.IntN(3000)) * time.Millisecond) // the flap period itself
	}
	r.C.Net.Heal()
	r.C.Log.Event("flapping over: %d network changes, term %d -> %d, longest pause between acknowledged writes %s",
		changes, e0, r.epochNow(), r.longestAckGap(t0, time.Now()).Round(time.Millisecond))
	r.t.Logf("%d network changes, term %d -> %d", changes, e0, r.epochNow())
}

// forward accepts on a new port and pipes to target, directly (a node's
// new address, outside the per-pair proxies).
func forward(target string) (net.Listener, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				s, err := net.DialTimeout("tcp", target, 5*time.Second)
				if err != nil {
					return
				}
				defer s.Close()
				go func() { _, _ = io.Copy(s, c); s.Close() }()
				_, _ = io.Copy(c, s)
			}()
		}
	}()
	return ln, nil
}

// mainAddressChange: the main node's machine gets a new address (a
// Tailscale or DHCP change). For 2 s it has no network at all, then it
// can dial out from the new address but nobody reaches the old one; a
// person runs `coyote node set-address` with the new address on another
// node. Expected: the copies find the main node at its new address and
// writes resume, or a failover happens and the old main node rejoins;
// nothing lost.
func mainAddressChange(r *Run) {
	p := r.Primary()
	ln, err := forward("127.0.0.1:" + strconv.Itoa(p.Listen))
	if err != nil {
		r.C.Fail("forwarder: %v", err)
	}
	defer ln.Close()
	newAddr := ln.Addr().String()
	t0 := r.C.Log.Mark(p.Name + " changes address to " + newAddr)
	r.C.Net.Isolate(p.Name, r.C.Names(), Drop)
	r.Let(2 * time.Second)
	for _, o := range r.C.Names() {
		if o != p.Name {
			// Connections p opens (from its new address) work both ways;
			// connections to its old address stay dropped.
			if l := r.C.Net.link(p.Name, o); l != nil {
				l.up.Store(int32(Pass))
				l.down.Store(int32(Pass))
			}
		}
	}
	other := r.Standbys(p)[0]
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := other.CLI(ctx, "node", "set-address", "node-"+p.Name, newAddr)
	r.C.Log.Event("set-address on %s: %v %s", other.Name, err, strings.TrimSpace(out))
	r.t.Logf("set-address: %v %s", err, strings.TrimSpace(out))
	if err != nil {
		r.Problem("set-address to the main node's new address failed: %v\n%s", err, out)
	}
	t1 := r.C.Log.Mark("address recorded")
	r.expectWritesBack("main node's address change", t0, ResumeBudget)
	r.Let(10 * time.Second)
	r.C.Log.Event("after the address change: main node %q, term %d; %s after set-address", r.C.PrimaryByCluster(), r.epochNow(), time.Since(t1).Round(time.Millisecond))
	if _, ok := r.C.Healthy(); !ok {
		r.C.Log.Event("not whole yet before the old address comes back")
	}
	err = poll(2*time.Minute, func() bool { _, ok := r.C.Healthy(); return ok })
	if err != nil {
		r.Problem("the cluster was not whole within 2 minutes of set-address with the old address still dead")
	}
}

var _ = errors.New
