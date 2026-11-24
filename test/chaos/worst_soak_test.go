//go:build chaos && unix && worst

package chaos

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestSoak is the long randomized run (scripts/worst/soak.sh): segments
// of SOAK_SEGMENT (default 30m) each on a fresh cluster (topology from
// the seed), faults drawn from the random runner's menu plus the worst
// cases that fit in a loop, each held 3-22 s, undone, healed. Every fault
// and every segment's verdict is appended to SOAK_OUT (JSON lines) as it
// happens, so a run cut short still reports what it did.
//
//	SOAK_SEGMENTS   number of segments (default 1)
//	SOAK_SEGMENT    fault time per segment (default 30m)
//	SOAK_OUT        JSON lines file (default soak.jsonl in the artifacts)
//	CHAOS_SEED      first seed (later ones: +7919 each)
func TestSoak(t *testing.T) {
	if os.Getenv("SOAK_SEGMENTS") == "" {
		t.Skip("SOAK_SEGMENTS not set: the soak runs only when asked (scripts/worst/soak.sh)")
	}
	segs, _ := strconv.Atoi(os.Getenv("SOAK_SEGMENTS"))
	dur, err := time.ParseDuration(os.Getenv("SOAK_SEGMENT"))
	if err != nil || dur <= 0 {
		dur = 30 * time.Minute
	}
	outPath := os.Getenv("SOAK_OUT")
	if outPath == "" {
		outPath = artifactRoot + "/soak.jsonl"
	}
	out := &soakLog{path: outPath}
	base := seedFor("soak")
	topos := []Topology{Three, Three, TwoWitness, Five, ThreeTwoWitness}
	menu := append(append([]fault(nil), faults...), soakFaults...)
	menu = append(menu, extraSoakFaults...)
	for i := range max(segs, 1) {
		seed := base + uint64(i)*7919
		topo := topos[seed%uint64(len(topos))]
		var r0 *Run
		sc := Scenario{Name: "soak", Topo: topo, Body: func(r *Run) {
			r0 = r
			end := time.Now().Add(dur)
			for k := 0; time.Now().Before(end); k++ {
				f := menu[r.Rng.IntN(len(menu))]
				hold := time.Duration(3+r.Rng.IntN(20)) * time.Second
				e0 := r.C.Epoch()
				start := r.C.Log.Mark(fmt.Sprintf("soak fault %d: %s for %s", k, f.name, hold))
				undo := f.apply(r)
				r.Let(hold)
				undo()
				r.Heal()
				healed := time.Now()
				r.Let(time.Duration(1+r.Rng.IntN(4)) * time.Second)
				back := ResumeAfter(r.W.H, start)
				out.write(map[string]any{
					"kind": "fault", "seed": seed, "topo": topo.String(), "n": k, "fault": f.name,
					"hold_s": hold.Seconds(), "writes_back_s": secs(back), "heal_s": healed.Sub(start).Seconds(),
					"term_before": e0, "term_after": r.C.Epoch(), "failover": r.C.Epoch() != e0,
					"max_ack_gap_s": r.longestAckGap(start, healed).Seconds(),
				})
			}
		}}
		t.Run(fmt.Sprintf("seed-%d-%s", seed, strings.ReplaceAll(topo.String(), " ", "")), func(t *testing.T) {
			rec := map[string]any{"kind": "segment", "seed": seed, "topo": topo.String(), "segment_s": dur.Seconds()}
			defer func() {
				rec["passed"] = !t.Failed()
				if r0 != nil {
					rec["promotions"] = len(r0.C.PromotionTimelines())
				}
				out.write(rec)
			}()
			v := runScenario(t, sc, seed)
			if v == nil {
				return
			}
			rec["problems"] = v.Problems
			rec["lost"] = 0
			for _, p := range v.Problems {
				if m := lostRe.FindStringSubmatch(p); m != nil {
					rec["lost"], _ = strconv.Atoi(m[1])
				}
			}
			for _, n := range v.Notes {
				if m := notesRe.FindStringSubmatch(n); m != nil {
					rec["ok"], _ = strconv.Atoi(m[1])
					rec["failed"], _ = strconv.Atoi(m[2])
					rec["unknown"], _ = strconv.Atoi(m[3])
				}
			}
		})
	}
}

var (
	lostRe  = regexp.MustCompile(`LOST WRITES: (\d+) acknowledged`)
	notesRe = regexp.MustCompile(`appends: (\d+) ok, (\d+) failed, (\d+) unknown`)
)

func secs(d time.Duration) any {
	if d < 0 {
		return nil
	}
	return d.Seconds()
}

type soakLog struct {
	mu   sync.Mutex
	path string
}

func (l *soakLog) write(v map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	v["at"] = time.Now().Format(time.RFC3339)
	b, _ := json.Marshal(v)
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

// extraSoakFaults: platform-specific faults (worst_linux_test.go adds
// netem packet loss there).
var extraSoakFaults []fault

// soakFaults are the worst cases that fit the soak's loop (each undone
// by its undo and the Heal after it).
var soakFaults = []fault{
	{"power cut of the main node", func(r *Run) func() {
		p := r.Primary()
		t0 := r.C.Log.Mark("power cut " + p.Name)
		r.C.PowerCut(p)
		r.hardKills = append(r.hardKills, t0)
		return func() { p.Restart() }
	}},
	{"power cut of a copy", func(r *Run) func() {
		s := r.pick(r.Standbys(r.Primary()))
		r.C.PowerCut(s)
		return func() { s.Restart() }
	}},
	{"power cut of every node", func(r *Run) func() {
		ms := r.C.Members()
		r.C.PowerCut(ms...)
		// Power comes back on every node at once: started one by one and
		// waited for, the first one waits for a majority that is not up.
		return func() {
			for _, n := range ms {
				n.Launch("up", "")
			}
		}
	}},
	{"OOM kill of a backend on the main node", func(r *Run) func() {
		p := r.Primary()
		if pid := p.pgChild("127.0.0.1("); pid > 0 {
			r.C.Log.Event("kill -9 backend %d on %s", pid, p.Name)
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		return func() {}
	}},
	{"database of a copy frozen", func(r *Run) func() {
		return r.freezePG(r.pick(r.Standbys(r.Primary())))
	}},
	{"latency spikes", func(r *Run) func() {
		r.C.Net.Spikes(30, 800*time.Millisecond)
		return r.C.Net.Heal
	}},
	{"one-way cuts around the main node", func(r *Run) func() {
		p := r.Primary()
		for _, o := range r.C.Names() {
			if o != p.Name && r.Rng.IntN(2) == 0 {
				if r.Rng.IntN(2) == 0 {
					r.C.Net.SetFlow(p.Name, o, Blackhole)
				} else {
					r.C.Net.SetFlow(o, p.Name, Blackhole)
				}
			}
		}
		return r.C.Net.Heal
	}},
}
