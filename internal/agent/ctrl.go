package agent

import (
	"context"
	"strings"
	"time"

	"coyotedb.com/coyote/internal/controller"
	"coyotedb.com/coyote/internal/pg"
)

// agentsAdapter gives the controller the other agents (agent RPC).
type agentsAdapter struct{ a *Agent }

func (x agentsAdapter) Status(ctx context.Context, id string) (*controller.NodeState, error) {
	var r NodeReport
	if err := x.a.call(ctx, id, "status", nil, &r); err != nil {
		return nil, err
	}
	return nodeState(r), nil
}

func (x agentsAdapter) Cut(ctx context.Context, id string, epoch uint64, hold time.Duration) (pg.LSN, error) {
	var cr cutResp
	// Every failover candidate may be promoted (D48 promotion-time check).
	if err := x.a.call(ctx, id, "cut_stream", cutReq{Epoch: epoch, HoldSeconds: int(hold / time.Second), Promote: true}, &cr); err != nil {
		return 0, err
	}
	return pg.ParseLSN(cr.LSN)
}

func (x agentsAdapter) Resume(ctx context.Context, id string) error {
	return x.a.call(ctx, id, "resume_stream", nil, nil)
}

func (x agentsAdapter) SetS(ctx context.Context, primary string, req controller.SetSRequest) (pg.LSN, error) {
	var r setSResp
	if err := x.a.call(ctx, primary, "set_s", req, &r); err != nil {
		return 0, err
	}
	return pg.ParseLSN(r.FlushLSN)
}

// nodeState is the controller's slice of a node's report.
func nodeState(r NodeReport) *controller.NodeState {
	n := &controller.NodeState{Phase: r.State, Marker: r.Marker, Epoch: r.Epoch, LastPrimaryEpoch: r.LastPrimaryEpoch,
		CutEpoch: r.CutEpoch, CutHold: r.CutHold}
	if r.Pages != nil && r.Pages.Damaged {
		n.PagesDamaged = strings.Join(r.Pages.Problems, "; ")
		if n.PagesDamaged == "" {
			n.PagesDamaged = "damaged data pages"
		}
	}
	if p := r.PG; p != nil {
		n.PGRunning, n.Role, n.Streaming, n.StreamCut = p.Running, p.Role, p.Streaming, p.StreamCut
		n.ReceiveLSN, n.ReplayLSN, n.FlushLSN = p.ReceiveLSN, p.ReplayLSN, p.FlushLSN
		n.Standbys, n.Slots = p.Standbys, p.Slots
		if p.Running && p.Role == "primary" && p.Error == "" {
			if k, names, err := pg.ParseSyncNames(p.SyncStandbyNames); err == nil {
				n.SyncCount, n.SyncNames = k, names
			} else {
				// Unknown S: report a name outside R so the controller resets it.
				n.SyncNames = []string{"?" + strings.TrimSpace(p.SyncStandbyNames)}
			}
		}
		if p.Error != "" {
			n.PGRunning = false // cannot be trusted this round
		}
	}
	return n
}
