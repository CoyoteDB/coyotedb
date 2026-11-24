package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"coyotedb.com/coyote/internal/brand"
	"coyotedb.com/coyote/internal/cluster"
	"coyotedb.com/coyote/internal/controller"
	"coyotedb.com/coyote/internal/pg"
)

// SwitchResult is what `dev switch` reports.
type SwitchResult struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Epoch   uint64 `json:"epoch"`
	Message string `json:"message"`
}

// runSwitch moves the primary role to `to` through the cluster state. It
// runs on the Raft leader (forwarded there otherwise), as the controller
// will in M4. Steps, in order:
//
//  1. Linearizable read; the target must be a data member, in the sync
//     set R when R is not empty (rule 3), and running as a standby.
//  2. Cut the WAL stream on every other reachable standby and confirm
//     (rule 2), noting the received (flushed) LSN of each.
//  3. Let the target catch up to the highest of those, then cut it too.
//     Abort if it is still behind (it might miss an acknowledged write).
//  4. Check rule 2's count: at least |R|-k+1 members of R are cut.
//  5. Compare-and-set the primary in the cluster state (expected epoch).
//  6. The target's agent sees it is primary and promotes itself (sync
//     list first, then pg_promote, then slots); the cut standbys see the
//     epoch moved on and follow the new primary through their forwarders;
//     the old primary's agent writes its rejoin marker, stops, rewinds
//     through the tunnel and comes back as a standby. Here we wait until
//     the target reports it is primary.
//
// Before step 5, any failure resumes the cut streams: nothing changed.
// Every step is safe to repeat, so a switch interrupted by a crash is
// finished by running it again.
//
// planned (`coyote switch`) adds the M4 rules: the target must be in R
// while copies are eligible (rule 9: not while running on one copy), the
// count is taken over R minus the last node that served, and step 5 is the
// strict Promote compare-and-set (which also puts the old primary into R
// when the target was the only member, so writes wait for it to rejoin).
// Without planned it is the M3 `dev switch` (a plain SetPrimary).
func (a *Agent) runSwitch(ctx context.Context, to string, planned bool) (*SwitchResult, error) {
	return a.runSwitchFrom(ctx, to, planned, "", 0)
}

// errMainMoved: a conditional switch (from, epoch) found another main node
// or term: nothing was done.
var errMainMoved = errors.New("the main node or term changed before the switch; nothing was done")

// runSwitchFrom is runSwitch; with from set, it goes ahead only while from
// is the main node at fromEpoch (checked on the leader's linearizable read, so
// a requester with a stale view never moves the role away from another
// node: D58).
func (a *Agent) runSwitchFrom(ctx context.Context, to string, planned bool, from string, fromEpoch uint64) (*SwitchResult, error) {
	if !a.cn.IsLeader() {
		return a.forwardSwitch(ctx, to, planned, from, fromEpoch)
	}
	if planned {
		// Until the main node reports that k copies hold everything it had
		// when it started (RenewLease Served), it stays a candidate in the
		// rule 2 count and cannot be cut: a planned switch right after a
		// change of main node would be refused for a confusing reason.
		// Wait a little for it instead (outside the controller's lock).
		if err := a.waitServed(ctx, 30*time.Second); err != nil {
			return nil, err
		}
	}
	// The controller holds this lock while it runs a step (at most ~1
	// minute); wait for it.
	for !a.switchMu.TryLock() {
		select {
		case <-ctx.Done():
			return nil, errors.New("another switch or failover step is in progress; try again shortly")
		case <-time.After(100 * time.Millisecond):
		}
	}
	defer a.switchMu.Unlock()

	st, err := a.cn.LinearizableState(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the cluster state: %w", err)
	}
	if from != "" && (st.PrimaryID != from || st.Epoch != fromEpoch) {
		return nil, errMainMoved
	}
	target, ok := findMember(st, to)
	switch {
	case !ok:
		return nil, fmt.Errorf("no node called %q in this cluster", to)
	case target.Role != cluster.RoleData:
		return nil, fmt.Errorf("%s is a witness; it has no database to become the main node", target.Name)
	case target.Status != cluster.StatusActive:
		return nil, fmt.Errorf("%s needs a fresh copy before it can become the main node", target.Name)
	}
	oldName := a.primaryName(st)
	if target.ID == st.PrimaryID {
		return &SwitchResult{From: oldName, To: target.Name, Epoch: st.Epoch, Message: target.Name + " is already the main node."}, nil
	}
	if planned {
		switch {
		case !st.StandbyEligible:
			return nil, fmt.Errorf("the cluster is running on one copy (%s holds writes no other node may have), so no other node can become the main node safely; wait until the copies have caught up", oldName)
		case !slices.Contains(st.Sync.Members, target.ID):
			return nil, fmt.Errorf("%s is not a full copy yet (not in the sync set), so it may be missing acknowledged writes; wait until it has caught up", target.Name)
		case st.LastServed != st.PrimaryID:
			return nil, errNotServed(oldName)
		}
	}
	// Rule 1 (mixed systems, D37): a switch between kinds of machines
	// needs matching fingerprints and probes and no blocked guard.
	if why := crossSystemSwitchBlocked(st, target); why != "" {
		return nil, errors.New(why)
	}
	if len(st.Sync.Members) > 0 && !slices.Contains(st.Sync.Members, target.ID) {
		return nil, fmt.Errorf("%s is not in the sync set, so it may be missing acknowledged writes (safety rule 3); add it first: %s dev set-sync", target.Name, brand.Command)
	}
	epoch := st.Epoch
	a.log.Info("switch: start", "from", oldName, "to", target.Name, "epoch", epoch)

	var tr NodeReport
	if err := a.call(ctx, target.ID, "status", nil, &tr); err != nil {
		return nil, fmt.Errorf("%s is not reachable: %w", target.Name, err)
	}
	if tr.PG == nil || !tr.PG.Running || tr.PG.Role != "standby" || tr.Marker != "" {
		return nil, fmt.Errorf("%s is not running as a healthy copy right now (%s)", target.Name, tr.State)
	}
	if err := fault(ctx, a, "switch-before-cut"); err != nil {
		return nil, err
	}

	// 2. Cut the other standbys. Each cut holds for switchCutHold; the
	// compare-and-set must happen within half of that, or the switch is
	// cancelled (so a standby that resumes by itself after a dead leader
	// can never meet a switch that still goes ahead).
	var cut []string
	cutStart := time.Now()
	hold := int(switchCutHold / time.Second)
	stepped := false // the old main node stepped down (step 4b): tell it the switch is off
	abort := func(why error) (*SwitchResult, error) {
		a.log.Warn("switch: aborted; resuming streams", "err", why)
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
		defer cancel()
		if stepped {
			if err := a.call(rctx, st.PrimaryID, "step_up", stepDownReq{Epoch: epoch}, nil); err != nil {
				a.log.Warn("switch: could not tell the old main node to carry on (it does by itself shortly)", "err", err)
			}
		}
		var notResumed []string
		for _, id := range cut {
			if err := a.call(rctx, id, "resume_stream", nil, nil); err != nil {
				notResumed = append(notResumed, a.memberName(id))
				a.log.Warn("switch: resume stream", "node", id, "err", err)
			}
		}
		msg := fmt.Sprintf("switch cancelled, nothing changed: %s is still the main node. Reason: %v", oldName, why)
		if len(notResumed) > 0 {
			msg += fmt.Sprintf(". These copies are still disconnected and reconnect after the next switch: %s", strings.Join(notResumed, ", "))
		}
		return nil, errors.New(msg)
	}
	var maxLSN pg.LSN
	maxFrom := ""
	for _, m := range st.DataMembers() {
		if m.ID == st.PrimaryID || m.ID == target.ID || m.Status != cluster.StatusActive {
			continue
		}
		var cr cutResp
		if err := a.call(ctx, m.ID, "cut_stream", cutReq{Epoch: epoch, HoldSeconds: hold}, &cr); err != nil {
			a.log.Warn("switch: could not cut a standby (it cannot confirm writes either)", "node", m.Name, "err", err)
			continue
		}
		cut = append(cut, m.ID)
		lsn, err := pg.ParseLSN(cr.LSN)
		if err != nil {
			return abort(err)
		}
		if lsn > maxLSN {
			maxLSN, maxFrom = lsn, m.Name
		}
	}

	// 3. Target catches up, then is cut.
	if maxLSN > 0 {
		if err := a.catchUp(ctx, target, maxLSN); err != nil {
			return abort(fmt.Errorf("%s did not catch up with %s: %w", target.Name, maxFrom, err))
		}
	}
	var cr cutResp
	if err := a.call(ctx, target.ID, "cut_stream", cutReq{Epoch: epoch, HoldSeconds: hold, Promote: true}, &cr); err != nil {
		return abort(fmt.Errorf("cut %s's stream: %w", target.Name, err))
	}
	cut = append(cut, target.ID)
	tlsn, err := pg.ParseLSN(cr.LSN)
	if err != nil {
		return abort(err)
	}
	if tlsn < maxLSN {
		return abort(fmt.Errorf("%s has received less (%s) than %s (%s); promoting it could lose acknowledged writes", target.Name, tlsn, maxFrom, maxLSN))
	}

	// 4. Rule 2's count over R.
	if planned {
		cands := controller.Candidates(st)
		got := 0
		for _, id := range cands {
			if slices.Contains(cut, id) {
				got++
			}
		}
		if need := controller.CutsNeeded(st, cands); got < need {
			return abort(fmt.Errorf("only %d of the %d copies in the sync set could be disconnected; %d are needed so the old main node cannot confirm more writes (safety rule 2)", got, len(cands), need))
		}
	} else if len(st.Sync.Members) > 0 {
		var n, got int
		for _, id := range st.Sync.Members {
			if id == st.PrimaryID {
				continue
			}
			n++
			if slices.Contains(cut, id) {
				got++
			}
		}
		if need := n; got < need { // rule 2 counted with k = 1 (D42)
			return abort(fmt.Errorf("only %d of the %d copies in the sync set could be disconnected; %d are needed so the old main node cannot confirm more writes (safety rule 2)", got, n, need))
		}
	}
	if err := fault(ctx, a, "switch-after-cut"); err != nil {
		return nil, err
	}

	// 4b. The old main node stops first (F6, D41): it confirms its
	// Postgres has exited, or its lease is waited out. Never two main
	// nodes, even when the old one is frozen.
	leaseSeq, err := a.oldMainStopped(ctx, st, cutStart, &stepped)
	if err != nil {
		return abort(err)
	}

	// 5. Compare-and-set, within half the cut hold of the first cut.
	left := switchCutHold/2 - time.Since(cutStart)
	if left <= 0 {
		return abort(fmt.Errorf("the switch took too long (%s) between disconnecting the copies and recording the new main node", time.Since(cutStart).Round(time.Second)))
	}
	var cas cluster.Command = &cluster.SetPrimary{ExpectedEpoch: epoch, NodeID: target.ID, LeaseDuration: st.Settings.LeaseDuration}
	if planned {
		var add []string
		rest := slices.DeleteFunc(slices.Clone(st.Sync.Members), func(id string) bool { return id == target.ID })
		if old, ok := st.Members[st.PrimaryID]; ok && len(rest) == 0 && old.Status == cluster.StatusActive {
			add = []string{old.ID}
		}
		// Fails if the old main node took its lease again meanwhile.
		cas = &cluster.Promote{ExpectedEpoch: epoch, ExpectedLeaseSeq: &leaseSeq, NodeID: target.ID,
			LeaseDuration: st.Settings.LeaseDuration, AddToSync: add}
	}
	cctx, cancel := context.WithTimeout(ctx, min(left, 10*time.Second))
	defer cancel()
	if err := a.cn.VerifyLeader(cctx); err != nil {
		return abort(fmt.Errorf("this node is no longer the cluster leader: %w", err))
	}
	if _, err := a.cn.Apply(cctx, cas); err != nil {
		return abort(fmt.Errorf("record the new main node: %w", err))
	}
	newEpoch := epoch + 1
	a.log.Info("switch: cluster state updated", "primary", target.Name, "epoch", newEpoch)
	if err := fault(ctx, a, "switch-after-cas"); err != nil {
		return nil, err
	}

	// 6. Wait for the promotion.
	deadline := time.Now().Add(2 * time.Minute)
	for {
		var r NodeReport
		err := a.call(ctx, target.ID, "status", nil, &r)
		if err == nil && r.PG != nil && r.PG.Role == "primary" && r.LastPrimaryEpoch >= newEpoch {
			break
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return nil, fmt.Errorf("the cluster now names %s as the main node (term %d), but it has not finished becoming the main node yet; "+
				"it keeps trying by itself. Check with `%s status`", target.Name, newEpoch, brand.Command)
		}
		select {
		case <-ctx.Done():
		case <-time.After(200 * time.Millisecond):
		}
	}
	msg := fmt.Sprintf("%s is now the main node (term %d).", target.Name, newEpoch)
	if st.PrimaryID != "" {
		msg += fmt.Sprintf(" %s will rejoin as a copy by itself.", oldName)
	}
	a.log.Info("switch: done", "primary", target.Name, "epoch", newEpoch)
	return &SwitchResult{From: oldName, To: target.Name, Epoch: newEpoch, Message: msg}, nil
}

// forwardSwitch runs the switch on the leader. Right after the leader
// failed, this node may still name it; wait (up to 30s) for a new one.
func (a *Agent) forwardSwitch(ctx context.Context, to string, planned bool, from string, epoch uint64) (*SwitchResult, error) {
	deadline := time.Now().Add(30 * time.Second)
	for {
		leader := a.cn.LeaderID()
		var err error
		switch {
		case a.cn.IsLeader():
			return a.runSwitchFrom(ctx, to, planned, from, epoch)
		case leader == "":
			err = errors.New("the cluster has no leader right now (not enough nodes reachable)")
		default:
			var r SwitchResult
			err = a.call(ctx, leader, "switch", switchReq{To: to, Planned: planned, From: from, Epoch: epoch}, &r)
			var re *rpcError
			if err == nil {
				return &r, nil
			}
			if errors.As(err, &re) && re.Code != codeNotLeader {
				return nil, err // the leader ran it and said no
			}
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return nil, fmt.Errorf("%w; try again shortly", err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// catchUp waits until the target has received at least lsn, reconnecting
// its stream first if it was cut earlier (a repeated switch).
func (a *Agent) catchUp(ctx context.Context, target cluster.Member, lsn pg.LSN) error {
	deadline := time.Now().Add(30 * time.Second)
	resumed := false
	for {
		var r NodeReport
		if err := a.call(ctx, target.ID, "status", nil, &r); err != nil {
			return err
		}
		if r.PG != nil && r.PG.ReceiveLSN != nil && *r.PG.ReceiveLSN >= lsn {
			return nil
		}
		if r.PG != nil && r.PG.StreamCut && !resumed {
			resumed = true
			if err := a.call(ctx, target.ID, "resume_stream", nil, nil); err != nil {
				return err
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("still behind %s after 30s", lsn)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// switchCutHold is how long a switch's stream cut holds on a standby if
// the switch never completes (its leader died).
const switchCutHold = controller.CutHold

// faultEnv names a test-only fault point: when the agent reaches it, it
// says so and stops there until it is killed. Used by the integration
// tests to kill an agent at an exact step; unset in normal use.
const faultEnv = "COYOTE_TEST_FAULT"

// slowStartDelay is how late the slow-start-after-clear-fence fault makes
// a primary start return.
const slowStartDelay = 25 * time.Second

func fault(ctx context.Context, a *Agent, point string) error {
	if os.Getenv(faultEnv) != point {
		return nil
	}
	a.log.Warn("test fault point reached; pausing", "point", point)
	a.say("Test fault point reached: %s (pausing).", point)
	<-ctx.Done()
	return ctx.Err()
}

func errNotServed(primary string) error {
	return fmt.Errorf("%s became the main node only recently and has not yet confirmed that a copy holds all of its data; try again in a few seconds", primary)
}

// waitServed waits until the cluster state records that the current main
// node has served (LastServed), polling linearizable reads.
func (a *Agent) waitServed(ctx context.Context, within time.Duration) error {
	deadline := time.Now().Add(within)
	for {
		st, err := a.cn.LinearizableState(ctx)
		if err != nil {
			return fmt.Errorf("read the cluster state: %w", err)
		}
		if st.PrimaryID == "" || st.LastServed == st.PrimaryID {
			return nil
		}
		if time.Now().After(deadline) {
			return errNotServed(a.primaryName(st))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}
