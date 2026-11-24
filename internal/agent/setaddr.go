package agent

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"coyotedb.com/coyote/internal/brand"
	"coyotedb.com/coyote/internal/cluster"
	"coyotedb.com/coyote/internal/control"
)

// SetAddressRequest is POST /v1/members/{node}/addresses.
type SetAddressRequest struct {
	Addresses []string `json:"addresses"`
	// Force records the addresses even if none of them answers from here
	// (the node is down, or this machine cannot reach it but others can).
	Force bool `json:"force,omitempty"`
	// Local records the addresses on this machine only, without the
	// cluster (D62): for when no leader can be elected because the
	// computers got new addresses. They are dialed first and forgotten
	// once the cluster's record holds them.
	Local bool `json:"local,omitempty"`
}

// SetAddressResponse reports the check of every address and what was
// recorded.
type SetAddressResponse struct {
	Node     string          `json:"node"`
	Probes   []PeerAddrProbe `json:"probes"`
	Recorded bool            `json:"recorded"`
	Message  string          `json:"message"`
	// Code, when nothing was recorded (D66): address_no_answer or
	// address_not_this_node.
	Code string `json:"code,omitempty"`
}

// refusedCode is why no probe passed: one that answered with the wrong
// certificate wins over plain silence.
func refusedCode(ps []PeerAddrProbe) string {
	if slices.ContainsFunc(ps, func(p PeerAddrProbe) bool { return p.Code == codeAddressNotThisNode }) {
		return codeAddressNotThisNode
	}
	return codeAddressNoAnswer
}

const setAddrProbeTimeout = 3 * time.Second

// apiSetAddress records the addresses other members use to reach a node.
// Each address is checked first from this node: a TCP connect plus the
// cluster's mutual-TLS handshake, which must present that node's own
// certificate (so a typo that reaches some other machine is caught). The
// record is pinned: the node keeps it instead of announcing what it
// detects, until it is started with an explicit --advertise.
func (a *Agent) apiSetAddress(w http.ResponseWriter, r *http.Request) {
	var req SetAddressRequest
	if err := control.ReadJSON(r, &req); err != nil {
		control.WriteError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	var addrs []string
	for _, ad := range req.Addresses {
		ad = strings.TrimSpace(ad)
		if ad == "" || slices.Contains(addrs, ad) {
			continue
		}
		if err := validHostPort(ad); err != nil {
			control.WriteError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("%q: %v (use HOST:PORT)", ad, err))
			return
		}
		addrs = append(addrs, ad)
	}
	if len(addrs) == 0 || len(addrs) > 8 {
		control.WriteError(w, http.StatusBadRequest, "bad_request", "give 1 to 8 addresses as HOST:PORT")
		return
	}
	if req.Local {
		a.setLocalAddress(w, r, req, addrs)
		return
	}
	st := a.cn.State()
	m, ok := findMember(st, r.PathValue("node"))
	if !ok {
		msg := fmt.Sprintf("no node called %q in this cluster", r.PathValue("node"))
		if !st.Bootstrapped() {
			// D62: the state is not loaded yet (no leader since this
			// agent started), which is not the same as "no such node".
			msg = fmt.Sprintf("this node has not heard from the cluster since it started (no leader yet), so it cannot change what the cluster records. "+
				"If the computers got new addresses, tell this one where %s is now with --local: %s node set-address --local %s HOST:PORT",
				r.PathValue("node"), brand.Command, r.PathValue("node"))
			control.WriteError(w, http.StatusConflict, codeNoLeader, msg)
			return
		}
		control.WriteError(w, http.StatusNotFound, "unknown_member", msg)
		return
	}
	resp := SetAddressResponse{Node: m.Name, Probes: a.probeMember(r.Context(), m.ID, addrs)}
	reachable := slices.ContainsFunc(resp.Probes, func(p PeerAddrProbe) bool { return p.OK })
	if !reachable && !req.Force {
		resp.Code = refusedCode(resp.Probes)
		resp.Message = fmt.Sprintf("None of these addresses answered as %s from this machine, so nothing was changed. If %s is down, or only other machines can reach it, run the command again with --force.", m.Name, m.Name)
		if resp.Code == codeAddressNotThisNode {
			resp.Message = fmt.Sprintf("Something answered, but it is not %s of this cluster (another computer or node has that address), so nothing was changed. Check which computer has the address now.", m.Name)
		}
		control.WriteJSON(w, http.StatusOK, resp) // Recorded false: the caller says so
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	pinned := true
	if _, err := a.cn.Apply(ctx, &cluster.UpdateMember{ID: m.ID, Addresses: addrs, Pinned: &pinned}); err != nil {
		apiErr(w, fmt.Errorf("record the addresses (the cluster must have a leader): %w", err))
		return
	}
	if m.ID == a.cn.ID() {
		// This node: a later plain `up` must not bring back the old ones.
		if err := a.st.update(func(f *agentFile) { f.Advertise = slices.Clone(addrs) }); err != nil {
			apiErr(w, err)
			return
		}
	}
	resp.Recorded = true
	resp.Message = fmt.Sprintf("Other nodes now reach %s at %s.", m.Name, strings.Join(addrs, ", "))
	control.WriteJSON(w, http.StatusOK, resp)
}

func (a *Agent) probeMember(ctx context.Context, id string, addrs []string) []PeerAddrProbe {
	out := make([]PeerAddrProbe, len(addrs))
	var wg sync.WaitGroup
	for i, ad := range addrs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pctx, cancel := context.WithTimeout(ctx, setAddrProbeTimeout)
			defer cancel()
			rtt, err := a.cn.ProbePeer(pctx, id, ad)
			p := PeerAddrProbe{Addr: ad, OK: err == nil}
			if err != nil {
				p.Error = err.Error()
				// A handshake failure comes after the connection (rtt set).
				p.Code = codeAddressNoAnswer
				if rtt > 0 {
					p.Code = codeAddressNotThisNode
				}
			} else {
				p.RTTMS = float64(rtt.Microseconds()) / 1000
			}
			out[i] = p
		}()
	}
	wg.Wait()
	return out
}

// setLocalAddress is set-address --local (D62): checked like a normal
// set-address (the address must answer as that very node), then kept on
// this machine only. It needs no leader: it is how a cluster whose
// computers all got new addresses finds its way back.
func (a *Agent) setLocalAddress(w http.ResponseWriter, r *http.Request, req SetAddressRequest, addrs []string) {
	id, name, ok := a.cn.FindPeer(r.PathValue("node"))
	if !ok {
		control.WriteError(w, http.StatusNotFound, "unknown_member", fmt.Sprintf("this node knows no other node called %q", r.PathValue("node")))
		return
	}
	if name == "" {
		name = id
	}
	resp := SetAddressResponse{Node: name, Probes: a.probeMember(r.Context(), id, addrs)}
	if !slices.ContainsFunc(resp.Probes, func(p PeerAddrProbe) bool { return p.OK }) && !req.Force {
		resp.Code = refusedCode(resp.Probes)
		resp.Message = fmt.Sprintf("None of these addresses answered as %s from this machine, so nothing was changed. Check that %s is running and the address is right; --force keeps it anyway.", name, name)
		if resp.Code == codeAddressNotThisNode {
			resp.Message = fmt.Sprintf("Something answered, but it is not %s of this cluster (another computer or node has that address), so nothing was changed.", name)
		}
		control.WriteJSON(w, http.StatusOK, resp)
		return
	}
	if err := a.cn.SetLocalAddrs(id, addrs); err != nil {
		apiErr(w, err)
		return
	}
	resp.Recorded = true
	resp.Message = fmt.Sprintf("This machine now reaches %s at %s. Once enough nodes reach each other to choose a leader, each node records its own new address for the whole cluster.",
		name, strings.Join(addrs, ", "))
	control.WriteJSON(w, http.StatusOK, resp)
}
