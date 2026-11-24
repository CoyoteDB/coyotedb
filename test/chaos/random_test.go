//go:build chaos && unix

package chaos

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fault is one randomized fault: apply it, and return how to undo it.
type fault struct {
	name  string
	apply func(r *Run) (undo func())
}

func (r *Run) pick(ns []*Node) *Node { return ns[r.Rng.IntN(len(ns))] }

var faults = []fault{
	{"kill -9 primary agent", func(r *Run) func() {
		p := r.Primary()
		r.HardKill(p)
		return func() { p.Restart() }
	}},
	{"kill -9 a copy's agent", func(r *Run) func() {
		s := r.pick(r.Standbys(r.Primary()))
		s.Kill9()
		return func() { s.Restart() }
	}},
	{"freeze primary agent", func(r *Run) func() {
		p := r.Primary()
		p.Stop(false)
		return p.Cont
	}},
	{"freeze primary agent and postmaster", func(r *Run) func() {
		p := r.Primary()
		p.Stop(true)
		return p.Cont
	}},
	{"freeze a copy's agent", func(r *Run) func() {
		s := r.pick(r.Standbys(r.Primary()))
		s.Stop(false)
		return s.Cont
	}},
	{"kill primary postmaster", func(r *Run) func() {
		r.Primary().KillPostmaster()
		return func() {}
	}},
	{"kill primary watchdog", func(r *Run) func() {
		p := r.Primary()
		p.KillWatchdog()
		return func() {
			if !p.Running() {
				p.Restart()
			}
		}
	}},
	{"isolate primary", func(r *Run) func() {
		p := r.Primary()
		m := Blackhole
		if r.Rng.IntN(2) == 0 {
			m = Drop
		}
		r.C.Net.Isolate(p.Name, r.C.Names(), m)
		return r.C.Net.Heal
	}},
	{"isolate a node", func(r *Run) func() {
		n := r.pick(r.C.Members())
		r.C.Net.Isolate(n.Name, r.C.Names(), Blackhole)
		return r.C.Net.Heal
	}},
	{"primary cannot send", func(r *Run) func() {
		p := r.Primary()
		for _, o := range r.C.Names() {
			if o != p.Name {
				r.C.Net.SetFlow(p.Name, o, Blackhole)
			}
		}
		return r.C.Net.Heal
	}},
	{"slow network", func(r *Run) func() {
		r.C.Net.Delay(time.Duration(20+r.Rng.IntN(200)) * time.Millisecond)
		return r.C.Net.Heal
	}},
	{"clean restart of a node", func(r *Run) func() {
		n := r.pick(r.C.Members())
		n.Interrupt()
		return func() { n.Restart() }
	}},
	{"planned switch", func(r *Run) func() {
		p := r.Primary()
		to := r.pick(r.Standbys(p))
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		if out, err := p.CLI(ctx, "switch", "--to", "node-"+to.Name); err != nil {
			r.C.Log.Event("switch refused (allowed while recovering): %v %s", err, strings.TrimSpace(out))
		}
		return func() {}
	}},
}

func randomSeeds() []uint64 {
	v := os.Getenv("CHAOS_SEEDS")
	if v == "" {
		return []uint64{seedFor("random")}
	}
	if n, err := strconv.Atoi(v); err == nil && !strings.Contains(v, ",") {
		base := seedFor("random")
		var out []uint64
		for i := range n {
			out = append(out, base+uint64(i)*7919)
		}
		return out
	}
	var out []uint64
	for _, s := range strings.Split(v, ",") {
		x, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
		if err == nil {
			out = append(out, x)
		}
	}
	return out
}

func randomDuration() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("CHAOS_DURATION")); err == nil && d > 0 {
		return d
	}
	return 90 * time.Second
}

// TestRandom: per seed, a topology and a sequence of faults drawn from the
// seed, each held a random time, undone, and the cluster must heal fully
// before the next. The checker runs at the end.
func TestRandom(t *testing.T) {
	if re := onlyRe(); re != nil && !re.MatchString("random") {
		t.Skip("CHAOS_ONLY excludes the randomized runner")
	}
	topos := []Topology{Three, Three, TwoWitness, Five}
	dur := randomDuration()
	for _, seed := range randomSeeds() {
		topo := topos[seed%uint64(len(topos))]
		sc := Scenario{Name: "random", Topo: topo, Body: func(r *Run) {
			end := time.Now().Add(dur)
			for i := 0; time.Now().Before(end); i++ {
				f := faults[r.Rng.IntN(len(faults))]
				hold := time.Duration(3+r.Rng.IntN(20)) * time.Second
				r.C.Log.Event("random fault %d: %s for %s", i, f.name, hold)
				undo := f.apply(r)
				r.Let(hold)
				undo()
				r.Heal()
				r.Let(time.Duration(1+r.Rng.IntN(4)) * time.Second)
			}
		}}
		t.Run(fmt.Sprintf("seed-%d-%s", seed, strings.ReplaceAll(topo.String(), " ", "")), func(t *testing.T) {
			runScenario(t, sc, seed)
		})
	}
}
