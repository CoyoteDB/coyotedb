//go:build unix && !linux && !darwin

package mono

import (
	"time"

	"golang.org/x/sys/unix"
)

// Other Unix systems (not a supported platform): CLOCK_MONOTONIC, which
// may not count suspend there.
func now() time.Duration {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		panic("mono: CLOCK_MONOTONIC: " + err.Error())
	}
	return time.Duration(ts.Nano()) + 1
}
