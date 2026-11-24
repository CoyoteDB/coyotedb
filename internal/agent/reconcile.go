package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"coyotedb.com/coyote/internal/brand"
	"coyotedb.com/coyote/internal/cluster"
	"coyotedb.com/coyote/internal/cluster/transport"
	"coyotedb.com/coyote/internal/pg"
)

// errLeft ends the loop after a clean `leave`.
var errLeft = errors.New("left the cluster")

// loop is the role reconcile loop: on every cluster state change, leader
// change, tick (1s) or nudge it compares what the cluster says this node
// should be with what Postgres is, and moves one step towards it.
func (a *Agent) loop(ctx context.Context) error {
	sub := a.cn.Subscribe()
	defer sub.Close()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var delay <-chan time.Time
	for {
		if delay == nil {
			err := a.reconcile(ctx)
			var fe *fatalError
			if errors.As(err, &fe) {
				return fe.err
			}
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				// A start that can never work stops the node with what to
				// do; a known passing one says it in plain words (D50).
				if stop := a.startProblem(err); stop != nil {
					return stop
				}
				if errors.Is(err, pg.ErrExitedDuringStartup) || pg.BinaryBroken(err) {
					delay = time.After(a.pgFailed(ctx, err))
				} else {
					delay = time.After(a.retryDelay(err))
				}
			} else {
				a.notPrimaryTries = 0
			}
		}
		pgDone := a.pgDone()
		select {
		case <-ctx.Done():
			return nil
		case err := <-a.exitCh:
			var rb *removedBy
			if errors.As(err, &rb) {
				return errors.Unwrap(a.becomeRemoved(rb.by))
			}
			return err
		case req := <-a.leaveCh:
			err := a.doLeave(ctx)
			req.reply <- err
			if err == nil {
				return errLeft
			}
		case <-a.guardLost():
			return errGuardLost
		case <-pgDone:
			srv, _ := a.currentPG()
			if srv != nil && pgDone == srv.Done() && !a.isFencing() {
				// The postmaster died (killed, crashed). Postgres never runs
				// without the agent, but the agent outlives Postgres: forget
				// it and let reconcile start it again (as a primary only
				// through a confirmed read, with a fresh lease), with backoff.
				err := srv.ExitErr()
				a.clearPG(srv)
				a.dropLease()
				delay = time.After(a.pgFailed(ctx, err))
			}
		case <-delay:
			delay = nil
		case <-a.kick:
			delay = nil
		case <-sub.C:
		case <-tick.C:
		}
	}
}

// crashBackoff is the wait before restarting a crashed Postgres (or one
// that died while starting): 1s, doubling per crash within three minutes
// of the last, at most maxCrashBackoff. It never gives up.
func (a *Agent) crashBackoff() time.Duration {
	if time.Since(a.lastCrash) > 3*time.Minute {
		a.crashes = 0
	}
	a.crashes++
	a.lastCrash = time.Now()
	return min(time.Second<<min(a.crashes-1, 6), maxCrashBackoff)
}

const (
	maxCrashBackoff = time.Minute
	// crashesForAttention: this many crashes in a row and the node says it
	// needs attention (it keeps trying).
	crashesForAttention = 5
)

// pgFailed handles a Postgres that crashed or died while starting: if it
// looks like its program files are damaged, check them in full and
// install them again; back off; after several in a row, say the node
// needs attention. It returns how long to wait before the next try.
func (a *Agent) pgFailed(ctx context.Context, err error) time.Duration {
	wait := a.crashBackoff()
	if pg.BinaryBroken(err) {
		a.healInstall(ctx, err)
	}
	a.log.Error("postgres stopped unexpectedly; starting it again", "err", err, "in", wait, "crashes_in_a_row", a.crashes)
	if a.crashes >= crashesForAttention {
		a.setPhase("needs-attention", fmt.Sprintf("Needs attention: PostgreSQL keeps stopping (%d times in a row, last: %v). "+
			"Trying again in %s. Check this machine's disk and memory; the details are in this node's log.", a.crashes, err, wait))
		return wait
	}
	a.say("PostgreSQL stopped unexpectedly (%v); starting it again in %s.", err, wait)
	a.setPhase("waiting", "The database stopped unexpectedly; starting it again…")
	return wait
}

// healInstall checks every Postgres program file against the install's
// manifest and, if any is damaged, downloads and installs them again.
func (a *Agent) healInstall(ctx context.Context, cause error) {
	if a.inst.Root == "" {
		return
	}
	bad := pg.CheckInstall(a.inst, true)
	if bad == nil {
		a.log.Info("postgres failed like a damaged program, but its files match the manifest", "err", cause)
		return
	}
	a.log.Warn("postgres program files are damaged; installing them again", "why", bad, "after", cause)
	inst, err := pg.Ensure(ctx, pg.FetchOptions{
		CacheDir: a.cacheDir(), Archive: a.cfg.Archive, Logger: a.log, Progress: a.downloadProgress(),
		Build: a.inst.Build, Full: true, Status: func(msg string) { a.setPhase("starting", msg) },
	})
	if err != nil {
		a.log.Error("could not install PostgreSQL again", "err", err)
		a.say("The PostgreSQL program files are damaged and could not be installed again yet (%v); trying again later.", err)
		return
	}
	a.inst = inst
	a.say("PostgreSQL's program files were damaged (%v) and have been installed again.", bad)
}

// retryDelay notes a failed step and says when to try again: 2s, except
// right after a change of main node, when the new one is still promoting
// (ErrUpstreamNotPrimary): then every 200ms for up to 10s. A retry only
// connects and asks pg_is_in_recovery().
func (a *Agent) retryDelay(err error) time.Duration {
	if !errors.Is(err, pg.ErrUpstreamNotPrimary) {
		a.notPrimaryTries = 0
		a.noteErr(err)
		return 2 * time.Second
	}
	a.notPrimaryTries++
	if a.notPrimaryTries <= 50 {
		a.log.Debug("reconcile: the new main node is still promoting; retrying soon", "err", err)
		return 200 * time.Millisecond
	}
	a.noteErr(err)
	return 2 * time.Second
}

// noteErr logs a failed step; the person sees it once per distinct message.
func (a *Agent) noteErr(err error) {
	a.log.Warn("reconcile step failed; will retry", "err", err)
	msg := err.Error()
	if diskFull(err) {
		// Plain words, said once per spell (the raw error names a new
		// temporary file every time; finding D3).
		if a.lastErrMsg != errMsgDiskFull {
			a.lastErrMsg = errMsgDiskFull
			a.say("The disk is full: free some space on the drive that holds %s. The node keeps trying by itself.", a.node.StateDir)
		}
		return
	}
	if msg != a.lastErrMsg {
		a.lastErrMsg = msg
		if i := strings.IndexByte(msg, '\n'); i > 0 {
			msg = msg[:i]
		}
		a.say("Problem (will retry): %s", msg)
	}
}

func (a *Agent) reconcile(ctx context.Context) error {
	// A state machine that stopped on an entry it cannot read (an older
	// program than the cluster's) must not keep running a database on a
	// stale view: stop and say so (D37).
	if err := a.cn.Err(); errors.Is(err, cluster.ErrHalted) {
		return fatal(&ExitError{Code: 1, Msg: haltedMessage(err)})
	} else if errors.Is(err, cluster.ErrStorage) {
		// The disk refused to save a Raft term (R1): Raft on this node
		// has stopped; stop the database and exit with the disk named.
		return fatal(&ExitError{Code: 1, Msg: storageMessage(err, a.clusterDir()), Cause: err})
	}
	if a.probeMembership(ctx) {
		return a.becomeRemoved("")
	}
	st := a.cn.State()
	if a.role == cluster.RoleWitness {
		return a.reconcileWitness(ctx, st)
	}
	o, err := a.observe(st, false)
	if err != nil {
		return err
	}
	act := decide(o)
	if act.kind == actConfirm {
		cst, err := a.confirm(ctx)
		if isDenied(err) {
			return a.becomeRemoved("")
		}
		if err != nil {
			a.log.Info("cannot confirm cluster state yet", "err", err)
			a.setPhase("waiting", confirmWaitMessage(o))
			return nil
		}
		st = cst
		if o, err = a.observe(st, true); err != nil {
			return err
		}
		if act = decide(o); act.kind == actConfirm {
			return errors.New("internal error: decision needs confirmation twice")
		}
	}
	a.retarget(st)
	if act.kind != actNone && act.kind != actWait {
		a.log.Info("reconcile", "action", act.kind.String(), "epoch", st.Epoch, "primary", st.PrimaryID)
	}
	switch act.kind {
	case actWait:
		a.setPhase("waiting", act.reason)
		return nil
	case actRemoved:
		return a.becomeRemoved("")
	case actNone:
		return a.steady(ctx, st)
	case actStartStandby, actClone, actRejoin, actReclone:
		// Rule 1 (mixed systems, D37): never become a copy of a main node
		// of another kind that this machine does not match.
		if why := a.mixedFollowBlocked(st); why != "" {
			a.setPhase("waiting", why)
			return nil
		}
	}
	unlock, err := a.lockPG(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	switch act.kind {
	case actInitPrimary:
		return a.doInitPrimary(ctx, st)
	case actStartPrimary:
		return a.doStartPrimary(ctx, st)
	case actStartStandby:
		return a.doStartStandby(ctx, st)
	case actClone:
		return a.doClone(ctx, st)
	case actRejoin:
		return a.doRejoin(ctx, st)
	case actPromote:
		return a.doPromote(ctx, st)
	case actRepoint:
		return a.doRepoint(ctx, st)
	case actFence:
		return a.doFence(ctx, st, act.reason)
	case actClearFence:
		return a.doClearFence(st)
	case actReclone:
		return a.doReclone(ctx, st)
	case actMarkActive:
		return a.doMarkActive(ctx, st)
	case actResume:
		return a.doResumeStandby(ctx, st)
	}
	return fmt.Errorf("internal error: unhandled action %s", act.kind)
}

func confirmWaitMessage(o observed) string {
	if o.Initialized && !o.StandbyDir && o.Marker == "" {
		return "Waiting to reach the other nodes. This node holds main-node data, so it must first confirm that no other node took over; " +
			"its database stays stopped until then."
	}
	return "Waiting to reach the other nodes…"
}

func (a *Agent) observe(st cluster.State, confirmed bool) (observed, error) {
	self := a.cn.ID()
	_, member := st.Members[self]
	if member {
		a.seenMember = true
	}
	f := a.st.get()
	o := observed{
		Self: self, Confirmed: confirmed, Bootstrapped: st.Bootstrapped(), Member: member, SeenMember: a.seenMember,
		Primary: st.PrimaryID, Epoch: st.Epoch,
		Initialized: a.node.Initialized(), StandbyDir: a.node.IsStandbyDir(),
		LastPrimaryEpoch: f.LastPrimaryEpoch, PromotingEpoch: f.PromotingEpoch, FreshPrimary: f.FreshPrimary, CutEpoch: f.CutEpoch,
		CutExpired: a.cutExpired(f.CutUntil),
		Fencing:    a.isFencing(), Recloned: f.Recloned,
	}
	if me, ok := st.Members[self]; ok {
		o.NeedsReclone = me.Status == cluster.StatusNeedsReclone
	}
	if pm, ok := st.Members[st.PrimaryID]; ok {
		o.PrimaryName = pm.Name
	}
	for _, fr := range st.Forced {
		if fr.NodeID == self && fr.Epoch == st.Epoch {
			o.ForcedSelf = true
		}
	}
	_, secErr := a.node.Secrets()
	o.HasSecrets = st.PGSecrets != nil || secErr == nil
	m, err := a.node.ReadMarker()
	if err != nil {
		return o, err
	}
	if m != nil {
		o.Marker, o.MarkerFencedEpoch = m.Reason, m.FencedEpoch
	}
	rs, err := a.node.ReadReplState()
	if err != nil {
		return o, err
	}
	o.StreamCut = rs.Upstream == nil
	srv, role := a.currentPG()
	o.PGRunning = srv != nil
	o.PGInRecovery = role == "standby"
	return o, nil
}

// retarget points the tunnel forwarder at the current primary. Existing
// pipes close; a streaming standby reconnects by itself.
func (a *Agent) retarget(st cluster.State) {
	if a.fwd == nil {
		return
	}
	want := st.PrimaryID
	if want == a.cn.ID() {
		want = ""
	}
	if a.fwd.Target() != want {
		a.log.Info("tunnel re-pointed", "target", want, "epoch", st.Epoch)
		a.fwd.SetTarget(want)
	}
}

// confirm returns cluster state that reflects every committed entry: a
// linearizable read on the leader (VerifyLeader + Barrier), and this
// node's own copy caught up to it. Without a quorum it fails, which is
// what keeps an isolated former primary from starting.
func (a *Agent) confirm(ctx context.Context) (cluster.State, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if a.cn.IsLeader() {
		return a.cn.LinearizableState(ctx)
	}
	leader := a.cn.LeaderID()
	if leader == "" {
		return cluster.State{}, errors.New("no leader known (no quorum reachable)")
	}
	var r confirmResp
	if err := a.call(ctx, leader, "confirm", nil, &r); err != nil {
		return cluster.State{}, err
	}
	if err := a.cn.WaitFor(ctx, func(s *cluster.State) bool { return s.AppliedIndex >= r.Index }); err != nil {
		return cluster.State{}, fmt.Errorf("catch up with the leader: %w", err)
	}
	return a.cn.State(), nil
}

// probeMembership asks the leader (or, without one, the other members)
// every few seconds whether this node is still a member. A removed node
// usually never receives the entry that removed it: from that moment the
// others refuse its certificate. The refusal is the signal.
func (a *Agent) probeMembership(ctx context.Context) bool {
	if !a.seenMember || a.cn.IsLeader() || time.Since(a.lastProbe) < 3*time.Second {
		return false
	}
	a.lastProbe = time.Now()
	ids := []string{a.cn.LeaderID()}
	if ids[0] == "" {
		ids = ids[:0]
		for id := range a.cn.State().Members {
			if id != a.cn.ID() {
				ids = append(ids, id)
			}
		}
	}
	for _, id := range ids {
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := a.call(cctx, id, "ping", nil, nil)
		cancel()
		if isDenied(err) {
			a.log.Warn("another member refused this node's certificate: it was removed", "by", id)
			return true
		}
		if err == nil {
			return false
		}
	}
	return false
}

// isDenied reports whether another member refused this node's
// certificate: the cluster no longer lists it.
func isDenied(err error) bool {
	var re *transport.RefusedError
	return errors.As(err, &re) && re.Denied
}

// ---- actions (each runs under lockPG) ----

func (a *Agent) doInitPrimary(ctx context.Context, st cluster.State) error {
	a.setPhase("starting", fmt.Sprintf("Creating a new database in %s …", a.node.StateDir))
	if _, err := a.node.InitDB(ctx, a.inst, a.log); err != nil {
		return err
	}
	if _, err := a.node.EnsureSecrets(); err != nil {
		return err
	}
	if err := a.st.update(func(f *agentFile) { f.FreshPrimary = false; f.LastPrimaryEpoch = st.Epoch }); err != nil {
		return err
	}
	return a.doStartPrimary(ctx, st)
}

func (a *Agent) doStartPrimary(ctx context.Context, st cluster.State) error {
	if err := a.syncSecrets(st); err != nil {
		return fatal(err)
	}
	// Rule 4: the lease first; Postgres accepts writes only while it is
	// held (the lease loop renews it and fences when it runs out).
	if err := a.acquireLease(ctx, st); err != nil {
		a.setPhase("waiting", "Waiting for the cluster to confirm this node may run as the main node…")
		return err
	}
	if err := a.startPG(ctx); err != nil {
		a.dropLease()
		return err
	}
	// A slow start (crash recovery) may have outlived the lease or the
	// term; the watchdog and the lease loop cover it meanwhile, and here
	// it is never declared the main node unless both still hold.
	if why := a.stillMain(st.Epoch); why != "" {
		a.log.Warn("not serving: stopping postgres", "why", why, "epoch", st.Epoch)
		if err := a.doFence(ctx, st, why); err != nil {
			return err
		}
		return errors.New(why)
	}
	a.pushWatchdogLease()
	if err := a.afterStartPrimary(ctx); err != nil {
		_ = a.stopPG()
		return err
	}
	if l, err := a.node.FlushLSN(ctx); err == nil {
		a.setStartLSN(st.Epoch, l)
	}
	a.setPhase("primary", "")
	return nil
}

// afterStartPrimary makes sure the replication and rewind roles match the
// cluster's secrets and the secrets are in the cluster state.
func (a *Agent) afterStartPrimary(ctx context.Context) error {
	s, err := a.node.EnsureSecrets()
	if err != nil {
		return err
	}
	c, err := a.node.Connect(ctx)
	if err != nil {
		return err
	}
	defer c.Close(context.Background())
	if err := pg.EnsureRoles(ctx, c, s); err != nil {
		return fmt.Errorf("set up replication roles: %w", err)
	}
	return a.publishSecrets(ctx, s)
}

func (a *Agent) publishSecrets(ctx context.Context, s pg.Secrets) error {
	if a.cn.State().PGSecrets != nil {
		return nil
	}
	actx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, err := a.cn.Apply(actx, &cluster.SetPGSecrets{Secrets: secretsTo(s)}); err != nil {
		return fmt.Errorf("store the database passwords in the cluster: %w", err)
	}
	a.log.Info("database passwords stored in the cluster state")
	return nil
}

// syncSecrets writes the cluster's passwords to this node if it has none,
// and refuses to go on if they differ.
func (a *Agent) syncSecrets(st cluster.State) error {
	local, err := a.node.Secrets()
	if err == nil {
		if c := st.PGSecrets; c != nil && (local.Superuser != c.Superuser || local.Replication != c.Replication || local.Rewind != c.Rewind) {
			return errors.New("this node's database passwords (cluster.secrets) differ from the cluster's; refusing to continue")
		}
		return nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if st.PGSecrets == nil {
		return nil
	}
	return a.node.WriteSecrets(secretsFrom(*st.PGSecrets))
}

// syncAppSecret keeps the application password in this node's secrets
// file equal to the cluster's (it can change: SetAppPassword).
func (a *Agent) syncAppSecret(st cluster.State) error {
	if st.PGSecrets == nil || st.PGSecrets.App == "" || st.PGSecrets.App == a.appSynced {
		return nil
	}
	local, err := a.node.Secrets()
	if err != nil {
		return err
	}
	if local.App != st.PGSecrets.App {
		local.App = st.PGSecrets.App
		if err := a.node.WriteSecrets(local); err != nil {
			return err
		}
	}
	a.appSynced = st.PGSecrets.App
	return nil
}

// ensureAppRole, on the primary: give the cluster an application password
// if it has none, and make the role (and its database) match it.
func (a *Agent) ensureAppRole(ctx context.Context, st cluster.State) error {
	if st.PGSecrets == nil {
		return nil
	}
	pw := st.PGSecrets.App
	if pw == "" {
		gen, err := pg.GeneratePassword()
		if err != nil {
			return err
		}
		actx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if _, err := a.cn.Apply(actx, &cluster.SetAppPassword{Password: gen}); err != nil {
			return fmt.Errorf("store the application password in the cluster: %w", err)
		}
		return nil // applied on the next pass, from the state
	}
	if pw == a.appApplied {
		return nil
	}
	c, err := a.node.Connect(ctx)
	if err != nil {
		return err
	}
	defer c.Close(context.Background())
	if err := pg.EnsureAppRole(ctx, c, pw); err != nil {
		return fmt.Errorf("application role: %w", err)
	}
	a.appApplied = pw
	a.log.Info("application role ready", "role", pg.AppUser, "database", pg.AppDatabase)
	return nil
}

func (a *Agent) doStartStandby(ctx context.Context, st cluster.State) error {
	if err := a.syncSecrets(st); err != nil {
		return fatal(err)
	}
	// The upstream is always our forwarder; its port may have changed.
	rs, err := a.node.ReadReplState()
	if err != nil {
		return err
	}
	if up := a.fwdUpstream(); rs.Upstream != nil && *rs.Upstream != up {
		rs.Upstream = &up
		if err := a.node.WriteReplState(rs); err != nil {
			return err
		}
	}
	if err := a.startPG(ctx); err != nil {
		return err
	}
	a.setPhase("standby", "")
	return nil
}

func (a *Agent) primaryName(st cluster.State) string {
	if m, ok := st.Members[st.PrimaryID]; ok {
		return m.Name
	}
	return "the main node"
}

// sourceReady reports whether the current primary answers as a primary
// through the tunnel (so clone and rewind are not started against a node
// that is not promoted yet or not reachable).
func (a *Agent) sourceReady(ctx context.Context, s pg.Secrets) error {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	ok, err := pg.UpstreamIsPrimary(cctx, a.fwdUpstream(), s)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("it is not the main node yet")
	}
	return nil
}

func (a *Agent) doClone(ctx context.Context, st cluster.State) error {
	if st.PGSecrets == nil {
		a.setPhase("waiting", "Waiting for the cluster's database passwords…")
		return nil
	}
	s := secretsFrom(*st.PGSecrets)
	from := a.primaryName(st)
	if err := a.sourceReady(ctx, s); err != nil {
		a.log.Info("clone source not ready", "err", err)
		a.setPhase("waiting", fmt.Sprintf("Waiting to reach %s to copy the data…", from))
		return nil
	}
	a.setPhase("cloning", fmt.Sprintf("Copying the data from %s…", from))
	a.setProgress(0)
	lastShown := -1
	err := a.node.Clone(ctx, pg.CloneOptions{
		Install: a.inst, Guard: a.guard, Source: a.fwdUpstream(), Secrets: s,
		Name: a.name, Port: a.pgPort, Logger: a.log,
		Progress: func(pct int) {
			a.setProgress(pct)
			if step := pct / 10 * 10; step > lastShown {
				lastShown = step
				a.say("  %d%%", step)
			}
		},
	})
	a.setProgress(-1)
	if err != nil {
		return fmt.Errorf("copy from %s: %w", from, err)
	}
	if err := a.st.update(func(f *agentFile) { f.CutEpoch, f.CutUntil = 0, time.Time{} }); err != nil {
		return err
	}
	a.say("Copy complete.")
	return a.doStartStandby(ctx, st)
}

// doRejoin is rule 6: this node's data may hold writes nobody else has.
// The marker is written first (before Postgres is stopped or started),
// then the data is rewound (or re-cloned) from the current primary
// through the tunnel, and the node starts as a standby.
func (a *Agent) doRejoin(ctx context.Context, st cluster.State) error {
	from := a.primaryName(st)
	if m, err := a.node.ReadMarker(); err != nil {
		return err
	} else if m == nil {
		if err := a.node.Demote(ctx, a.inst, fmt.Sprintf("cluster epoch %d: %s is the main node", st.Epoch, from)); err != nil {
			return fmt.Errorf("write the rejoin marker: %w", err)
		}
	}
	if srv, _ := a.currentPG(); srv != nil {
		a.say("%s is now the main node; this node steps down and rejoins as a copy.", from)
		if err := a.stopPG(); err != nil {
			return err
		}
	}
	if err := a.syncSecrets(st); err != nil {
		return fatal(err)
	}
	s, err := a.node.Secrets()
	if err != nil {
		return err
	}
	if err := a.sourceReady(ctx, s); err != nil {
		a.log.Info("rejoin source not ready", "err", err)
		a.setPhase("waiting", fmt.Sprintf("Waiting to reach %s (the main node) to rejoin as a copy…", from))
		return nil
	}
	a.setPhase("rejoining", fmt.Sprintf("Rejoining as a copy of %s (rewind through the tunnel)…", from))
	recloned, err := a.node.RewindOrReclone(ctx, pg.RewindOptions{
		Install: a.inst, Guard: a.guard, Source: a.fwdUpstream(), Logger: a.log,
	})
	if err != nil {
		return fmt.Errorf("rejoin from %s: %w", from, err)
	}
	if recloned {
		a.say("Rewind was not possible; copied the data again from %s.", from)
	} else {
		a.say("Rewound to %s's history.", from)
	}
	if err := a.st.update(func(f *agentFile) { f.CutEpoch, f.CutUntil = 0, time.Time{} }); err != nil {
		return err
	}
	a.followChecked = 0
	return a.doStartStandby(ctx, st)
}

// doPromote: the cluster (confirmed) names this standby primary. Its
// stream is cut first (rule 2, normally already done by the switch), the
// epoch is remembered before promoting (rule 6 on restart), the sync list
// is set from R so commits wait for a copy from the first write (rule 3),
// then pg_promote and slots for every other data node (rule 7).
func (a *Agent) doPromote(ctx context.Context, st cluster.State) error {
	rs, err := a.node.ReadReplState()
	if err != nil {
		return err
	}
	if rs.Upstream != nil {
		if _, err := a.node.CutStream(ctx, 30*time.Second); err != nil {
			return fmt.Errorf("cut the stream before promoting: %w", err)
		}
	}
	// PromotingEpoch (D56) stays set until Postgres is out of recovery and
	// checkpointed on its new timeline: primary data found with it set is
	// a promotion cut short, started as a standby again, never as a
	// primary on the old timeline.
	if err := a.st.update(func(f *agentFile) {
		f.LastPrimaryEpoch, f.PromotingEpoch = st.Epoch, st.Epoch
		f.CutEpoch, f.CutUntil = 0, time.Time{}
	}); err != nil {
		return err
	}
	names, k := syncNamesFor(st, a.cn.ID())
	if err := a.node.SetSync(ctx, k, names, 30*time.Second); err != nil {
		return fmt.Errorf("set the sync list before promoting: %w", err)
	}
	// Rule 4: hold the lease before Postgres can accept a single write.
	if err := a.acquireLease(ctx, st); err != nil {
		return err
	}
	a.pushWatchdogLease()
	var members []string
	for _, m := range st.DataMembers() {
		if m.ID != a.cn.ID() && m.Status == cluster.StatusActive {
			members = append(members, m.Name)
		}
	}
	po := pg.PromoteOptions{Members: members}
	tli, err := a.node.Promote(ctx, po)
	switch {
	case !errors.Is(err, pg.ErrAlreadyPrimary):
		// This call found a standby, so any later "already a primary"
		// in this term is our own promotion.
		a.promotedEpoch = st.Epoch
	case a.promotedEpoch == st.Epoch:
		// Our own earlier try in this term got past pg_promote and then
		// failed (a slot step, say): finish the steps instead of
		// retrying the promote forever. A Postgres that was already a
		// primary before our first try is never finished here.
		a.log.Info("finishing an interrupted promotion", "epoch", st.Epoch)
		tli, err = a.node.FinishPromote(ctx, po)
	}
	if err != nil {
		return fmt.Errorf("promote: %w", err)
	}
	if err := a.st.update(func(f *agentFile) { f.PromotingEpoch = 0 }); err != nil {
		return err
	}
	// The lease loop and the watchdog covered the promotion; never call it
	// done if the lease or the term did not survive it.
	if why := a.stillMain(st.Epoch); why != "" {
		a.log.Warn("not serving after the promotion: stopping postgres", "why", why, "epoch", st.Epoch)
		if err := a.doFence(ctx, st, why); err != nil {
			return err
		}
		return errors.New(why)
	}
	a.setPGRole("primary")
	if l, err := a.node.FlushLSN(ctx); err == nil {
		a.setStartLSN(st.Epoch, l)
	}
	a.following = ""
	a.setPhase("primary", fmt.Sprintf("This node is now the main node (term %d, timeline %d).", st.Epoch, tli))
	return nil
}

// doResumeStandby (D56): this node's promotion in the current term was cut
// short (power cut, crash, fence) after Postgres had removed
// standby.signal, so its data looks like primary data still on the old
// main node's timeline. It is started as a standby again, with the stream
// still cut; the next round promotes it (actPromote), onto a new timeline.
func (a *Agent) doResumeStandby(ctx context.Context, st cluster.State) error {
	a.log.Warn("the promotion of this node did not finish; starting it as a copy again to promote it anew", "epoch", st.Epoch)
	a.say("This node's promotion to main node (term %d) was interrupted; finishing it.", st.Epoch)
	if err := a.node.ResumeStandby(); err != nil {
		return fmt.Errorf("resume an interrupted promotion: %w", err)
	}
	return a.doStartStandby(ctx, st)
}

// doRepoint resumes a cut stream towards the current primary.
func (a *Agent) doRepoint(ctx context.Context, st cluster.State) error {
	if err := a.node.SetUpstream(ctx, a.fwdUpstream(), 60*time.Second); err != nil {
		return fmt.Errorf("follow %s: %w", a.primaryName(st), err)
	}
	return a.st.update(func(f *agentFile) { f.CutEpoch, f.CutUntil = 0, time.Time{} })
}

func (a *Agent) doFence(ctx context.Context, st cluster.State, reason string) error {
	if err := a.node.Demote(ctx, a.inst, fmt.Sprintf("fenced at cluster epoch %d", st.Epoch)); err != nil {
		return err
	}
	if err := a.stopPG(); err != nil {
		return err
	}
	a.setPhase("waiting", reason)
	return nil
}

// doClearFence: this node fenced itself at the current epoch and a
// confirmed read still names it primary (nobody else was promoted). Its
// data is the latest; drop the marker so it starts again (as a primary
// through the usual confirmed path, or as a standby first if it was
// fenced mid-promotion).
func (a *Agent) doClearFence(st cluster.State) error {
	a.log.Warn("fenced at this epoch and still the main node: starting again", "epoch", st.Epoch)
	a.say("The cluster still names this node as the main node (term %d) and no other node took over; starting its database again.", st.Epoch)
	if os.Getenv(faultEnv) == "slow-start-after-clear-fence" {
		a.slowStart = true
	}
	return a.node.ClearMarker()
}

// doReclone is rule 8 on the member itself: copy the data again from the
// primary (the old data stays until the new copy is complete).
func (a *Agent) doReclone(ctx context.Context, st cluster.State) error {
	from := a.primaryName(st)
	if err := a.syncSecrets(st); err != nil {
		return fatal(err)
	}
	s, err := a.node.Secrets()
	if err != nil {
		return err
	}
	if err := a.sourceReady(ctx, s); err != nil {
		a.setPhase("waiting", fmt.Sprintf("This node was away too long; waiting to reach %s to copy the data again…", from))
		return nil
	}
	if srv, _ := a.currentPG(); srv != nil {
		if err := a.stopPG(); err != nil {
			return err
		}
	}
	if a.node.Initialized() {
		if err := a.node.WriteMarker(pg.Marker{Reason: pg.ReasonRecloning, Since: time.Now().UTC(), Detail: "away too long (rule 8)"}); err != nil {
			return err
		}
	}
	a.setPhase("cloning", fmt.Sprintf("This node was away too long; copying the data again from %s…", from))
	a.setProgress(0)
	err = a.node.Clone(ctx, pg.CloneOptions{
		Install: a.inst, Guard: a.guard, Source: a.fwdUpstream(), Secrets: s,
		Name: a.name, Port: a.pgPort, Reclone: a.node.Initialized(), Logger: a.log,
		Progress: func(pct int) { a.setProgress(pct) },
	})
	a.setProgress(-1)
	if err != nil {
		return fmt.Errorf("copy again from %s: %w", from, err)
	}
	if err := a.st.update(func(f *agentFile) { f.Recloned = true; f.CutEpoch, f.CutUntil = 0, time.Time{} }); err != nil {
		return err
	}
	a.followChecked = 0
	a.say("Copied the data again from %s.", from)
	if err := a.doStartStandby(ctx, st); err != nil {
		return err
	}
	return a.doMarkActive(ctx, st)
}

// doMarkActive: the re-clone is done; this node is a full member again
// (the controller adds it back to the sync set once it has caught up).
func (a *Agent) doMarkActive(ctx context.Context, _ cluster.State) error {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	act := cluster.StatusActive
	if _, err := a.cn.Apply(cctx, &cluster.UpdateMember{ID: a.cn.ID(), Status: &act}); err != nil {
		return fmt.Errorf("tell the cluster the copy is fresh: %w", err)
	}
	return a.st.update(func(f *agentFile) { f.Recloned = false })
}

// becomeRemoved stops Postgres for good and ends the agent.
func (a *Agent) becomeRemoved(by string) error {
	why := "removed from the cluster"
	if by != "" {
		why += " by " + by
	}
	_ = a.stopPG()
	if err := a.st.update(func(f *agentFile) { f.Removed = why }); err != nil {
		a.log.Error("record removal", "err", err)
	}
	a.log.Warn("this node is no longer a member; stopping", "why", why)
	return fatal(&ExitError{Code: 1, Msg: removedMessage(why, a.node.StateDir)})
}

func removedMessage(why, dir string) string {
	return fmt.Sprintf("This node was %s, so its database was stopped. The data is still in %s. "+
		"To use this machine in a cluster again, start with an empty data folder and a new invite (`%s join --data-dir <new folder>`, then paste the code).",
		why, dir, brand.Command)
}

// ---- steady state ----

func (a *Agent) steady(ctx context.Context, st cluster.State) error {
	srv, role := a.currentPG()
	if srv == nil {
		return nil
	}
	switch role {
	case "primary":
		a.setPhase("primary", "")
		if err := a.primaryDuties(ctx, st); err != nil {
			return err
		}
	case "standby":
		a.setPhase("standby", "")
		if err := a.standbyDuties(ctx, st); err != nil {
			return err
		}
	}
	a.announceReady(st)
	return nil
}

func (a *Agent) reconcileWitness(ctx context.Context, st cluster.State) error {
	if !st.Bootstrapped() {
		a.setPhase("waiting", "Waiting to reach the other nodes…")
		return nil
	}
	if _, ok := st.Members[a.cn.ID()]; ok {
		a.seenMember = true
	} else {
		if a.seenMember {
			return a.becomeRemoved("")
		}
		cst, err := a.confirm(ctx)
		if isDenied(err) {
			return a.becomeRemoved("")
		}
		if err != nil {
			a.setPhase("waiting", "Waiting to reach the other nodes…")
			return nil
		}
		if _, ok := cst.Members[a.cn.ID()]; !ok {
			return a.becomeRemoved("")
		}
		a.seenMember = true
	}
	a.setPhase("witness", "")
	a.announceReady(st)
	return nil
}

// announceReady prints the banner once per run, the first time the node
// reaches its role.
func (a *Agent) announceReady(st cluster.State) {
	if a.ready {
		return
	}
	a.ready = true
	_, role := a.currentPG()
	a.say("")
	switch {
	case a.role == cluster.RoleWitness:
		a.say("Witness %s is running: it votes in the cluster and keeps no data.", a.name)
	default:
		a.say("PostgreSQL %s is running on 127.0.0.1:%d.", pg.MajorVersion, a.pgPort)
		if role == "primary" {
			a.say("  Node:      %s, main node (primary)", a.name)
		} else {
			a.say("  Node:      %s, copy (read-only standby) of %s", a.name, a.primaryName(st))
		}
		a.say("  Connect:   %s", pg.ConnString(a.pgPort, "", false))
		a.say("  Password:  %s conninfo --data-dir %s", brand.Command, quoteArg(a.node.StateDir))
		label := "Backups:"
		for _, l := range a.backupBanner(st) {
			a.say("  %-10s %s", label, l)
			label = ""
		}
	}
	a.say("  Add nodes: %s invite --data-dir %s", brand.Command, quoteArg(a.node.StateDir))
	a.say("  Logs:      %s", a.node.LogDir())
	a.say("Press Ctrl-C to stop.")
}

// standbyDuties: announce streaming; after every change of primary, check
// that this copy can follow the new one without a rewind. A copy that
// holds WAL the new primary never had (it received more from the old
// primary than the one that was promoted) can never stream from it: it
// is rewound like a former primary. The sync set is the controller's job.
func (a *Agent) standbyDuties(ctx context.Context, st cluster.State) error {
	if err := a.syncAppSecret(st); err != nil {
		a.log.Warn("application password", "err", err)
	}
	if err := a.refreshPGTLS(ctx, st); err != nil {
		a.log.Warn("client TLS certificate", "err", err)
	}
	if st.PrimaryID != "" && st.PrimaryID != a.cn.ID() && a.followChecked != st.Epoch {
		rs, err := a.node.ReadReplState()
		if err != nil {
			return err
		}
		if rs.Upstream != nil {
			s, err := a.node.Secrets()
			if err != nil {
				return err
			}
			cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			ok, err := a.node.FollowCheck(cctx, a.fwdUpstream(), s)
			cancel()
			switch {
			case err != nil:
				a.log.Info("cannot check yet whether this copy can follow the main node", "err", err)
			case ok:
				a.followChecked = st.Epoch
			default:
				a.log.Warn("this copy has WAL the new main node never had; rewinding", "primary", a.primaryName(st))
				unlock, err := a.lockPG(ctx)
				if err != nil {
					return err
				}
				defer unlock()
				if err := a.node.Demote(ctx, a.inst, fmt.Sprintf("cluster epoch %d: diverged from %s", st.Epoch, a.primaryName(st))); err != nil {
					return err
				}
				return a.doRejoin(ctx, st)
			}
		}
	}
	streaming := a.isStreaming(ctx)
	if done, err := a.watchDiverged(ctx, st, streaming); done || err != nil {
		return err
	}
	if streaming && a.following != st.PrimaryID {
		a.following = st.PrimaryID
		a.say("Following %s live.", a.primaryName(st))
	}
	if !streaming {
		a.following = ""
	}
	a.watchReceiver(ctx, st, streaming)
	return nil
}

func (a *Agent) isStreaming(ctx context.Context) bool {
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	c, err := a.node.Connect(cctx)
	if err != nil {
		return false
	}
	defer c.Close(context.Background())
	var k int
	if err := c.QueryRow(cctx, "SELECT count(*) FROM pg_stat_wal_receiver WHERE status = 'streaming'").Scan(&k); err != nil {
		return false
	}
	return k > 0
}
