package agent

import (
	"context"
	"fmt"
	"time"

	"coyotedb.com/coyote/internal/cluster"
	"coyotedb.com/coyote/internal/pg"
)

// The same-timeline divergence watch (D56, finding P1). A copy whose WAL
// differs from the main node's under one timeline ID can never stream
// again (Postgres rejects the main node's records at its position and
// retries forever), and neither the follow check nor pg_rewind can see it:
// both reason with timeline histories only. While a copy is not streaming,
// its last replayed WAL page is compared with the main node's every
// divergeCheckEvery; after divergeStrikes differing results in a row the
// copy is copied again from the main node, with a plain message.
const (
	divergeCheckEvery = 20 * time.Second
	divergeStrikes    = 3
)

// divergeWatch is the watch's state (reconcile goroutine only).
type divergeWatch struct {
	last    time.Time
	strikes int
}

// note records one check result; it reports whether to re-clone now.
func (w *divergeWatch) note(v pg.SameTimelineVerdict) bool {
	if !v.Known || !v.Diverged {
		w.strikes = 0
		return false
	}
	w.strikes++
	return w.strikes >= divergeStrikes
}

// due reports whether a check should run at now, and marks it run.
func (w *divergeWatch) due(now time.Time) bool {
	if !w.last.IsZero() && now.Sub(w.last) < divergeCheckEvery {
		return false
	}
	w.last = now
	return true
}

// watchDiverged runs on every steady pass of a copy (standbyDuties). It
// reports whether it re-cloned this node (the caller then stops its pass).
func (a *Agent) watchDiverged(ctx context.Context, st cluster.State, streaming bool) (bool, error) {
	if streaming || st.PrimaryID == "" || st.PrimaryID == a.cn.ID() {
		a.diverge = divergeWatch{}
		return false, nil
	}
	if !a.diverge.due(time.Now()) {
		return false, nil
	}
	s, err := a.node.Secrets()
	if err != nil {
		return false, nil
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	v, err := a.node.SameTimelineCheck(cctx, a.fwdUpstream(), s)
	cancel()
	if err != nil {
		a.log.Debug("same-timeline check not possible now", "err", err)
		return false, nil
	}
	if v.Known && v.Diverged {
		a.log.Warn("this copy's WAL differs from the main node's on the same timeline", "timeline", v.TLI,
			"replay_lsn", v.Replay.String(), "strike", a.diverge.strikes+1, "of", divergeStrikes, "primary", a.primaryName(st))
	}
	if !a.diverge.note(v) {
		return false, nil
	}
	a.diverge = divergeWatch{}
	from := a.primaryName(st)
	a.say("This copy's history differs from %s's (both wrote changes on timeline %d), so it can never follow it. Copying the data again from %s.",
		from, v.TLI, from)
	unlock, err := a.lockPG(ctx)
	if err != nil {
		return true, err
	}
	defer unlock()
	if err := a.node.WriteMarker(pg.Marker{Reason: pg.ReasonRecloning, Timeline: v.TLI,
		Detail: fmt.Sprintf("cluster epoch %d: WAL differs from %s's on timeline %d at %s", st.Epoch, from, v.TLI, v.Replay)}); err != nil {
		return true, err
	}
	return true, a.doRejoin(ctx, st)
}
