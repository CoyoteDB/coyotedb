//go:build darwin

package mono

import (
	"time"

	"golang.org/x/sys/unix"
)

func now() time.Duration {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		panic("mono: CLOCK_MONOTONIC: " + err.Error()) // present since macOS 10.12
	}
	return time.Duration(ts.Nano()) + 1
}
