//go:build chaos && unix

package chaos

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Run is one scenario run: a cluster, its workload and a seeded RNG.
type Run struct {
	t    *testing.T
	C    *Cluster
	W    *Workload
	Rng  *rand.Rand
	Seed uint64
	// hardKills: instants the primary was killed hard (resume budget).
	hardKills []time.Time
	// expectEpoch, if set, the epoch must not have changed by the end.
	noFailover bool
	startEpoch uint64
	// problems found by the scenario that must not stop the run before
	// the final Check (it is the main evidence); reported with its own.
	problems []string
}

// Problem records a scenario finding and lets the run go on to Check.
func (r *Run) Problem(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	r.C.Log.Event("PROBLEM (scenario): %s", msg)
	r.problems = append(r.problems, msg)
}

// Scenario is a named fault story.
type Scenario struct {
	Name string
	Topo Topology
	// Env is given to every agent while the cluster forms (fault points).
	Env  []string
	Body func(r *Run)
	// ViaProxy: the workload is an application on the client ports.
	ViaProxy bool
	// Prepare, if set, runs after the nodes get their ports and folders
	// and before the cluster forms (worst-case scenarios put a node's
	// folder on a small or faulty volume here).
	Prepare func(c *Cluster)
}

// ResumeBudget: with default timings (6s lease), writes must be
// acknowledged again within this long after a hard primary kill.
const ResumeBudget = 30 * time.Second

func seedFor(name string) uint64 {
	if s := os.Getenv("CHAOS_SEED"); s != "" {
		v, err := strconv.ParseUint(s, 10, 64)
		if err == nil {
			return v
		}
	}
	return uint64(time.Now().UnixNano())
}

// runScenario returns the checker's verdict (nil if the run stopped
// before the check); its problems are already reported on t.
func runScenario(t *testing.T, sc Scenario, seed uint64) *Verdict {
	dir := filepath.Join(artifactRoot, fmt.Sprintf("%s-%s-%d", time.Now().Format("20060102-150405"), sc.Name, seed))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	log := NewRunLog(dir)
	log.Event("scenario %s, %s, seed %d", sc.Name, sc.Topo, seed)
	t.Logf("scenario %s seed %d (re-run: CHAOS_SEED=%d CHAOS_ONLY='^%s$' make chaos)", sc.Name, seed, seed, sc.Name)
	c := NewCluster(t, dir, sc.Topo, log)
	c.FormEnv = sc.Env
	c.ViaProxy = sc.ViaProxy
	r := &Run{t: t, C: c, Seed: seed, Rng: rand.New(rand.NewPCG(seed, 0x5eed))}
	var strays []string
	t.Cleanup(func() {
		if c != nil {
			strays = c.Shutdown()
		}
		if len(strays) > 0 {
			t.Errorf("STRAY PROCESSES after shutdown:\n%s", strings.Join(strays, "\n"))
		}
		if r.W != nil {
			writeHistory(dir, r.W.H)
		}
		log.Close()
		if t.Failed() {
			t.Logf("CHAOS FAILURE: scenario %s seed %d; artifacts kept in %s", sc.Name, seed, dir)
			fmt.Printf("CHAOS FAILURE: scenario %s seed %d; artifacts in %s\n", sc.Name, seed, dir)
			return
		}
		if os.Getenv("CHAOS_KEEP") == "" {
			os.RemoveAll(dir)
		}
	})
	if sc.Prepare != nil {
		sc.Prepare(c)
	}
	c.Form()
	r.startEpoch = c.Epoch()
	r.W = StartWorkload(c, seed)
	r.Let(3 * time.Second)
	sc.Body(r)
	r.Heal()
	r.Let(3 * time.Second)
	r.W.Stop()
	if r.noFailover {
		if e := c.Epoch(); e != r.startEpoch {
			c.Fail("the main node changed (term %d -> %d) although it must not have", r.startEpoch, e)
		}
	}
	v := Check(c, r.W.H)
	v.Problems = append(r.problems, v.Problems...)
	if c.ViaProxy {
		for _, p := range checkRule10(r.W.H) {
			v.fail("%s", p)
		}
	}
	for _, n := range v.Notes {
		log.Event("check: %s", n)
		t.Logf("check: %s", n)
	}
	for _, k := range r.hardKills {
		d := ResumeAfter(r.W.H, k)
		if d < 0 {
			v.fail("writes never resumed after the hard kill at %s", k.Format("15:04:05.000"))
			continue
		}
		record(&resumes, d)
		log.Event("writes resumed %s after the hard kill", d.Round(time.Millisecond))
		if d > ResumeBudget {
			v.fail("writes resumed only %s after a hard primary kill (budget %s)", d.Round(time.Millisecond), ResumeBudget)
		}
	}
	for _, p := range v.Problems {
		log.Event("PROBLEM: %s", p)
		t.Errorf("%s", p)
	}
	return v
}

// Let lets the workload run for d under the current faults.
func (r *Run) Let(d time.Duration) {
	r.C.Log.Event("let it run %s (%s)", d, r.C.Net.Describe())
	time.Sleep(d) // the fault's duration, not a wait for a condition
}

// Primary is the node the cluster names main node; it fails the run if
// there is none.
func (r *Run) Primary() *Node {
	var name string
	if err := poll(time.Minute, func() bool { name = r.C.PrimaryByCluster(); return name != "" }); err != nil {
		r.C.Fail("no main node")
	}
	return r.C.Node(name)
}

// Standbys are the running data nodes other than p.
func (r *Run) Standbys(p *Node) []*Node {
	var out []*Node
	for _, n := range r.C.Data() {
		if n != p {
			out = append(out, n)
		}
	}
	return out
}

// WaitNewPrimary waits until a node other than old answers read-write
// (and the cluster names it), and records how long since t0.
func (r *Run) WaitNewPrimary(old *Node, t0 time.Time, within time.Duration) *Node {
	var p *Node
	err := poll(within, func() bool {
		name := r.C.PrimaryByCluster()
		if name == "" || name == old.Name {
			return false
		}
		n := r.C.Node(name)
		if rep, ok := n.Report(); ok && rep.PG != nil && rep.PG.Role == "primary" {
			p = n
			return true
		}
		return false
	})
	if err != nil {
		r.C.Fail("no new main node within %s of losing %s", within, old.Name)
	}
	d := time.Since(t0)
	r.W.H.add(func(h *History) { h.Primaries = append(h.Primaries, PrimaryChange{At: time.Now(), Node: p.Name}) })
	r.C.Log.Event("new main node %s, %s after the fault", p.Name, d.Round(time.Millisecond))
	for _, k := range r.hardKills {
		if k.Equal(t0) {
			record(&fails, d)
		}
	}
	return p
}

// WaitServed waits until the cluster records n as having served with its
// copies caught up (state.LastServed). Before that, n may be the only
// node holding writes acknowledged in the previous term, so losing it too
// rightly blocks the next failover (rule 2) until it is back.
func (r *Run) WaitServed(n *Node, within time.Duration) {
	err := poll(within, func() bool {
		for _, m := range r.C.Members() {
			if m.Running() && m != n {
				if s, ok := m.Status(); ok && s.Cluster.LastServed == "node-"+n.Name {
					return true
				}
			}
		}
		return false
	})
	if err != nil {
		r.C.Fail("%s was never recorded as having served with its copies caught up", n.Name)
	}
	r.C.Log.Event("%s has served with its copies caught up", n.Name)
}

// HardKill kill -9s the primary's agent (its watchdog stops Postgres) and
// counts it for the resume budget.
func (r *Run) HardKill(p *Node) time.Time {
	t0 := r.C.Log.Mark("hard kill " + p.Name)
	p.Kill9()
	r.hardKills = append(r.hardKills, t0)
	return t0
}

// Heal undoes every fault and waits until the cluster is whole again.
func (r *Run) Heal() {
	r.C.Log.Event("heal")
	r.C.Net.Heal()
	for _, n := range r.C.Members() {
		n.mu.Lock()
		stopped := n.stopped
		n.mu.Unlock()
		if stopped {
			n.Cont()
		}
	}
	for _, n := range r.C.Members() {
		if !n.Running() {
			n.Restart()
		}
	}
	r.C.WaitHealthy(4 * time.Minute)
	r.C.Log.Event("healthy again")
}

func onlyRe() *regexp.Regexp {
	if s := os.Getenv("CHAOS_ONLY"); s != "" {
		return regexp.MustCompile(s)
	}
	return nil
}
