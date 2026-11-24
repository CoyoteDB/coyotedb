package cluster

import (
	"bytes"
	"fmt"
	"runtime"
	"time"

	"github.com/hashicorp/raft"
)

// keyCurrentTerm is the key hashicorp/raft keeps its current term under
// in the stable store (raft.go, keyCurrentTerm).
var keyCurrentTerm = []byte("CurrentTerm")

// storageStopWait bounds how long Shutdown waits for Raft after this
// node's disk refused a write (see guardedStable).
const storageStopWait = 10 * time.Second

// guardedStable is the raft.StableStore Raft runs on. It exists for one
// failure: the disk refusing to save a new term. hashicorp/raft cannot go
// on without that write (setCurrentTerm), and panics, which kills the
// whole program with a stack trace. Swallowing the error is not an
// option either: Raft would then act in a term it has not saved, and
// after a restart could vote twice in it.
//
// So when that write fails, guardedStable reports the failure (fail) and
// ends the calling goroutine with runtime.Goexit instead of returning.
// That goroutine is Raft's main loop, or a transport connection handler
// running the heartbeat fast path (processHeartbeat -> appendEntries);
// the handler's deferred close drops that one connection, and the main
// loop meets the same failure on its next term change. (The first term of
// a new cluster is written by raft.BootstrapCluster, which open calls
// with the unwrapped store, so it returns its error.) Goexit
// runs the deferred calls on the way out: a leader's loop stops its
// replication and heartbeats and fails its pending entries with
// ErrLeadershipLost; an RPC being handled is answered "not granted / not
// accepted" with the old term. Nothing claims the new term, the in-memory
// term is never raised, and this node takes no further part in voting or
// replication: to the others it is a node that stopped. raft.Shutdown
// still returns, because the main loop's goroutine has ended.
//
// The other stable store writes (the vote) return their errors: Raft
// handles those itself and does not panic.
type guardedStable struct {
	raft.StableStore
	fail func(error)
}

func newGuardedStable(s raft.StableStore, fail func(error)) *guardedStable {
	return &guardedStable{StableStore: s, fail: fail}
}

func (s *guardedStable) SetUint64(key []byte, val uint64) error {
	err := s.StableStore.SetUint64(key, val)
	if err != nil && bytes.Equal(key, keyCurrentTerm) {
		s.fail(fmt.Errorf("save the Raft term %d: %w", val, err))
		runtime.Goexit()
	}
	return err
}

// storageFailed records that this node's disk refused a write Raft cannot
// go on without (once), logs it and wakes subscribers so the owner (the
// agent) sees Err() at once.
func (n *Node) storageFailed(err error) {
	n.failMu.Lock()
	first := n.failErr == nil
	if first {
		n.failErr = fmt.Errorf("%w: %w", ErrStorage, err)
	}
	n.failMu.Unlock()
	if !first {
		return
	}
	n.log.Error("this node's disk refused a write that Raft needs; Raft on this node stopped (it takes no further part in votes or replication)", "err", err)
	n.fsm.notify()
}

// storageErr is the recorded storage failure, or nil.
func (n *Node) storageErr() error {
	n.failMu.Lock()
	defer n.failMu.Unlock()
	return n.failErr
}
