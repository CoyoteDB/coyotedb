//go:build windows

package pg

import "os"

// procID is a process as listed.
type procID struct{ pid int }

// killServerTree kills a postmaster and every postgres.exe it started
// (F7). The postmaster goes first so it cannot start new children while
// they are listed; Windows keeps the parent pid in each child's record
// after the parent exits. The Job Object also ends them all with the
// agent. stopParent is unused (Windows has no stop signal).
func killServerTree(pm int, _ func() error, killParent func() error) []procID {
	_ = killParent()
	kids, err := childProcesses(uint32(pm))
	if err != nil {
		return nil
	}
	var killed []procID
	for _, c := range kids {
		if ok, err := isPostgresProcess(int(c)); err == nil && ok {
			_ = killPID(int(c))
			killed = append(killed, procID{pid: int(c)})
		}
	}
	return killed
}

func rawStopKill(pid int) (stop, kill func() error) {
	return func() error { return nil }, func() error { return killPID(pid) }
}

// procsGone reports whether every listed process has exited.
func procsGone(ps []procID) bool {
	for _, p := range ps {
		if ok, err := isPostgresProcess(p.pid); err == nil && ok && processAlive(p.pid) {
			return false
		}
	}
	return true
}

func ownStopKill(p *os.Process) (stop, kill func() error) {
	return func() error { return nil }, p.Kill
}
