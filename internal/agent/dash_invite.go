package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"coyotedb.com/coyote/internal/cluster"
	"coyotedb.com/coyote/internal/cluster/transport"
	"coyotedb.com/coyote/internal/invite"
	"coyotedb.com/coyote/internal/pki"
)

// probeTimeout bounds an invite check's contact with the cluster.
const probeTimeout = 5 * time.Second

// InviteProbe is check-invite's contact with the cluster (D50). It opens
// the join stream exactly as a join does: TLS, with the server's chain
// checked against the CA fingerprint pinned in the code. Nothing is sent
// on it, so the invite is not used up. It cannot tell yet whether the
// secret itself is still unused, nor the cluster's name or platform (that
// needs a new request in the join protocol).
type InviteProbe struct {
	// Reachable: an address answered as a member of the invite's cluster.
	Reachable bool `json:"reachable"`
	// Via is the address that answered.
	Via string `json:"via,omitempty"`
	// ClusterVerified: the answer was signed by the cluster this code was
	// made for (its CA fingerprint matched).
	ClusterVerified bool `json:"cluster_verified"`
	// WrongCluster: a machine answered, but it is not the invite's cluster
	// (another cluster on that address, or something else entirely).
	WrongCluster bool `json:"wrong_cluster,omitempty"`
	// Tried lists every address, when none answered as the cluster.
	Tried []string `json:"tried,omitempty"`
	// Message is one plain sentence about the result.
	Message string `json:"message"`
}

// probeInvite tries every address of the code at once; the first that
// answers as the cluster wins.
func probeInvite(ctx context.Context, inv *invite.Invite, redirect func(string) string) InviteProbe {
	type res struct {
		addr string
		err  error
	}
	tt := transport.Timing{DialTimeout: probeTimeout, Redirect: redirect}
	ch := make(chan res, len(inv.Addrs))
	var wg sync.WaitGroup
	for _, addr := range inv.Addrs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := transport.DialJoin(ctx, addr, inv.ClusterID, inv.CAFingerprint, tt)
			if err == nil {
				c.Close()
			}
			ch <- res{addr, err}
		}()
	}
	go func() { wg.Wait(); close(ch) }()
	p := InviteProbe{}
	for r := range ch {
		switch {
		case r.err == nil:
			return InviteProbe{Reachable: true, ClusterVerified: true, Via: r.addr,
				Message: fmt.Sprintf("The cluster answered at %s, and it is the cluster this code was made for.", r.addr)}
		case errors.Is(r.err, pki.ErrFingerprint):
			p.WrongCluster = true
		}
	}
	p.Tried = append([]string(nil), inv.Addrs...)
	if p.WrongCluster {
		p.Message = "A machine answered, but it is not the cluster this code was made for, so nothing was sent to it. Check that the code is the newest one, or create a new invite."
	} else {
		p.Message = fmt.Sprintf("No answer from %s. Check that the computer that made the code is running, that both computers are on the same network (or Tailscale), and that its firewall lets port %s in.",
			strings.Join(inv.Addrs, ", "), firstPort(inv.Addrs))
	}
	return p
}

func firstPort(addrs []string) string {
	for _, a := range addrs {
		if i := strings.LastIndexByte(a, ':'); i >= 0 {
			return a[i+1:]
		}
	}
	return "7481"
}

// cancelledBy marks a join secret cancelled from the dashboard. Cancel
// uses the existing "consume join secret" command (every version of the
// state machine knows it): the secret is marked used, by no node, so no
// joiner can ever use it, and a retry can never match it (that needs the
// joiner's own node id and key). Not a node id: those are 16 hex digits.
const cancelledBy = "cancelled"

// apiCancelInvite is DELETE /v1/invites/{id}: cancel an unused invite.
func (d *dashServer) apiCancelInvite(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	has := func(st *cluster.State) bool {
		for h := range st.JoinSecrets {
			if inviteID(h) == id {
				return true
			}
		}
		return false
	}
	// An invite made a moment ago through another node (the leader) may
	// not have reached this node's copy of the state yet.
	if cur := d.a.cn.State(); !has(&cur) {
		wctx, wcancel := context.WithTimeout(r.Context(), 3*time.Second)
		_ = d.a.cn.WaitFor(wctx, has)
		wcancel()
	}
	st := d.a.cn.State()
	var hash string
	for h := range st.JoinSecrets {
		if inviteID(h) == id {
			if hash != "" {
				writeErr(w, http.StatusConflict, "failed", "that invite id is not unique; cancel it from a newer dashboard", nil)
				return
			}
			hash = h
		}
	}
	js, ok := st.JoinSecrets[hash]
	switch {
	case hash == "" || !ok:
		writeErr(w, http.StatusNotFound, "unknown_invite", "there is no such invite (it may have expired and been cleared)", nil)
		return
	case js.Used && js.UsedBy == cancelledBy:
		w.WriteHeader(http.StatusNoContent) // already cancelled
		return
	case js.Used:
		writeErr(w, http.StatusConflict, "secret_used", "that invite was already used; to take that computer out, remove it from the cluster", nil)
		return
	case !time.Now().Before(js.ExpiresAt):
		writeErr(w, http.StatusConflict, "secret_expired", "that invite has already expired; it cannot be used any more", nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if _, err := d.a.cn.Apply(ctx, &cluster.ConsumeJoinSecret{Hash: hash, NodeID: cancelledBy, At: time.Now()}); err != nil {
		var ce *cluster.Error
		if errors.As(err, &ce) && (errors.Is(err, cluster.ErrSecretUsed) || errors.Is(err, cluster.ErrSecretExpired)) {
			apiErr(w, err)
			return
		}
		apiErr(w, fmt.Errorf("could not cancel the invite (the cluster must have a leader): %w", err))
		return
	}
	d.a.log.Info("dashboard: invite cancelled", "id", id)
	w.WriteHeader(http.StatusNoContent)
}
