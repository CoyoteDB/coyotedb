//go:build docker

package docker

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"coyotedb.com/coyote/test/chaos/check"
	"coyotedb.com/coyote/test/testports"
)

// TestDockerUpdate moves a running cluster from one CoyoteDB release to
// the next, one computer at a time, exactly as docker/README.md ("Updating
// to a new version") tells people to: the copies first, then the witness,
// then a planned switch-over, then the old main node. Rows are written all
// the time through the client ports; every data node's own Postgres is
// probed for writability all the time (the chaos checker's commit probe,
// TESTING.md). At the end: no acknowledged row lost, identical data on
// every node, never two main nodes, every container on the new version,
// and a new computer on the new version can join.
//
//	COYOTE_DOCKER_FROM   the release the cluster starts on
//	                     (default coyotedb/coyote:0.1.0, pulled if missing)
//	COYOTE_DOCKER_TO     the new image (default coyote-test/coyote:next),
//	                     built from the working tree unless
//	                     COYOTE_DOCKER_BUILD=0
//	COYOTE_DOCKER_TO_VERSION  the version it is built with (0.1.1-test)
func TestDockerUpdate(t *testing.T) {
	from := env("COYOTE_DOCKER_FROM", "coyotedb/coyote:0.1.0")
	if _, err := docker(context.Background(), "image", "inspect", from); err != nil {
		dockerT(t, 20*time.Minute, "pull", from)
	}
	to := env("COYOTE_DOCKER_TO", "coyote-test/coyote:next")
	toVersion := env("COYOTE_DOCKER_TO_VERSION", "0.1.1-test")
	buildImage(t, to, "COYOTE_VERSION="+toVersion, "COYOTE_IMAGE="+to)
	fromV, toV := imageVersion(t, from), imageVersion(t, to)
	t.Logf("updating from %s (%s) to %s (%s)", from, fromV, to, toV)
	if fromV == toV {
		t.Fatalf("both images report %q; the test needs two different versions", fromV)
	}

	r := newRig(t, from)
	n1, n2, n3, w := r.node("n1"), r.node("n2"), r.node("n3"), r.node("w")
	r.form()
	cfg := r.appConfig()

	pr := startProbes(t, r, []node{n1, n2, n3})
	probesStopped := false
	defer func() {
		if !probesStopped {
			pr.halt()
		}
	}()
	wr := startWriter(t, cfg)
	writerStopped := false
	defer func() {
		if !writerStopped {
			wr.halt()
		}
	}()
	moreWrites := func(what string, n int) {
		t.Helper()
		before := wr.okCount()
		waitFor(t, fmt.Sprintf("%d acknowledged writes %s", n, what), 3*time.Minute, func() error {
			if got := wr.okCount() - before; got < n {
				return fmt.Errorf("%d so far", got)
			}
			return nil
		})
	}
	moreWrites("before the update", 300)

	// healthy: main is the main node, every other data node a copy that
	// follows live and is in the sync set again, every node reachable.
	healthy := func(main string, d time.Duration) time.Duration {
		t.Helper()
		var copies []string
		for _, n := range r.data() {
			if n.name != main {
				copies = append(copies, n.name)
			}
		}
		took := r.waitHealthy(r.node(main), main, copies, d)
		waitFor(t, "every node reachable and both copies in the sync set", d, func() error {
			s, err := r.status(r.node(main))
			if err != nil {
				return err
			}
			for _, m := range s.Nodes {
				if !m.Reachable {
					return fmt.Errorf("%s not reachable", m.Name)
				}
			}
			for _, c := range copies {
				if !slices.Contains(s.Cluster.SyncSet, c) {
					return fmt.Errorf("sync set %v has no %s", s.Cluster.SyncSet, c)
				}
			}
			return nil
		})
		return took
	}

	// replace stops a node cleanly, removes its container (the volume
	// stays) and runs the same command on the new image.
	replace := func(n node, mode string) {
		t.Helper()
		at := time.Now()
		dockerT(t, 3*time.Minute, "stop", n.container())
		if code := strings.TrimSpace(dockerT(t, time.Minute, "inspect", "--format", "{{.State.ExitCode}}", n.container())); code != "0" {
			t.Errorf("%s: exit code %s after docker stop, want 0", n.name, code)
		}
		if !n.witness {
			// A copy says "shut down in recovery", a main node "shut down".
			if st := r.controlState(n); st != "shut down" && st != "shut down in recovery" {
				t.Errorf("%s: pg_controldata says %q after docker stop, want a clean shutdown", n.name, st)
			}
		}
		dockerT(t, time.Minute, "rm", n.container())
		r.startWith(to, n, mode, "")
		t.Logf("%s: stopped cleanly and started on %s after %s", n.name, to, time.Since(at).Round(100*time.Millisecond))
	}

	// 1. The copies, one at a time. The main node stays on the old version
	// and keeps taking writes; the other copy keeps them saved.
	for _, c := range []node{n2, n3} {
		replace(c, "join")
		d := healthy("n1", 10*time.Minute)
		t.Logf("%s back on the new version, following live, in the sync set, after %s", c.name, d.Round(time.Second))
		moreWrites("after updating "+c.name, 100)
	}

	// 2. The witness.
	replace(w, "witness")
	waitFor(t, "the witness back", 5*time.Minute, func() error {
		s, err := r.status(n1)
		if err != nil {
			return err
		}
		if st, _ := s.state("w"); st != "witness" {
			return fmt.Errorf("witness is %q", st)
		}
		return nil
	})
	healthy("n1", 5*time.Minute)
	moreWrites("after updating the witness", 100)

	// 3. A planned switch-over to an updated copy.
	at := time.Now()
	out := dockerT(t, 7*time.Minute, "exec", n1.container(), "coyote", "switch", "--to", "n2")
	t.Logf("switch: %s", strings.TrimSpace(out))
	d := healthy("n2", 5*time.Minute)
	t.Logf("n2 is the main node and n1 follows it %s after the switch started (%s until healthy)", time.Since(at).Round(100*time.Millisecond), d.Round(time.Second))
	if ok, found := wr.firstOKAfter(at); found {
		t.Logf("first write acknowledged after the switch was sent %s after it started", ok.Sub(at).Round(100*time.Millisecond))
	}
	moreWrites("after the switch", 100)

	// 4. The old main node, now a copy.
	replace(n1, "up")
	d = healthy("n2", 10*time.Minute)
	t.Logf("n1 back on the new version as a copy after %s", d.Round(time.Second))
	moreWrites("after updating n1", 100)

	// 5. A new computer on the new version can join the updated cluster
	// (the cluster records the PostgreSQL build every node now runs, rule
	// 1), and gets every row.
	n4 := node{name: "n4"}
	var err error
	if n4.clusterPort, err = testports.Free(); err != nil {
		t.Fatal(err)
	}
	if n4.clientPort, err = testports.Free(); err != nil {
		t.Fatal(err)
	}
	r.nodes = append(r.nodes, n4) // removed with the others at the end
	r.startWith(to, n4, "join", r.invite(n2))
	d = r.waitHealthy(n2, "n2", []string{"n1", "n3", "n4"}, 10*time.Minute)
	t.Logf("n4 joined on the new version and follows live after %s", d.Round(time.Second))

	// 6. Every container runs the new version.
	for _, n := range r.nodes {
		img := strings.TrimSpace(dockerT(t, time.Minute, "inspect", "--format", "{{.Config.Image}}", n.container()))
		v := strings.TrimSpace(dockerT(t, time.Minute, "exec", n.container(), "coyote", "version"))
		if img != to || !strings.Contains(v, " "+toV+" ") {
			t.Errorf("%s runs %s (%q), want %s (%s)", n.name, img, v, to, toV)
		} else {
			t.Logf("%s: %s", n.name, v)
		}
	}

	// 7. No node reports a problem: in particular none says its database
	// programs changed since it joined (a PostgreSQL minor update must be
	// recorded, not reported as drift, rule 1).
	waitFor(t, "no warnings on any data node", 3*time.Minute, func() error {
		for _, n := range r.data() {
			s, err := r.status(n)
			if err != nil {
				return err
			}
			if len(s.Warnings) > 0 {
				return fmt.Errorf("%s: %q", n.name, s.Warnings)
			}
		}
		return nil
	})

	// 8. Rows (on all four data nodes), then the two-mains check.
	wr.halt()
	writerStopped = true
	r.checkRows(wr, n2)
	pr.halt()
	probesStopped = true
	pr.verdict()
}

// imageVersion is the version an image's coyote reports.
func imageVersion(t *testing.T, img string) string {
	t.Helper()
	out := dockerT(t, 2*time.Minute, "run", "--rm", "--entrypoint", "coyote", img, "version")
	f := strings.Fields(out)
	if len(f) < 2 {
		t.Fatalf("%s: coyote version printed %q", img, out)
	}
	return f[1]
}

// ---- the commit probe ----

// probes runs the chaos checker's commit probe (docs/TESTING.md "Two main
// nodes: the commit probe"; test/docker/probe) inside every data node's
// container, against its own Postgres, from the start to the end of the
// roll. A replaced container gets the probe again as soon as it runs.
// The probe's instants come from the kernel every container shares, so
// one clock orders all nodes; check.TwoMains judges them.
type probes struct {
	t    *testing.T
	bin  string
	mu   sync.Mutex
	all  []check.Probe
	stop chan struct{}
	wg   sync.WaitGroup
}

func startProbes(t *testing.T, r *rig, nodes []node) *probes {
	arch := strings.TrimSpace(dockerT(t, time.Minute, "version", "--format", "{{.Server.Arch}}"))
	bin := filepath.Join(t.TempDir(), "coyote-probe")
	cmd := exec.Command("go", "build", "-o", bin, "./probe")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build the probe for linux/%s: %v\n%s", arch, err, out)
	}
	p := &probes{t: t, bin: bin, stop: make(chan struct{})}
	for _, n := range nodes {
		cs, err := r.adminConn(n)
		if err != nil {
			t.Fatalf("%s: admin connection string: %v", n.name, err)
		}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			for !p.stopped() {
				if err := p.run(n, cs); err != nil && !p.stopped() {
					select { // container being replaced: try again soon
					case <-p.stop:
					case <-time.After(100 * time.Millisecond):
					}
				}
			}
		}()
	}
	waitFor(t, "every probe answering", 2*time.Minute, func() error {
		p.mu.Lock()
		defer p.mu.Unlock()
		seen := map[string]bool{}
		for _, pr := range p.all {
			seen[pr.Node] = true
		}
		if len(seen) < len(nodes) {
			return fmt.Errorf("%d of %d", len(seen), len(nodes))
		}
		return nil
	})
	return p
}

// adminConn is the node's own Postgres, as the superuser, from inside its
// container (127.0.0.1 only, rule 11). It stays the same across restarts.
func (r *rig) adminConn(n node) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cs, err := docker(ctx, "exec", n.container(), "coyote", "conninfo", "--admin")
	return strings.TrimSpace(cs), err
}

// run copies the probe into the node's container and runs it until the
// container stops or the probes are halted (closing its stdin ends it).
func (p *probes) run(n node, cs string) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	_, err := docker(ctx, "cp", p.bin, n.container()+":/tmp/coyote-probe")
	cancel()
	if err != nil {
		return err
	}
	cmd := exec.Command("docker", "exec", "-i", "-e", "COYOTE_PROBE_CONN="+cs, n.container(), "/tmp/coyote-probe")
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-p.stop:
		case <-done:
		}
		_ = in.Close()
	}()
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		var l probeLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			continue
		}
		k, ok := probeKinds[l.Kind]
		if !ok {
			continue
		}
		p.mu.Lock()
		p.all = append(p.all, check.Probe{Node: n.name, Start: time.Duration(l.Start), End: time.Duration(l.End), Kind: k, Conn: l.Conn, Detail: l.Detail})
		p.mu.Unlock()
	}
	close(done)
	return cmd.Wait()
}

// probeLine is test/docker/probe's output line.
type probeLine struct {
	Start  int64  `json:"start_ns"`
	End    int64  `json:"end_ns"`
	Kind   string `json:"kind"`
	Conn   string `json:"conn"`
	Detail string `json:"detail"`
}

var probeKinds = map[string]check.Kind{"rw": check.RW, "not-rw": check.NotRW, "down": check.Down, "unknown": check.NoAnswer}

func (p *probes) stopped() bool {
	select {
	case <-p.stop:
		return true
	default:
		return false
	}
}

func (p *probes) halt() {
	close(p.stop)
	p.wg.Wait()
}

// verdict runs the two-mains check over every probe.
func (p *probes) verdict() {
	p.mu.Lock()
	all := slices.Clone(p.all)
	p.mu.Unlock()
	kinds := map[string]map[check.Kind]int{}
	for _, pr := range all {
		if kinds[pr.Node] == nil {
			kinds[pr.Node] = map[check.Kind]int{}
		}
		kinds[pr.Node][pr.Kind]++
	}
	for _, n := range slices.Sorted(maps.Keys(kinds)) {
		k := kinds[n]
		p.t.Logf("probes %s: %d rw, %d not-rw, %d down, %d unknown", n, k[check.RW], k[check.NotRW], k[check.Down], k[check.NoAnswer])
	}
	rep := check.TwoMains(all)
	for _, s := range rep.Notes {
		if strings.HasPrefix(s, "handover") {
			p.t.Log(s)
		}
	}
	for _, s := range rep.Proven {
		p.t.Errorf("two main nodes (proven): %s", s)
	}
	for _, s := range rep.Unproven {
		p.t.Errorf("two main nodes not excluded: %s", s)
	}
	if len(rep.Proven)+len(rep.Unproven) == 0 {
		p.t.Logf("never two main nodes: %d probes, %d write-accepting stretches, each handover proven apart", len(all), len(rep.Runs))
	}
}
