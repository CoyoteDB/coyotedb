package cluster

import (
	"context"
	"sync"

	"github.com/hashicorp/raft"
)

// reconcile (leader only) moves the Raft configuration towards what the
// membership and the suffrage policy (D17) want, one change at a time, each
// change chosen by NextStep so a working quorum is kept. Then it records
// actual suffrage in the state. It is idempotent and runs on every state
// change, leader change and every ReconcileInterval, so a crash between a
// state change and a config change is repaired.
func (n *Node) reconcile() {
	n.memberMu.Lock()
	defer n.memberMu.Unlock()
	if !n.IsLeader() {
		return
	}
	// A newly elected leader (for instance one just restarted) applies the
	// log it inherited only after it commits an entry of its own term.
	// Planning from the state before that would see members missing and
	// remove live voters from the configuration: wait until the state is
	// complete, once per term.
	if term := n.raft.CurrentTerm(); n.caughtUpTerm != term {
		ctx, cancel := context.WithTimeout(context.Background(), n.timing.ApplyTimeout)
		err := n.Barrier(ctx)
		cancel()
		if err != nil {
			n.log.Debug("reconcile: state not caught up yet", "err", err)
			return
		}
		n.caughtUpTerm = term
	}
	st := n.fsm.State()
	if !st.Bootstrapped() {
		return
	}
	desired := DesiredSuffrage(st.SortedMembers(), st.PrimaryID)

	const maxSteps = 8
	var probes map[string]probe
	for range maxSteps {
		cur, err := n.RaftConfiguration()
		if err != nil {
			n.log.Warn("reconcile: read raft configuration", "err", err)
			return
		}
		if probes == nil {
			probes = n.probeAll(cur, desired)
		}
		leaderApplied := n.fsm.appliedIndex()
		healthy := func(id string) bool { return probes[id].ok }
		ready := func(id string) bool {
			p := probes[id]
			return p.ok && p.applied+n.timing.PromoteMaxLag >= leaderApplied
		}
		step, ok := NextStep(cur, desired, n.id.NodeID, healthy, ready)
		if !ok {
			break
		}
		id := raft.ServerID(step.ID)
		addr := raft.ServerAddress(step.ID)
		var f raft.Future
		switch step.Kind {
		case StepAddNonvoter:
			f = n.raft.AddNonvoter(id, addr, 0, n.timing.ApplyTimeout)
		case StepPromote:
			f = n.raft.AddVoter(id, addr, 0, n.timing.ApplyTimeout)
		case StepRemove:
			f = n.raft.RemoveServer(id, 0, n.timing.ApplyTimeout)
		case StepDemote:
			f = n.raft.DemoteVoter(id, 0, n.timing.ApplyTimeout)
		case StepTransferLeadership:
			f = n.raft.LeadershipTransferToServer(id, addr)
		}
		n.log.Info("raft configuration change", "step", step.Kind.String(), "member", step.ID)
		if err := f.Error(); err != nil {
			n.log.Warn("raft configuration change failed", "step", step.Kind.String(), "member", step.ID, "err", err)
			return
		}
		if step.Kind == StepTransferLeadership {
			return
		}
		if step.Kind == StepAddNonvoter {
			// A new server was not probed yet.
			probes = nil
		}
	}
	n.recordSuffrage(st)
}

func (n *Node) recordSuffrage(st State) {
	cur, err := n.RaftConfiguration()
	if err != nil {
		return
	}
	for _, s := range cur {
		m, ok := st.Members[s.ID]
		if !ok || m.Suffrage == s.Suffrage {
			continue
		}
		data, err := EncodeCommand(&SetSuffrage{ID: s.ID, Suffrage: s.Suffrage})
		if err != nil {
			return
		}
		if _, err := n.applyLocal(data); err != nil {
			n.log.Debug("record suffrage", "member", s.ID, "err", err)
			return
		}
	}
}

type probe struct {
	ok      bool
	applied uint64
}

// probeAll asks every other server in the configuration or membership for
// its status, in parallel.
func (n *Node) probeAll(cur []ConfigServer, desired map[string]Suffrage) map[string]probe {
	ids := map[string]bool{}
	for _, s := range cur {
		ids[s.ID] = true
	}
	for id := range desired {
		ids[id] = true
	}
	delete(ids, n.id.NodeID)
	out := make(map[string]probe, len(ids))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(n.ctx, n.timing.DialTimeout)
			defer cancel()
			st, err := n.PeerStatus(ctx, id)
			mu.Lock()
			out[id] = probe{ok: err == nil, applied: st.AppliedIndex}
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}
