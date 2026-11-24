//go:build chaos && unix

// Package chaos is CoyoteDB's chaos tier (`make chaos`): real agents and
// real Postgres on localhost, every agent-to-agent connection through a
// proxy per directed pair (drop, blackhole, delay, one-way cuts,
// partitions), process faults (kill -9, SIGSTOP, postmaster and watchdog
// kills, kills at chosen steps), clients that find the primary themselves
// and record every outcome, and a checker (lost or phantom writes,
// identical copies, bank total, Porcupine on a register, two read-write
// nodes at once, two nodes acknowledging on one timeline, write-resume
// time after a hard kill).
//
// Environment:
//
//	CHAOS_SEED       run every scenario with this seed (to re-run a failure)
//	CHAOS_SEEDS      randomized runner: a count (e.g. 5) or a list (1,2,3)
//	CHAOS_DURATION   randomized runner: fault time per seed (default 90s)
//	CHAOS_ONLY       regexp: run only matching named scenarios
//	CHAOS_ARTIFACTS  where failed runs are kept (default .coyote-dev/chaos-artifacts)
//	CHAOS_KEEP       keep the artifacts of passing runs too
package chaos

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

var (
	coyoteBin    string
	pgCache      string
	artifactRoot string

	statsMu sync.Mutex
	resumes []time.Duration // write resume after a hard primary kill
	fails   []time.Duration // kill -> new primary recorded
	// A node that went down and came back: from its restart until the
	// main node sees it streaming, and until it is back in the sync set.
	backStreaming []time.Duration
	backInSync    []time.Duration
)

func TestMain(m *testing.M) { os.Exit(runMain(m)) }

func runMain(m *testing.M) int {
	binDir, err := os.MkdirTemp("", "coyote-chaos-bin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(binDir)
	coyoteBin = filepath.Join(binDir, "coyote")
	build := exec.Command("go", "build", "-o", coyoteBin, "coyotedb.com/coyote/cmd/coyote")
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build coyote:", err)
		return 1
	}
	if pgCache = os.Getenv("COYOTE_PG_CACHE"); pgCache == "" {
		pgCache, _ = filepath.Abs(filepath.Join("..", "..", ".coyote-dev", "cache"))
	}
	if artifactRoot = os.Getenv("CHAOS_ARTIFACTS"); artifactRoot == "" {
		artifactRoot, _ = filepath.Abs(filepath.Join("..", "..", ".coyote-dev", "chaos-artifacts"))
	}
	if err := os.MkdirAll(artifactRoot, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	code := m.Run()
	printStats()
	return code
}

func record(dst *[]time.Duration, d time.Duration) {
	statsMu.Lock()
	defer statsMu.Unlock()
	*dst = append(*dst, d)
}

func printStats() {
	statsMu.Lock()
	defer statsMu.Unlock()
	for _, x := range []struct {
		name string
		ds   []time.Duration
	}{
		{"new main node recorded after a hard primary kill", fails},
		{"writes acknowledged again after a hard primary kill", resumes},
		{"returned node streaming again after its restart", backStreaming},
		{"returned node back in the sync set after its restart", backInSync},
	} {
		if len(x.ds) == 0 {
			continue
		}
		s := slices.Clone(x.ds)
		slices.Sort(s)
		pct := func(p float64) time.Duration { return s[min(len(s)-1, int(p*float64(len(s))))] }
		fmt.Printf("chaos: %s: n=%d min=%s p50=%s p90=%s max=%s\n", x.name, len(s),
			s[0].Round(10*time.Millisecond), pct(0.5).Round(10*time.Millisecond), pct(0.9).Round(10*time.Millisecond), s[len(s)-1].Round(10*time.Millisecond))
	}
}
