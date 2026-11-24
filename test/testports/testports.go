// Package testports hands out TCP ports for the test harnesses.
//
// Picking a port by binding port 0 and closing it races with every other
// program on the machine: port 0 comes from the OS's ephemeral range, which
// is exactly where other programs' outgoing connections get their local
// ports, so a port released by the picker is often taken again before the
// agent binds it (seen on a slow Linux container). Free picks from a range
// below every ephemeral range (Linux 32768-60999, macOS and Windows
// 49152-65535), never hands out the same port twice in one test process,
// and checks the port is free at that moment. The harnesses also relaunch
// a node on new ports if an agent still reports a port in use.
package testports

import (
	"fmt"
	"math/rand/v2"
	"net"
	"strconv"
	"strings"
	"sync"
)

const (
	low  = 20000
	high = 32000 // below Linux's default ip_local_port_range
)

var (
	mu   sync.Mutex
	used = map[int]bool{}
)

// Free returns a port on 127.0.0.1 that was free when checked.
func Free() (int, error) {
	mu.Lock()
	defer mu.Unlock()
	for range 2000 {
		p := low + rand.IntN(high-low)
		if used[p] {
			continue
		}
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
		if err != nil {
			continue
		}
		ln.Close()
		used[p] = true
		return p, nil
	}
	return 0, fmt.Errorf("testports: no free port in %d-%d", low, high)
}

// InUse reports whether an agent's output says a local port was taken
// (its start-up check; see internal/agent reservePorts).
func InUse(output string) bool {
	return strings.Contains(output, "is already in use")
}
