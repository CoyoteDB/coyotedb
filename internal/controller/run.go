package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"coyotedb.com/coyote/internal/cluster"
	"coyotedb.com/coyote/internal/pg"
)

// Cluster is the Raft side the controller uses (a *cluster.Node).
type Cluster interface {
	IsLeader() bool
	State() cluster.State
	VerifyLeader(ctx context.Context) error
	LinearizableState(ctx context.Context) (cluster.State, error)
	Apply(ctx context.Context, cmd cluster.Command) (cluster.Result, error)
}

// SetSRequest asks the primary's agent to set its sync list S.
type SetSRequest struct {
	Epoch uint64   `json:"epoch"` // refused unless the primary is primary at this epoch
	Names []string `json:"names"`
	K     int      `json:"k"`
	// Barrier: after S is confirmed, run a CHECKPOINT so every later
	// commit waits for the new S, then return the primary's flush LSN.
	Barrier bool `json:"barrier"`
}

// Agents reaches the data nodes' agents.
type Agents interface {
	Status(ctx context.Context, id string) (*NodeState, error)
	// Cut is rule 2's step on a standby: no WAL receiver, its WAL end
	// returned. The cut resumes by itself after hold if the epoch has not
	// moved on.
	Cut(ctx context.Context, id string, epoch uint64, hold time.Duration) (pg.LSN, error)
	Resume(ctx context.Context, id string) error
	// SetS sets the primary's S and returns its flush LSN (after the
	// barrier, if asked).
	SetS(ctx context.Context, primary string, req SetSRequest) (pg.LSN, error)
}

// Config wires a Controller.
type Config struct {
	Cluster Cluster
	Agents  Agents
	Clock   Clock // nil: system clock
	Timing  cluster.Timing
	Log     *slog.Logger
	// Margin is added to the lease before the primary counts as gone.
	Margin time.Duration // 0: DefaultMargin
	// Interval between observations.
	Interval time.Duration // 0: DefaultInterval
	// Busy is held while a step runs; planned switches take it too, so the
	// two never interleave.
	Busy *sync.Mutex
	// Fault is a test hook called at named points ("failover-after-cut",
	// "failover-before-cas", "failover-after-cas", "sync-after-s",
	// "degrade-after-eligible"); nil in normal use.
	Fault func(ctx context.Context, point string) error
}

const (
	DefaultMargin   = time.Second
	DefaultInterval = 500 * time.Millisecond
	// CutHold is how long a failover's (or switch's) stream cut holds on a
	// standby if the step never completes (its controller died). The
	// compare-and-set must happen within half of it.
	CutHold = 90 * time.Second
	// catchUpTimeout bounds waiting for copies to hold every write.
	catchUpTimeout = 30 * time.Second
)

// Report is the controller's state for `status` (agent RPC
// controller_status on the leader).
type Report struct {
	Leader     bool      `json:"leader"`
	View       View      `json:"view"`
	Copies     []Copy    `json:"copies,omitempty"`
	LastAction string    `json:"last_action,omitempty"`
	LastAt     time.Time `json:"last_action_at,omitzero"`
	LastError  string    `json:"last_error,omitempty"`
}

// Copy is one data node other than the primary, as the controller sees it.
type Copy struct {
	ID              string        `json:"id"`
	Name            string        `json:"name"`
	Streaming       bool          `json:"streaming"`
	Listed          bool          `json:"listed"` // in the primary's sync list (writes wait for it)
	LagBytes        int64         `json:"lag_bytes"`
	NotStreamingFor time.Duration `json:"not_streaming_ns,omitempty"`
}

// Controller is the failover brain. Run it on every node; it acts only
// while its node is the Raft leader.
type Controller struct {
	cfg Config

	mu     sync.Mutex
	report Report

	// Memory, reset whenever this node (re)gains leadership. Monotonic.
	leader    bool
	electedAt Reading
	leaseKey  [3]any
	leaseAt   Reading
	downSince map[string]Reading
	noSync    Reading // zero: enough copies connected
	blocked   string  // why the last failover attempt gave up
	// blockedLogged: when "main node not replaced" was last logged.
	blockedLogged Reading
}

// New returns a controller.
func New(cfg Config) *Controller {
	if cfg.Clock == nil {
		cfg.Clock = NewSystemClock()
	}
	if cfg.Margin <= 0 {
		cfg.Margin = DefaultMargin
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.Busy == nil {
		cfg.Busy = &sync.Mutex{}
	}
	if cfg.Fault == nil {
		cfg.Fault = func(context.Context, string) error { return nil }
	}
	return &Controller{cfg: cfg, downSince: map[string]Reading{}}
}

// Report returns the latest report.
func (c *Controller) Report() Report {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.report
	r.View.Notes = slices.Clone(r.View.Notes)
	r.Copies = slices.Clone(r.Copies)
	return r
}

// Run observes and acts until ctx ends.
func (c *Controller) Run(ctx context.Context) {
	t := time.NewTicker(c.cfg.Interval)
	defer t.Stop()
	for {
		c.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick is one observation and at most one step.
func (c *Controller) Tick(ctx context.Context) {
	if !c.cfg.Cluster.IsLeader() {
		if c.leader {
			c.cfg.Log.Info("controller: not the leader any more")
		}
		c.leader = false
		c.mu.Lock()
		c.report = Report{}
		c.mu.Unlock()
		return
	}
	now := c.cfg.Clock.Now()
	if !c.leader {
		// A newly elected leader applies the log it inherited only after
		// committing an entry of its own term: decide nothing until the
		// local state is complete.
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, err := c.cfg.Cluster.LinearizableState(rctx)
		cancel()
		if err != nil {
			c.cfg.Log.Debug("controller: leader state not caught up yet", "err", err)
			return
		}
	}
	st := c.cfg.Cluster.State()
	if !c.leader {
		c.leader, c.electedAt = true, now
		c.leaseKey, c.leaseAt = [3]any{}, now
		c.downSince, c.noSync, c.blocked = map[string]Reading{}, Reading{}, ""
		c.cfg.Log.Info("controller: this node is the leader; watching the main node", "epoch", st.Epoch,
			"waits", LeaseLimit(st, c.cfg.Margin))
	}
	if key := [3]any{st.Epoch, st.Lease.Seq, st.PrimaryID}; key != c.leaseKey {
		if c.leaseKey[0] != nil && key[0] != c.leaseKey[0] {
			c.blocked = ""
		}
		c.leaseKey, c.leaseAt = key, now
	}
	in := c.observe(ctx, st, now)
	plan := Decide(in)
	if plan.View.FailoverBlocked == "" && !plan.View.LeaseHealthy && c.blocked != "" {
		plan.View.FailoverBlocked = c.blocked
	}
	// Say why the main node is not being replaced: when it starts, and
	// every 15s while it lasts (the text carries the lease age).
	if why := plan.View.FailoverBlocked; why == "" {
		c.blockedLogged = Reading{}
	} else if c.blockedLogged.IsZero() || now.Sub(c.blockedLogged) >= 15*time.Second {
		c.cfg.Log.Warn("controller: main node not replaced", "why", why, "epoch", st.Epoch)
		c.blockedLogged = now
	}
	copies := copiesOf(in)
	c.mu.Lock()
	c.report.Leader, c.report.View, c.report.Copies = true, plan.View, copies
	c.mu.Unlock()
	if plan.Kind == ActNone {
		return
	}
	if !c.cfg.Busy.TryLock() {
		return // a planned switch is running
	}
	defer c.cfg.Busy.Unlock()
	c.cfg.Log.Info("controller: step", "step", plan.Kind.String(), "member", plan.Member, "why", plan.Why, "epoch", st.Epoch)
	err := c.execute(ctx, st, in, plan)
	c.mu.Lock()
	c.report.LastAction, c.report.LastAt = plan.Kind.String()+": "+plan.Why, time.Now()
	c.report.LastError = ""
	if err != nil {
		c.report.LastError = err.Error()
	}
	c.mu.Unlock()
	if err != nil {
		c.cfg.Log.Warn("controller: step failed; will decide again", "step", plan.Kind.String(), "err", err)
	}
}

func copiesOf(in Input) []Copy {
	var out []Copy
	for _, id := range sortedIDs(in.Members) {
		m := in.Members[id]
		out = append(out, Copy{ID: id, Name: m.Name, Streaming: m.Streaming, Listed: m.Listed, LagBytes: m.LagBytes, NotStreamingFor: m.NotStreamingFor})
	}
	return out
}

// observe asks every data node's agent for its report (in parallel) and
// builds the Input.
func (c *Controller) observe(ctx context.Context, st cluster.State, now Reading) Input {
	in := Input{
		State:     st,
		LeaderFor: now.Sub(c.electedAt),
		LeaseAge:  now.Sub(c.leaseAt),
		Margin:    c.cfg.Margin,
		Members:   map[string]MemberObs{},
	}
	ls := st.Settings
	ls.LeaseDuration = max(st.Lease.Duration, st.Settings.LeaseDuration)
	if err := cluster.CheckLeaseTiming(ls, c.cfg.Timing); err != nil {
		in.TimingProblem = err.Error()
	}
	data := st.DataMembers()
	reports := make([]*NodeState, len(data))
	var wg sync.WaitGroup
	for i, m := range data {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
			defer cancel()
			if r, err := c.cfg.Agents.Status(cctx, m.ID); err == nil {
				reports[i] = r
			}
		}()
	}
	wg.Wait()
	var pr *NodeState
	for i, m := range data {
		if m.ID == st.PrimaryID {
			pr = reports[i]
		}
	}
	in.Primary = pr
	primaryOK := pr != nil && pr.PGRunning && pr.Role == "primary" && pr.Epoch == st.Epoch
	rows := map[string]pg.StandbyRow{}
	slots := map[string]pg.Slot{}
	listed := map[string]bool{}
	if primaryOK {
		for _, r := range pr.Standbys {
			rows[strings.ToLower(r.Name)] = r
		}
		for _, s := range pr.Slots {
			slots[s.Name] = s
		}
		for _, n := range pr.SyncNames {
			listed[strings.ToLower(n)] = true
		}
	}
	for i, m := range data {
		if m.ID == st.PrimaryID {
			continue
		}
		o := MemberObs{ID: m.ID, Name: m.Name, Voter: m.Suffrage == cluster.Voter, Active: m.Status == cluster.StatusActive,
			Removing: m.Removing, InR: slices.Contains(st.Sync.Members, m.ID), Report: reports[i], LagBytes: -1}
		if primaryOK {
			row, ok := rows[strings.ToLower(m.Name)]
			o.Streaming = ok && row.State == "streaming"
			o.Listed = listed[strings.ToLower(m.Name)]
			if ok && row.FlushLSN != nil {
				f := *row.FlushLSN
				o.FlushLSN = &f
				if pr.FlushLSN != nil {
					o.LagBytes = max(int64(*pr.FlushLSN)-int64(f), 0)
				}
			}
			if s, ok := slots[pg.SlotName(m.Name)]; ok && s.WALStatus == "lost" {
				o.SlotLost = true
			}
			if o.Streaming {
				delete(c.downSince, m.ID)
			} else if _, ok := c.downSince[m.ID]; !ok {
				c.downSince[m.ID] = now
			}
		}
		if since, ok := c.downSince[m.ID]; ok {
			o.NotStreamingFor = now.Sub(since)
		}
		in.Members[m.ID] = o
	}
	for id := range c.downSince {
		if _, ok := in.Members[id]; !ok {
			delete(c.downSince, id)
		}
	}
	if primaryOK {
		n, need := 0, min(max(pr.SyncCount, 1), len(pr.SyncNames))
		for _, nm := range pr.SyncNames {
			if r, ok := rows[strings.ToLower(nm)]; ok && r.State == "streaming" {
				n++
			}
		}
		if len(pr.SyncNames) > 0 && n < need {
			if c.noSync.IsZero() {
				c.noSync = now
			}
		} else {
			c.noSync = Reading{}
		}
	}
	if !c.noSync.IsZero() {
		in.NoSyncFor = now.Sub(c.noSync)
	}
	return in
}

func (c *Controller) execute(ctx context.Context, st cluster.State, in Input, p Plan) error {
	switch p.Kind {
	case ActFailover:
		return c.failover(ctx, st, in, p)
	case ActResumeCut:
		cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		return c.cfg.Agents.Resume(cctx, p.Member)
	case ActApplyS:
		return c.applyS(ctx, st, in)
	case ActRecordIneligible:
		return c.write(ctx, &cluster.SetStandbyEligible{ExpectedEpoch: st.Epoch, Eligible: false, At: time.Now().UTC()})
	case ActSyncAdd:
		return c.syncAdd(ctx, st, in, p.Member)
	case ActSyncRemove:
		return c.syncRemove(ctx, st, in, p.Member)
	case ActDegrade:
		return c.degrade(ctx, st)
	case ActMakeEligible:
		return c.makeEligible(ctx, st, in, p.Member)
	case ActPrune:
		return c.prune(ctx, st, in, p.Member)
	case ActMarkReclone:
		s := cluster.StatusNeedsReclone
		e := st.Epoch
		return c.write(ctx, &cluster.UpdateMember{ExpectedEpoch: &e, ID: p.Member, Status: &s})
	}
	return fmt.Errorf("internal error: unhandled step %s", p.Kind)
}

// write applies cmd after checking this node is still the leader. Every
// command it is given is a compare-and-set on the epoch.
func (c *Controller) write(ctx context.Context, cmd cluster.Command) error {
	return c.writeWithin(ctx, cmd, writeTimeout)
}

// writeTimeout bounds one controller write (leadership check + Apply).
const writeTimeout = 10 * time.Second

// writeWithin is write with the leadership check and the Apply bounded
// together by d.
func (c *Controller) writeWithin(ctx context.Context, cmd cluster.Command, d time.Duration) error {
	cctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	if err := c.cfg.Cluster.VerifyLeader(cctx); err != nil {
		return fmt.Errorf("not the leader any more: %w", err)
	}
	_, err := c.cfg.Cluster.Apply(cctx, cmd)
	return err
}

// ---- failover (rule 2) ----

// failover: the primary's lease ran out. Steps, each safe to repeat:
//
//  1. Linearizable read: still the same epoch, primary and lease sequence.
//  2. Cut the stream on every candidate (R minus the last node that
//     served), in parallel; each answers with its WAL end.
//  3. ChooseTarget: at least |candidates|-k+1 cut, highest WAL end wins.
//  4. Within half the cut hold, after a leadership check: Promote, a
//     compare-and-set on the epoch and on the lease sequence that was
//     seen to expire (if the primary renewed meanwhile, it fails).
//
// The target's agent then renews the lease for the new epoch, sets its
// sync list, promotes and creates slots; the cut standbys see the epoch
// move on and follow it; the old primary rejoins as a copy. If anything
// fails before step 4, the cuts are undone and nothing changed. If this
// controller dies after step 2, the cuts resume by themselves after
// CutHold (the next controller re-cuts first if it fails over).
func (c *Controller) failover(ctx context.Context, seen cluster.State, in Input, p Plan) error {
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	st, err := c.cfg.Cluster.LinearizableState(rctx)
	cancel()
	if err != nil {
		return fmt.Errorf("read the cluster state: %w", err)
	}
	if st.Epoch != seen.Epoch || st.PrimaryID != seen.PrimaryID || st.Lease.Seq != p.ExpectedLeaseSeq {
		return nil // something changed; decide again
	}
	cands := Candidates(st)
	start := c.cfg.Clock.Now()
	results := map[string]CutResult{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, id := range cands {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 25*time.Second)
			defer cancel()
			lsn, err := c.cfg.Agents.Cut(cctx, id, st.Epoch, CutHold)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				results[id] = CutResult{Err: err.Error()}
				c.cfg.Log.Warn("failover: could not cut a copy's stream", "node", name(st, id), "err", err)
				return
			}
			results[id] = CutResult{Cut: true, LSN: lsn}
		}()
	}
	wg.Wait()
	var cut []string
	for _, id := range cands {
		if results[id].Cut {
			cut = append(cut, id)
		}
	}
	abort := func(why string) error {
		c.blocked = fmt.Sprintf("%s (the main node) is gone, but no other node was made the main node: %s", name(st, st.PrimaryID), why)
		c.resume(ctx, st, cut)
		return errors.New(why)
	}
	if err := c.cfg.Fault(ctx, "failover-after-cut"); err != nil {
		return err
	}
	damaged := map[string]string{}
	for id, m := range in.Members {
		if m.Report.Damaged() {
			damaged[id] = m.Report.PagesDamaged
		}
	}
	target, why := ChooseTarget(st, results, damaged)
	if target == "" {
		return abort(why)
	}
	var add []string
	oneCopy := !hasLiveCopy(st, in, results, target) && st.Settings.SingleDataNodeMode != cluster.ModeStrict
	rest := slices.DeleteFunc(slices.Clone(st.Sync.Members), func(id string) bool { return id == target })
	if old, ok := st.Members[st.PrimaryID]; !oneCopy && len(rest) == 0 && ok && old.ID != target && old.Role == cluster.RoleData && old.Status == cluster.StatusActive {
		// Writes on the new primary wait for the old one to rejoin (strict
		// mode, or another copy exists): never accept writes on one copy
		// silently.
		add = []string{old.ID}
	}
	if err := c.cfg.Fault(ctx, "failover-before-cas"); err != nil {
		return err
	}
	seq := p.ExpectedLeaseSeq
	cmd := &cluster.Promote{ExpectedEpoch: st.Epoch, ExpectedLeaseSeq: &seq, NodeID: target,
		LeaseDuration: st.Settings.LeaseDuration, AddToSync: add}
	if oneCopy {
		// Rule 9 at once: no other data node can follow the new primary
		// now (the old one just lost its lease; any other is down), so
		// waiting degrade_after for a copy that cannot come would only
		// block writes. Eligibility false and R cleared in the same entry.
		cmd.OneCopy, cmd.At = true, time.Now().UTC()
	}
	// The compare-and-set lands within half the cut hold of the first cut
	// or not at all from here: the write's own context ends there (F5).
	// (A Raft entry whose Apply timed out may still commit later; it is
	// still a compare-and-set on the epoch and the lease sequence.)
	left := CutHold/2 - c.cfg.Clock.Now().Sub(start)
	if left <= 0 {
		return abort(fmt.Sprintf("disconnecting the copies took too long (%s)", c.cfg.Clock.Now().Sub(start).Round(time.Second)))
	}
	if err := c.writeWithin(ctx, cmd, min(left, writeTimeout)); err != nil {
		return abort(fmt.Sprintf("recording the new main node failed: %v", err))
	}
	lsns := map[string]string{}
	for id, r := range results {
		if r.Cut {
			lsns[name(st, id)] = r.LSN.String()
		}
	}
	c.cfg.Log.Warn("failover: new main node recorded", "from", name(st, st.PrimaryID), "to", name(st, target),
		"epoch", st.Epoch+1, "wal_ends", lsns, "waited", p.View.LeaseAge.Round(time.Millisecond), "one_copy", oneCopy)
	c.blocked = ""
	return c.cfg.Fault(ctx, "failover-after-cas")
}

// hasLiveCopy: some data member other than the target and the primary
// that just lost its lease could follow the target now (it was cut, or it
// is a healthy standby).
func hasLiveCopy(st cluster.State, in Input, results map[string]CutResult, target string) bool {
	for _, m := range st.DataMembers() {
		if m.ID == target || m.ID == st.PrimaryID || m.Status != cluster.StatusActive {
			continue
		}
		if results[m.ID].Cut {
			return true
		}
		if o, ok := in.Members[m.ID]; ok && o.Report.IsHealthyStandby() {
			return true
		}
	}
	return false
}

func (c *Controller) resume(ctx context.Context, st cluster.State, ids []string) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()
	for _, id := range ids {
		if err := c.cfg.Agents.Resume(rctx, id); err != nil {
			c.cfg.Log.Warn("could not resume a copy's stream; it resumes by itself later", "node", name(st, id), "err", err)
		}
	}
}

// ---- the sync set (rules 3 and 9) ----

// targetS is the S the primary should have: every member of R except the
// primary while copies are eligible; while running on one copy, only
// what S already has that is still in R (never adds a wait by itself).
func targetS(st cluster.State, in Input) ([]string, int) {
	var ids []string
	if st.StandbyEligible {
		for _, id := range st.Sync.Members {
			if id != st.PrimaryID {
				ids = append(ids, id)
			}
		}
	} else if in.Primary != nil {
		for _, n := range in.Primary.SyncNames {
			if id, ok := idByName(st, n); ok && slices.Contains(st.Sync.Members, id) && id != st.PrimaryID {
				ids = append(ids, id)
			}
		}
	}
	return namesOf(st, ids), effK(st, len(ids))
}

func effK(st cluster.State, n int) int {
	if n == 0 {
		return 0
	}
	return min(max(st.Sync.K, 1), n)
}

func (c *Controller) setS(ctx context.Context, st cluster.State, names []string, k int, barrier bool) (pg.LSN, error) {
	cctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	// Rule 3: S must stay within R. Check against a linearizable read (a
	// stale copy could still list a member already taken out of R).
	cur, err := c.cfg.Cluster.LinearizableState(cctx)
	if err != nil {
		return 0, fmt.Errorf("not the leader any more: %w", err)
	}
	if cur.Epoch != st.Epoch || cur.PrimaryID != st.PrimaryID {
		return 0, fmt.Errorf("the main node changed (term %d to %d) while planning a sync list change", st.Epoch, cur.Epoch)
	}
	inR := namesOf(cur, cur.Sync.Members)
	for _, nm := range names {
		if !slices.ContainsFunc(inR, func(r string) bool { return strings.EqualFold(r, nm) }) {
			return 0, fmt.Errorf("internal error: sync list %v is not within the sync set %v (rule 3)", names, inR)
		}
	}
	return c.cfg.Agents.SetS(cctx, st.PrimaryID, SetSRequest{Epoch: st.Epoch, Names: names, K: k, Barrier: barrier})
}

func (c *Controller) setR(ctx context.Context, st cluster.State, members []string) error {
	ms := slices.Clone(members)
	slices.Sort(ms)
	ms = slices.Compact(ms)
	k := 0
	if len(ms) > 0 {
		k = min(max(st.Sync.K, 1), len(ms))
	}
	return c.write(ctx, &cluster.SetSyncSet{ExpectedEpoch: st.Epoch, Members: ms, K: k})
}

func (c *Controller) applyS(ctx context.Context, st cluster.State, in Input) error {
	names, k := targetS(st, in)
	_, err := c.setS(ctx, st, names, k, false)
	return err
}

// syncAdd: R first, then S (rule 3). Adding only tightens rule 2.
func (c *Controller) syncAdd(ctx context.Context, st cluster.State, in Input, id string) error {
	if err := c.setR(ctx, st, append(slices.Clone(st.Sync.Members), id)); err != nil {
		return err
	}
	st = c.cfg.Cluster.State()
	return c.applyS(ctx, st, in)
}

// syncRemove: S first (confirmed by sync_state, then a barrier so every
// later commit waits for the new S), then wait until k of the remaining
// copies hold every write the primary had flushed at the barrier (each
// acknowledged write was only promised to be on k members of the old S,
// which included the one going), and only then R.
func (c *Controller) syncRemove(ctx context.Context, st cluster.State, in Input, id string) error {
	nextR := slices.DeleteFunc(slices.Clone(st.Sync.Members), func(x string) bool { return x == id })
	var ids []string
	for _, x := range nextR {
		if x != st.PrimaryID {
			ids = append(ids, x)
		}
	}
	names, k := namesOf(st, ids), effK(st, len(ids))
	if k == 0 {
		return errors.New("internal error: removing the last copy from the sync set (that is a degrade)")
	}
	l, err := c.setS(ctx, st, names, k, true)
	if err != nil {
		return err
	}
	if err := c.cfg.Fault(ctx, "sync-after-s"); err != nil {
		return err
	}
	if err := c.waitFlushed(ctx, st, names, k, l); err != nil {
		return err
	}
	return c.setR(ctx, st, nextR)
}

// degrade (rule 9): record standby_eligible=false first (from now on no
// copy may be promoted), then stop waiting for copies.
func (c *Controller) degrade(ctx context.Context, st cluster.State) error {
	if st.StandbyEligible {
		if err := c.write(ctx, &cluster.SetStandbyEligible{ExpectedEpoch: st.Epoch, Eligible: false, At: time.Now().UTC()}); err != nil {
			return err
		}
		c.cfg.Log.Warn("controller: no copy connected for degrade_after; the main node carries on alone", "after", st.Settings.DegradeAfter)
	}
	if err := c.cfg.Fault(ctx, "degrade-after-eligible"); err != nil {
		return err
	}
	_, err := c.setS(ctx, st, nil, 0, true)
	return err
}

// prune: while running on one copy, take a long-gone member out of S
// (if there) and then out of R. No copy is eligible, so no catch-up wait.
func (c *Controller) prune(ctx context.Context, st cluster.State, in Input, id string) error {
	names, _ := targetS(st, in)
	m := name(st, id)
	if slices.ContainsFunc(names, func(n string) bool { return strings.EqualFold(n, m) }) {
		names = slices.DeleteFunc(names, func(n string) bool { return strings.EqualFold(n, m) })
		if _, err := c.setS(ctx, st, names, effK(st, len(names)), true); err != nil {
			return err
		}
	}
	return c.setR(ctx, st, slices.DeleteFunc(slices.Clone(st.Sync.Members), func(x string) bool { return x == id }))
}

// makeEligible (rule 9): a copy is streaming and nearly caught up. In R
// (first), then in S (confirmed, barrier: from here every commit waits for
// it), then wait until it holds every write the primary had flushed at the
// barrier; only then record standby_eligible=true.
func (c *Controller) makeEligible(ctx context.Context, st cluster.State, in Input, id string) error {
	if !slices.Contains(st.Sync.Members, id) {
		if err := c.setR(ctx, st, append(slices.Clone(st.Sync.Members), id)); err != nil {
			return err
		}
		st = c.cfg.Cluster.State()
	}
	names, _ := targetS(st, in)
	m := name(st, id)
	if !slices.ContainsFunc(names, func(n string) bool { return strings.EqualFold(n, m) }) {
		names = append(names, m)
	}
	sort.Strings(names)
	need := max(st.Sync.K, 1)
	k := min(need, len(names))
	l, err := c.setS(ctx, st, names, k, true)
	if err != nil {
		return err
	}
	if k < need {
		return nil // more copies must be back before writes are on k of them
	}
	if err := c.waitFlushed(ctx, st, names, k, l); err != nil {
		return err
	}
	if err := c.write(ctx, &cluster.SetStandbyEligible{ExpectedEpoch: st.Epoch, Eligible: true, At: time.Now().UTC()}); err != nil {
		return err
	}
	c.cfg.Log.Info("controller: copies are full copies again", "sync", names)
	return nil
}

// waitFlushed waits until at least k of the named standbys have flushed
// lsn, as the primary sees it.
func (c *Controller) waitFlushed(ctx context.Context, st cluster.State, names []string, k int, lsn pg.LSN) error {
	deadline := time.Now().Add(catchUpTimeout)
	want := map[string]bool{}
	for _, n := range names {
		want[strings.ToLower(n)] = true
	}
	for {
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		r, err := c.cfg.Agents.Status(cctx, st.PrimaryID)
		cancel()
		if err == nil {
			n := 0
			for _, row := range r.Standbys {
				if want[strings.ToLower(row.Name)] && row.FlushLSN != nil && *row.FlushLSN >= lsn {
					n++
				}
			}
			if n >= k {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the copies did not catch up to %s within %s", lsn, catchUpTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func name(st cluster.State, id string) string {
	if m, ok := st.Members[id]; ok {
		return m.Name
	}
	return id
}

func namesOf(st cluster.State, ids []string) []string {
	var out []string
	for _, id := range ids {
		out = append(out, name(st, id))
	}
	sort.Strings(out)
	return out
}

func idByName(st cluster.State, n string) (string, bool) {
	for id, m := range st.Members {
		if strings.EqualFold(m.Name, n) {
			return id, true
		}
	}
	return "", false
}
