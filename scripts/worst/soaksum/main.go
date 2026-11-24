// Command soaksum summarises a TestSoak results file (JSON lines):
// segments passed, lost writes, failovers, and the time until writes were
// acknowledged again after each kind of fault.
//
//	go run ./scripts/worst/soaksum soak.jsonl
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"
)

type rec struct {
	Kind       string   `json:"kind"`
	Seed       uint64   `json:"seed"`
	Topo       string   `json:"topo"`
	Fault      string   `json:"fault"`
	WritesBack *float64 `json:"writes_back_s"`
	// MaxGap: longest pause between acknowledged writes from the fault to
	// the heal. Preferred: writes_back_s counts a write that was already
	// on its way back when the fault began.
	MaxGap     *float64 `json:"max_ack_gap_s"`
	Failover   bool     `json:"failover"`
	Passed     bool     `json:"passed"`
	Lost       int      `json:"lost"`
	OK         int      `json:"ok"`
	SegmentS   float64  `json:"segment_s"`
	Problems   []string `json:"problems"`
	Promotions int      `json:"promotions"`
	HoldS      float64  `json:"hold_s"`
}

func pct(s []float64, p float64) float64 { return s[min(len(s)-1, int(p*float64(len(s))))] }

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: soaksum soak.jsonl")
		os.Exit(2)
	}
	f, err := os.Open(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer f.Close()
	byFault := map[string][]float64{}
	never := map[string]int{}
	var faults, failovers, segs, passed, lost, ok int
	var hours float64
	var failedSegs []rec
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var r rec
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			continue
		}
		switch r.Kind {
		case "fault":
			faults++
			if r.Failover {
				failovers++
			}
			if r.MaxGap != nil {
				byFault[r.Fault] = append(byFault[r.Fault], *r.MaxGap)
			} else if r.WritesBack == nil {
				never[r.Fault]++
			} else {
				byFault[r.Fault] = append(byFault[r.Fault], *r.WritesBack)
			}
		case "segment":
			segs++
			hours += r.SegmentS / 3600
			lost += r.Lost
			ok += r.OK
			if r.Passed {
				passed++
			} else {
				failedSegs = append(failedSegs, r)
			}
		}
	}
	fmt.Printf("segments: %d (%d passed), fault time %.1f h, faults %d, failovers (term changed) %d\n", segs, passed, hours, faults, failovers)
	fmt.Printf("acknowledged appends checked: %d, lost: %d\n\n", ok, lost)
	fmt.Println("| fault | n | longest write pause (s; older files: first write back): min / p50 / p90 / max | not back before heal |")
	fmt.Println("|---|---|---|---|")
	var names []string
	for k := range byFault {
		names = append(names, k)
	}
	for k := range never {
		if _, ok := byFault[k]; !ok {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	var all []float64
	for _, k := range names {
		s := slices.Clone(byFault[k])
		slices.Sort(s)
		all = append(all, s...)
		if len(s) == 0 {
			fmt.Printf("| %s | %d | - | %d |\n", k, never[k], never[k])
			continue
		}
		fmt.Printf("| %s | %d | %.1f / %.1f / %.1f / %.1f | %d |\n", k, len(s)+never[k], s[0], pct(s, .5), pct(s, .9), s[len(s)-1], never[k])
	}
	if len(all) > 0 {
		slices.Sort(all)
		fmt.Printf("| all | %d | %.1f / %.1f / %.1f / %.1f | |\n", len(all), all[0], pct(all, .5), pct(all, .9), all[len(all)-1])
	}
	for _, r := range failedSegs {
		fmt.Printf("\nFAILED segment seed %d (%s):\n", r.Seed, r.Topo)
		for _, p := range r.Problems {
			fmt.Println("  -", p)
		}
	}
}
