package agent

import (
	"context"
	"time"

	"coyotedb.com/coyote/internal/cluster"
	"coyotedb.com/coyote/internal/pg"
)

// The copy's WAL receiver watch (D57, finding N1). A copy that the cluster
// expects to stream from the main node but whose WAL receiver has been
// stuck on one connection attempt for receiverStuckAfter is restarted: its
// connection is most likely to a socket whose other end is gone (the
// tunnel's close was lost). If the copy still does not stream after
// receiverWarnAfter, the person is told in plain words. A copy whose disk
// is full says so too (finding D3): Postgres keeps running, its WAL
// receiver just fails every few seconds, so nothing else would.
const (
	// receiverStuckAfter: one receiver process that has not reached
	// streaming for this long is restarted. A healthy one streams within a
	// second or two; one that cannot reach the main node fails by itself
	// within the tunnel's dial timeout (10 s) and is replaced.
	receiverStuckAfter = 30 * time.Second
	// receiverWarnAfter: not streaming for this long is said in the output.
	receiverWarnAfter = 60 * time.Second
	// diskLowBytes: below this much free space a copy cannot store WAL (one
	// segment is 16 MB, and Postgres makes it whole before writing to it).
	diskLowBytes = 64 << 20
	// diskOKBytes: the disk message is re-armed only above this, so a value
	// that hovers near the limit is not repeated.
	diskOKBytes = 256 << 20
)

// recvWatch is the watch's state (reconcile goroutine only). Durations are
// measured with time.Now's monotonic reading; this is not lease logic.
type recvWatch struct {
	notSince  time.Time // first sample not streaming; zero while streaming
	pid       int       // receiver pid last seen while not streaming
	pidSince  time.Time // when pid was first seen
	lastKick  time.Time // last restart
	warned    bool      // the "not receiving" line was said for this episode
	diskWarn  bool      // the disk-full line was said and not yet re-armed
	restarted int       // restarts in this episode (for the log)
}

// recvSample is one look at the receiver.
type recvSample struct {
	streaming bool
	pid       int
}

// recvVerdict is what the watch wants done after a sample.
type recvVerdict struct {
	restart bool // restart the receiver with this pid
	warn    bool // say once that the copy is not receiving
}

// reset forgets the episode (streaming again, or not expected to stream).
func (w *recvWatch) reset() {
	diskWarn := w.diskWarn
	*w = recvWatch{diskWarn: diskWarn}
}

// observe updates the watch with a sample taken at now.
func (w *recvWatch) observe(now time.Time, s recvSample) recvVerdict {
	if s.streaming {
		w.reset()
		return recvVerdict{}
	}
	if w.notSince.IsZero() {
		w.notSince = now
	}
	var v recvVerdict
	switch {
	case s.pid == 0:
		w.pid, w.pidSince = 0, time.Time{}
	case s.pid != w.pid:
		w.pid, w.pidSince = s.pid, now
	case now.Sub(w.pidSince) >= receiverStuckAfter && (w.lastKick.IsZero() || now.Sub(w.lastKick) >= receiverStuckAfter):
		v.restart = true
		w.lastKick = now
		w.restarted++
	}
	if !w.warned && now.Sub(w.notSince) >= receiverWarnAfter {
		w.warned = true
		v.warn = true
	}
	return v
}

// diskSample updates the disk-full state with the free bytes seen; it
// reports whether to say that the disk is full.
func (w *recvWatch) diskSample(free uint64) bool {
	switch {
	case free < diskLowBytes && !w.diskWarn:
		w.diskWarn = true
		return true
	case free > diskOKBytes:
		w.diskWarn = false
	}
	return false
}

// watchReceiver runs on every steady-state pass of a copy (standbyDuties).
// streaming is what isStreaming just saw.
func (a *Agent) watchReceiver(ctx context.Context, st cluster.State, streaming bool) {
	now := time.Now()
	if free, _, err := pg.DiskSpace(a.node.PGData()); err == nil && a.recv.diskSample(free) {
		a.log.Warn("disk almost full on a copy", "free_bytes", free, "dir", a.node.PGData())
		a.say("The disk is full on this computer (%s free for %s): this copy cannot store the changes it receives from %s, so it falls behind. Free some space; it catches up by itself.",
			sizeText(free), a.node.StateDir, a.primaryName(st))
	}
	if !a.expectsStream(st) {
		a.recv.reset()
		return
	}
	s := recvSample{streaming: streaming}
	if !streaming {
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		r, err := a.node.WALReceiverState(cctx)
		cancel()
		if err != nil {
			a.log.Debug("cannot read the WAL receiver state", "err", err)
			return
		}
		s = recvSample{streaming: r.Streaming, pid: r.PID}
		if r.Streaming {
			// Became streaming between the two looks.
			a.recv.reset()
			return
		}
		a.log.Debug("copy not streaming", "receiver_pid", r.PID, "wait_event", r.WaitEvent)
	}
	v := a.recv.observe(now, s)
	if v.restart {
		a.log.Warn("the WAL receiver has not reached the main node for a while; restarting it",
			"pid", s.pid, "primary", a.primaryName(st), "restarts", a.recv.restarted)
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := a.node.RestartWALReceiver(cctx, a.inst, s.pid)
		cancel()
		if err != nil && !pg.IsNotReceiver(err) {
			a.log.Warn("could not restart the WAL receiver", "err", err)
		}
	}
	if v.warn {
		a.say("This copy has not received changes from %s for a minute. It keeps trying by itself; if this goes on, check the network between this computer and %s.",
			a.primaryName(st), a.primaryName(st))
	}
}

// expectsStream: the cluster says another node is the main node, this
// copy's stream is not cut (rule 2), and the tunnel points at that node.
func (a *Agent) expectsStream(st cluster.State) bool {
	if st.PrimaryID == "" || st.PrimaryID == a.cn.ID() || a.fwd == nil || a.fwd.Target() != st.PrimaryID {
		return false
	}
	rs, err := a.node.ReadReplState()
	return err == nil && rs.Upstream != nil
}

// errMsgDiskFull is noteErr's key for "a step failed on a full disk".
const errMsgDiskFull = "\x00disk full"
