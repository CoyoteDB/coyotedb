//go:build unix && !linux && !darwin

package pg

import "errors"

// Not a supported platform: no child listing; only the postmaster is
// killed.
type procRec struct {
	ppid  int
	start int64
}

func procInfo(int) (procRec, error) { return procRec{}, errors.New("not supported on this system") }

func childProcs(int) ([]procID, error) { return nil, errors.New("not supported on this system") }
