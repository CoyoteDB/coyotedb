package controller

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"coyotedb.com/coyote/internal/brand"
	"coyotedb.com/coyote/internal/cluster"
)

// ActKind is one controller step.
type ActKind int

const (
	ActNone             ActKind = iota
	ActFailover                 // the primary is gone: rule 2 cut, choose, compare-and-set
	ActResumeCut                // a failover/switch cut was left behind by a dead controller: undo it
	ActApplyS                   // the primary's sync list S differs from R: set it from R
	ActRecordIneligible         // S is empty while R claims to be eligible: record the truth
	ActSyncAdd                  // R first, then S (rule 3)
	ActSyncRemove               // S first, confirm, wait for the rest to hold every write, then R
	ActDegrade                  // rule 9: standby_eligible=false first, then clear S
	ActMakeEligible             // rule 9: S includes it, barrier, it catches up, eligible=true
	ActPrune                    // while not eligible: drop a lost member from S, then R
	ActMarkReclone              // rule 8: member gone too long or its slot lost WAL
)

func (k ActKind) String() string {
	return [...]string{"none", "failover", "resume-cut", "apply-sync-list", "record-not-eligible", "sync-add",
		"sync-remove", "degrade", "make-eligible", "prune", "mark-reclone"}[k]
}

// Plan is what Decide returns: at most one step, plus what status says.
type Plan struct {
	Kind   ActKind
	Member string // the member the step is about
	Why    string // log line
	// ExpectedLeaseSeq is the lease sequence that was seen to expire
	// (ActFailover): the compare-and-set fails if the primary renewed.
	ExpectedLeaseSeq uint64
	View             View
}

// View is the controller's plain-words summary for `status`.
type View struct {
	LeaseHealthy bool `json:"lease_healthy"`
	// LeaseAge: how long since the controller saw the lease renewed.
	LeaseAge time.Duration `json:"lease_age_ns"`
	// FailoverBlocked says why the main node is not being replaced
	// although its lease ran out ("" when not blocked).
	FailoverBlocked string `json:"failover_blocked,omitempty"`
	// WritesWaiting: commits wait for a copy that is not connected.
	WritesWaiting    bool          `json:"writes_waiting"`
	WritesWaitingFor time.Duration `json:"writes_waiting_ns,omitempty"`
	// OneCopy: running on one data node (standby_eligible is false).
	OneCopy bool     `json:"one_copy"`
	Notes   []string `json:"notes,omitempty"`
}

func (p Plan) with(k ActKind, m, why string) Plan { p.Kind, p.Member, p.Why = k, m, why; return p }

// Decide is the controller's decision table. It is pure: everything it
// looks at is in Input. Steps, in order of priority:
//
//  1. The primary's lease ran out (no renewal seen for lease + margin,
//     counted from no earlier than this controller's election): fail
//     over, unless that is impossible or unsafe, in which case nothing is
//     done and View.FailoverBlocked says why (unavailable beats wrong).
//  2. Otherwise keep the cluster tidy, one step at a time: undo cuts left
//     behind by a dead controller; keep S a subset of R; record "one copy"
//     honestly; mark members gone too long for a re-clone; manage the sync
//     set (add before degrade, remove lost members while enough others
//     stream, degrade after degrade_after unless strict); make a copy
//     eligible again once it has caught up.
func Decide(in Input) Plan {
	st := in.State
	s := st.Settings
	p := Plan{}
	p.View.LeaseAge = in.LeaseAge
	p.View.OneCopy = !st.StandbyEligible
	name := func(id string) string {
		if m, ok := st.Members[id]; ok {
			return m.Name
		}
		return id
	}
	if st.PrimaryID == "" {
		p.View.Notes = append(p.View.Notes, "No node is the main node, so there is nothing to keep running.")
		return p
	}
	primary := name(st.PrimaryID)
	limit := LeaseLimit(st, in.Margin)
	gone := in.LeaseAge >= limit && in.LeaderFor >= limit
	p.View.LeaseHealthy = in.LeaseAge < limit
	if gone {
		return decideFailover(in, p, primary)
	}

	// ---- primary healthy: maintenance ----

	// 1. Orphaned cuts (a switch or failover whose controller died before
	// its compare-and-set): the main node is fine, so let them follow it.
	for _, id := range sortedIDs(in.Members) {
		m := in.Members[id]
		if r := m.Report; r.IsHealthyStandby() && r.StreamCut && r.CutHold && r.CutEpoch == st.Epoch {
			return p.with(ActResumeCut, id, fmt.Sprintf("%s's stream was cut by a switch that never finished; resuming it", m.Name))
		}
	}
	pr := in.Primary
	if pr == nil {
		// Its lease is renewed but this node (the leader) cannot reach it:
		// a main node no leader reaches stops renewing (D62), so this ends
		// in a normal failover unless it is reachable again first.
		p.View.Notes = append(p.View.Notes, fmt.Sprintf("%s (the main node) does not answer the leader. If no leader can reach it for a few seconds, "+
			"it stops renewing its lease and another node takes over.", primary))
		return p
	}
	if !pr.PGRunning || pr.Role != "primary" || pr.Epoch != st.Epoch {
		p.View.Notes = append(p.View.Notes, fmt.Sprintf("Waiting for %s to report as the running main node.", primary))
		return p
	}

	// D55: damaged data is reported (the copy retires itself; it is
	// never added back to the sync set nor promoted while damaged).
	if pr.Damaged() {
		p.View.Notes = append(p.View.Notes, fmt.Sprintf("%s (the main node) has damaged data pages: %s. "+
			"Check its disk, and switch to a healthy copy.", primary, pr.PagesDamaged))
	}
	for _, id := range sortedIDs(in.Members) {
		m := in.Members[id]
		if !m.Report.Damaged() {
			continue
		}
		then := "it is copied fresh from " + primary + "."
		if s.SingleDataNodeMode == cluster.ModeStrict && m.InR && len(st.Sync.Members) <= 2 {
			then = "this cluster is strict and it is the only copy, so it is kept until it is replaced (remove it and join it again)."
		}
		p.View.Notes = append(p.View.Notes, fmt.Sprintf("%s's copy has damaged data pages (%s): it is never made the main node; %s",
			m.Name, m.Report.PagesDamaged, then))
	}
	// D59: a remove or leave in progress. The cluster state refuses to add
	// such a member to the sync set; the controller does not try.
	for _, id := range sortedIDs(in.Members) {
		if m := in.Members[id]; m.Removing {
			p.View.Notes = append(p.View.Notes, fmt.Sprintf("%s is being removed from the cluster, so it is not added to the sync set. "+
				"If that removal stopped part way, run `%s remove %s` again to finish it.", m.Name, brand.Command, m.Name))
		}
	}

	// What R and S are.
	rIDs := slices.DeleteFunc(slices.Clone(st.Sync.Members), func(id string) bool { return id == st.PrimaryID })
	byName := map[string]string{}
	for id, m := range st.Members {
		byName[strings.ToLower(m.Name)] = id
	}
	var sIDs []string
	stray := false
	for _, n := range pr.SyncNames {
		id, ok := byName[strings.ToLower(n)]
		if !ok || !slices.Contains(rIDs, id) {
			stray = true
			continue
		}
		sIDs = append(sIDs, id)
	}
	// 2. S must be a subset of R (rule 3). Taking a name out of S that is
	// not in R is always allowed: R is what promotion trusts.
	if stray {
		return p.with(ActApplyS, "", "the main node's sync list names a node outside the sync set; resetting it from the sync set")
	}
	streaming := func(ids []string) (n int) {
		for _, id := range ids {
			if in.Members[id].Streaming {
				n++
			}
		}
		return n
	}
	k := max(st.Sync.K, 1)

	// 3. Eligible but no copy is waited for: writes are not copied, so say so.
	if st.StandbyEligible && len(sIDs) == 0 {
		return p.with(ActRecordIneligible, "", "the main node waits for no copy; recording that it runs on one copy")
	}

	// 4. Rule 8: members gone too long, or whose slot lost WAL.
	for _, id := range sortedIDs(in.Members) {
		m := in.Members[id]
		if !m.Active {
			continue
		}
		away := m.NotStreamingFor >= s.EffectiveGoneAfter()
		if !m.SlotLost && !away {
			continue
		}
		why := fmt.Sprintf("%s has been away longer than %s", m.Name, s.EffectiveGoneAfter())
		if m.SlotLost {
			why = fmt.Sprintf("%s's replication slot lost the WAL it needed", m.Name)
		}
		if !m.InR {
			return p.with(ActMarkReclone, id, why+"; it will copy the data again when it returns")
		}
		rest := slices.DeleteFunc(slices.Clone(rIDs), func(x string) bool { return x == id })
		switch {
		case !st.StandbyEligible:
			return p.with(ActPrune, id, why+"; taking it out of the sync set first")
		case streaming(rest) >= k:
			return p.with(ActSyncRemove, id, why+"; taking it out of the sync set first")
		}
		// It is the last copy: rule 9 below decides (degrade or wait).
	}

	// Running on one copy (standby_eligible=false is recorded) while the
	// main node still waits for copies that are not connected: a degrade,
	// or a make-eligible, that stopped half way. Finish it: stop waiting.
	// Every round decides this again until S is cleared, so one failed
	// attempt (the main node briefly without its lease) is just retried.
	if !st.StandbyEligible && len(sIDs) > 0 && streaming(sIDs) < min(k, len(sIDs)) &&
		s.SingleDataNodeMode != cluster.ModeStrict {
		p.View.WritesWaiting = true
		p.View.WritesWaitingFor = in.NoSyncFor
		return p.with(ActDegrade, "", "running on one copy, but the main node still waits for a copy that is not connected; it stops waiting")
	}
	if !st.StandbyEligible {
		return decideOneCopy(in, p, rIDs, primary)
	}

	// 5. Eligible: add caught-up streaming standbys (before any degrade).
	for _, id := range candidateOrder(in) {
		m := in.Members[id]
		if m.Active && !m.Removing && !m.InR && m.Streaming && m.LagBytes >= 0 && m.LagBytes <= MaxAddLag && !m.Report.Damaged() {
			return p.with(ActSyncAdd, id, fmt.Sprintf("%s is streaming and caught up; adding it to the sync set", m.Name))
		}
	}
	// 6. S is missing members of R (for example right after a promotion).
	for _, id := range rIDs {
		if !slices.Contains(sIDs, id) {
			return p.with(ActApplyS, "", "the main node's sync list is missing members of the sync set")
		}
	}
	// 7. Lost members of R go, if enough others stream.
	for _, id := range rIDs {
		m := in.Members[id]
		rest := slices.DeleteFunc(slices.Clone(rIDs), func(x string) bool { return x == id })
		if !m.Streaming && m.NotStreamingFor >= SyncDropAfter(s) && streaming(rest) >= k {
			return p.with(ActSyncRemove, id, fmt.Sprintf("%s stopped streaming %s ago; taking it out of the sync set", m.Name, m.NotStreamingFor.Round(time.Second)))
		}
	}
	// 8. Rule 9: not enough copies are connected.
	if streaming(sIDs) < min(k, len(sIDs)) {
		p.View.WritesWaiting = true
		p.View.WritesWaitingFor = in.NoSyncFor
		var names []string
		for _, id := range sIDs {
			names = append(names, name(id))
		}
		sort.Strings(names)
		if s.SingleDataNodeMode == cluster.ModeStrict {
			p.View.Notes = append(p.View.Notes, fmt.Sprintf("Writes are waiting: every write must reach a copy on %s, which is not connected. "+
				"This cluster is set to strict, so writes wait until it is back.", strings.Join(names, " or ")))
			return p
		}
		if in.NoSyncFor >= s.DegradeAfter {
			return p.with(ActDegrade, "", fmt.Sprintf("no copy connected for %s; running on one copy", in.NoSyncFor.Round(time.Second)))
		}
		p.View.Notes = append(p.View.Notes, fmt.Sprintf("Writes are waiting: every write must reach a copy on %s, which is not connected. "+
			"If it is not back within %s, the main node carries on alone.", strings.Join(names, " or "), (s.DegradeAfter-in.NoSyncFor).Round(time.Second)))
	}
	return p
}

// LeaseLimit is how long the controller waits without seeing the lease
// renewed before it treats the primary as gone: the longer of the lease
// the primary was given at its promotion (it keeps that one for its whole
// epoch) and the current setting, plus the margin.
func LeaseLimit(st cluster.State, margin time.Duration) time.Duration {
	return max(st.Lease.Duration, st.Settings.LeaseDuration) + margin
}

// decideOneCopy: standby_eligible is false (one data node carries the
// writes). A copy becomes eligible again once it streams and has caught
// up; it then (re)enters R in rule 3's order.
func decideOneCopy(in Input, p Plan, rIDs []string, primary string) Plan {
	st := in.State
	since := ""
	if !st.StandbyEligibleSince.IsZero() {
		since = " since " + st.StandbyEligibleSince.Local().Format("15:04:05 Mon 2 Jan")
	}
	for _, id := range candidateOrder(in) {
		m := in.Members[id]
		if m.Active && !m.Removing && m.Streaming && m.LagBytes >= 0 && m.LagBytes <= MaxAddLag && !m.Report.Damaged() {
			return p.with(ActMakeEligible, id, fmt.Sprintf("%s is streaming and caught up; making it a full copy again", m.Name))
		}
	}
	if len(st.DataMembers()) > 1 {
		p.View.Notes = append(p.View.Notes, fmt.Sprintf("Running on one copy%s: writes are saved on %s only. "+
			"Another data node becomes a full copy again once it is connected and caught up.", since, primary))
	}
	return p
}

// decideFailover: the primary's lease ran out.
func decideFailover(in Input, p Plan, primary string) Plan {
	st := in.State
	block := func(why string) Plan {
		p.View.FailoverBlocked = why
		return p
	}
	data, witnesses := 0, 0
	for _, m := range st.Members {
		if m.Role == cluster.RoleData {
			data++
		} else {
			witnesses++
		}
	}
	gone := fmt.Sprintf("%s (the main node) has not been heard from for %s", primary, in.LeaseAge.Round(100*time.Millisecond))
	switch {
	case in.TimingProblem != "":
		return block(gone + ", but this cluster's timing settings are unsafe, so no other node is made the main node: " + in.TimingProblem)
	case data == 2 && witnesses == 0:
		// D17: two voters cannot tell a dead node from a cut cable. Hook for
		// the forced manual promote (M6).
		return block(gone + ". This cluster has two data nodes and no witness, so it cannot pick a new main node by itself " +
			"(it cannot tell a dead machine from a broken network). Start " + primary + " again; adding a witness makes this automatic.")
	case data < 2:
		return block(gone + ", and there is no other data node to take over.")
	case !st.StandbyEligible:
		since := ""
		if !st.StandbyEligibleSince.IsZero() {
			since = " since " + st.StandbyEligibleSince.Local().Format("15:04:05 Mon 2 Jan")
		}
		return block(gone + ". The other copies may be missing writes saved" + since + " (the cluster was running on one copy), " +
			"so none is made the main node. Start " + primary + " again.")
	}
	cands := Candidates(st)
	if len(cands) == 0 {
		return block(gone + ", and no other node is in the sync set, so none is known to hold every write.")
	}
	need := CutsNeeded(st, cands)
	ok := 0
	var bad []string
	for _, id := range cands {
		m, found := in.Members[id]
		if found && m.Report.IsHealthyStandby() {
			ok++
		}
		if found && m.Report.Damaged() {
			bad = append(bad, fmt.Sprintf("%s (%s)", m.Name, m.Report.PagesDamaged))
		}
	}
	if len(bad) == len(cands) {
		// D55: no candidate can be promoted without damaged data; do not
		// even cut the streams. Unavailable beats unreadable.
		why := damagedRefusal(bad)
		return block(gone + ". " + strings.ToUpper(why[:1]) + why[1:] + ".")
	}
	if ok < need {
		var names []string
		for _, id := range cands {
			names = append(names, st.Members[id].Name)
		}
		return block(fmt.Sprintf("%s. Only %d of the copies in the sync set (%s) can be reached; %d are needed to be sure no write is lost (safety rule 2).",
			gone, ok, strings.Join(names, ", "), need))
	}
	p.ExpectedLeaseSeq = st.Lease.Seq
	return p.with(ActFailover, "", gone)
}

// Candidates are the members that may be promoted by a failover: R minus
// the last node that served as primary (state.LastServed), active data
// members only. A named primary that never renewed its lease never served,
// so it stays a candidate (and must be cut too): it may hold writes the
// others lack.
func Candidates(st cluster.State) []string {
	var out []string
	for _, id := range st.Sync.Members {
		m, ok := st.Members[id]
		if !ok || m.Role != cluster.RoleData || m.Status != cluster.StatusActive || id == st.LastServed {
			continue
		}
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// CutsNeeded is rule 2's count: with every acknowledged write on at least
// k members of the candidates, any |cands|-k+1 of them include one that
// has it.
//
// k here is 1, whatever Raft's sync count K says (D42). Postgres waits
// for ANY min(K, |S|) of S, and S can be smaller than R (a member enters R
// before S, rule 3; `dev set-sync` raises K before S; a new primary has
// not set S yet). The controller cannot know which smaller count an
// acknowledged write had, so it counts the only one it can be sure of:
// one copy, so every candidate must be cut. With K = 1 (every cluster
// outside `dev`) this is the count it always was.
func CutsNeeded(st cluster.State, cands []string) int {
	return len(cands)
}

// candidateOrder: members of R first, then voters, then by id.
func candidateOrder(in Input) []string {
	ids := sortedIDs(in.Members)
	sort.SliceStable(ids, func(i, j int) bool {
		a, b := in.Members[ids[i]], in.Members[ids[j]]
		if a.InR != b.InR {
			return a.InR
		}
		if a.Voter != b.Voter {
			return a.Voter
		}
		return false
	})
	return ids
}

func sortedIDs(m map[string]MemberObs) []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}
