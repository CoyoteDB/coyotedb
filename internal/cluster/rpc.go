package cluster

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"slices"
	"time"

	"github.com/hashicorp/raft"

	"coyotedb.com/coyote/internal/cluster/transport"
)

// RPC wire format, on StreamRPC (members only) and StreamJoin (anyone).
// One request and one response per stream, each a frame:
//
//	u32 BE length, then JSON
//	request:  {"method": "...", "body": {...}}
//	response: {"error": {"code": "...", "msg": "..."} | null, "body": {...}}
//
// Methods: "join" (StreamJoin, or StreamRPC when a follower forwards to the
// leader), "apply" (forward a command to the leader), "status".
const maxFrame = 4 << 20

type rpcRequest struct {
	Method string          `json:"method"`
	Body   json.RawMessage `json:"body"`
}

type rpcResponse struct {
	Error *wireError      `json:"error,omitempty"`
	Body  json.RawMessage `json:"body,omitempty"`
}

func writeFrame(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) > maxFrame {
		return fmt.Errorf("rpc frame too large (%d bytes)", len(b))
	}
	buf := make([]byte, 4, 4+len(b))
	binary.BigEndian.PutUint32(buf, uint32(len(b)))
	_, err = w.Write(append(buf, b...))
	return err
}

func readFrame(r io.Reader, v any) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > maxFrame {
		return fmt.Errorf("rpc frame too large (%d bytes)", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// callConn performs one RPC on an open stream and closes it.
func callConn(ctx context.Context, c net.Conn, method string, req, resp any) error {
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() { _ = c.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if err := writeFrame(c, rpcRequest{Method: method, Body: body}); err != nil {
		return &sendError{err}
	}
	var r rpcResponse
	if err := readFrame(c, &r); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("rpc %s: %w", method, err)
	}
	if r.Error != nil {
		return fromWire(r.Error)
	}
	if resp != nil && len(r.Body) > 0 {
		return json.Unmarshal(r.Body, resp)
	}
	return nil
}

// sendError marks a failure before the request was fully sent (safe to
// retry elsewhere).
type sendError struct{ err error }

func (e *sendError) Error() string { return "send request: " + e.err.Error() }
func (e *sendError) Unwrap() error { return e.err }

// call dials nodeID's rpc stream and performs one RPC.
func (n *Node) call(ctx context.Context, nodeID, method string, req, resp any) error {
	ctx, cancel := context.WithTimeout(ctx, n.timing.RPCTimeout)
	defer cancel()
	c, err := n.tr.Dial(ctx, nodeID, transport.StreamRPC)
	if err != nil {
		return &sendError{err}
	}
	return callConn(ctx, c, method, req, resp)
}

// ---- server side ----

func (n *Node) serveRPC(s *transport.Stream) {
	n.serveRequests(s, true)
}

func (n *Node) serveJoin(s *transport.Stream) {
	n.serveRequests(s, false)
}

func (n *Node) serveRequests(s *transport.Stream, member bool) {
	if err := s.Accept(); err != nil {
		return
	}
	// A stranger gets a short deadline to send its request; members the
	// full RPC timeout.
	readTimeout := n.timing.RPCTimeout
	if !member {
		readTimeout = n.timing.JoinRequestTimeout
	}
	_ = s.SetDeadline(time.Now().Add(readTimeout))
	var req rpcRequest
	if err := readFrame(s, &req); err != nil {
		return
	}
	_ = s.SetDeadline(time.Now().Add(n.timing.RPCTimeout))
	ctx, cancel := context.WithTimeout(n.ctx, n.timing.RPCTimeout)
	defer cancel()
	var body any
	var err error
	switch {
	case req.Method == "join":
		body, err = n.rpcJoin(ctx, req.Body)
	case req.Method == "apply" && member:
		body, err = n.rpcApply(ctx, req.Body)
	case req.Method == "status" && member:
		body = n.Status()
	default:
		err = fmt.Errorf("unknown method %q", req.Method)
	}
	if req.Method == "join" && n.dropJoinResponses.Load() > 0 {
		n.dropJoinResponses.Add(-1)
		return // test fault: the response is lost after the join was processed
	}
	if req.Method == "apply" && n.dropApplyResponses.Load() > 0 && removesSender(req.Body, s.Peer) {
		n.dropApplyResponses.Add(-1)
		return // test fault: the reply to a node's own removal is lost after it was processed
	}
	resp := rpcResponse{Error: toWire(err)}
	if err == nil && body != nil {
		resp.Body, err = json.Marshal(body)
		if err != nil {
			resp = rpcResponse{Error: toWire(err)}
		}
	}
	_ = writeFrame(s, resp)
}

// removesSender reports whether an apply request body is the sending
// node's own removal (for the dropApplyResponses test fault).
func removesSender(body json.RawMessage, peer string) bool {
	var req applyRequest
	if json.Unmarshal(body, &req) != nil {
		return false
	}
	cmd, err := DecodeCommand(req.Command)
	if err != nil {
		return false
	}
	rm, ok := cmd.(*RemoveMember)
	return ok && rm.ID == peer
}

type applyRequest struct {
	Command []byte `json:"command"`
	// Relay asks a non-leader to pass the command on to its leader (one
	// hop). Used by a node that does not know the leader, e.g. after a
	// restart at a new address the leader cannot reach yet.
	Relay bool `json:"relay,omitempty"`
}

func (n *Node) rpcApply(ctx context.Context, raw json.RawMessage) (any, error) {
	var req applyRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	// Validate before it reaches the log: a bad entry would halt every FSM.
	if _, err := DecodeCommand(req.Command); err != nil {
		return nil, err
	}
	if !n.IsLeader() && req.Relay {
		leader := n.LeaderID()
		if leader == "" || leader == n.id.NodeID {
			return nil, ErrNoLeader
		}
		var res Result
		err := n.call(ctx, leader, "apply", applyRequest{Command: req.Command}, &res)
		var se *sendError
		if errors.As(err, &se) {
			return nil, ErrNoLeader // not delivered: the caller may retry
		}
		return res, err
	}
	res, err := n.applyLocal(req.Command)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// relayApply sends a command through any other member when this node does
// not know the leader.
func (n *Node) relayApply(ctx context.Context, data []byte) (Result, error) {
	st := n.fsm.State()
	ids := map[string]bool{}
	for id := range st.Members {
		ids[id] = true
	}
	n.cacheMu.Lock()
	for id := range n.cache.Members {
		ids[id] = true
	}
	n.cacheMu.Unlock()
	delete(ids, n.id.NodeID)
	lastErr := error(ErrNoLeader)
	for _, id := range slices.Sorted(maps.Keys(ids)) {
		if !n.authorize(id) {
			continue
		}
		var res Result
		err := n.call(ctx, id, "apply", applyRequest{Command: data, Relay: true}, &res)
		var se *sendError
		if _, removed := n.refusedAfterAdmit(err); removed || err == nil || !(errors.As(err, &se) || errors.Is(err, ErrNoLeader) || errors.Is(err, ErrNotLeader)) {
			return res, err
		}
		lastErr = err
	}
	return Result{}, lastErr
}

// refusedAfterAdmit reports whether err is a refusal of this node's
// certificate by a member that accepted this node earlier in this process,
// and which member. At any one member this node's membership only ever goes
// absent, present, absent: ids are never reused, and a member's view is
// carried across its restarts by the peer cache (authorize). A member
// that once accepted this node and now refuses it has therefore applied
// this node's removal, which means the removal is committed.
func (n *Node) refusedAfterAdmit(err error) (string, bool) {
	var re *transport.RefusedError
	if errors.As(err, &re) && re.Denied && re.NodeID != "" && n.tr.Admitted(re.NodeID) {
		return re.NodeID, true
	}
	return "", false
}

// NodeStatus is returned by the status RPC and Node.Status.
type NodeStatus struct {
	NodeID       string `json:"node_id"`
	Name         string `json:"name"`
	RaftState    string `json:"raft_state"`
	LeaderID     string `json:"leader_id"`
	AppliedIndex uint64 `json:"applied_index"` // last applied command index
	LastIndex    uint64 `json:"last_index"`
	Term         uint64 `json:"term"`
	Halted       string `json:"halted,omitempty"`
}

// Status returns this node's view.
func (n *Node) Status() NodeStatus {
	_, leader := n.raft.LeaderWithID()
	st := NodeStatus{
		NodeID:       n.id.NodeID,
		Name:         n.id.Name,
		RaftState:    n.raft.State().String(),
		LeaderID:     string(leader),
		AppliedIndex: n.fsm.appliedIndex(),
		LastIndex:    n.raft.LastIndex(),
		Term:         n.raft.CurrentTerm(),
	}
	if err := n.fsm.Halted(); err != nil {
		st.Halted = err.Error()
	}
	return st
}

// PeerStatus asks another member for its status.
func (n *Node) PeerStatus(ctx context.Context, nodeID string) (NodeStatus, error) {
	var st NodeStatus
	err := n.call(ctx, nodeID, "status", struct{}{}, &st)
	return st, err
}

// ---- proposals ----

// Apply proposes cmd and waits until it is committed and applied. On a
// follower the command is forwarded to the leader. The error is the
// command's own result (e.g. ErrEpochMismatch) or a delivery error.
//
// Retries happen only when the command certainly was not appended (no
// leader yet, or the leader changed before accepting it); otherwise an
// uncertain outcome is returned as an error.
func (n *Node) Apply(ctx context.Context, cmd Command) (Result, error) {
	data, err := EncodeCommand(cmd)
	if err != nil {
		return Result{}, err
	}
	for {
		if err := n.fsm.Halted(); err != nil {
			return Result{}, err
		}
		var res Result
		if n.raft.State() == raft.Leader {
			res, err = n.applyLocal(data)
		} else if _, leader := n.raft.LeaderWithID(); leader == "" {
			res, err = n.relayApply(ctx, data)
		} else if string(leader) == n.id.NodeID {
			err = ErrNotLeader
		} else {
			err = n.call(ctx, string(leader), "apply", applyRequest{Command: data}, &res)
		}
		if by, ok := n.refusedAfterAdmit(err); ok {
			// Retrying cannot help: this node was removed.
			return Result{}, errorf(ErrRemoved, "%s refused this node's certificate", by)
		}
		var se *sendError
		retry := errors.Is(err, ErrNotLeader) || errors.Is(err, ErrNoLeader) || errors.As(err, &se)
		if !retry {
			return res, err
		}
		select {
		case <-ctx.Done():
			return Result{}, fmt.Errorf("%w (last error: %v)", ctx.Err(), err)
		case <-time.After(20 * time.Millisecond):
		case <-n.ctx.Done():
			return Result{}, errors.New("node is shutting down")
		}
	}
}

func (n *Node) applyLocal(data []byte) (Result, error) {
	f := n.raft.Apply(data, n.timing.ApplyTimeout)
	if err := f.Error(); err != nil {
		// These are refused before the entry is appended: safe to retry.
		if errors.Is(err, raft.ErrNotLeader) || errors.Is(err, raft.ErrLeadershipTransferInProgress) {
			return Result{}, errorf(ErrNotLeader, "%v", err)
		}
		return Result{}, err
	}
	r, ok := f.Response().(*applyResponse)
	if !ok || r == nil {
		return Result{}, errors.New("no response from state machine")
	}
	return r.Result, r.Err
}
