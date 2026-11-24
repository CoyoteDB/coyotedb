package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"coyotedb.com/coyote/internal/cluster"
	"coyotedb.com/coyote/internal/pg"
)

// Sync set handling (safety rule 3). R (cluster state) lists the members
// that may be promoted; Postgres' synchronous_standby_names S is always a
// subset of R minus the primary itself, as "ANY k (...)":
//
//	add a member:    R first (Raft entry, epoch CAS), then S
//	remove a member: S first, reload, confirm (sync_state), barrier, wait
//	                 until k of the rest hold every write, then R
//
// The controller (internal/controller) decides every change and asks the
// primary's agent to set S (RPC set_s). The primary sets S by itself only
// right before it promotes (from R). Changes run under lockPG.

// syncNamesFor renders S for primary self from R: names and count.
func syncNamesFor(st cluster.State, self string) ([]string, int) {
	var names []string
	for _, id := range st.Sync.Members {
		if id == self {
			continue
		}
		if m, ok := st.Members[id]; ok {
			names = append(names, m.Name)
		}
	}
	slices.Sort(names)
	if len(names) == 0 {
		return nil, 0
	}
	k := max(st.Sync.K, 1)
	return names, min(k, len(names))
}

// primaryDuties keeps slots and S in step with the cluster state. It runs
// on state changes and at most every 2s otherwise.
func (a *Agent) primaryDuties(ctx context.Context, st cluster.State) error {
	if st.AppliedIndex == a.lastDutyIdx && time.Since(a.lastDuties) < 2*time.Second {
		return nil
	}
	f := a.st.get()
	if st.PrimaryID == a.cn.ID() && st.Epoch > f.LastPrimaryEpoch {
		// Still primary, continuously, in a newer epoch.
		if err := a.st.update(func(f *agentFile) { f.LastPrimaryEpoch = st.Epoch }); err != nil {
			return err
		}
	}
	unlock, err := a.lockPG(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if st.PGSecrets == nil {
		s, err := a.node.EnsureSecrets()
		if err != nil {
			return err
		}
		if err := a.publishSecrets(ctx, s); err != nil {
			return err
		}
	}
	if err := a.reconcileSlots(ctx, st); err != nil {
		return fmt.Errorf("replication slots: %w", err)
	}
	if err := a.refreshPGTLS(ctx, st); err != nil {
		return fmt.Errorf("client TLS certificate: %w", err)
	}
	if err := a.syncAppSecret(st); err != nil {
		return fmt.Errorf("application password: %w", err)
	}
	if err := a.ensureAppRole(ctx, st); err != nil {
		return err
	}
	a.computeServed(ctx, st.Epoch)
	a.lastDuties, a.lastDutyIdx = time.Now(), st.AppliedIndex
	return nil
}

// reconcileSlots: one physical slot per other active data member; slots
// of nodes that are no longer members are dropped once inactive. A member
// marked "needs re-clone" (rule 8) has its slot dropped once, to free the
// WAL it held; the slot its new copy creates is kept.
func (a *Agent) reconcileSlots(ctx context.Context, st cluster.State) error {
	c, err := a.node.Connect(ctx)
	if err != nil {
		return err
	}
	defer c.Close(context.Background())
	want := map[string]bool{}
	member := map[string]bool{}
	recl := map[string]string{}
	for _, m := range st.DataMembers() {
		if m.ID == a.cn.ID() {
			continue
		}
		member[pg.SlotName(m.Name)] = true
		if m.Status == cluster.StatusActive {
			want[pg.SlotName(m.Name)] = true
			delete(a.recloneSlotDropped, m.ID)
		} else if m.Status == cluster.StatusNeedsReclone && !a.recloneSlotDropped[m.ID] {
			recl[pg.SlotName(m.Name)] = m.ID
		}
	}
	slots, err := pg.ListSlots(ctx, c)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, s := range slots {
		have[s.Name] = true
		if id, ok := recl[s.Name]; ok && !s.Active {
			if err := pg.DropSlot(ctx, c, s.Name); err != nil {
				return err
			}
			a.recloneSlotDropped[id] = true
			a.log.Warn("dropped the slot of a member that must copy the data again (rule 8)", "slot", s.Name)
			continue
		}
		if strings.HasPrefix(s.Name, "coyote_") && !member[s.Name] && !s.Active {
			if err := pg.DropSlot(ctx, c, s.Name); err != nil {
				return err
			}
			a.log.Info("dropped the slot of a former member", "slot", s.Name)
		}
	}
	for name := range want {
		if !have[name] {
			if _, err := pg.CreateSlot(ctx, c, name); err != nil {
				return err
			}
			a.log.Info("created replication slot", "slot", name)
		}
	}
	return nil
}

// applyS sets S if it differs from what is persisted.
func (a *Agent) applyS(ctx context.Context, names []string, k int) error {
	rs, err := a.node.ReadReplState()
	if err != nil {
		return err
	}
	cur := slices.Clone(rs.SyncNames)
	slices.Sort(cur)
	if slices.Equal(cur, names) && rs.SyncCount == k {
		return nil
	}
	a.log.Info("sync list", "names", names, "count", k)
	return a.node.SetSync(ctx, k, names, 30*time.Second)
}

var errNotPrimary = errors.New("this node is not the main node")

// primaryState checks that this node is the running primary of the
// current epoch and returns the state.
func (a *Agent) primaryState() (cluster.State, error) {
	st := a.cn.State()
	if _, role := a.currentPG(); st.PrimaryID != a.cn.ID() || role != "primary" {
		return st, errNotPrimary
	}
	return st, nil
}

func (a *Agent) setR(ctx context.Context, epoch uint64, members []string, k int) error {
	ms := slices.Clone(members)
	slices.Sort(ms)
	if len(ms) == 0 {
		k = 0
	} else {
		k = min(max(k, 1), len(ms))
	}
	_, err := a.cn.Apply(ctx, &cluster.SetSyncSet{ExpectedEpoch: epoch, Members: ms, K: k})
	return err
}

// setS is the controller's RPC: set S on this primary (confirmed by
// sync_state), optionally run the barrier, and return the flush LSN.
func (a *Agent) setS(ctx context.Context, epoch uint64, names []string, k int, barrier bool) (pg.LSN, error) {
	unlock, err := a.lockPG(ctx)
	if err != nil {
		return 0, err
	}
	defer unlock()
	st, err := a.primaryState()
	if err != nil {
		return 0, err
	}
	switch {
	case st.Epoch != epoch:
		return 0, fmt.Errorf("%w for term %d (the cluster is at term %d)", errNotPrimary, epoch, st.Epoch)
	case !a.holdsLease(epoch):
		// Usually passing: it is still starting, or a renewal is late. The
		// controller asks again on its next round.
		return 0, fmt.Errorf("%w right now: it does not hold its main-node lease for term %d at this moment", errNotPrimary, epoch)
	}
	for _, n := range names {
		m, ok := findMember(st, n)
		if !ok || m.Role != cluster.RoleData || !slices.Contains(st.Sync.Members, m.ID) || m.ID == a.cn.ID() {
			return 0, fmt.Errorf("%s is not a copy in the sync set (rule 3: the sync list stays inside it)", n)
		}
	}
	sorted := slices.Clone(names)
	slices.Sort(sorted)
	if err := a.applyS(ctx, sorted, k); err != nil {
		return 0, err
	}
	if barrier {
		if err := a.node.SyncBarrier(ctx); err != nil {
			return 0, fmt.Errorf("sync barrier: %w", err)
		}
	}
	return a.node.FlushLSN(ctx)
}

// waitCopies waits until at least k of the named standbys have flushed
// lsn (this primary's pg_stat_replication).
func (a *Agent) waitCopies(ctx context.Context, names []string, k int, lsn pg.LSN, timeout time.Duration) error {
	if k == 0 {
		return nil
	}
	c, err := a.node.Connect(ctx)
	if err != nil {
		return err
	}
	defer c.Close(context.Background())
	deadline := time.Now().Add(timeout)
	for {
		rs, err := pg.ReplicationStatus(ctx, c)
		if err != nil {
			return err
		}
		n := 0
		for _, r := range rs.Standbys {
			if slices.ContainsFunc(names, func(x string) bool { return strings.EqualFold(x, r.Name) }) && r.FlushLSN != nil && *r.FlushLSN >= lsn {
				n++
			}
		}
		if n >= k {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the other copies (%s) did not catch up within %s", strings.Join(names, ", "), timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// syncRemove takes a member out of S (confirmed, barrier), waits until k
// of the remaining copies hold every write, then takes it out of R.
func (a *Agent) syncRemove(ctx context.Context, id string) error {
	unlock, err := a.lockPG(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	st, err := a.primaryState()
	if err != nil {
		return err
	}
	if !slices.Contains(st.Sync.Members, id) {
		return nil
	}
	next := slices.DeleteFunc(slices.Clone(st.Sync.Members), func(s string) bool { return s == id })
	tmp := st.Clone()
	tmp.Sync.Members = next
	names, k := syncNamesFor(tmp, a.cn.ID())
	if err := a.applyS(ctx, names, k); err != nil {
		return err
	}
	if err := a.node.SyncBarrier(ctx); err != nil {
		return err
	}
	l, err := a.node.FlushLSN(ctx)
	if err != nil {
		return err
	}
	if err := a.waitCopies(ctx, names, k, l, 30*time.Second); err != nil {
		return err
	}
	if err := a.setR(ctx, st.Epoch, next, st.Sync.K); err != nil {
		return err
	}
	a.log.Info("removed from the sync set", "member", id)
	return nil
}

// setSync is `dev set-sync`: replace R (by node names) and k, in rule-3
// order: removals S-then-R, additions R-then-S.
func (a *Agent) setSync(ctx context.Context, names []string, count int) error {
	unlock, err := a.lockPG(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	st, err := a.primaryState()
	if err != nil {
		return err
	}
	var want []string
	for _, n := range names {
		m, ok := findMember(st, n)
		if !ok {
			return fmt.Errorf("no node called %q in this cluster", n)
		}
		if m.Role != cluster.RoleData {
			return fmt.Errorf("%s is a witness; only data nodes can hold a synchronous copy", m.Name)
		}
		if !slices.Contains(want, m.ID) {
			want = append(want, m.ID)
		}
	}
	if count < 0 || (len(want) > 0 && count > len(want)) {
		return fmt.Errorf("--count must be between 1 and %d", len(want))
	}
	keep := slices.DeleteFunc(slices.Clone(st.Sync.Members), func(id string) bool { return !slices.Contains(want, id) })
	if len(keep) < len(st.Sync.Members) {
		tmp := st.Clone()
		tmp.Sync.Members, tmp.Sync.K = keep, count
		n, k := syncNamesFor(tmp, a.cn.ID())
		if err := a.applyS(ctx, n, k); err != nil {
			return err
		}
		if err := a.node.SyncBarrier(ctx); err != nil {
			return err
		}
		l, err := a.node.FlushLSN(ctx)
		if err != nil {
			return err
		}
		if err := a.waitCopies(ctx, n, k, l, 30*time.Second); err != nil {
			return err
		}
		if err := a.setR(ctx, st.Epoch, keep, count); err != nil {
			return err
		}
	}
	st = a.cn.State()
	if err := a.setR(ctx, st.Epoch, want, count); err != nil {
		return err
	}
	st = a.cn.State()
	n, k := syncNamesFor(st, a.cn.ID())
	return a.applyS(ctx, n, k)
}

// findMember finds a member by name or id.
func findMember(st cluster.State, s string) (cluster.Member, bool) {
	if m, ok := st.Members[s]; ok {
		return m, true
	}
	for _, m := range st.Members {
		if m.Name == s {
			return m, true
		}
	}
	return cluster.Member{}, false
}
