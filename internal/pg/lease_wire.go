package pg

import (
	"fmt"
	"io"
	"time"
)

// The agent's side of the watchdog's lease messages (watchdog.go), the
// same on every OS: deadlines are absolute internal/mono readings.

// armUntil tells the watchdog that the next postgres started is a primary
// that may run until deadline (sent before it is spawned).
func (g *Guard) armUntil(deadline time.Duration) error {
	return g.wire(fmt.Sprintf("armat %d\n", max(int64(deadline), 1)))
}

// setLeaseUntil tells the watchdog that postgres pid may run as a primary
// until deadline (0: no limit, a standby).
func (g *Guard) setLeaseUntil(pid int, deadline time.Duration) error {
	return g.wire(fmt.Sprintf("leaseat %d %d\n", pid, max(int64(deadline), 0)))
}

// wire writes one control line. Each line is one small write, so lines
// from different goroutines never interleave.
func (g *Guard) wire(line string) error {
	if g.closing.Load() {
		return nil
	}
	_, err := io.WriteString(g.ctl, line)
	return err
}
