//go:build chaos && unix

package chaos

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Slow or dead peers and slow disks must not make a healthy main node
// fence itself (F1): its lease renewals may never wait behind one dead
// Raft leader or one stalled log write.
func init() {
	scenarios = append(scenarios,
		Scenario{Name: "leader-copy-power-cut", Topo: Three, Body: func(r *Run) {
			// The Raft leader is a copy, not the main node. It loses power
			// and its address goes silent (dials hang, nothing is refused):
			// the main node's renewals must reach the newly elected leader
			// before its lease starts stopping Postgres.
			p, l := r.leaderOnCopy()
			epoch, fences := r.C.Epoch(), fenceCount(p)
			r.C.Log.Mark("power cut Raft leader " + l.Name + " (a copy; its address blackholed)")
			r.C.Net.Isolate(l.Name, r.C.Names(), Blackhole)
			r.C.PowerCut(l)
			r.Let(15 * time.Second)
			r.expectNoFence(p, epoch, fences)
			r.C.Net.Heal()
			l.Restart()
			r.Let(10 * time.Second)
		}},
		stallScenario("leader-disk-stall-1s", time.Second, false),
		stallScenario("leader-disk-stall-3s", 3*time.Second, false),
		stallScenario("main-leader-disk-stall-3s", 3*time.Second, true),
		stallScenario("leader-disk-stall-4500ms", 4500*time.Millisecond, false),
	)
}

// stallScenario stalls every Raft log write on the Raft leader for d
// (once, like an fsync that hangs), with the leader on the main node or
// on a copy.
func stallScenario(name string, d time.Duration, onMain bool) Scenario {
	return Scenario{Name: name, Topo: Three, Env: []string{"COYOTE_TEST_RAFT_STALL_FILE=raft-stall"}, Body: func(r *Run) {
		var p, l *Node
		if onMain {
			p = r.Primary()
			l = r.leaderOn(p)
		} else {
			p, l = r.leaderOnCopy()
		}
		epoch, fences := r.C.Epoch(), fenceCount(p)
		r.C.Log.Mark("stall the Raft log of " + l.Name + " for " + d.String())
		until := time.Now().Add(d).UnixNano()
		if err := os.WriteFile(filepath.Join(clusterDir(r, l), "raft-stall"), []byte(strconv.FormatInt(until, 10)), 0o600); err != nil {
			r.C.Fail("write the stall file: %v", err)
		}
		r.Let(d + 12*time.Second)
		r.expectNoFence(p, epoch, fences)
	}}
}

// leaderOnCopy returns the main node and a copy that is the Raft leader,
// switching the main node away from the Raft leader if they coincide.
func (r *Run) leaderOnCopy() (p, l *Node) {
	for range 4 {
		p = r.Primary()
		if l = r.raftLeader(p); l != p {
			return p, l
		}
		to := r.Standbys(p)[0]
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		out, err := p.CLI(ctx, "switch", "--to", "node-"+to.Name)
		cancel()
		if err != nil {
			r.C.Fail("switch away from the Raft leader: %v\n%s", err, out)
		}
		r.WaitNewPrimary(p, time.Now(), time.Minute)
		r.C.WaitHealthy(2 * time.Minute)
	}
	r.C.Fail("the Raft leader stays on the main node")
	return nil, nil
}

// leaderOn returns the Raft leader, which must be p (it switches the main
// node to the Raft leader otherwise).
func (r *Run) leaderOn(p *Node) *Node {
	for range 4 {
		l := r.raftLeader(p)
		if l == p {
			return l
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		out, err := p.CLI(ctx, "switch", "--to", "node-"+l.Name)
		cancel()
		if err != nil {
			r.C.Fail("switch to the Raft leader: %v\n%s", err, out)
		}
		r.WaitNewPrimary(p, time.Now(), time.Minute)
		r.C.WaitHealthy(2 * time.Minute)
		p = r.Primary()
	}
	r.C.Fail("could not put the main node on the Raft leader")
	return nil
}

// raftLeader is the member p's status names Raft leader.
func (r *Run) raftLeader(p *Node) *Node {
	var name string
	_ = poll(30*time.Second, func() bool {
		st, ok := p.Status()
		if !ok {
			return false
		}
		for _, m := range st.Nodes {
			if m.Leader {
				name = strings.TrimPrefix(m.Name, "node-")
				return true
			}
		}
		return false
	})
	if name == "" {
		r.C.Fail("no Raft leader in the status of %s", p.Name)
	}
	return r.C.Node(name)
}

// clusterDir is the node's cluster state folder (the one holding raft/).
func clusterDir(r *Run, n *Node) string {
	var found string
	_ = filepath.WalkDir(n.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || found != "" {
			return fs.SkipDir
		}
		if d.IsDir() && d.Name() == "raft" {
			found = filepath.Dir(p)
			return fs.SkipAll
		}
		if d.IsDir() && d.Name() == "pgdata" {
			return fs.SkipDir
		}
		return nil
	})
	if found == "" {
		r.C.Fail("no raft folder under %s", n.Dir)
	}
	return found
}

func fenceCount(n *Node) int { return strings.Count(n.Output(), "lost its main-node lease") }

// expectNoFence: p stayed the main node at the same term and never
// stopped its database for want of a lease.
func (r *Run) expectNoFence(p *Node, epoch uint64, fences int) {
	if got := fenceCount(p) - fences; got > 0 {
		r.C.Fail("%s fenced itself %d time(s) although nothing was wrong with it", p.Name, got)
	}
	if e, now := r.C.Epoch(), r.C.PrimaryByCluster(); e != epoch || now != p.Name {
		r.C.Fail("the main node changed (term %d -> %d, now %q): a needless failover", epoch, e, now)
	}
	r.C.Log.Event("%s stayed the main node at term %d without fencing", p.Name, epoch)
}
