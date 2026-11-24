package agent

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"coyotedb.com/coyote/internal/netdetect"
)

// The address check (D66, replaces D62's re-detection while running).
//
// A node's address is configured once: given with --advertise, or detected
// at the first start and saved. It is never changed by the node itself:
// big systems (etcd, Consul, CockroachDB, Patroni) take an explicit
// address and say clearly when it is wrong, and so does this one. Every
// few seconds the node checks that this computer still has the addresses
// the cluster records for it. When one is gone it says so, with the
// address it has now and the exact command that fixes it (`coyote node
// set-address`), in `coyote status`, in the log (once) and in the API; and
// it tells the other nodes (RPC addr_report, which it can still send:
// connecting out works from the new address), so their status and
// dashboard show it too, although they can no longer reach it.
//
// What stays from D62 (safety, not configuration): connections bound to an
// address this computer lost are closed (Node.DropDeadConns), and a main
// node no leader reaches gives up its lease (lease.go).
const (
	// addrCheckEvery: how often the recorded addresses are compared with
	// this computer's (cheap: one interface listing).
	addrCheckEvery = 5 * time.Second
	// addrReportTTL: a report from another node is shown this long after
	// it last arrived (it repeats it every addrCheckEvery while it lasts).
	addrReportTTL = 4 * addrCheckEvery
	// addrUnreachableAfter: how long this node must reach other nodes while
	// no leader reaches it before it calls that address_unreachable.
	addrUnreachableAfter = 10 * time.Second
	// addrDetectEvery: while an address is gone, what this computer has
	// now is detected again at most this often (or when its interfaces
	// change).
	addrDetectEvery = time.Minute
)

// addrReport is what a node finds about its own addresses. It is sent to
// the other nodes as RPC addr_report (an empty one clears it).
type addrReport struct {
	// Recorded: what the cluster records for this node.
	Recorded []string `json:"recorded"`
	// Gone: recorded addresses this computer had and no longer has.
	Gone []string `json:"gone,omitempty"`
	// Now: what this computer would use now (detected, HOST:PORT); empty
	// when it cannot tell (a listener on one address, detection failed).
	Now []string `json:"now,omitempty"`
	// NoLeader: this node reaches other nodes, but no leader has reached
	// it for a while (others cannot connect to it: one-way).
	NoLeader bool `json:"no_leader,omitempty"`
	// LeaderContactMS: how long since a leader last reached it (-1: never
	// since it started).
	LeaderContactMS int64 `json:"leader_contact_ms,omitempty"`
	// Since: when this finding started.
	Since time.Time `json:"since"`
}

func (r *addrReport) empty() bool { return r == nil || (len(r.Gone) == 0 && !r.NoLeader) }

func (r *addrReport) key() string {
	if r.empty() {
		return ""
	}
	return fmt.Sprint(r.Recorded, r.Gone, r.Now, r.NoLeader)
}

// addrWatch holds this node's own finding and the ones other nodes sent.
type addrWatch struct {
	mu     sync.Mutex
	self   *addrReport
	others map[string]receivedReport // by node id

	// detection cache (only used while an address is gone)
	nowAddrs []string
	nowKey   string
	nowAt    time.Time

	unreachSince time.Time
	handedOver   time.Time
}

type receivedReport struct {
	addrReport
	At time.Time
}

func (w *addrWatch) selfReport() *addrReport {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.self == nil {
		return nil
	}
	c := *w.self
	return &c
}

// received lists the reports other nodes sent that are still fresh.
func (w *addrWatch) received(now time.Time) map[string]receivedReport {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := map[string]receivedReport{}
	for id, r := range w.others {
		if now.Sub(r.At) <= addrReportTTL {
			out[id] = r
		} else {
			delete(w.others, id)
		}
	}
	return out
}

func (w *addrWatch) receive(from string, r addrReport, now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.others == nil {
		w.others = map[string]receivedReport{}
	}
	if r.empty() {
		delete(w.others, from)
		return
	}
	w.others[from] = receivedReport{addrReport: r, At: now}
}

// addrLoop runs the address check and the dead-connection sweep until ctx
// ends.
func (a *Agent) addrLoop(ctx context.Context) {
	needRecord := a.advertiseExplicit
	t := time.NewTicker(time.Second)
	defer t.Stop()
	var lastCheck time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		// Whatever the node advertises: connections on an address this
		// computer lost are dead (D62).
		a.cn.DropDeadConns()
		if needRecord {
			needRecord = !a.noteRecorded()
		}
		if time.Since(lastCheck) < addrCheckEvery {
			continue
		}
		lastCheck = time.Now()
		a.checkAddress(ctx)
	}
}

// checkAddress compares the cluster's record for this node with what this
// computer has, keeps the finding, logs a change once and tells the other
// nodes.
func (a *Agent) checkAddress(ctx context.Context) {
	now := time.Now()
	prev := a.addrs.selfReport()
	var next *addrReport
	if m, ok := a.cn.OwnRecord(); ok && len(m.Addresses) > 0 {
		next = a.findAddress(ctx, m.Addresses, now)
		a.keepConfigured(m.Addresses)
	}
	if next != nil && prev != nil && next.key() == prev.key() {
		next.Since = prev.Since
	}
	a.addrs.mu.Lock()
	a.addrs.self = next
	a.addrs.mu.Unlock()

	switch {
	case next.key() != prev.key() && !next.empty():
		p := a.selfAddressProblem(next)
		a.log.Warn(p.Summary, "code", p.Code, "recorded", next.Recorded, "gone", next.Gone, "now", next.Now, "fix", strings.Join(p.Commands, " ; "))
	case next.empty() && !prev.empty():
		a.log.Info("address problem solved: the cluster's record matches this computer again", "recorded", a.recordedAddrs())
	}
	a.leaveLeadershipIfGone(ctx, next, now)
	if !next.empty() || !prev.empty() {
		r := addrReport{}
		if next != nil {
			r = *next
		}
		a.sendAddrReport(ctx, r)
	}
}

func (a *Agent) recordedAddrs() []string {
	m, _ := a.cn.OwnRecord()
	return m.Addresses
}

// findAddress returns this node's finding (nil: nothing wrong).
func (a *Agent) findAddress(ctx context.Context, recorded []string, now time.Time) *addrReport {
	have := a.localAddrs()
	saved := a.st.get().AddrSeenLocal
	var seen, gone []string
	for _, ad := range recorded {
		known, present := have(ad)
		switch {
		case !known: // a host name: not checked
		case present:
			seen = append(seen, ad)
		case slices.Contains(saved, ad):
			seen = append(seen, ad)
			gone = append(gone, ad)
		}
	}
	if !sameSet(seen, saved) {
		if err := a.st.update(func(f *agentFile) { f.AddrSeenLocal = seen }); err != nil {
			a.log.Warn("could not save the address check", "err", err)
		}
	}
	r := &addrReport{Recorded: slices.Clone(recorded), Gone: gone, Since: now}
	if len(gone) > 0 {
		r.Now = a.currentAddrs(ctx, now)
	}
	// One-way: this node reaches others, yet no leader has reached it.
	age := a.cn.LeaderContactAge()
	reached := false
	for _, p := range a.peerReports() {
		if slices.ContainsFunc(p.Addresses, func(x PeerAddrProbe) bool { return x.OK }) {
			reached = true
		}
	}
	a.addrs.mu.Lock()
	if a.cn.State().Bootstrapped() && reached && age >= addrUnreachableAfter {
		if a.addrs.unreachSince.IsZero() {
			a.addrs.unreachSince = now
		}
		r.NoLeader = now.Sub(a.addrs.unreachSince) >= addrUnreachableAfter
		r.LeaderContactMS = -1
		if age < time.Duration(1<<62) {
			r.LeaderContactMS = age.Milliseconds()
		}
	} else {
		a.addrs.unreachSince = time.Time{}
	}
	a.addrs.mu.Unlock()
	if r.empty() {
		return nil
	}
	return r
}

// leaveLeadershipIfGone: a Raft leader whose every recorded address is
// gone hands the leadership to another node (D66). Nobody can connect to
// it, so the others cannot forward commands to it, and as the leader it
// would keep its own lease (its leader contact is always fresh) while no
// copy can stream from it. Once it is a follower that no leader reaches,
// the lease rule (D62) makes a main node give up its lease and the cluster
// fails over the normal way. Safety only: no address is changed.
func (a *Agent) leaveLeadershipIfGone(ctx context.Context, r *addrReport, now time.Time) {
	if r == nil || len(r.Gone) == 0 || len(r.Gone) < len(r.Recorded) || !a.cn.IsLeader() || len(a.cn.State().Members) < 2 {
		return
	}
	a.addrs.mu.Lock()
	recent := now.Sub(a.addrs.handedOver) < 30*time.Second
	if !recent {
		a.addrs.handedOver = now
	}
	a.addrs.mu.Unlock()
	if recent {
		return
	}
	a.log.Warn("this computer no longer has its recorded address and nobody can connect to it: handing the leadership to another node", "gone", r.Gone)
	// Not on the loop: the dead-connection sweep must go on meanwhile.
	go func() {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if err := a.cn.TransferLeadership(cctx); err != nil {
			a.log.Warn("could not hand over the leadership", "err", err)
		}
	}()
}

// keepConfigured keeps the saved address (agent.json) equal to the
// cluster's record once it is recorded, so the next start announces what
// the cluster uses (a `node set-address` run on another computer included).
func (a *Agent) keepConfigured(recorded []string) {
	if a.cn.ExplicitPending() {
		return
	}
	if f := a.st.get(); slices.Equal(f.Advertise, recorded) {
		return
	}
	if err := a.st.update(func(f *agentFile) { f.Advertise = slices.Clone(recorded) }); err != nil {
		a.log.Warn("could not save this node's address", "err", err)
	}
}

// localAddrs returns whether this computer has an address: known is false
// for a host name (not checked).
func (a *Agent) localAddrs() func(hostPort string) (known, present bool) {
	if f := os.Getenv(testAddrEnv); f != "" {
		// Tests: the file lists exactly what this "computer" has.
		b, err := os.ReadFile(f)
		list := strings.Split(strings.TrimSpace(string(b)), ",")
		return func(hp string) (bool, bool) {
			if err != nil {
				return false, false
			}
			return true, slices.ContainsFunc(list, func(x string) bool { return strings.TrimSpace(x) == hp })
		}
	}
	ips := interfaceIPs()
	return func(hp string) (bool, bool) {
		h, _, err := net.SplitHostPort(hp)
		if err != nil {
			return false, false
		}
		ip, err := netip.ParseAddr(h)
		if err != nil || ips == nil {
			return false, false
		}
		ip = ip.Unmap().WithZone("")
		return true, ip.IsLoopback() || ip.IsUnspecified() || ips[ip]
	}
}

// interfaceIPs lists this computer's addresses (nil: cannot tell).
func interfaceIPs() map[netip.Addr]bool {
	ifAddrs, err := net.InterfaceAddrs()
	if err != nil || len(ifAddrs) == 0 {
		return nil
	}
	out := make(map[netip.Addr]bool, len(ifAddrs))
	for _, x := range ifAddrs {
		if p, ok := x.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(p.IP); ok {
				out[ip.Unmap().WithZone("")] = true
			}
		}
	}
	return out
}

// currentAddrs is what detection would choose now (cached; detected again
// when the interfaces change or after addrDetectEvery).
func (a *Agent) currentAddrs(ctx context.Context, now time.Time) []string {
	key := ""
	if os.Getenv(testAddrEnv) == "" {
		var ks []string
		for ip := range interfaceIPs() {
			ks = append(ks, ip.String())
		}
		slices.Sort(ks)
		key = strings.Join(ks, ",")
	}
	a.addrs.mu.Lock()
	if key != "" && key == a.addrs.nowKey && now.Sub(a.addrs.nowAt) < addrDetectEvery {
		out := slices.Clone(a.addrs.nowAddrs)
		a.addrs.mu.Unlock()
		return out
	}
	a.addrs.mu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	adv, _, err := detectAdvertise(cctx, a.listen, netdetect.Options{})
	if err != nil {
		adv = nil
	}
	a.addrs.mu.Lock()
	a.addrs.nowAddrs, a.addrs.nowKey, a.addrs.nowAt = slices.Clone(adv), key, now
	a.addrs.mu.Unlock()
	return adv
}

// sendAddrReport tells every other member what this node found (they
// cannot ask: they may not reach it).
func (a *Agent) sendAddrReport(ctx context.Context, r addrReport) {
	st := a.cn.State()
	var wg sync.WaitGroup
	for id := range st.Members {
		if id == a.cn.ID() {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			_ = a.call(cctx, id, "addr_report", r, nil)
		}()
	}
	wg.Wait()
}

func sameSet(x, y []string) bool {
	if len(x) != len(y) {
		return false
	}
	x, y = slices.Clone(x), slices.Clone(y)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}

// noteRecorded: once the cluster records the addresses given with
// --advertise / COYOTE_ADVERTISE, remember that value, so giving it again
// at the next start does not replace an address set later with `node
// set-address` (D62). It reports whether that is done.
func (a *Agent) noteRecorded() bool {
	if a.cn.ExplicitPending() {
		return false
	}
	if err := a.st.update(func(f *agentFile) { f.AdvertiseRecorded = slices.Clone(a.cfg.Advertise) }); err != nil {
		a.log.Warn("could not save the recorded addresses", "err", err)
		return false
	}
	return true
}

// selfAddressProblem is this node's own finding as a problem (the log).
func (a *Agent) selfAddressProblem(r *addrReport) Problem {
	now := time.Now()
	return addressProblem(a.name, *r, a.name, true, true, a.cn.LeaderID() != "", now, now)
}
