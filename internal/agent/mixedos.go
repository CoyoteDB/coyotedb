package agent

// Mixed operating systems, Step 2 phase 2 (docs/MIXED_OS.md, D29, D37).
// Safety rule 1 lets data nodes of different kinds share a cluster only
// when the cluster allows it and their fingerprints and behaviour probes
// are equal (cluster.checkPlatform decides, in Raft). This file is the
// agent's side:
//
//   - joinPlatform: a joining data node measures itself on a scratch
//     database before anything is copied (MIXED_OS.md item 4), so the
//     leader can refuse it before cloning.
//   - the guard loop (item 7): on every data node, after each Postgres
//     start it measures the live database again and announces it; every
//     AuditInterval it runs the portability audit (conditions 2-5), every
//     verifyInterval the quick integrity check (condition 7); results go
//     into the member's cluster.Guard record.
//   - drift: a node whose measurement changed while a member (an OS or
//     program update) and no longer matches a data node of another kind is
//     refused by the cluster; it records that, stops following a main node
//     of another kind, and blocks new mixed joins and cross-system
//     switches (mixedFollowBlocked, crossSystemSwitchBlocked).
//   - guardNow: the leader's MixedJoinCheck asks a data node to audit
//     right before admitting a mixed joiner ("the join check runs it too").
//   - ensurePortable (item 5): on a main node, the imported OS/ICU
//     collations are dropped and amcheck is installed.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"coyotedb.com/coyote/internal/brand"
	"coyotedb.com/coyote/internal/cluster"
	"coyotedb.com/coyote/internal/pg"
)

const (
	// testFakeOSEnv (tests only): record this operating system in the
	// platform record instead of the real one, so a mixed cluster can be
	// exercised on one machine. Never set in normal use.
	testFakeOSEnv = "COYOTE_TEST_FAKE_OS"
	// testFPSaltEnv (tests only): add a line to this node's measured
	// fingerprint, as if an OS or program update had changed it.
	testFPSaltEnv = "COYOTE_TEST_FINGERPRINT_SALT"
	// testFPSaltFileEnv (tests only): like testFPSaltEnv, read from this
	// file at every measurement, so a test can make a running node's
	// next measurement (after a Postgres restart) differ.
	testFPSaltFileEnv = "COYOTE_TEST_FINGERPRINT_SALT_FILE"

	// guardTick is how often the guard loop looks for work.
	guardTick = 15 * time.Second
	// verifyInterval is the quick integrity check's period (the "slower
	// timer" of MIXED_OS.md item 7); firstVerifyDelay delays the first one
	// after the agent starts.
	verifyInterval   = time.Hour
	firstVerifyDelay = 5 * time.Minute
	// guardStepTimeout bounds one audit, measurement or check.
	guardStepTimeout = 5 * time.Minute
	// scratchDirName is the scratch database folder for joinPlatform.
	scratchDirName = "fpcheck"

	// linuxRecommended is the product recommendation (D38).
	linuxRecommended = "Tip: Linux is the recommended system for database nodes. macOS and Windows are supported too."
)

// platformOS is the operating system recorded in the platform record.
func platformOS() string {
	if v := os.Getenv(testFakeOSEnv); v != "" {
		return v
	}
	return runtime.GOOS
}

// mixedState is the guard loop's memory (one per agent).
type mixedState struct {
	work sync.Mutex // serialises guard work (loop and guardNow)

	mu sync.Mutex
	// measuredPID: the postmaster the last successful measurement was of;
	// measuredBuilds: the size of the cluster's PostgreSQL build table then.
	measuredPID    int
	measuredBuilds int
	// portablePID: the primary postmaster ensurePortable last ran for.
	portablePID int
	drift       string
	audit       *pg.AuditReport
	verify      *pg.VerifyReport
	lastErr     string
	written     *cluster.Guard
	kick        chan struct{}
	started     time.Time // when the guard loop started
	auditRole   string    // role at the last audit
	// capsKey/capsAt: the member set last confirmed to know the D37
	// fields, and when (mixedFieldsOK).
	capsKey string
	capsAt  time.Time

	// D48 builtin-only state.
	builtin    *pg.BuiltinOnlyState // last quick check
	auditDDL   *pg.BuiltinOnlyState // the quick check the last audit started from
	ddlAuditAt time.Time            // last audit started because the DDL log moved
	builtinPID int                  // primary postmaster PrepareBuiltinOnly last fully ran for
	preparedAt time.Time            // leader's mixed_prepare (primary)
	handoverAt time.Time            // last hand-over attempt of a drifted main node
}

func (m *mixedState) kickNow() {
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

func (m *mixedState) getDrift() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.drift
}

// platformFrom turns a measurement into the platform record.
func (a *Agent) platformFrom(fp pg.Fingerprint, pr pg.ProbeResult) cluster.Platform {
	text, hash := fp.Canonical(), fp.Hash()
	salt := os.Getenv(testFPSaltEnv)
	if f := os.Getenv(testFPSaltFileEnv); f != "" {
		if b, err := os.ReadFile(f); err == nil {
			salt = strings.TrimSpace(string(b))
		}
	}
	if salt != "" {
		text += "test_salt=" + salt + "\n"
		h := sha256.Sum256([]byte(text))
		hash = hex.EncodeToString(h[:])
	}
	return cluster.Platform{
		OS: platformOS(), Arch: runtime.GOARCH, PGBuildSHA: a.inst.Build.SHA256,
		Fingerprint: hash, FingerprintText: text,
		ProbeDigest: pr.Digest(), ProbeGroups: pr.MustMatchGroups(),
	}
}

// joinPlatform measures a joining data node on a scratch database (it has
// no data yet), so the leader compares it before anything is copied.
func (a *Agent) joinPlatform(ctx context.Context) (cluster.Platform, error) {
	a.say("Checking this machine's database programs (a minute at most)…")
	cctx, cancel := context.WithTimeout(ctx, guardStepTimeout)
	defer cancel()
	fp, pr, err := pg.ScratchCheck(cctx, pg.ScratchOptions{
		Install: a.inst, Guard: a.guard, Dir: filepath.Join(a.node.StateDir, scratchDirName), Logger: a.log,
	})
	if err != nil {
		return cluster.Platform{}, fmt.Errorf("could not check this machine's database programs: %w", err)
	}
	p := a.platformFrom(fp, pr)
	a.log.Info("measured on a scratch database", "fingerprint", p.Fingerprint, "probe", p.ProbeDigest)
	return p, nil
}

// measure measures the running local Postgres.
func (a *Agent) measure(ctx context.Context) (cluster.Platform, error) {
	c, err := a.node.Connect(ctx)
	if err != nil {
		return cluster.Platform{}, err
	}
	defer c.Close(context.WithoutCancel(ctx))
	fp, err := pg.CollectFingerprint(ctx, a.inst, a.node.PGData(), c)
	if err != nil {
		return cluster.Platform{}, err
	}
	pr, err := pg.RunProbe(ctx, c, pg.ProbeOptions{})
	if err != nil {
		return cluster.Platform{}, err
	}
	return a.platformFrom(fp, pr), nil
}

// explainPlatform is cluster.Config.ExplainPlatform.
func explainPlatform(joiner, member cluster.Platform) []string {
	return pg.ExplainCanonical(joiner.FingerprintText, member.FingerprintText)
}

// unlikeData lists the data members that are neither alike to p nor on
// its PostgreSQL line (other than self): a non-empty list means p's node
// is in a mixed cluster.
func unlikeData(st cluster.State, self string, p cluster.Platform) []cluster.Member {
	var out []cluster.Member
	for _, m := range st.DataMembers() {
		if m.ID != self && !st.Compatible(m.Platform, p) {
			out = append(out, m)
		}
	}
	return out
}

// guardLoop runs on data nodes for the agent's lifetime.
func (a *Agent) guardLoop(ctx context.Context) {
	a.mixed.mu.Lock()
	a.mixed.started = time.Now()
	a.mixed.mu.Unlock()
	t := time.NewTicker(guardTick)
	defer t.Stop()
	for {
		a.guardStep(ctx, false)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-a.mixed.kick:
		}
	}
}

// guardNow audits at once and records the result (the leader's mixed
// join check). It fails if this node's Postgres is not running.
func (a *Agent) guardNow(ctx context.Context) error {
	if a.role != cluster.RoleData {
		return errors.New("a witness has no database to check")
	}
	if srv, _ := a.currentPG(); srv == nil {
		return errors.New("this node's database is not running")
	}
	if err := a.guardStep(ctx, true); err != nil {
		return err
	}
	a.mixed.mu.Lock()
	defer a.mixed.mu.Unlock()
	if a.mixed.lastErr != "" {
		return errors.New(a.mixed.lastErr)
	}
	return nil
}

// guardStep does whatever guard work is due; force audits now. The error
// is only for guardNow (the loop logs and retries).
func (a *Agent) guardStep(ctx context.Context, force bool) error {
	for !a.mixed.work.TryLock() {
		select {
		case <-ctx.Done():
			return fmt.Errorf("the database check is busy: %w", ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	defer a.mixed.work.Unlock()
	if a.cn == nil {
		return nil
	}
	if a.mixed.getDrift() != "" && a.primaryUnlike(a.cn.State()) {
		a.retireDrifted(ctx)
	}
	// D70: this program's pinned PostgreSQL builds into the cluster's
	// table (once every member knows it), before the platform is announced.
	a.registerPGBuilds(ctx)
	srv, role := a.currentPG()
	if srv == nil {
		return nil
	}
	st := a.cn.State()
	me, ok := st.Members[a.cn.ID()]
	if !ok || me.Role != cluster.RoleData {
		return nil
	}
	pid := srv.PID()
	m := &a.mixed
	m.mu.Lock()
	// A refused announcement (drift) is tried again when the cluster's
	// PostgreSQL build table changed: the refusal may have been a minor
	// update that the table now allows (D70), e.g. once the last member of
	// a rolling update runs a program that knows the table.
	newServer := pid != m.measuredPID || (m.drift != "" && len(st.PGBuilds) != m.measuredBuilds)
	needPortable := role == "primary" && pid != m.portablePID
	// Audit at once after a start or a change of role (a promoted copy
	// checks right away: a guard failing after failover shows at once).
	auditDue := force || newServer || role != m.auditRole || m.audit == nil || time.Since(m.audit.At) >= pg.AuditInterval
	// The first integrity check waits a while after the agent starts, to
	// keep starts light; then every verifyInterval.
	verifyDue := (m.verify == nil && time.Since(m.started) >= firstVerifyDelay) || (m.verify != nil && time.Since(m.verify.At) >= verifyInterval)
	m.lastErr = ""
	m.mu.Unlock()

	if needPortable {
		if err := a.ensurePortable(ctx); err != nil {
			a.log.Warn("mixed systems: could not finish the portable set-up (retried on the next start)", "err", err)
		}
		m.mu.Lock()
		m.portablePID = pid
		m.mu.Unlock()
	}
	if newServer {
		if err := a.refreshPlatform(ctx, st, role); err != nil {
			a.log.Info("mixed systems: measuring this node failed; will retry", "err", err)
		} else {
			m.mu.Lock()
			m.measuredPID, m.measuredBuilds = pid, len(st.PGBuilds)
			m.mu.Unlock()
		}
	}
	// D48: builtin-only. The main node keeps the guard in every database
	// (or removes it when the cluster does not mix machines), and a
	// drifted main node refuses writes and hands over. Every data node
	// runs the quick check each tick and audits at once when the DDL log
	// moved (event-driven, at most every ddlAuditGap), so a finding reaches
	// the cluster state within a tick, not up to AuditInterval later.
	builtinOn := a.builtinWanted(st) || force
	if role == "primary" {
		a.primaryBuiltin(ctx, st, pid, builtinOn)
		a.driftedMain(ctx, st)
	}
	if builtinOn {
		if a.quickBuiltin(ctx) {
			m.mu.Lock()
			if force || m.audit == nil || time.Since(m.ddlAuditAt) >= ddlAuditGap {
				auditDue = true
				m.ddlAuditAt = time.Now()
			}
			m.mu.Unlock()
		}
	} else {
		m.mu.Lock()
		m.builtin = nil
		m.mu.Unlock()
	}
	var stepErr error
	if auditDue {
		m.mu.Lock()
		from := m.builtin
		m.mu.Unlock()
		cctx, cancel := context.WithTimeout(ctx, guardStepTimeout)
		r, err := pg.AuditCluster(cctx, a.node.ConnectDB, pg.AuditOptions{NodeOS: runtime.GOOS})
		cancel()
		if err != nil {
			stepErr = fmt.Errorf("the database check could not run: %w", err)
			a.log.Info("mixed systems: audit failed; will retry", "err", err)
		} else {
			m.mu.Lock()
			m.audit, m.auditRole, m.auditDDL = &r, role, from
			m.mu.Unlock()
			if r.Blocking() {
				a.log.Warn("mixed systems: the audit found something that cannot be shared between kinds of machines", "findings", len(r.Findings))
			}
		}
	}
	if verifyDue && !force {
		cctx, cancel := context.WithTimeout(ctx, guardStepTimeout)
		r, err := pg.VerifyCluster(cctx, a.node.ConnectDB, pg.VerifyOptions{Level: pg.VerifyQuick, InstallAmcheck: role == "primary", Pause: 10 * time.Millisecond})
		cancel()
		if err != nil {
			a.log.Info("mixed systems: integrity check failed to run; will retry", "err", err)
		} else {
			m.mu.Lock()
			m.verify = &r
			m.mu.Unlock()
			if r.Damaged() {
				a.log.Error("integrity check found damage", "findings", len(r.Findings))
			}
		}
	}
	if stepErr != nil {
		m.mu.Lock()
		m.lastErr = stepErr.Error()
		m.mu.Unlock()
	}
	return a.writeGuard(ctx, force)
}

// refreshPlatform measures the running database and announces it. A
// refusal means drift: the node changed and no longer matches a data node
// of another kind.
func (a *Agent) refreshPlatform(ctx context.Context, st cluster.State, role string) error {
	cctx, cancel := context.WithTimeout(ctx, guardStepTimeout)
	defer cancel()
	live, err := a.measure(cctx)
	if err != nil {
		return err
	}
	self := a.cn.ID()
	rec := st.Members[self].Platform
	if live.Equal(rec) {
		a.setDrift("")
		return nil
	}
	if !a.mixedFieldsOK(cctx) {
		return errors.New("not every member knows the mixed-system fields yet")
	}
	_, err = a.cn.Apply(cctx, &cluster.UpdateMember{ID: self, Platform: &live})
	switch {
	case err == nil:
		a.log.Info("announced this node's measured platform", "fingerprint", live.Fingerprint, "probe", live.ProbeDigest)
		a.setDrift("")
		return nil
	case errors.Is(err, cluster.ErrPlatformMismatch), errors.Is(err, cluster.ErrMixedOff):
	default:
		return err
	}
	why := "This machine's database programs or behaviour changed since it joined (an operating-system or program update?), and it no longer matches"
	others := unlikeData(st, self, live)
	var names []string
	for _, o := range others {
		names = append(names, o.Name)
	}
	if len(names) > 0 {
		why += " " + strings.Join(names, ", ")
	} else {
		why += " the other data nodes"
	}
	why += "."
	if len(others) > 0 && others[0].Platform.FingerprintText != "" {
		if lines := pg.ExplainCanonical(live.FingerprintText, others[0].Platform.FingerprintText); len(lines) > 0 {
			why += " " + lines[0]
		} else if live.ProbeDigest != others[0].Platform.ProbeDigest {
			why += " The behaviour check gives different answers."
		}
	}
	if role == "primary" && st.Settings.MixedOS {
		why += " As the main node it refuses writes (reads still work) and hands over to a healthy copy if there is one."
	}
	a.log.Warn("mixed systems: drift", "why", why, "err", err)
	a.setDrift(why)
	if role == "standby" && a.primaryUnlike(st) {
		// Rule 1: do not keep a copy of a main node of another kind that
		// this machine no longer matches. Stopped under the Postgres lock;
		// the loop's start paths then refuse (mixedFollowBlocked).
		a.say("%s", why)
		a.say("This copy stops following the main node to keep the data safe. Fix: undo the update, or take this node out (`%s remove %s`) and join it again.", brand.Command, a.name)
		unlock, err := a.lockPG(ctx)
		if err != nil {
			return nil
		}
		if err := a.stopPG(); err != nil {
			a.log.Warn("stop postgres after drift", "err", err)
		}
		unlock()
		a.retireDrifted(ctx)
		a.kickLoop()
	}
	return nil
}

func (a *Agent) setDrift(why string) {
	a.mixed.mu.Lock()
	defer a.mixed.mu.Unlock()
	a.mixed.drift = why
}

func (a *Agent) kickLoop() {
	select {
	case a.kick <- struct{}{}:
	default:
	}
}

// primaryUnlike reports whether the main node is a different kind of
// machine than this node's record: neither alike nor on the same
// PostgreSQL line (D70; which of those two may copy which is
// minorFollowBlocked's business).
func (a *Agent) primaryUnlike(st cluster.State) bool {
	p, ok := st.Members[st.PrimaryID]
	me, ok2 := st.Members[a.cn.ID()]
	return ok && ok2 && p.ID != me.ID && !st.Compatible(p.Platform, me.Platform)
}

// writeGuard records the guard results in the cluster when they changed,
// or when the audit time moved on (the mixed join check needs a recent
// one), or when forced.
func (a *Agent) writeGuard(ctx context.Context, force bool) error {
	m := &a.mixed
	m.mu.Lock()
	g := cluster.Guard{Drift: m.drift}
	var fs []pg.Finding
	if m.audit != nil {
		g.AuditAt, g.AuditBlocking = m.audit.At.UTC().Truncate(time.Second), m.audit.Blocking()
		fs = append(fs, m.audit.Findings...)
	}
	if m.verify != nil {
		g.VerifyAt, g.VerifyDamaged = m.verify.At.UTC().Truncate(time.Second), m.verify.Damaged()
		fs = append(fs, m.verify.Findings...)
	}
	if m.builtin != nil && m.audit != nil {
		// D48: the quick builtin-only check counts as part of the audit.
		g.AuditBlocking = g.AuditBlocking || m.builtin.Blocking()
		fs = append(fs, m.builtin.Findings...)
	}
	prev := m.written
	m.mu.Unlock()
	pg.SortFindings(fs)
	for _, f := range fs {
		if f.Severity != pg.SeverityBlock && f.Severity != pg.SeverityWarn {
			continue
		}
		if len(g.Findings) == cluster.MaxGuardFindings {
			break
		}
		s := f.String()
		if len(s) > 300 {
			s = s[:297] + "..."
		}
		g.Findings = append(g.Findings, s)
	}
	if g.AuditAt.IsZero() && g.VerifyAt.IsZero() && g.Drift == "" {
		return nil
	}
	if prev != nil && !force && sameGuard(*prev, g) {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if !a.mixedFieldsOK(cctx) {
		return errors.New("not every member runs a version that can record the check results yet")
	}
	if _, err := a.cn.Apply(cctx, &cluster.UpdateMember{ID: a.cn.ID(), Guard: &g}); err != nil {
		a.log.Info("mixed systems: could not record the check results; will retry", "err", err)
		return fmt.Errorf("could not record the check results: %w", err)
	}
	m.mu.Lock()
	m.written = &g
	m.mu.Unlock()
	return nil
}

func sameGuard(a, b cluster.Guard) bool {
	return a.AuditAt.Equal(b.AuditAt) && a.VerifyAt.Equal(b.VerifyAt) && a.AuditBlocking == b.AuditBlocking &&
		a.VerifyDamaged == b.VerifyDamaged && a.Drift == b.Drift && slices.Equal(a.Findings, b.Findings)
}

// ensurePortable (MIXED_OS.md item 5) runs on a main node after it
// starts: drop the collations initdb imported from the operating system
// and ICU (condition 2; kept and reported if something uses one), and
// install amcheck where the integrity check needs it (condition 7). Both
// are idempotent and replicate to the copies.
func (a *Agent) ensurePortable(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, guardStepTimeout)
	defer cancel()
	n, kept, err := pg.DropImportedCollationsEverywhere(cctx, a.node.ConnectDB)
	if err != nil {
		return err
	}
	if n > 0 || len(kept) > 0 {
		a.log.Info("removed operating-system sort rules", "dropped", n, "kept_in_use", len(kept))
	}
	for _, db := range []string{"template1", "postgres", pg.AppDatabase} {
		c, err := a.node.ConnectDB(cctx, db)
		if err != nil {
			if db == pg.AppDatabase {
				continue // not created yet
			}
			return err
		}
		// Housekeeping: never wait for a copy (see DropImportedCollations).
		_, err = c.Exec(cctx, "SET synchronous_commit = local")
		if err == nil {
			_, err = c.Exec(cctx, "CREATE EXTENSION IF NOT EXISTS amcheck")
		}
		c.Close(context.WithoutCancel(cctx))
		if err != nil {
			return fmt.Errorf("install amcheck in %s: %w", db, err)
		}
	}
	return nil
}

// mixedJoinCheck is cluster.Config.MixedJoinCheck (on the leader): every
// reachable data node audits now and records it, before a mixed joiner is
// admitted (every node's record counts in checkPlatform, so a stale
// finding of one node must be refreshed too). At least one must succeed;
// an unreachable node keeps its last record.
func (a *Agent) mixedJoinCheck(ctx context.Context, joiner cluster.Member) error {
	st := a.cn.State()
	// D48: before anything else, the main node makes the cluster
	// builtin-only (only when the joiner matches it; otherwise admit
	// refuses on the fingerprint and nothing is changed). A database or
	// collation that cannot be made so refuses the join for good.
	if p, ok := st.Members[st.PrimaryID]; ok && p.Role == cluster.RoleData && joiner.Platform.Measured() && p.Platform.Measured() &&
		joiner.Platform.Fingerprint == p.Platform.Fingerprint && joiner.Platform.ProbeDigest == p.Platform.ProbeDigest {
		var pr prepareResp
		if err := a.call(ctx, p.ID, "mixed_prepare", nil, &pr); err != nil {
			return fmt.Errorf("the main node %s could not get ready for a machine of another kind: %w", p.Name, err)
		}
		if pr.Blocked != "" {
			return &cluster.Error{Code: cluster.ErrMixedBlocked.Code, Msg: cluster.ErrMixedBlocked.Msg + ": " + pr.Blocked}
		}
	}
	order := []string{}
	if p, ok := st.Members[st.PrimaryID]; ok && p.Role == cluster.RoleData {
		order = append(order, p.ID)
	}
	for _, m := range st.DataMembers() {
		if m.ID != st.PrimaryID {
			order = append(order, m.ID)
		}
	}
	var errs []error
	var mu sync.Mutex
	var wg sync.WaitGroup
	ok := 0
	for _, id := range order {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := a.call(ctx, id, "guard_now", nil, nil)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", a.memberName(id), err))
				return
			}
			ok++
			a.log.Info("mixed join: fresh audit recorded", "by", a.memberName(id), "joiner", joiner.Name)
		}()
	}
	wg.Wait()
	if ok > 0 {
		return nil
	}
	if len(errs) == 0 {
		return errors.New("no data node can check the database right now")
	}
	return fmt.Errorf("no data node could check the database right now: %w", errors.Join(errs...))
}

// mixedFollowBlocked says why this node must not become (or start as) a
// copy of the current main node, or "" (rule 1): a node whose
// measurement drifted never follows a main node of another kind.
func (a *Agent) mixedFollowBlocked(st cluster.State) string {
	if !a.primaryUnlike(st) {
		return ""
	}
	if d := a.mixed.getDrift(); d != "" {
		return d + " It does not copy the main node (" + a.primaryName(st) + ", another kind of machine) until that is fixed."
	}
	me, p := st.Members[a.cn.ID()].Platform, st.Members[st.PrimaryID].Platform
	if !st.Settings.MixedOS || !me.Measured() || !p.Measured() || me.Fingerprint != p.Fingerprint || me.ProbeDigest != p.ProbeDigest {
		return fmt.Sprintf("This machine (%s) does not match the main node %s (%s), so it does not copy its data.",
			systemName(me), a.primaryName(st), systemName(p))
	}
	return ""
}

// crossSystemSwitchBlocked says why a switch to target must not happen,
// or "": a switch between kinds of machines needs mixing on, both
// measured and equal, and no data node's guard blocked (MIXED_OS.md item
// 7). Switches between alike machines are not affected.
func crossSystemSwitchBlocked(st cluster.State, target cluster.Member) string {
	p, ok := st.Members[st.PrimaryID]
	if !ok || st.Compatible(p.Platform, target.Platform) {
		return ""
	}
	tp, pp := target.Platform, p.Platform
	if !st.Settings.MixedOS || !tp.Measured() || !pp.Measured() || tp.Fingerprint != pp.Fingerprint || tp.ProbeDigest != pp.ProbeDigest {
		return fmt.Sprintf("%s (%s) does not match the main node %s (%s), so it cannot become the main node.",
			target.Name, systemName(tp), p.Name, systemName(pp))
	}
	for _, m := range st.DataMembers() {
		g := m.Guard
		if m.ID == st.PrimaryID && g != nil && !g.AuditBlocking && !g.VerifyDamaged {
			// The main node's own drift is about its programs, not the
			// data: handing over away from it is the fix (D48).
			continue
		}
		if g.Blocked() {
			return fmt.Sprintf("%s cannot become the main node: it is a different kind of machine than %s, and %s reports a problem with sharing the data between kinds of machines (see `%s status`).",
				target.Name, p.Name, m.Name, brand.Command)
		}
	}
	return ""
}

// systemName is "macOS arm64" and the like.
func systemName(p cluster.Platform) string {
	os := map[string]string{"darwin": "macOS", "linux": "Linux", "windows": "Windows"}[p.OS]
	if os == "" {
		os = p.OS
	}
	return os + " " + p.Arch
}

// mixedWarnings are status warnings about mixed systems, in plain words.
func mixedWarnings(st cluster.State) []string {
	var out []string
	mixed := false
	data := st.DataMembers()
	for _, m := range data {
		if len(unlikeData(st, m.ID, m.Platform)) > 0 {
			mixed = true
		}
	}
	for _, m := range data {
		g := m.Guard
		switch {
		case g == nil:
		case g.Drift != "":
			out = append(out, fmt.Sprintf("%s: %s", m.Name, g.Drift))
		case g.VerifyDamaged:
			out = append(out, fmt.Sprintf("%s: the integrity check found damage: %s", m.Name, strings.Join(g.Findings, "; ")))
		case g.AuditBlocking && (mixed || st.Settings.MixedOS):
			out = append(out, fmt.Sprintf("%s: the database uses something that cannot be shared between kinds of machines, so no other kind of machine can join or take over (by a switch or a failover): %s", m.Name, strings.Join(g.Findings, "; ")))
		}
	}
	return out
}

// hasMixedFields reports whether the state already holds D37 fields.
func hasMixedFields(st cluster.State) bool {
	if st.Settings.MixedOS {
		return true
	}
	for _, m := range st.Members {
		if m.Guard != nil || m.Platform.Fingerprint != "" || m.Platform.ProbeDigest != "" {
			return true
		}
	}
	return false
}

// haltedMessage is what a node says when its state machine stopped on an
// entry it cannot read (its program is older than the cluster's).
func haltedMessage(err error) string {
	return fmt.Sprintf("This node's %s is older than the rest of the cluster: it cannot read the cluster's latest changes, so it stopped its database and exits to stay safe. "+
		"Update %s on this machine to the version the other nodes run, then start it again. (%v)", brand.Command, brand.Command, err)
}

// retireDrifted takes a drifted copy out of failover (rule 1 with rules
// 2 and 3): it asks the main node to remove it from the sync set the
// usual way (S first, then R: the same sync_remove that leave and remove
// use), and once it is out of R marks itself "needs re-clone", so neither
// the controller (Candidates: active members of R) nor the state machine
// (Promote refuses a member that is not active) can pick it. Marking it
// while still in R would shrink the rule 2 count, so it never does that.
// Retried on every guard step while the drift lasts; meanwhile its
// Postgres is stopped, so it cannot be cut and therefore is never chosen
// by a failover (ChooseTarget picks only confirmed cuts).
func (a *Agent) retireDrifted(ctx context.Context) {
	st := a.cn.State()
	self := a.cn.ID()
	me, ok := st.Members[self]
	if !ok || me.Role != cluster.RoleData || st.PrimaryID == self {
		return
	}
	if slices.Contains(st.Sync.Members, self) {
		if st.PrimaryID == "" {
			return
		}
		cctx, cancel := context.WithTimeout(ctx, time.Minute)
		err := a.call(cctx, st.PrimaryID, "sync_remove", syncNodeReq{Node: self}, nil)
		cancel()
		if err != nil {
			a.log.Info("drift: could not leave the sync set yet; will retry", "err", err)
			return
		}
		a.log.Warn("drift: left the sync set (not a failover candidate any more)")
		return // the next step sees the new state
	}
	if me.Status == cluster.StatusActive {
		s := cluster.StatusNeedsReclone
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if _, err := a.cn.Apply(cctx, &cluster.UpdateMember{ID: self, Status: &s}); err != nil {
			a.log.Info("drift: could not mark this copy for a fresh copy yet; will retry", "err", err)
			return
		}
		a.log.Warn("drift: marked for a fresh copy once the machine matches again")
	}
}

// capsResp is the "caps" RPC answer. An agent from before D37 answers
// "unknown agent method", which counts as "does not know".
type capsResp struct {
	MixedOS bool `json:"mixed_os"`
	Backup  bool `json:"backup"` // knows the backup settings (D63)
	// PGBuilds: knows the PostgreSQL build table (RegisterPGBuilds, D70).
	PGBuilds bool `json:"pg_builds"`
}

// capsCacheFor is how long a positive "every member knows the mixed
// fields" answer is reused for the same set of members.
const capsCacheFor = time.Minute

// mixedFieldsOK reports whether every member's agent knows the D37 state
// fields, so writing them cannot halt an older member's state machine
// (its decoder is strict) during a rolling upgrade. Any member that does
// not answer counts as "no": the write waits.
func (a *Agent) mixedFieldsOK(ctx context.Context) bool {
	st := a.cn.State()
	if hasMixedFields(st) {
		// The threshold was passed: every member then answered caps, and
		// since then the leader refuses joiners with an older protocol
		// (cluster.JoinProtocol). Only a downgrade of a member could bring
		// an older program back; that is not supported (an older program
		// halts on the fields, and from this version on a halted node
		// stops with a message: haltedMessage).
		return true
	}
	ids := slices.Sorted(maps.Keys(st.Members))
	key := strings.Join(ids, ",")
	m := &a.mixed
	m.mu.Lock()
	if m.capsKey == key && time.Since(m.capsAt) < capsCacheFor {
		m.mu.Unlock()
		return true
	}
	m.mu.Unlock()
	for _, id := range ids {
		if id == a.cn.ID() {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		var r capsResp
		err := a.call(cctx, id, "caps", nil, &r)
		cancel()
		if err != nil || !r.MixedOS {
			a.log.Info("mixed systems: not every member knows the new fields yet; waiting", "member", a.memberName(id), "err", err)
			return false
		}
	}
	m.mu.Lock()
	m.capsKey, m.capsAt = key, time.Now()
	m.mu.Unlock()
	return true
}
