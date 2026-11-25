package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"coyotedb.com/coyote/internal/cluster"
	"coyotedb.com/coyote/internal/cluster/transport"
	"coyotedb.com/coyote/internal/controller"
	"coyotedb.com/coyote/internal/pg"
)

// Agent RPC: one JSON request and one JSON response per stream, on
// transport.StreamAgent (members only; the transport checks the peer's
// certificate against the membership). Bodies never contain secrets.
//
//	request:  {"method": "...", "body": {...}}
//	response: {"error": "...", "code": "...", "body": {...}}
//
// Methods:
//
//	status         this node's report (role, Postgres, replication)
//	confirm        (leader) linearizable read: index, epoch, primary
//	cut_stream     cut this standby's stream (rule 2); keeps it cut while the epoch is unchanged
//	resume_stream  undo a cut (an aborted switch)
//	switch            (leader) run a switch to another data node
//	sync_remove       (primary) remove a member from S, then R (leave, remove)
//	set_sync          (primary) replace R and k (dev set-sync)
//	set_s             (primary) the controller sets S; returns the flush LSN
//	controller_status (leader) the controller's report
//	removed           tell a node it was removed from the cluster
//	guard_now         (data node) run the portability audit now and record it (mixed join check)
//	caps              which newer features this agent knows ("mixed_os": the D37 state fields; "backup": D63; "pg_builds": D70)
//	backup_now        take a full backup now (D63)
//	mixed_prepare     (primary) make every database builtin-only before a mixed join (D48)
//	addr_report       what the calling node found about its own address (D66; empty clears it)
type rpcRequest struct {
	Method string          `json:"method"`
	Body   json.RawMessage `json:"body,omitempty"`
}

type rpcResponse struct {
	Error string          `json:"error,omitempty"`
	Code  string          `json:"code,omitempty"`
	Body  json.RawMessage `json:"body,omitempty"`
}

// rpcPeerKey: the context key for the verified id of the member that
// opened the RPC stream.
type rpcPeerKey struct{}

// rpcError is an error returned by a remote agent.
type rpcError struct {
	Code string
	Msg  string
}

func (e *rpcError) Error() string { return e.Msg }

const (
	codeNotLeader  = "not_leader"
	codeNotPrimary = "not_primary"
)

type confirmResp struct {
	Index   uint64 `json:"index"`
	Epoch   uint64 `json:"epoch"`
	Primary string `json:"primary"`
}

type cutReq struct {
	Epoch uint64 `json:"epoch"`
	// HoldSeconds: resume by itself after this long if the epoch has not
	// moved on (0: hold until the next switch or resume-stream).
	HoldSeconds int `json:"hold_seconds,omitempty"`
	// Promote: this copy may be made the main node after the cut (a
	// failover candidate, or a switch's target), so it runs the D48
	// promotion-time check. Older agents ignore it.
	Promote bool `json:"promote,omitempty"`
}

type cutResp struct {
	LSN string `json:"lsn"`
	// PGBuild is the sha256 of the PostgreSQL build this node runs (D70;
	// absent from agents before it).
	PGBuild string `json:"pg_build,omitempty"`
}

type syncNodeReq struct {
	Node string `json:"node"`
}

type setSyncReq struct {
	Standbys []string `json:"standbys"`
	Count    int      `json:"count"`
}

type switchReq struct {
	To string `json:"to"`
	// Planned: the `coyote switch` rules (target eligible and in R, strict
	// compare-and-set). Without it, the M3 `dev switch`.
	Planned bool `json:"planned,omitempty"`
	// From and Epoch, when set, make the switch conditional: it goes ahead
	// only if From is still the main node at Epoch (a main node handing
	// over for its health, D58, whose own view may be stale).
	From  string `json:"from,omitempty"`
	Epoch uint64 `json:"epoch,omitempty"`
}

type setSResp struct {
	FlushLSN string `json:"flush_lsn"`
}

type removedReq struct {
	By string `json:"by"`
}

// call runs method on nodeID's agent (directly when nodeID is this node).
func (a *Agent) call(ctx context.Context, nodeID, method string, in, out any) error {
	var body json.RawMessage
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = b
	}
	var resp rpcResponse
	if nodeID == a.cn.ID() {
		resp = a.dispatch(ctx, rpcRequest{Method: method, Body: body})
	} else {
		c, err := a.cn.Dial(ctx, nodeID, transport.StreamAgent)
		if err != nil {
			return fmt.Errorf("reach %s: %w", a.memberName(nodeID), err)
		}
		defer c.Close()
		if dl, ok := ctx.Deadline(); ok {
			_ = c.SetDeadline(dl)
		}
		stop := context.AfterFunc(ctx, func() { _ = c.SetDeadline(time.Unix(1, 0)) })
		defer stop()
		if err := json.NewEncoder(c).Encode(rpcRequest{Method: method, Body: body}); err != nil {
			return fmt.Errorf("send to %s: %w", a.memberName(nodeID), err)
		}
		if err := json.NewDecoder(io.LimitReader(c, 16<<20)).Decode(&resp); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("no answer from %s: %w", a.memberName(nodeID), err)
		}
	}
	if resp.Error != "" {
		return &rpcError{Code: resp.Code, Msg: resp.Error}
	}
	if out != nil && len(resp.Body) > 0 {
		return json.Unmarshal(resp.Body, out)
	}
	return nil
}

func (a *Agent) memberName(id string) string {
	if m, ok := a.cn.State().Members[id]; ok {
		return m.Name
	}
	return id
}

// serveAgent is the StreamAgent handler.
func (a *Agent) serveAgent(s *transport.Stream) {
	if err := s.Accept(); err != nil {
		return
	}
	_ = s.SetDeadline(time.Now().Add(10 * time.Second))
	var req rpcRequest
	if err := json.NewDecoder(io.LimitReader(s, 1<<20)).Decode(&req); err != nil {
		return
	}
	timeout := 30 * time.Second
	if req.Method == "switch" {
		timeout = 5 * time.Minute
	}
	_ = s.SetDeadline(time.Now().Add(timeout + 5*time.Second))
	ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), rpcPeerKey{}, s.Peer), timeout)
	defer cancel()
	resp := a.dispatch(ctx, req)
	_ = json.NewEncoder(s).Encode(resp)
}

func (a *Agent) dispatch(ctx context.Context, req rpcRequest) rpcResponse {
	out, err := a.handle(ctx, req)
	if err != nil {
		code := ""
		switch {
		case errors.Is(err, errNotPrimary):
			code = codeNotPrimary
		case errors.Is(err, cluster.ErrNotLeader):
			code = codeNotLeader
		}
		var re *rpcError
		if errors.As(err, &re) && code == "" {
			code = re.Code
		}
		return rpcResponse{Error: err.Error(), Code: code}
	}
	if out == nil {
		return rpcResponse{}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return rpcResponse{Error: err.Error()}
	}
	return rpcResponse{Body: b}
}

func decodeBody(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, v)
}

func (a *Agent) handle(ctx context.Context, req rpcRequest) (any, error) {
	switch req.Method {
	case "ping":
		return nil, nil
	case "status":
		r := a.report(ctx)
		return r, nil
	case "confirm":
		if !a.cn.IsLeader() {
			return nil, cluster.ErrNotLeader
		}
		st, err := a.cn.LinearizableState(ctx)
		if err != nil {
			return nil, err
		}
		return confirmResp{Index: st.AppliedIndex, Epoch: st.Epoch, Primary: st.PrimaryID}, nil
	case "cut_stream":
		var r cutReq
		if err := decodeBody(req.Body, &r); err != nil {
			return nil, err
		}
		lsn, err := a.cutStreamFor(ctx, r.Epoch, time.Duration(r.HoldSeconds)*time.Second, r.Promote)
		if err != nil {
			return nil, err
		}
		return cutResp{LSN: lsn.String(), PGBuild: a.inst.Build.SHA256}, nil
	case "resume_stream":
		return nil, a.resumeStream(ctx)
	case "step_down", "step_up": // planned switch, old main node (lease_stepdown.go)
		return a.handleStepDown(ctx, req.Method, req.Body)
	case "switch":
		var r switchReq
		if err := decodeBody(req.Body, &r); err != nil {
			return nil, err
		}
		return a.runSwitchFrom(ctx, r.To, r.Planned, r.From, r.Epoch)
	case "sync_remove":
		var r syncNodeReq
		if err := decodeBody(req.Body, &r); err != nil {
			return nil, err
		}
		return nil, a.syncRemove(ctx, r.Node)
	case "set_s":
		var r controller.SetSRequest
		if err := decodeBody(req.Body, &r); err != nil {
			return nil, err
		}
		l, err := a.setS(ctx, r.Epoch, r.Names, r.K, r.Barrier)
		if err != nil {
			return nil, err
		}
		return setSResp{FlushLSN: l.String()}, nil
	case "controller_status":
		if !a.cn.IsLeader() {
			return nil, cluster.ErrNotLeader
		}
		return a.ctrl.Report(), nil
	case "set_sync":
		var r setSyncReq
		if err := decodeBody(req.Body, &r); err != nil {
			return nil, err
		}
		return nil, a.setSync(ctx, r.Standbys, r.Count)
	case "guard_now":
		return nil, a.guardNow(ctx)
	case "caps":
		return capsResp{MixedOS: true, Backup: true, PGBuilds: true}, nil
	case "backup_now":
		return a.rpcBackupNow(ctx, req.Body)
	case "mixed_prepare":
		return a.mixedPrepare(ctx)
	case "addr_report":
		// Only about the caller itself: the transport verified its id.
		from, _ := ctx.Value(rpcPeerKey{}).(string)
		if from == "" {
			return nil, fmt.Errorf("addr_report needs a member's connection")
		}
		var r addrReport
		if err := decodeBody(req.Body, &r); err != nil {
			return nil, err
		}
		a.addrs.receive(from, r, time.Now())
		return nil, nil
	case "removed":
		var r removedReq
		if err := decodeBody(req.Body, &r); err != nil {
			return nil, err
		}
		// The loop sees the removal in the state too; this makes it prompt
		// even if this node never receives that entry. The loop owns
		// Postgres, so it does the stopping.
		a.requestExit(&removedBy{by: r.By})
		return nil, nil
	}
	return nil, fmt.Errorf("unknown agent method %q", req.Method)
}

// removedBy asks the loop to stop for good: this node was removed.
type removedBy struct{ by string }

func (r *removedBy) Error() string { return "removed by " + r.by }

// cutStream is the rule-2 step on a standby: clear primary_conninfo,
// reload, confirm the WAL receiver is gone, and remember the epoch so the
// node does not reconnect by itself while a switch is in progress.
func (a *Agent) cutStream(ctx context.Context, epoch uint64, hold time.Duration) (pg.LSN, error) {
	return a.cutStreamFor(ctx, epoch, hold, false)
}

// cutStreamFor is cutStream; promote adds the D48 promotion-time check.
func (a *Agent) cutStreamFor(ctx context.Context, epoch uint64, hold time.Duration, promote bool) (pg.LSN, error) {
	unlock, err := a.lockPG(ctx)
	if err != nil {
		return 0, err
	}
	defer unlock()
	if _, role := a.currentPG(); role != "standby" {
		return 0, errors.New("this node is not running as a copy (standby)")
	}
	if d := a.mixed.getDrift(); d != "" && a.primaryUnlike(a.cn.State()) {
		// Rule 1 (D37): a drifted copy is never a failover target; an
		// uncut copy is never chosen (and counts as not reached for rule 2).
		return 0, errors.New("this copy no longer matches the main node (it changed since it joined); it may not take over")
	}
	if epoch == 0 {
		epoch = a.cn.State().Epoch
	}
	var until time.Time
	if hold > 0 {
		until = time.Now().Add(hold)
		a.noteCut(until, hold) // measured on the monotonic clock while this run lasts
	}
	if err := a.st.update(func(f *agentFile) { f.CutEpoch, f.CutUntil = epoch, until }); err != nil {
		return 0, err
	}
	lsn, err := a.node.CutStream(ctx, 30*time.Second)
	if err != nil {
		return 0, err
	}
	a.log.Info("stream cut", "epoch", epoch, "received_lsn", lsn.String())
	if promote {
		// D48, rule 1: a copy of another kind checks, now that its
		// catalog is final, that the data may move to it. A refusal leaves
		// the stream cut under its hold (it resumes by itself, or with the
		// controller's resume), exactly like a copy that was not reached:
		// the failover waits, nothing is promoted onto it.
		if why := a.promoteBlocked(ctx, a.cn.State(), lsn); why != "" {
			a.log.Warn("mixed systems: refused to be made the main node", "why", why)
			return 0, errors.New(why)
		}
	}
	return lsn, nil
}

// resumeStream undoes a cut (an aborted switch): follow the primary again.
func (a *Agent) resumeStream(ctx context.Context) error {
	unlock, err := a.lockPG(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if err := a.st.update(func(f *agentFile) { f.CutEpoch, f.CutUntil = 0, time.Time{} }); err != nil {
		return err
	}
	if _, role := a.currentPG(); role != "standby" {
		return nil
	}
	rs, err := a.node.ReadReplState()
	if err != nil || rs.Upstream != nil {
		return err
	}
	st := a.cn.State()
	if st.PrimaryID == "" || st.PrimaryID == a.cn.ID() {
		return nil
	}
	return a.node.SetUpstream(ctx, a.fwdUpstream(), 60*time.Second)
}
