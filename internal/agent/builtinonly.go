package agent

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"time"

	"coyotedb.com/coyote/internal/brand"
	"coyotedb.com/coyote/internal/cluster"
	"coyotedb.com/coyote/internal/pg"
)

// Builtin-only and promotion-time checks for clusters that mix kinds of
// machines (D48, safety rule 1's exception). See pg/builtinonly.go for the
// database side.

const (
	// preparedFor is how long a main node keeps the builtin-only guard
	// after the leader prepared it for a mixed join that did not happen
	// (then a cluster that does not mix machines gets today's freedom back).
	preparedFor = 10 * time.Minute
	// ddlAuditGap is the shortest time between two audits started because
	// the DDL log moved (a busy schema would otherwise audit every tick);
	// the promotion-time check closes the gap.
	ddlAuditGap = time.Minute
	// promoteCheckTimeout bounds the promotion-time check (inside the
	// controller's cut, which allows 25 s per copy and half the 90 s cut
	// hold overall).
	promoteCheckTimeout = 20 * time.Second
	// handoverEvery is how often a drifted main node retries handing over.
	handoverEvery = 30 * time.Second
)

// builtinWanted reports whether this cluster must be builtin-only: it
// mixes machines, or the leader just prepared it for a mixed join.
func (a *Agent) builtinWanted(st cluster.State) bool {
	if st.Settings.MixedOS {
		return true
	}
	a.mixed.mu.Lock()
	defer a.mixed.mu.Unlock()
	return !a.mixed.preparedAt.IsZero() && time.Since(a.mixed.preparedAt) < preparedFor
}

// primaryBuiltin keeps the main node's databases builtin-only while it is
// wanted (installing the guard into new databases within one guard tick,
// template0 again after every start), and removes the guard when it is
// not.
func (a *Agent) primaryBuiltin(ctx context.Context, st cluster.State, pid int, wanted bool) {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	m := &a.mixed
	if !wanted {
		c, err := a.node.ConnectDB(cctx, "postgres")
		if err != nil {
			return
		}
		on, err := pg.BuiltinOnlyInstalled(cctx, c)
		c.Close(context.WithoutCancel(cctx))
		if err != nil || !on {
			return
		}
		if err := pg.UnprepareBuiltinOnly(cctx, a.node.ConnectDB); err != nil {
			a.log.Warn("mixed systems: could not remove the built-in-sorting guard (retried)", "err", err)
			return
		}
		a.log.Info("mixed systems: removed the built-in-sorting guard (this cluster does not mix kinds of machines)")
		return
	}
	m.mu.Lock()
	full := m.builtinPID != pid
	q := m.builtin
	m.mu.Unlock()
	if !full && q != nil && !needsRepair(*q) {
		return
	}
	res, err := pg.PrepareBuiltinOnly(cctx, a.node.ConnectDB, full)
	if err != nil {
		a.log.Warn("mixed systems: could not finish the built-in-sorting guard (retried)", "err", err)
		return
	}
	if res.Dropped > 0 || len(res.Kept) > 0 {
		a.log.Info("mixed systems: removed sort rules that are not built in", "dropped", res.Dropped, "kept_in_use", len(res.Kept))
	}
	if full {
		m.mu.Lock()
		m.builtinPID = pid
		m.mu.Unlock()
	}
}

// needsRepair: the quick check found a database without the guard or
// with collations the main node can remove.
func needsRepair(q pg.BuiltinOnlyState) bool {
	for _, f := range q.Findings {
		if f.Check == pg.CheckBuiltinOnlyName && strings.HasPrefix(f.Object, "database ") && !strings.Contains(f.Problem, "sorts text with") {
			return true
		}
	}
	return false
}

// prepareResp is the "mixed_prepare" RPC answer. Blocked is a plain-words
// reason the cluster cannot mix machines (definitive, not retried).
type prepareResp struct {
	Blocked string `json:"blocked,omitempty"`
}

// mixedPrepare (main node, the leader's mixed join check) makes the
// cluster builtin-only before a machine of another kind is admitted.
// Nothing is changed for a cluster that cannot be: a database that sorts
// with ICU or the operating system, or a collation in use, refuses the
// join with the reason.
func (a *Agent) mixedPrepare(ctx context.Context) (prepareResp, error) {
	if _, role := a.currentPG(); role != "primary" {
		return prepareResp{}, errors.New("this node is not the running main node")
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	q, err := pg.CheckBuiltinOnly(cctx, a.node.ConnectDB)
	if err != nil {
		return prepareResp{}, err
	}
	var why []string
	for _, f := range q.Findings {
		if strings.Contains(f.Problem, "sorts text with") {
			why = append(why, f.Object+": "+f.Problem+" "+f.Fix)
		}
	}
	if len(why) > 0 {
		return prepareResp{Blocked: strings.Join(why, " ")}, nil
	}
	a.mixed.mu.Lock()
	a.mixed.preparedAt = time.Now()
	a.mixed.mu.Unlock()
	srv, _ := a.currentPG()
	res, err := pg.PrepareBuiltinOnly(cctx, a.node.ConnectDB, true)
	if err != nil {
		return prepareResp{}, err
	}
	if srv != nil {
		a.mixed.mu.Lock()
		a.mixed.builtinPID = srv.PID()
		a.mixed.mu.Unlock()
	}
	if len(res.Kept) > 0 {
		var objs []string
		for _, f := range res.Kept {
			objs = append(objs, f.Database+": "+f.Object)
		}
		return prepareResp{Blocked: "Sort rules that are not built in are in use (" + strings.Join(objs, ", ") +
			"), so text would sort differently on another kind of machine. Change what uses them to COLLATE pg_c_utf8 (`" + brand.Command + " status` lists the objects), then join again."}, nil
	}
	// D49: structures that recompute float or geometric maths refuse the
	// join, listed by name.
	after, err := pg.CheckBuiltinOnly(cctx, a.node.ConnectDB)
	if err != nil {
		return prepareResp{}, err
	}
	var objs []string
	for _, f := range after.Findings {
		if f.Severity == pg.SeverityBlock && !strings.HasPrefix(f.Object, "database ") {
			objs = append(objs, f.Database+": "+f.Object)
		}
	}
	if len(objs) > 0 {
		return prepareResp{Blocked: "These recompute decimal (floating-point) or geometric maths, which can differ in the last digit between kinds of machines: " +
			strings.Join(objs, ", ") + ". Drop them or build them on an exact type (numeric, integer); plain indexes on float columns are fine. Then join again."}, nil
	}
	a.log.Info("mixed systems: this cluster is now built-in-sorting only (prepared for a machine of another kind)")
	return prepareResp{}, nil
}

// quickBuiltin runs the quick builtin-only check and keeps it; it reports
// whether something changed since the last audit (the DDL log moved or
// databases came or went), so the audit should run now.
func (a *Agent) quickBuiltin(ctx context.Context) (changed bool) {
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	q, err := pg.CheckBuiltinOnly(cctx, a.node.ConnectDB)
	cancel()
	m := &a.mixed
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		a.log.Info("mixed systems: quick check failed; will retry", "err", err)
		return false
	}
	m.builtin = &q
	return m.auditDDL == nil || !m.auditDDL.SameDDL(q)
}

// promoteBlocked is the promotion-time check (D48, safety rule 1): a copy
// of another kind than the main node it follows checks, after its stream
// was cut and everything it received is applied, that the data can move
// to it: its last recorded guard result in the cluster state does not
// block (or a fresh audit clears it), its own quick builtin-only check
// passes, and the audit covers every change made so far (audited now if
// the DDL log moved since). "" means it may be promoted. Same-kind copies
// are never affected.
func (a *Agent) promoteBlocked(ctx context.Context, st cluster.State, lsn pg.LSN) string {
	if !a.primaryUnlike(st) {
		return ""
	}
	cctx, cancel := context.WithTimeout(ctx, promoteCheckTimeout)
	defer cancel()
	from := a.primaryName(st)
	refuse := func(why string) string {
		return fmt.Sprintf("this copy is a different kind of machine than %s and may not take over: %s", from, why)
	}
	if !st.Settings.MixedOS {
		return refuse("mixing kinds of machines is off for this cluster")
	}
	// This copy's own last recorded result (in the cluster state): damage
	// found on its own disk always refuses. A blocking audit result makes
	// it audit afresh below; its own catalog is the same as the main
	// node's up to the cut, so a fresh clean audit is the truth (another
	// member's record may be stale: a node that died before re-checking).
	ownBlocked := false
	if g := st.Members[a.cn.ID()].Guard; g != nil {
		if g.VerifyDamaged {
			return refuse("its own integrity check found damage (" + strings.Join(g.Findings, "; ") + ")")
		}
		ownBlocked = g.AuditBlocking
	}
	if err := a.node.WaitReplayed(cctx, lsn, promoteCheckTimeout/2); err != nil {
		return refuse(fmt.Sprintf("it could not apply everything it received in time to check it (%v)", err))
	}
	q, err := pg.CheckBuiltinOnly(cctx, a.node.ConnectDB)
	if err != nil {
		return refuse(fmt.Sprintf("the check could not run (%v)", err))
	}
	if q.Blocking() {
		return refuse(findingsText(q.Findings))
	}
	a.mixed.mu.Lock()
	fresh := !ownBlocked && a.mixed.audit != nil && a.mixed.auditDDL != nil && a.mixed.auditDDL.SameDDL(q) && !a.mixed.audit.Blocking()
	a.mixed.mu.Unlock()
	if fresh {
		return ""
	}
	r, err := pg.AuditCluster(cctx, a.node.ConnectDB, pg.AuditOptions{NodeOS: runtime.GOOS})
	if err != nil {
		return refuse(fmt.Sprintf("the database check could not finish in time (%v)", err))
	}
	a.mixed.mu.Lock()
	a.mixed.audit, a.mixed.auditDDL, a.mixed.builtin = &r, &q, &q
	a.mixed.mu.Unlock()
	if r.Blocking() {
		return refuse(findingsText(r.Findings))
	}
	return ""
}

func findingsText(fs []pg.Finding) string {
	var out []string
	for _, f := range fs {
		if f.Severity == pg.SeverityBlock {
			out = append(out, f.String())
		}
		if len(out) == 3 {
			break
		}
	}
	return strings.Join(out, "; ")
}

// driftedMain handles a main node whose own measurement drifted (D48,
// rule 1): it never keeps serving writes silently. Client writes are
// refused at once (they would be made by programs that now behave
// differently from the copies'), and it hands over by a planned switch to
// a healthy copy when there is one. Once the drift is gone the refusal is
// lifted. Runs in the guard loop on the main node only.
func (a *Agent) driftedMain(ctx context.Context, st cluster.State) {
	m := &a.mixed
	drift := m.getDrift()
	if !st.Settings.MixedOS {
		return // the setting belongs to this program only in mixed clusters
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	c, err := a.node.Connect(cctx)
	if err != nil {
		cancel()
		return
	}
	changed, err := pg.SetWritesRefused(cctx, c, drift != "")
	c.Close(context.WithoutCancel(cctx))
	cancel()
	if err != nil {
		a.log.Warn("mixed systems: could not change whether writes are refused (retried)", "err", err)
	} else if changed && drift != "" {
		a.log.Error("mixed systems: this main node changed since it joined; it refuses writes until it hands over or the change is undone", "why", drift)
		a.say("This main node's database programs changed since it joined, so it no longer matches the other data nodes. It refuses writes now (reads still work) and hands over to a healthy copy if there is one.")
	} else if changed {
		a.log.Info("mixed systems: the change on this main node is gone; writes are accepted again")
	}
	if drift == "" {
		return
	}
	m.mu.Lock()
	due := time.Since(m.handoverAt) >= handoverEvery
	if due {
		m.handoverAt = time.Now()
	}
	m.mu.Unlock()
	if !due {
		return
	}
	target, ok := handoverTarget(st, a.cn.ID())
	if !ok {
		a.log.Warn("mixed systems: no healthy copy to hand over to; writes stay refused", "why", drift)
		return
	}
	sctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	a.log.Warn("mixed systems: handing over to a healthy copy", "to", target.Name)
	if _, err := a.runSwitch(sctx, target.Name, true); err != nil {
		a.log.Warn("mixed systems: hand-over failed; writes stay refused, retried", "to", target.Name, "err", err)
		return
	}
	a.say("Handed over to %s; this node now stops and waits for a fresh copy once it matches again.", target.Name)
}

// handoverTarget picks a copy a drifted main node can hand over to: an
// active member of the sync set (it holds every acknowledged write) with
// no drift of its own, that a switch would accept (crossSystemSwitchBlocked).
// Copies of the main node's own kind first, then by name.
func handoverTarget(st cluster.State, self string) (cluster.Member, bool) {
	me := st.Members[self]
	var ok []cluster.Member
	for _, id := range st.Sync.Members {
		m, found := st.Members[id]
		if !found || id == self || m.Role != cluster.RoleData || m.Status != cluster.StatusActive {
			continue
		}
		if m.Guard != nil && m.Guard.Drift != "" {
			continue
		}
		if crossSystemSwitchBlocked(st, m) != "" {
			continue
		}
		ok = append(ok, m)
	}
	slices.SortFunc(ok, func(x, y cluster.Member) int {
		ax, ay := x.Platform.Alike(me.Platform), y.Platform.Alike(me.Platform)
		if ax != ay {
			if ax {
				return -1
			}
			return 1
		}
		return strings.Compare(x.Name, y.Name)
	})
	if len(ok) == 0 {
		return cluster.Member{}, false
	}
	return ok[0], true
}
