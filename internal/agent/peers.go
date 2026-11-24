package agent

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"coyotedb.com/coyote/internal/cluster"
	"coyotedb.com/coyote/internal/netdetect"
)

// PeerProbeInterval is how often a node measures the round trip to every
// other member (one TCP connect per address, in parallel).
const PeerProbeInterval = 30 * time.Second

// PeerReport is one member's addresses as seen from this node.
type PeerReport struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Addresses []PeerAddrProbe `json:"addresses"`
	At        time.Time       `json:"measured_at"`
}

// PeerAddrProbe is one address's result.
type PeerAddrProbe struct {
	Addr  string  `json:"addr"`
	OK    bool    `json:"ok"`
	RTTMS float64 `json:"rtt_ms,omitempty"`
	Error string  `json:"error,omitempty"`
	// Code, when not OK (set-address, D66): address_no_answer (nothing
	// accepted the connection) or address_not_this_node (something
	// answered without that node's certificate).
	Code string `json:"code,omitempty"`
}

type peerTable struct {
	mu   sync.Mutex
	rows []PeerReport
}

func (a *Agent) peerReports() []PeerReport {
	a.peers.mu.Lock()
	defer a.peers.mu.Unlock()
	return slices.Clone(a.peers.rows)
}

// peerLoop measures the round trips until ctx ends.
func (a *Agent) peerLoop(ctx context.Context) {
	t := time.NewTimer(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		a.probePeers(ctx, a.cn.State())
		t.Reset(PeerProbeInterval)
	}
}

func (a *Agent) probePeers(ctx context.Context, st cluster.State) {
	var all []string
	for id, m := range st.Members {
		if id != a.cn.ID() {
			all = append(all, m.Addresses...)
		}
	}
	res := map[string]netdetect.ProbeResult{}
	for _, r := range netdetect.Probe(ctx, all, 0, nil) {
		res[r.Addr] = r
	}
	now := time.Now()
	var rows []PeerReport
	for id, m := range st.Members {
		if id == a.cn.ID() {
			continue
		}
		pr := PeerReport{ID: id, Name: m.Name, At: now}
		for _, ad := range m.Addresses {
			r := res[ad]
			p := PeerAddrProbe{Addr: ad, OK: r.OK}
			if r.OK {
				p.RTTMS = float64(r.RTT.Microseconds()) / 1000
			} else if r.Err != nil {
				p.Error = r.Err.Error()
			}
			pr.Addresses = append(pr.Addresses, p)
		}
		rows = append(rows, pr)
	}
	slices.SortFunc(rows, func(x, y PeerReport) int { return strings.Compare(x.Name, y.Name) })
	a.peers.mu.Lock()
	a.peers.rows = rows
	a.peers.mu.Unlock()
}
