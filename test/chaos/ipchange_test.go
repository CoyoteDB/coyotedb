//go:build chaos && unix

package chaos

import "time"

// A main node that the others cannot connect to, while it can still
// connect to them (D62, case 2): its
// lease renewals reach the leader, but no copy can stream from it, so
// every write waits. It must give up its lease and the cluster must fail
// over the normal way; the checker proves no acknowledged write is lost
// and there is never more than one main node.
func init() {
	scenarios = append(scenarios,
		Scenario{Name: "main-unreachable-inbound", Topo: Three, Body: func(r *Run) {
			p, _ := r.leaderOnCopy()
			t0 := r.C.Log.Mark("nobody can connect to " + p.Name + " (it can still connect out)")
			r.C.Net.Unreachable(p.Name, r.C.Names())
			r.WaitNewPrimary(p, t0, time.Minute)
			r.Let(8 * time.Second)
			r.C.Log.Mark("connections to " + p.Name + " work again")
			r.C.Net.Heal()
			r.Let(15 * time.Second)
		}},
	)
}
