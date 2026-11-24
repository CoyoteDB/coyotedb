package cluster

import (
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/hashicorp/raft"
)

// readOnlyStable fails every write once broken is set, the way a stable
// store on a disk that turned read-only does.
type readOnlyStable struct {
	raft.StableStore
	path   string
	broken *atomic.Bool
}

func (s *readOnlyStable) err() error {
	return &os.PathError{Op: "write", Path: s.path, Err: syscall.EROFS}
}

func (s *readOnlyStable) Set(k, v []byte) error {
	if s.broken.Load() {
		return s.err()
	}
	return s.StableStore.Set(k, v)
}

func (s *readOnlyStable) SetUint64(k []byte, v uint64) error {
	if s.broken.Load() {
		return s.err()
	}
	return s.StableStore.SetUint64(k, v)
}

// R1: a node whose disk refuses to save a new Raft term must not panic
// (hashicorp/raft does, in setCurrentTerm). Raft on it stops, Err says
// why, Shutdown returns promptly, and the other two carry on.
func TestStableStoreTermWriteFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		leader bool // break the leader (else a follower)
	}{{"follower", false}, {"leader", true}} {
		t.Run(tc.name, func(t *testing.T) {
			h := newClusterHarness(t)
			broken := map[string]*atomic.Bool{}
			cfg := func(name string) Config {
				c := h.cfg(name, RoleData, macPlat)
				b := &atomic.Bool{}
				broken[name] = b
				c.wrapStable = func(s raft.StableStore) raft.StableStore {
					return &readOnlyStable{StableStore: s, path: filepath.Join(c.Dir, dirRaft, fileRaftDB), broken: b}
				}
				return c
			}
			a := h.bootstrap(cfg("a"))
			b := h.join(a, cfg("b"))
			c := h.join(a, cfg("c"))
			h.waitConfig(a, map[string]Suffrage{a.ID(): Voter, b.ID(): Voter, c.ID(): Voter})
			ld := h.leader()

			// victim: the node whose disk breaks; target: the node that
			// takes over (a new term, which the victim must then save).
			var victim, target *Node
			for _, n := range h.live() {
				switch {
				case tc.leader && n == ld, !tc.leader && n != ld && victim == nil:
					victim = n
				case n != ld && target == nil:
					target = n
				}
			}
			if victim == nil || target == nil || victim == target {
				t.Fatalf("pick nodes: victim %v target %v", victim, target)
			}
			sub := victim.Subscribe()
			defer sub.Close()
			broken[victim.Name()].Store(true)
			if victim.Err() != nil {
				t.Fatalf("Err before any write: %v", victim.Err())
			}

			addr := raft.ServerAddress(target.ID())
			if err := ld.raft.LeadershipTransferToServer(raft.ServerID(target.ID()), addr).Error(); err != nil && !tc.leader {
				t.Fatalf("leadership transfer: %v", err)
			}

			waitUntil(t, "victim reports the storage failure", func() bool { return victim.Err() != nil })
			err := victim.Err()
			if !errors.Is(err, ErrStorage) || !errors.Is(err, syscall.EROFS) {
				t.Fatalf("Err = %v; want ErrStorage wrapping EROFS", err)
			}
			if errors.Is(err, ErrHalted) {
				t.Fatalf("Err %v matches ErrHalted", err)
			}
			select {
			case <-sub.C:
			case <-time.After(waitLong):
				t.Fatal("subscribers were not woken by the storage failure")
			}
			// The in-memory term was never raised past the saved one:
			// nothing on this node acts in a term it has not saved.
			saved, gerr := victim.store.GetUint64(keyCurrentTerm)
			if gerr != nil {
				t.Fatal(gerr)
			}
			if cur := victim.raft.CurrentTerm(); cur != saved {
				t.Fatalf("in-memory term %d, saved term %d", cur, saved)
			}

			// The other two elect a leader and commit without it.
			var survivors []*Node
			for _, n := range h.live() {
				if n != victim {
					survivors = append(survivors, n)
				}
			}
			waitUntil(t, "a leader among the survivors", func() bool {
				return survivors[0].IsLeader() || survivors[1].IsLeader()
			})
			if victim.IsLeader() {
				t.Fatal("the failed node still says it leads")
			}
			k := h.propose(survivors[0])
			h.waitReplicated(k, survivors...)

			// Raft's main loop on the victim ended, so Shutdown does not
			// hang on it (and does not need the bounded wait either).
			start := time.Now()
			h.stop(victim.Name())
			if d := time.Since(start); d >= storageStopWait {
				t.Fatalf("shutdown took %s", d)
			}
		})
	}
}

// Writes other than the term (the vote) return their error unchanged:
// Raft handles those without panicking.
func TestGuardedStablePassesOtherErrors(t *testing.T) {
	var b atomic.Bool
	b.Store(true)
	var failed error
	s := newGuardedStable(&readOnlyStable{StableStore: raft.NewInmemStore(), path: "x", broken: &b}, func(err error) { failed = err })
	if err := s.SetUint64([]byte("LastVoteTerm"), 3); !errors.Is(err, syscall.EROFS) {
		t.Fatalf("vote term write: %v", err)
	}
	if err := s.Set([]byte("LastVoteCand"), []byte("x")); !errors.Is(err, syscall.EROFS) {
		t.Fatalf("vote candidate write: %v", err)
	}
	if failed != nil {
		t.Fatalf("failure recorded for a vote write: %v", failed)
	}

	// The term write ends the calling goroutine after reporting.
	done := make(chan struct{})
	returned := false
	go func() {
		defer close(done)
		_ = s.SetUint64(keyCurrentTerm, 4)
		returned = true
	}()
	<-done
	if returned {
		t.Fatal("SetUint64(CurrentTerm) returned after a failed write")
	}
	if !errors.Is(failed, syscall.EROFS) {
		t.Fatalf("recorded failure %v", failed)
	}

	// A working store passes the term through.
	b.Store(false)
	if err := s.SetUint64(keyCurrentTerm, 5); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.GetUint64(keyCurrentTerm); v != 5 {
		t.Fatalf("term %d", v)
	}
}
