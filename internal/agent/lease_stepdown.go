package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"coyotedb.com/coyote/internal/cluster"
	"coyotedb.com/coyote/internal/controller"
	"coyotedb.com/coyote/internal/mono"
)

// Planned switch, old main node's side (F6, D41). Before the switch
// records the new main node, the old one stops its Postgres and confirms
// (step_down); it then does not take its lease again for the same term
// until the switch is over: committed (the term moves on), cancelled
// (step_up), or stepDownHold passed. If the old main node cannot be
// reached, the switch waits out its lease instead (switch.go).

// stepDownHold: how long a stepped-down main node stays down for a switch
// that never says how it ended. The switch records the new main node
// within half its cut hold of the first cut, with a write bounded by
// writeTimeout; past that it can no longer succeed.
const stepDownHold = controller.CutHold/2 + 10*time.Second

// stepDownLockWait: how long a step-down waits for the Postgres lock
// before it goes ahead without it. The switch gives step_down 8 s in all.
const stepDownLockWait = 500 * time.Millisecond

type stepDownReq struct {
	Epoch uint64 `json:"epoch"`
}

// stepHoldClock: a step-down in force for epoch until the monotonic
// deadline.
type stepHoldClock struct {
	mu    sync.Mutex
	epoch uint64
	until time.Duration
}

func (h *stepHoldClock) set(epoch uint64, until time.Duration) {
	h.mu.Lock()
	h.epoch, h.until = epoch, until
	h.mu.Unlock()
}

func (h *stepHoldClock) clear(epoch uint64) {
	h.mu.Lock()
	if h.epoch == epoch {
		h.epoch, h.until = 0, 0
	}
	h.mu.Unlock()
}

// blocks reports whether a step-down keeps this node from taking the
// lease for epoch now.
func (h *stepHoldClock) blocks(epoch uint64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.epoch == epoch && mono.Now() < h.until
}

// handleStepDown serves the step_down and step_up RPCs.
func (a *Agent) handleStepDown(ctx context.Context, method string, body json.RawMessage) (any, error) {
	var r stepDownReq
	if err := decodeBody(body, &r); err != nil {
		return nil, err
	}
	if method == "step_up" {
		a.stepHold.clear(r.Epoch)
		a.nudge()
		return nil, nil
	}
	return nil, a.stepDown(ctx, r.Epoch)
}

// stepDown stops this main node's Postgres for a planned switch at epoch
// and returns once it has exited. From the moment it is asked no renewal
// goes out, and the lease is not taken again for epoch while the hold
// lasts. Safe to repeat.
func (a *Agent) stepDown(ctx context.Context, epoch uint64) error {
	switch st := a.cn.State(); {
	case st.Epoch != epoch:
		return fmt.Errorf("this node is at term %d, not %d", st.Epoch, epoch)
	case st.PrimaryID != a.cn.ID():
		return errors.New("this node is not the main node")
	}
	// The Postgres lock keeps this off a start or a duty in progress, but
	// only for a moment: a main node whose Postgres hangs (D2, D58) may
	// hold it for as long as the hang lasts (a duty waiting on a query),
	// and that is exactly when it must step down. Without it, this is the
	// lease fence's path, which runs without the lock too: fenceTarget
	// finds the running or starting server, or vetoes a start not yet
	// spawned; and the lease cannot be taken again (fencing, stepHold).
	lctx, lcancel := context.WithTimeout(ctx, stepDownLockWait)
	if unlock, err := a.lockPG(lctx); err == nil {
		defer unlock()
	} else {
		a.log.Warn("planned switch: the database is busy (it may hang); stepping down without waiting for it")
	}
	lcancel()
	l := &a.lease
	// Take over from a fence already under way, or stop renewing now.
	for {
		l.mu.Lock()
		if !l.fencing {
			if l.k != nil {
				l.fencing = true
			}
			l.mu.Unlock()
			break
		}
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond): // the running fence ends soon
		}
	}
	// From here on it runs to the end whatever ctx does: renewals have
	// stopped, so Postgres must stop too (never a primary without a lease).
	a.stepHold.set(epoch, mono.Now()+stepDownHold)
	defer func() {
		l.mu.Lock()
		l.k, l.fencing = nil, false
		l.mu.Unlock()
		a.nudge()
	}()
	// Renewals already sent may still land; wait (a little) for their
	// answers so the switch reads the lease sequence after them. One that
	// lands later only makes the switch's compare-and-set fail.
	for wait := time.Now().Add(2 * time.Second); time.Now().Before(wait); {
		l.mu.Lock()
		n := l.inflight
		l.mu.Unlock()
		if n == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond) // polling interval; each renewal has its own timeout
	}
	srv := a.fenceTarget() // also a Postgres still starting, or about to be spawned
	if srv == nil {
		return nil
	}
	a.log.Warn("planned switch: stopping the main node's postgres", "epoch", epoch)
	a.say("A planned switch is moving the main-node role away from this node; its database is stopping.")
	done := make(chan error, 1)
	// Immediate, as when the old main node sees the new term by itself:
	// it rewinds and rejoins as a copy anyway (rule 6), and its WAL is on
	// disk, so a cancelled switch only means crash recovery.
	go func() { done <- srv.Fence(0, 3*time.Second) }()
	// Written while Postgres stops (rule 6): restarts go through a
	// confirmed read, and if the switch is cancelled this node starts
	// again as the main node of the same term.
	mctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	merr := a.node.Fenced(mctx, a.inst, epoch, "stepped down for a planned switch")
	cancel()
	if err := <-done; err != nil {
		return fmt.Errorf("postgres did not stop: %w", err)
	}
	a.clearPG(srv)
	if merr != nil {
		return fmt.Errorf("postgres stopped, but the marker could not be written: %w", merr)
	}
	return nil
}

var errSteppedDown = errors.New("this node stepped down for a planned switch; it takes the main-node role back only if the switch is cancelled")

// oldMainStopped is the planned switch's step 4b, on the leader, after the
// copies are cut and before the new main node is recorded (F6, D41). It
// returns the lease sequence the compare-and-set must expect once the old
// main node can no longer be serving:
//
//   - it confirmed that its Postgres exited (step_down) and holds off
//     taking its lease again; or
//   - it did not answer (frozen, cut off): its lease is waited out, a full
//     lease plus margin from when this leader saw its sequence last move,
//     as a failover does (its own watchdog stops Postgres before that).
//
// A healthy switch costs one fast stop; a frozen old main node costs the
// rest of its lease, never safety. stepped is set once it stepped down.
func (a *Agent) oldMainStopped(ctx context.Context, st cluster.State, cutStart time.Time, stepped *bool) (uint64, error) {
	if st.PrimaryID == "" {
		return st.Lease.Seq, nil
	}
	old := a.primaryName(st)
	// Leave room for the compare-and-set within half the cut hold.
	giveUp := cutStart.Add(switchCutHold/2 - 5*time.Second)
	read := func() (cluster.State, error) {
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		cur, err := a.cn.LinearizableState(rctx)
		if err != nil {
			return cur, fmt.Errorf("read the cluster state: %w", err)
		}
		if cur.Epoch != st.Epoch || cur.PrimaryID != st.PrimaryID {
			return cur, fmt.Errorf("the main node changed during the switch (term %d to %d)", st.Epoch, cur.Epoch)
		}
		return cur, nil
	}
	stepDown := func() bool {
		// Lock wait (stepDownLockWait) + immediate stop (3 s) + final kill + marker.
		cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
		err := a.call(cctx, st.PrimaryID, "step_down", stepDownReq{Epoch: st.Epoch}, nil)
		if err != nil {
			a.log.Warn("switch: the old main node did not confirm it stopped", "node", old, "err", err)
			return false
		}
		*stepped = true
		return true
	}
	// The lease sequence as read right before asking: if it has not moved
	// a full lease plus margin after this read returned, the old main
	// node's last acknowledged renewal was sent before that, so its lease
	// is over (a renewal that lands later moves the sequence and restarts
	// the wait). Reading it before the step-down, not after it failed,
	// saves the step-down's own time when the old main node's agent is
	// alive but its Postgres cannot be stopped (a hung disk, D58): its
	// renewals stopped when it was asked.
	cur, err := read()
	if err != nil {
		return 0, err
	}
	seq, since := cur.Lease.Seq, mono.Now()
	if stepDown() {
		cur, err := read()
		return cur.Lease.Seq, err
	}
	limit := controller.LeaseLimit(st, controller.DefaultMargin)
	a.log.Warn("switch: waiting out the old main node's lease before recording the new one", "node", old, "wait", limit-(mono.Now()-since))
	for {
		// Re-read every time, also once the wait is over: the sequence
		// must not have moved (the step-down above may itself have taken
		// a while).
		if cur, err = read(); err != nil {
			return 0, err
		}
		if cur.Lease.Seq != seq { // it renewed: alive after all
			seq, since = cur.Lease.Seq, mono.Now()
			if stepDown() {
				cur, err := read()
				return cur.Lease.Seq, err
			}
			continue
		}
		if mono.Now()-since >= limit {
			return seq, nil
		}
		if time.Now().After(giveUp) {
			return 0, fmt.Errorf("%s neither confirmed that it stopped nor let its lease run out in time (it is still renewing it)", old)
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(200 * time.Millisecond): // poll the lease sequence
		}
	}
}
