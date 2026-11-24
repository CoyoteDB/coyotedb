package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"coyotedb.com/coyote/internal/cluster"
	"coyotedb.com/coyote/internal/controller"
	"coyotedb.com/coyote/internal/pg"
)

// The primary lease (safety rule 4), the primary's side. A node runs
// Postgres as a primary (or promotes it) only while it holds the lease:
//
//   - acquireLease renews the lease once, synchronously, for the epoch the
//     cluster names this node primary in; only then does the caller start
//     or promote Postgres;
//   - leaseLoop renews every renew interval and, when renewals stop
//     succeeding, fences: a fast stop at 60% of the lease, immediate at
//     80%, kill at 90% (controller.LeaseKeeper does the math, counting from
//     when each renewal was sent, on a suspend-aware monotonic clock);
//   - after each renewal the watchdog is told the absolute deadline
//     (WatchdogAt of the lease after that renewal was sent, on the
//     suspend-aware clock both processes share, D39) until which Postgres
//     may run without another one, so a frozen agent is fenced too;
//   - when the cluster definitively moves on (another epoch or primary),
//     the lease is lost at once.
//
// After a fence the node keeps a "demoted" marker carrying the fenced
// epoch. If a confirmed read still names it primary at that very epoch
// (no one else was promoted, e.g. after a laptop sleep with no failover
// possible), the marker is cleared and it starts as primary again;
// otherwise it rejoins as a copy like any former primary (rule 6).
type leaseState struct {
	// pushMu is held across working out a watchdog deadline and writing
	// it, so the watchdog receives deadlines in the order they were
	// worked out (never an older one after a newer one). Taken before mu,
	// never while holding mu.
	pushMu   sync.Mutex
	mu       sync.Mutex
	k        *controller.LeaseKeeper
	epoch    uint64
	interval time.Duration
	lastSent controller.Reading
	inflight int // renewals sent and not answered yet
	fencing  bool
	// served: the cluster has recorded this node as LastServed for epoch
	// (sent with a renewal once servedReady). servedReady is set by the
	// primary's duties once k copies in S hold all the WAL it had when it
	// began serving (startLSN).
	served      bool
	servedReady bool
	startLSN    pg.LSN
	startSet    bool
	// unreached: renewals are held back because no Raft leader has
	// reached this node for too long (D62); said once per stretch.
	unreached bool
}

// contactLimit is how long without a Raft leader reaching this node the
// main node keeps renewing its lease (D62): about a third of the lease,
// never less than an ordinary leader election.
func (a *Agent) contactLimit(lease time.Duration) time.Duration {
	t := a.cn.Timing()
	return max(lease*3/10, t.HeartbeatTimeout+t.ElectionTimeout)
}

// renewFrom is the moment a renewal sent at now may count from: never
// later than the last time a Raft leader reached this node (D62). The
// lease thus proves contact both ways: this node reached the leader (the
// renewal was applied) and the leader reached this node. The contact age
// comes from Go's monotonic clock, which may undercount a suspend; that
// only makes the shift smaller, never the lease longer than from now.
func renewFrom(now controller.Reading, contactAge time.Duration) controller.Reading {
	if contactAge > 0 {
		now.Mono -= min(contactAge, now.Mono-1)
	}
	return now
}

// leaseInterval is how often the primary renews: the setting, but never
// more than a third of the lease it holds.
func leaseInterval(st cluster.State, lease time.Duration) time.Duration {
	iv := st.Settings.LeaseRenewInterval
	if iv <= 0 || iv > lease/3 {
		iv = lease / 3
	}
	return max(iv, cluster.MinLeaseRenewInterval)
}

func leaseDuration(st cluster.State) time.Duration {
	if st.Lease.Duration > 0 {
		return st.Lease.Duration
	}
	return st.Settings.LeaseDuration
}

// holdsLease reports whether this node holds a live lease for epoch.
func (a *Agent) holdsLease(epoch uint64) bool {
	l := &a.lease
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.k != nil && l.epoch == epoch && !l.fencing && l.k.Phase(a.clock.Now()) == controller.LeaseHeld
}

func (a *Agent) isFencing() bool {
	a.lease.mu.Lock()
	defer a.lease.mu.Unlock()
	return a.lease.fencing
}

// acquireLease takes the lease for st.Epoch with one synchronous renewal.
// It must succeed before Postgres runs as a primary.
func (a *Agent) acquireLease(ctx context.Context, st cluster.State) error {
	if a.holdsLease(st.Epoch) {
		return nil
	}
	if a.stepHold.blocks(st.Epoch) {
		return errSteppedDown
	}
	dur := leaseDuration(st)
	if dur <= 0 {
		return errors.New("the cluster has no lease length set")
	}
	k := controller.NewLeaseKeeper(dur)
	sent := a.clock.Now()
	age := a.cn.LeaderContactAge()
	if age > a.contactLimit(dur) {
		return fmt.Errorf("could not take the main-node lease: no leader has reached this node for %s (the other nodes may not be able to reach this computer)", roundAge(age))
	}
	tok := k.Sent(renewFrom(sent, age), st.Epoch)
	cctx, cancel := context.WithTimeout(ctx, dur/2)
	defer cancel()
	if _, err := a.cn.Apply(cctx, &cluster.RenewLease{NodeID: a.cn.ID(), Epoch: st.Epoch}); err != nil {
		return fmt.Errorf("could not take the main-node lease (the cluster must confirm this node is the main node): %w", err)
	}
	k.Acked(tok, st.Epoch)
	now := a.clock.Now()
	if k.Phase(now) != controller.LeaseHeld {
		return fmt.Errorf("taking the main-node lease took too long (%s)", now.Sub(sent).Round(time.Millisecond))
	}
	l := &a.lease
	l.mu.Lock()
	if l.fencing {
		l.mu.Unlock()
		return errors.New("still stopping the database after losing the lease")
	}
	l.k, l.epoch, l.interval, l.lastSent = k, st.Epoch, leaseInterval(st, dur), sent
	l.served, l.servedReady, l.startSet, l.unreached = false, false, false, false
	l.mu.Unlock()
	a.log.Info("primary lease taken", "epoch", st.Epoch, "lease", dur)
	a.pushWatchdogLease()
	return nil
}

// leaseDeadline is the watchdog deadline (an internal/mono reading) for a
// primary Postgres under the held lease (false: no lease, it is being
// fenced, or it no longer counts as held).
func (a *Agent) leaseDeadline() (time.Duration, bool) {
	l := &a.lease
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.k == nil || l.fencing || l.k.Phase(a.clock.Now()) != controller.LeaseHeld {
		return 0, false
	}
	return l.k.WatchdogDeadline()
}

// primaryStart tracks a primary start from before its deadline is taken
// until its process exists (or the start gave up), so a fence in that
// window can veto it instead of finding nothing to stop (F3).
type primaryStart struct {
	veto    bool          // under pgMu: a fence ran; the start must not spawn
	spawned chan struct{} // closed once the process exists or the start gave up
	once    sync.Once
}

func (p *primaryStart) settled() { p.once.Do(func() { close(p.spawned) }) }

// beginPrimaryStart registers a primary start (one at a time: pgSem).
func (a *Agent) beginPrimaryStart() *primaryStart {
	p := &primaryStart{spawned: make(chan struct{})}
	a.pgMu.Lock()
	a.primaryStart = p
	a.pgMu.Unlock()
	return p
}

// endPrimaryStart: the start returned (its process, if any, is a.srv or
// gone).
func (a *Agent) endPrimaryStart(p *primaryStart) {
	a.pgMu.Lock()
	if a.primaryStart == p {
		a.primaryStart = nil
	}
	a.pgMu.Unlock()
	p.settled()
}

// startDeadline is StartOptions.PrimaryDeadline for p: called right
// before the spawn; refuses once a fence vetoed the start or the lease is
// not held any more.
func (a *Agent) startDeadline(p *primaryStart) (time.Duration, bool) {
	a.pgMu.Lock()
	veto := p.veto
	a.pgMu.Unlock()
	if veto {
		return 0, false
	}
	return a.leaseDeadline()
}

// spawnedPrimary is StartOptions.OnSpawn for p: from here the lease loop
// renews and fences the starting server, and the watchdog gets the
// newest deadline (renewals may have landed since it was armed).
func (a *Agent) spawnedPrimary(p *primaryStart, s *pg.Server) {
	a.pgMu.Lock()
	a.starting = s
	a.pgMu.Unlock()
	p.settled()
	a.pushWatchdogLease()
}

// fenceTarget is the Postgres a fence must stop: the running or starting
// one; or, while a primary start is between taking its deadline and
// spawning, the start is vetoed and the fence waits to see whether a
// process came to exist anyway (then that one is stopped).
func (a *Agent) fenceTarget() *pg.Server {
	if srv := a.leasePG(); srv != nil {
		return srv
	}
	a.pgMu.Lock()
	p := a.primaryStart
	if p != nil {
		p.veto = true
	}
	a.pgMu.Unlock()
	if p == nil {
		return nil
	}
	<-p.spawned
	return a.leasePG()
}

// stillMain says why a primary that has just finished starting must not
// serve ("" if it may): the lease must still be held for epoch and the
// cluster must still name this node main node at that epoch.
func (a *Agent) stillMain(epoch uint64) string {
	if !a.holdsLease(epoch) {
		return "the main-node lease ran out while PostgreSQL was starting"
	}
	if st := a.cn.State(); st.Epoch != epoch || st.PrimaryID != a.cn.ID() {
		return fmt.Sprintf("the cluster moved on while PostgreSQL was starting (term %d, main node %s)", st.Epoch, a.primaryName(st))
	}
	return ""
}

// dropLease forgets the lease (this node is not a primary any more).
func (a *Agent) dropLease() {
	l := &a.lease
	l.pushMu.Lock()
	defer l.pushMu.Unlock()
	l.mu.Lock()
	had := l.k != nil
	l.k = nil
	l.mu.Unlock()
	if had {
		if srv := a.leasePG(); srv != nil {
			_ = srv.SetLeaseUntil(a.guard, 0)
		}
	}
}

// setStartLSN records the WAL end at which this primary began serving.
func (a *Agent) setStartLSN(epoch uint64, lsn pg.LSN) {
	l := &a.lease
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.epoch == epoch && !l.startSet {
		l.startLSN, l.startSet = lsn, true
	}
}

// pushWatchdogLease tells the watchdog until when Postgres may run: the
// lease's absolute deadline, fixed by the newest acknowledged renewal's
// send time. However late this line is written, the deadline in it is
// never later than the lease allows; pushMu keeps the lines in order.
func (a *Agent) pushWatchdogLease() {
	l := &a.lease
	l.pushMu.Lock()
	defer l.pushMu.Unlock()
	srv := a.leasePG()
	if srv == nil {
		return
	}
	l.mu.Lock()
	var dl time.Duration
	active := l.k != nil
	if active {
		dl, _ = l.k.WatchdogDeadline()
	}
	l.mu.Unlock()
	if !active {
		return
	}
	// 0 means "no limit" to the watchdog: never send it for a primary.
	if err := srv.SetLeaseUntil(a.guard, max(dl, 1)); err != nil {
		a.log.Warn("could not tell the watchdog about the lease", "err", err)
	}
}

// renewRetry is how often another renewal goes out while one is
// unanswered; maxRenewInflight caps how many are outstanding at once.
const (
	renewRetry       = 250 * time.Millisecond
	maxRenewInflight = 24
)

// leaseLoop renews the lease and fences when it runs out.
func (a *Agent) leaseLoop(ctx context.Context) {
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		a.leaseStep(ctx)
	}
}

func (a *Agent) leaseStep(ctx context.Context) {
	l := &a.lease
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.k == nil || l.fencing {
		return
	}
	st := a.cn.State()
	if st.Epoch > l.epoch || (st.Epoch == l.epoch && st.PrimaryID != a.cn.ID()) {
		l.k.Lost(fmt.Sprintf("the cluster moved on (term %d, main node %s)", st.Epoch, a.primaryName(st)))
	}
	now := a.clock.Now()
	switch phase := l.k.Phase(now); phase {
	case controller.LeaseHeld:
		// One renewal per interval while they are answered. While one is
		// unanswered (a dead or cut-off Raft leader, a stalled disk),
		// another goes out every renewRetry: each looks up the leader
		// afresh, so the first one sent after an election (or after the
		// stall) renews the lease, and none waits behind a dead peer.
		// Every renewal may take until the lease starts stopping
		// Postgres: an answer after that is no use.
		since := now.Sub(l.lastSent)
		due := (l.inflight == 0 && since >= l.interval) ||
			(l.inflight > 0 && l.inflight < maxRenewInflight && since >= min(renewRetry, l.interval))
		// A main node that no leader reaches stops renewing (D62). It can
		// often still reach the leader (dialing out works when its address
		// changed or a firewall drops incoming connections), so renewals
		// would succeed while no copy can connect to it and every write
		// waits: the lease would hide a total outage. Held back, the lease
		// runs out, this node stops its database, and the controller fails
		// over the normal way (rule 2 cut, then promote).
		age := a.cn.LeaderContactAge()
		if due && age > a.contactLimit(l.k.Lease()) {
			due = false
			if !l.unreached {
				l.unreached = true
				a.log.Warn("no leader has reached this node; not renewing the main-node lease", "contact_age", roundAge(age))
				a.say("No other node has reached this one for %s, so it stops renewing its main-node lease: if that lasts, its database stops and another node takes over. "+
					"Check that the other computers can reach this one at %s.", roundAge(age), strings.Join(a.cn.AdvertiseAddrs(), ", "))
			}
		} else if due && l.unreached {
			l.unreached = false
			a.log.Info("a leader reaches this node again; renewing the main-node lease")
		}
		if due {
			l.inflight++
			l.lastSent = now
			tok := l.k.Sent(renewFrom(now, age), l.epoch)
			timeout := max(l.k.Until(now, controller.FenceFastAt), l.interval)
			go a.renew(ctx, tok, l.epoch, l.servedReady && !l.served, timeout)
		}
	case controller.LeaseNone:
	default:
		l.fencing = true
		k, epoch := l.k, l.epoch
		fast := max(k.Until(now, controller.FenceImmediateAt), 0)
		imm := max(k.Until(now, controller.FenceKillAt)-fast, 0)
		why := k.LostReason()
		if why != "" {
			// Another node may already be the main node: no fast stop.
			fast, imm = 0, 300*time.Millisecond
		} else {
			why = fmt.Sprintf("the lease was not renewed for %s", k.Elapsed(now).Round(100*time.Millisecond))
		}
		go a.fence(epoch, phase, why, fast, imm)
	}
}

func (a *Agent) renew(ctx context.Context, tok, epoch uint64, served bool, timeout time.Duration) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, err := a.cn.Apply(cctx, &cluster.RenewLease{NodeID: a.cn.ID(), Epoch: epoch, Served: served})
	l := &a.lease
	l.mu.Lock()
	l.inflight--
	if l.k == nil || l.epoch != epoch {
		l.mu.Unlock()
		return
	}
	switch {
	case err == nil:
		l.k.Acked(tok, epoch)
		if served {
			l.served = true
			a.log.Info("all earlier writes are on the copies; recorded as the node that serves", "epoch", epoch)
		}
	case errors.Is(err, cluster.ErrEpochMismatch) || errors.Is(err, cluster.ErrNotPrimary):
		l.k.Lost(err.Error())
	default:
		a.log.Info("lease renewal failed (will retry)", "err", err)
	}
	l.mu.Unlock()
	if err == nil {
		a.pushWatchdogLease()
	}
}

// fence stops Postgres because the lease ran out (or was lost), writes the
// demoted marker with the epoch, and forgets the lease.
func (a *Agent) fence(epoch uint64, phase controller.LeasePhase, why string, fast, imm time.Duration) {
	defer func() {
		a.lease.mu.Lock()
		a.lease.k, a.lease.fencing = nil, false
		a.lease.mu.Unlock()
		a.nudge()
	}()
	srv := a.fenceTarget() // also a Postgres still starting, or about to be spawned
	if srv == nil {
		return
	}
	if os.Getenv(faultEnv) == stallSpawnFault {
		a.stallSpawn.Store(true) // the next primary start comes after a cleared fence
	}
	a.log.Warn("primary lease lost: stopping postgres", "why", why, "phase", phase.String(), "epoch", epoch, "fast_for", fast, "immediate_for", imm)
	a.say("This node lost its main-node lease (%s); its database is stopped so no other node's writes can conflict.", why)
	done := make(chan error, 1)
	go func() { done <- srv.Fence(fast, imm) }()
	// The marker is written while Postgres stops: if this process dies
	// right now, a restart still goes through a confirmed read (rule 6).
	mctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := a.node.Fenced(mctx, a.inst, epoch, "lease lost: "+strings.TrimSpace(why)); err != nil {
		a.log.Error("could not write the demoted marker", "err", err)
	}
	cancel()
	if err := <-done; err != nil {
		a.log.Error("fence: postgres did not stop", "err", err)
		return
	}
	a.clearPG(srv)
	a.log.Warn("postgres stopped (fenced)", "epoch", epoch)
}

// clearPG forgets srv if it is still the current server (it exited).
func (a *Agent) clearPG(srv *pg.Server) {
	a.pgMu.Lock()
	defer a.pgMu.Unlock()
	if a.srv == srv {
		a.srv, a.pgRole = nil, ""
		empty := ""
		a.pgAddr.Store(&empty)
	}
}

// computeServed: once k copies in S have flushed the WAL this primary had
// when it began serving, it may be recorded as LastServed (so a later
// failover counts rule 2 over R without it). With no S there is nothing
// to wait for.
func (a *Agent) computeServed(ctx context.Context, epoch uint64) {
	l := &a.lease
	l.mu.Lock()
	if l.k == nil || l.epoch != epoch || l.servedReady || !l.startSet {
		l.mu.Unlock()
		return
	}
	start := l.startLSN
	l.mu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	c, err := a.node.Connect(cctx)
	if err != nil {
		return
	}
	defer c.Close(context.Background())
	rs, err := pg.ReplicationStatus(cctx, c)
	if err != nil || rs.Role != "primary" {
		return
	}
	k, names, err := pg.ParseSyncNames(rs.SyncStandbyNames)
	if err != nil {
		return
	}
	ok := 0
	for _, r := range rs.Standbys {
		for _, n := range names {
			if strings.EqualFold(n, r.Name) && r.FlushLSN != nil && *r.FlushLSN >= start {
				ok++
			}
		}
	}
	if len(names) == 0 || ok >= k {
		l.mu.Lock()
		if l.epoch == epoch {
			l.servedReady = true
		}
		l.mu.Unlock()
	}
}

// roundAge shows a contact age ("never" for no contact at all).
func roundAge(d time.Duration) string {
	if d > 24*time.Hour {
		return "a long time"
	}
	return d.Round(100 * time.Millisecond).String()
}
