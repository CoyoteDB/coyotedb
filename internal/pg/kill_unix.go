//go:build unix

package pg

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// procID is a process as listed: its pid and when it started, so a pid
// that was reused by another process is never signalled.
type procID struct {
	pid   int
	start int64 // OS-specific start time; only compared for equality
}

// killServerTree kills a postmaster and every process it started (F7).
// Postgres puts each child in its own session (setsid), so the
// postmaster's process group does not reach them, and a backend busy in a
// loop never checks whether the postmaster died: left running it keeps
// the shared memory, and the next start fails.
//
// Order: stop the postmaster (SIGSTOP; it can no longer fork, and the
// children that exit stay zombies it has not reaped, so their pids
// cannot be reused), list its children, SIGKILL each one and its own
// process group (whatever it started), then SIGKILL the postmaster.
// stopParent and killParent signal the postmaster itself (through
// os.Process for our own child, which never signals a reaped pid). It
// returns the children it killed, so the caller can wait for them.
func killServerTree(pm int, stopParent, killParent func() error) []procID {
	_ = stopParent()
	var killed []procID
	seen := map[procID]bool{}
	for round := 0; round < 3; round++ { // a child forked just before the stop
		kids, err := childProcs(pm)
		if err != nil {
			break
		}
		fresh := 0
		for _, k := range kids {
			if seen[k] {
				continue
			}
			seen[k] = true
			fresh++
			if same, err := sameProc(k); err != nil || !same {
				continue
			}
			_ = unix.Kill(-k.pid, unix.SIGKILL) // its group (setsid: it leads one)
			_ = unix.Kill(k.pid, unix.SIGKILL)
			killed = append(killed, k)
		}
		if fresh == 0 {
			break
		}
	}
	_ = killParent()
	return killed
}

// rawStopKill signals pid directly (a postmaster that is not our child;
// the caller has checked that it is one).
func rawStopKill(pid int) (stop, kill func() error) {
	return func() error { return unix.Kill(pid, unix.SIGSTOP) },
		func() error { return unix.Kill(pid, unix.SIGKILL) }
}

// sameProc reports whether k is still the process that was listed.
func sameProc(k procID) (bool, error) {
	p, err := procInfo(k.pid)
	if errors.Is(err, errNoProc) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return p.start == k.start, nil
}

var errNoProc = errors.New("no such process")

// procsGone reports whether every listed process has exited (zombies
// count as gone).
func procsGone(ps []procID) bool {
	for _, p := range ps {
		if same, err := sameProc(p); err == nil && same && processAlive(p.pid) {
			return false
		}
	}
	return true
}

// ownStopKill signals our own child through os.Process, which refuses
// once the process has been reaped (its pid may be reused).
func ownStopKill(p *os.Process) (stop, kill func() error) {
	return func() error { return p.Signal(unix.SIGSTOP) }, p.Kill
}
