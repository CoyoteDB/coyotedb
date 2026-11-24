package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"coyotedb.com/coyote/internal/controller"
)

// Test-only fault hooks for the lease (used by test/chaos). Both are
// inert unless their environment variable is set.

// stallSpawnFault (COYOTE_TEST_FAULT): after a lease fence, the next
// primary start stalls 8 s right before Postgres would be spawned, after
// every file is written: a slow disk in the window the start's lease
// deadline must cover (F3).
const (
	stallSpawnFault = "stall-before-primary-spawn"
	stallSpawnFor   = 8 * time.Second
)

// stallBeforeSpawn returns the StartOptions.TestBeforeSpawn hook for this
// primary start (nil unless the fault is armed).
func (a *Agent) stallBeforeSpawn(ctx context.Context) func() {
	if os.Getenv(faultEnv) != stallSpawnFault || !a.stallSpawn.CompareAndSwap(true, false) {
		return nil
	}
	return func() {
		a.say("Test fault point reached: %s (%s).", stallSpawnFault, stallSpawnFor)
		t := time.NewTimer(stallSpawnFor)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
		}
	}
}

// wallStepEnv (tests only): while the file it names (relative to the
// cluster state dir) holds a Go duration, every wall-clock reading the
// agent's lease and cut-hold code takes is that much ahead; the monotonic
// clock is unchanged. Models an NTP / VM time-sync step (F4, F5), which
// needs root to do for real.
const wallStepEnv = "COYOTE_TEST_WALL_STEP_FILE"

type wallStep struct {
	file string
	say  func(format string, args ...any)
	mu   sync.Mutex
	last time.Duration
}

func newWallStep(clusterDir string, say func(string, ...any)) *wallStep {
	f := os.Getenv(wallStepEnv)
	if f == "" {
		return nil
	}
	if !filepath.IsAbs(f) {
		f = filepath.Join(clusterDir, f)
	}
	return &wallStep{file: f, say: say}
}

// step is the current skew (0 if the file is missing or unreadable).
func (w *wallStep) step() time.Duration {
	if w == nil {
		return 0
	}
	var d time.Duration
	if b, err := os.ReadFile(w.file); err == nil {
		d, _ = time.ParseDuration(strings.TrimSpace(string(b)))
	}
	w.mu.Lock()
	changed := d != w.last
	w.last = d
	w.mu.Unlock()
	if changed && d != 0 {
		w.say("Test fault point reached: wall clock stepped by %s.", d)
	}
	return d
}

// steppedClock is a controller.Clock whose wall readings carry the step.
type steppedClock struct {
	controller.Clock
	w *wallStep
}

func (c steppedClock) Now() controller.Reading {
	r := c.Clock.Now()
	r.Wall = r.Wall.Add(c.w.step())
	return r
}

// wallNow is the wall clock as the cut-hold code reads it.
func (a *Agent) wallNow() time.Time { return time.Now().Add(a.wallStep.step()) }
