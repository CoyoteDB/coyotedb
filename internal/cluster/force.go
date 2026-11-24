package cluster

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
	"go.etcd.io/bbolt"
)

// ForceNewClusterConfirmation must be passed verbatim to ForceNewCluster.
const ForceNewClusterConfirmation = "make this node the only voter; changes other nodes made that never reached it are lost"

// ForceNewCluster is manual disaster recovery: when a quorum is gone for
// good, a surviving node rewrites its Raft configuration so it is the only
// voter (raft.RecoverCluster), starts, and records a ForceRecovered command:
// every other member is removed (they must re-join; data nodes need a
// re-clone), the epoch is bumped, the survivor becomes primary if it is a
// data node, the sync set is cleared, standby_eligible is false, and a
// "forced" marker is appended to State.Forced.
//
// The node must not be running. confirm must equal
// ForceNewClusterConfirmation. Every other node of the old cluster must be
// stopped and never restarted with its old state.
func ForceNewCluster(ctx context.Context, cfg Config, confirm string) (*Node, error) {
	if confirm != ForceNewClusterConfirmation {
		return nil, ErrNotConfirmed
	}
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	has, err := HasState(cfg.Dir)
	if err != nil {
		return nil, err
	}
	if !has {
		return nil, errors.New("cluster: no cluster state on this node")
	}
	id, err := loadIdentity(cfg.Dir)
	if err != nil {
		return nil, err
	}
	tmp := &Node{cfg: cfg, timing: cfg.Timing, id: id, log: cfg.Logger.With("node", id.Name)}
	hlog := tmp.hclogger()
	store, err := raftboltdb.New(raftboltdb.Options{
		Path:        filepath.Join(cfg.Dir, dirRaft, fileRaftDB),
		BoltOptions: &bbolt.Options{Timeout: 2 * time.Second},
	})
	if err != nil {
		return nil, fmt.Errorf("open raft store (is the node still running?): %w", err)
	}
	snaps, err := raft.NewFileSnapshotStoreWithLogger(filepath.Join(cfg.Dir, dirRaft), 2, hlog)
	if err != nil {
		store.Close()
		return nil, err
	}
	_, trans := raft.NewInmemTransport(raft.ServerAddress(id.NodeID))
	conf := raft.Configuration{Servers: []raft.Server{
		{Suffrage: raft.Voter, ID: raft.ServerID(id.NodeID), Address: raft.ServerAddress(id.NodeID)},
	}}
	err = raft.RecoverCluster(tmp.raftConfig(hlog), NewFSM(nil), store, store, snaps, trans, conf)
	trans.Close()
	if cerr := store.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, fmt.Errorf("recover cluster: %w", err)
	}
	cfg.Logger.Warn("forced new cluster: this node is now the only voter", "node", id.NodeID)

	n, err := start(ctx, cfg, openOpts{pauseReconcile: true})
	if err != nil {
		return nil, err
	}
	for !n.IsLeader() {
		select {
		case <-ctx.Done():
			n.Shutdown()
			return nil, fmt.Errorf("forced node did not become leader: %w", ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
	if err := n.Barrier(ctx); err != nil {
		n.Shutdown()
		return nil, err
	}
	if _, err := n.Apply(ctx, &ForceRecovered{NodeID: id.NodeID, At: n.now()}); err != nil {
		n.Shutdown()
		return nil, fmt.Errorf("record forced recovery: %w", err)
	}
	n.paused.Store(false)
	n.kickReconcile()
	return n, nil
}
