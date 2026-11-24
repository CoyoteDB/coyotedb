//go:build linux

package mono

import (
	"time"

	"golang.org/x/sys/unix"
)

func now() time.Duration {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts); err != nil {
		panic("mono: CLOCK_BOOTTIME: " + err.Error()) // present since Linux 2.6.39
	}
	return time.Duration(ts.Nano()) + 1
}
