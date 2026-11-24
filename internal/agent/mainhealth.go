package agent

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"coyotedb.com/coyote/internal/brand"
	"coyotedb.com/coyote/internal/cluster"
	"coyotedb.com/coyote/internal/mono"
	"coyotedb.com/coyote/internal/pg"
)

// Main-node health (D58, worst-case findings D1 and D2).
//
// The lease (rule 4) proves that the main node's *agent* is alive and in
// touch with the cluster; it says nothing about whether its Postgres can
// still save anything. A disk that stalls, a Postgres that hangs, or a
// full disk left a main node that kept its lease while acknowledging
// nothing, for as long as the fault lasted.
//
// So the main node's agent also checks its own database:
//
//   - every second it writes one tiny WAL record and flushes it to disk
//     (pg.HealthProbeSQL: no table, no lock, does not wait for the
//     copies), with its own connection;
//   - every second it reads the free space of the disk that holds the
//     data folder.
//
// When the probe has not succeeded for stallAfter while every try either
// hung or hit a full disk ("stalled"), or the disk has had less than
// diskCritical free for diskLowFor ("disk full", counted also while its
// Postgres is down), the main node first passes on the Raft leadership if
// it has it (the switch runs on the leader and must write the Raft log),
// then hands its role over with a planned switch (runSwitch, D41) to the
// copy in the sync
// set that is running, not cut and has received the most, and, for a full
// disk, has room. The switch keeps every rule it already keeps: rule 2
// (every copy cut before the new main node is recorded), rule 3 (only a
// member of R), the old main node confirms its Postgres exited (or its
// lease is waited out) before the compare-and-set, and the target catches
// up to every other copy first. A hand-over is therefore never less safe
// than a planned switch, which is never less safe than the old main node
// dying at that moment: a failover would do the same.
//
// What does NOT count (no false positives, no flapping):
//
//   - a slow but answering probe: it is one tiny record, so only a WAL
//     flush that takes stallAfter (12 s) counts; heavy writes, long
//     checkpoints and big batches delay it by milliseconds to a second;
//   - errors that say nothing about this machine's disk (Postgres down or
//     restarting, a refused setting, xid wraparound): they are the same on
//     every node, and Postgres down is the restart loop's job;
//   - copies being slow or absent: the probe does not wait for them;
//   - a main node that is too new (mainSettle), a cluster that cannot
//     fail over by itself (D17, rule 9), or one where no copy qualifies:
//     then it stays and says so in plain words;
//   - after a switch that failed, the next try waits retryAfter; after
//     "staying" (nothing was tried) it looks again after stayRetry.
//
// Early refusal of writes on a nearly full disk was considered and
// rejected (D58): Postgres already refuses a write it cannot save (it is
// never acknowledged, nothing is corrupted), and refusing earlier turns a
// possible outage into a certain one. Instead the node warns early
// (diskWarn) and hands over to a copy with room before the disk is full.
const (
	healthEvery        = time.Second
	stallAfter         = 12 * time.Second
	healthProbeTimeout = 30 * time.Second
	diskCritical       = 64 << 20  // below this, the disk counts as full
	diskTargetMin      = 256 << 20 // a copy needs this much room to take over for a full disk
	diskWarn           = 1 << 30   // below this, say so (once)
	diskLowFor         = 3 * time.Second
	mainSettle         = 10 * time.Second
	retryAfter         = 15 * time.Second
	stayRetry          = 3 * time.Second // after "staying" (blocked, or no copy qualifies)

	handOverLimit = 90 * time.Second
)

// Reasons a main node hands over.
const (
	reasonStall = "stalled"
	reasonDisk  = "disk full"
)

// mainHealth is the health check's state (healthLoop's goroutines).
type mainHealth struct {
	mu sync.Mutex
	// The probe: one at a time.
	conn       *pgx.Conn     // owned by the probe in flight
	inflight   bool          // a probe is running
	inflightAt time.Duration // mono: when it started
	badSince   time.Duration // mono: start of the first probe in the current stalled / disk-full run; 0: none
	badDisk    bool          // that run includes a disk-full answer
	// The disk.
	free, total uint64
	diskOK      bool          // free/total are known
	lowSince    time.Duration // mono: free < diskCritical since; 0: not low
	warned      bool          // diskWarn said for this low spell
	// Hand-over.
	epoch     uint64        // the term the state above is for
	handing   bool          // a hand-over is running
	nextTry   time.Duration // mono: no hand-over before this
	saidStay  string        // the "staying" message last said (said once)
	mainSince time.Duration // mono: when this process first saw itself named main node at epoch
}

// probeDone records a finished probe that started at start (mono).
func (h *mainHealth) probeDone(start time.Duration, out pg.ProbeOutcome) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.inflight = false
	switch out {
	case pg.ProbeOK, pg.ProbeOther:
		h.badSince, h.badDisk = 0, false
	case pg.ProbeStalled, pg.ProbeDiskFull:
		if h.badSince == 0 {
			h.badSince = start
		}
		h.badDisk = h.badDisk || out == pg.ProbeDiskFull
	}
}

// stalledFor is how long the probe has been failing in a way that counts
// (stalled or disk full), including a probe still in flight.
func (h *mainHealth) stalledFor(now time.Duration) time.Duration {
	since := h.badSince
	if since == 0 && h.inflight {
		since = h.inflightAt
	}
	if since == 0 {
		return 0
	}
	return now - since
}

// noteDisk records a free-space reading.
func (h *mainHealth) noteDisk(now time.Duration, free, total uint64, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err != nil {
		h.diskOK = false
		return
	}
	h.free, h.total, h.diskOK = free, total, true
	switch {
	case free >= diskCritical:
		h.lowSince = 0
	case h.lowSince == 0:
		h.lowSince = now
	}
	if free >= diskWarn {
		h.warned = false
	}
}

// diskFree is the last free-space reading, if any.
func (h *mainHealth) diskFree() (uint64, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.free, h.diskOK
}

// reason says why this main node should hand over now ("" if it should
// not), from the probe and the disk at now.
func (h *mainHealth) reason(now time.Duration) string {
	stalled := h.stalledFor(now)
	switch {
	case h.lowSince != 0 && now-h.lowSince >= diskLowFor:
		return reasonDisk
	case stalled >= stallAfter && h.badDisk:
		return reasonDisk
	case stalled >= stallAfter:
		return reasonStall
	}
	return ""
}

// reset forgets the probe's history (a new term, or no longer main).
func (h *mainHealth) reset(epoch uint64) {
	h.badSince, h.badDisk, h.epoch, h.saidStay, h.mainSince = 0, false, epoch, "", 0
}

// healthLoop runs on data nodes for the agent's life.
func (a *Agent) healthLoop(ctx context.Context) {
	t := time.NewTicker(healthEvery)
	defer t.Stop()
	defer func() {
		h := &a.health
		h.mu.Lock()
		if !h.inflight && h.conn != nil {
			_ = h.conn.Close(context.Background())
			h.conn = nil
		}
		h.mu.Unlock()
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		a.healthStep(ctx)
	}
}

func (a *Agent) healthStep(ctx context.Context) {
	h := &a.health
	now := mono.Now()
	free, total, err := pg.DiskSpace(a.node.PGData())
	h.noteDisk(now, free, total, err)
	if err == nil && free < diskWarn {
		h.mu.Lock()
		warn := !h.warned
		h.warned = true
		h.mu.Unlock()
		if warn {
			a.log.Warn("disk almost full", "dir", a.node.StateDir, "free_mb", free>>20)
			a.say("The disk that holds %s is almost full (%s left). Free some space soon: when it is full, this node cannot save writes.", a.node.StateDir, sizeText(free))
		}
	}

	st := a.cn.State()
	srv, role := a.currentPG()
	// The main node: the one the cluster names. Its Postgres may be down
	// (crashed on the full disk, being restarted) and it may even have
	// dropped its lease with it: then there is nothing to probe, but a
	// full disk still counts. Asking for the switch needs no lease (any
	// node may ask for one); the switch itself checks everything.
	main := st.PrimaryID == a.cn.ID()
	pgUp := srv != nil && role == "primary" && a.holdsLease(st.Epoch)
	h.mu.Lock()
	if !main || h.epoch != st.Epoch {
		h.reset(st.Epoch)
	}
	if !main {
		if !h.inflight && h.conn != nil {
			_ = h.conn.Close(context.Background())
			h.conn = nil
		}
		h.mu.Unlock()
		return
	}
	if h.mainSince == 0 {
		h.mainSince = now
	}
	switch {
	case pgUp && !h.inflight:
		h.inflight, h.inflightAt = true, now
		go a.probe(ctx, now)
	case !pgUp && !h.inflight:
		h.badSince, h.badDisk = 0, false // the restart loop's job, not a stall
	}
	why := h.reason(now)
	if why == "" {
		h.saidStay = "" // healthy again: a new problem is said again
	}
	stalled := h.stalledFor(now)
	mainFor := now - h.mainSince
	go1 := why != "" && !h.handing && now >= h.nextTry
	if go1 {
		h.handing = true
	}
	h.mu.Unlock()
	if go1 {
		go a.handOver(ctx, st.Epoch, why, stalled, free, mainFor)
	}
}

// probe runs one health probe that started at start (mono).
func (a *Agent) probe(ctx context.Context, start time.Duration) {
	h := &a.health
	pctx, cancel := context.WithTimeout(ctx, healthProbeTimeout)
	defer cancel()
	h.mu.Lock()
	c := h.conn
	h.conn = nil
	h.mu.Unlock()
	var err error
	if c == nil {
		c, err = a.node.Connect(pctx)
	}
	if err == nil {
		err = pg.ProbeWrite(pctx, c)
	}
	out := pg.ClassifyProbe(pctx, err)
	if ctx.Err() != nil {
		out = pg.ProbeOther // the agent is stopping
	}
	if err != nil && c != nil {
		_ = c.Close(context.Background())
		c = nil
	}
	if err != nil && out != pg.ProbeOther {
		a.log.Info("main-node health probe failed", "err", err, "after", (mono.Now() - start).Round(time.Millisecond))
	}
	h.mu.Lock()
	h.conn = c
	h.mu.Unlock()
	h.probeDone(start, out)
}

// handOver moves the main-node role away from this node for why, if the
// cluster allows it and a copy qualifies; otherwise it says why it stays.
func (a *Agent) handOver(ctx context.Context, epoch uint64, why string, stalled time.Duration, free uint64, mainFor time.Duration) {
	h := &a.health
	retry := retryAfter
	defer func() {
		h.mu.Lock()
		h.handing = false
		h.nextTry = mono.Now() + retry
		h.mu.Unlock()
	}()
	what := whatHappened(why, stalled, free)
	st := a.cn.State()
	if st.Epoch != epoch || st.PrimaryID != a.cn.ID() {
		return
	}
	stay := func(msg string) {
		retry = stayRetry // only status reads were spent: look again soon
		h.mu.Lock()
		said := h.saidStay == msg
		h.saidStay = msg
		h.mu.Unlock()
		if !said {
			a.log.Warn("main node unhealthy; staying", "why", why, "reason", msg)
			a.say("%s %s", what, msg)
		}
	}
	if msg := handOverBlocked(st, a.cn.ID(), mainFor); msg != "" {
		stay(msg)
		return
	}
	reps := a.copyReports(ctx, st)
	target, msg := pickHandOver(st, a.cn.ID(), why, free, reps)
	if target == "" {
		stay(msg)
		return
	}
	name := st.Members[target].Name
	a.log.Warn("main node unhealthy: handing over", "why", why, "to", name, "stalled_for", stalled.Round(time.Millisecond), "free", free)
	a.say("%s Handing the main-node role to %s.", what, name)
	sctx, cancel := context.WithTimeout(ctx, handOverLimit)
	defer cancel()
	// The switch runs on the Raft leader. If that is this node, hand the
	// leadership on first: a full or stalled disk here cannot write the
	// Raft log the switch needs (seen in D1: the switch's own read failed
	// with "no space left on device").
	if a.cn.IsLeader() {
		tctx, tcancel := context.WithTimeout(sctx, 10*time.Second)
		if err := a.cn.TransferLeadership(tctx); err != nil {
			a.log.Warn("hand-over: could not pass on the cluster leadership first (going ahead)", "err", err)
		}
		tcancel()
	}
	// Conditional on this node still being the main node at epoch, on the
	// leader's linearizable read: this node's own view can be stale (a
	// full disk stops its Raft log, so it never learns of a new term).
	res, err := a.runSwitchFrom(sctx, name, true, a.cn.ID(), epoch)
	if err != nil && strings.Contains(err.Error(), errMainMoved.Error()) {
		a.log.Info("hand-over not needed: the main node already moved", "epoch", epoch)
		return
	}
	if err != nil {
		a.log.Warn("hand-over failed", "to", name, "err", err)
		a.say("Could not hand the main-node role to %s: %v. Trying again shortly.", name, err)
		return
	}
	retry = 0
	a.say("%s", res.Message)
}

// whatHappened is the first sentence of the plain message.
func whatHappened(why string, stalled time.Duration, free uint64) string {
	if why == reasonDisk {
		return fmt.Sprintf("This node's disk is full (%s left), so its database cannot save writes.", sizeText(free))
	}
	return fmt.Sprintf("This node's database has not been able to save anything for %s (its disk may have stalled, or PostgreSQL hangs).",
		stalled.Round(time.Second))
}

// handOverBlocked says why the cluster does not allow this main node to
// hand over by itself now ("" if it does). It mirrors the controller's
// failover rules (decideFailover): a hand-over is a failover the old main
// node asks for.
func handOverBlocked(st cluster.State, self string, mainFor time.Duration) string {
	data, witnesses := 0, 0
	for _, m := range st.Members {
		if m.Role == cluster.RoleData {
			data++
		} else {
			witnesses++
		}
	}
	switch {
	case data < 2:
		return "There is no other data node to take over; it keeps trying."
	case data == 2 && witnesses == 0:
		return "This cluster has two data nodes and no witness, so it does not move the main-node role by itself; fix this computer, or move the role with `" + brand.Command + " switch`."
	case !st.StandbyEligible:
		return "The cluster is running on one copy (this node holds writes the others may lack), so no other node may take over; it keeps trying."
	case st.LastServed != self:
		return "It became the main node only recently and no copy has confirmed holding all its data yet; it keeps trying."
	case mainFor < mainSettle:
		return "It became the main node only seconds ago; it waits a little before handing over."
	}
	return ""
}

// copyReport is one copy's status, as the hand-over sees it.
type copyReport struct {
	ID  string
	Rep *NodeReport // nil: not reachable
}

// copyReports asks every other active member of R for its status.
func (a *Agent) copyReports(ctx context.Context, st cluster.State) []copyReport {
	var ids []string
	for _, id := range st.Sync.Members {
		if m, ok := st.Members[id]; ok && id != a.cn.ID() && m.Role == cluster.RoleData && m.Status == cluster.StatusActive {
			ids = append(ids, id)
		}
	}
	out := make([]copyReport, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			var r NodeReport
			out[i].ID = id
			if err := a.call(cctx, id, "status", nil, &r); err == nil {
				out[i].Rep = &r
			}
		}()
	}
	wg.Wait()
	return out
}

// pickHandOver chooses the copy to hand over to: a member of R that runs
// as a healthy, uncut copy, has the room a full disk needs, and has
// received the most (ties: name). It returns "" and the reason when none
// qualifies.
func pickHandOver(st cluster.State, self, why string, free uint64, reps []copyReport) (string, string) {
	type cand struct {
		id, name string
		lsn      pg.LSN
	}
	var cs []cand
	noRoom := 0
	for _, c := range reps {
		m, ok := st.Members[c.ID]
		r := c.Rep
		if !ok || c.ID == self || !slices.Contains(st.Sync.Members, c.ID) || m.Status != cluster.StatusActive || r == nil {
			continue
		}
		if r.PG == nil || !r.PG.Running || r.PG.Role != "standby" || r.PG.StreamCut || r.Marker != "" {
			continue
		}
		if why == reasonDisk && (r.DiskFree == nil || *r.DiskFree < diskTargetMin || *r.DiskFree <= free) {
			noRoom++
			continue
		}
		var lsn pg.LSN
		if r.PG.ReceiveLSN != nil {
			lsn = *r.PG.ReceiveLSN
		}
		cs = append(cs, cand{c.ID, m.Name, lsn})
	}
	if len(cs) == 0 {
		if why == reasonDisk && noRoom > 0 {
			return "", "No other node has room to take over (their disks are nearly full too): free some space on this computer; writes may be refused until then."
		}
		return "", "No copy is ready to take over right now (none is running and caught up); it keeps trying."
	}
	slices.SortFunc(cs, func(x, y cand) int {
		if c := cmp.Compare(y.lsn, x.lsn); c != 0 {
			return c
		}
		return cmp.Compare(x.name, y.name)
	})
	return cs[0].id, ""
}

// sizeText is a size in plain words.
func sizeText(b uint64) string {
	switch {
	case b >= 10<<30:
		return fmt.Sprintf("%d GB", b>>30)
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
	default:
		return fmt.Sprintf("%d MB", b>>20)
	}
}
