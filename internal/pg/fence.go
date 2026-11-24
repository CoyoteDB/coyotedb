package pg

import (
	"fmt"
	"time"
)

// Fence stops Postgres against a deadline (safety rule 4): a fast stop
// first, an immediate stop once fastFor has passed, and a kill once
// immediateFor more has passed. A zero or negative duration skips that
// step (a fence that starts late goes straight to the stronger one). It
// returns once the postmaster has exited, or an error if it is still
// running 5s after the kill.
//
// Unlike Stop it may be called while a Stop is in progress, and more than
// once: each call only ever escalates.
func (s *Server) Fence(fastFor, immediateFor time.Duration) error {
	select {
	case <-s.done:
		return nil
	default:
	}
	if fastFor > 0 {
		s.log.Warn("fencing postgres", "mode", "fast", "pid", s.PID(), "for", fastFor)
		if err := requestStop(s.inst, s.node.PGData(), s.PID(), modeFast); err != nil {
			s.log.Warn("fence: fast stop request failed", "err", err)
		}
		if waitClosed(s.done, fastFor) {
			return nil
		}
	}
	if immediateFor > 0 {
		s.log.Warn("fencing postgres", "mode", "immediate", "pid", s.PID(), "for", immediateFor)
		if err := requestStop(s.inst, s.node.PGData(), s.PID(), modeImmediate); err != nil {
			s.log.Warn("fence: immediate stop request failed", "err", err)
		}
		if waitClosed(s.done, immediateFor) {
			return nil
		}
	}
	s.log.Warn("fencing postgres", "mode", "kill", "pid", s.PID())
	return s.killAll(5 * time.Second)
}

// killAll kills the postmaster and every process it started, and waits
// up to d for all of them to exit.
func (s *Server) killAll(d time.Duration) error {
	stop, kill := ownStopKill(s.cmd.Process)
	kids := killServerTree(s.PID(), stop, kill)
	if !waitClosed(s.done, d) {
		return fmt.Errorf("postgres (pid %d) did not exit after kill", s.PID())
	}
	if !waitProcsGone(kids, d) {
		return fmt.Errorf("postgres (pid %d) exited, but some of its processes are still running after kill", s.PID())
	}
	return nil
}

// waitProcsGone polls until every listed process has exited or d passes.
func waitProcsGone(ps []procID, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for !procsGone(ps) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond) // polling interval
	}
	return true
}

// SetLeaseUntil tells the process guard until when this Postgres may keep
// running as a primary without a fresh SetLeaseUntil (deadline is an
// internal/mono reading; 0: no limit, a standby). The guard's watchdog
// process (every OS) stops Postgres itself once the deadline passes, so a
// primary whose agent is frozen (SIGSTOP, a suspended or stuck process) is
// still fenced, and a message delivered late can only carry a deadline
// that is already as early as when it was worked out.
func (s *Server) SetLeaseUntil(g *Guard, deadline time.Duration) error {
	if g == nil {
		return nil
	}
	return g.setLeaseUntil(s.PID(), deadline)
}
