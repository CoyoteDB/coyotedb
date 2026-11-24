package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"coyotedb.com/coyote/internal/pg"
)

// agentFile is <data-dir>/agent.json: what the agent must remember across
// restarts that is neither cluster state (Raft) nor Postgres' own files.
// Written durably (fsync + directory fsync): LastPrimaryEpoch is a safety
// fact (rule 6).
type agentFile struct {
	Format int `json:"format"`
	// Role is "data" or "witness", fixed when the node bootstraps or joins.
	Role string `json:"role,omitempty"`
	// Listen / Advertise / ControlPort as last given on the command line,
	// reused by a plain `up`.
	Listen    string   `json:"listen,omitempty"`
	Advertise []string `json:"advertise,omitempty"`
	// AdvertiseRecorded is the --advertise / COYOTE_ADVERTISE value the
	// cluster last recorded for this node (D62). The same value given
	// again does not replace an address set later with `node
	// set-address`; a different one does.
	AdvertiseRecorded []string `json:"advertise_recorded,omitempty"`
	// AddrSeenLocal lists the addresses the cluster records for this node
	// that this computer was seen to have (D66). Only these can be
	// reported as gone: an address that was never on this computer (a
	// router's or a container host's, forwarded in) is not checked.
	AddrSeenLocal []string `json:"addr_seen_local,omitempty"`
	ControlPort   int      `json:"control_port,omitempty"`
	ClientPort    int      `json:"client_port,omitempty"`
	// TunnelPort is the local forwarder port (127.0.0.1) that is this
	// node's Postgres upstream; kept stable so primary_conninfo is too.
	TunnelPort int `json:"tunnel_port,omitempty"`
	// LastPrimaryEpoch is the epoch at which this node last became primary
	// (written before promoting). A node whose data is primary data starts
	// it as a primary only if the confirmed cluster epoch equals this.
	LastPrimaryEpoch uint64 `json:"last_primary_epoch,omitempty"`
	// PromotingEpoch is the epoch of a promotion that has not finished:
	// written with LastPrimaryEpoch before pg_promote, cleared once
	// Postgres is out of recovery and checkpointed on its new timeline.
	// Primary data found with it set is never started as a primary: it is
	// started as a standby again and promoted again (D56).
	PromotingEpoch uint64 `json:"promoting_epoch,omitempty"`
	// FreshPrimary: this node bootstrapped the cluster and may initdb.
	// Cleared once the first database exists.
	FreshPrimary bool `json:"fresh_primary,omitempty"`
	// CutEpoch: the stream was cut (rule 2) while the cluster was at this
	// epoch. The cut is kept until the epoch moves on, the switch is
	// aborted (resume), or CutUntil passes with the epoch unchanged (the
	// switch's leader died before its compare-and-set; the switch aborts
	// itself well before that time). Zero CutUntil: kept until the next
	// switch or resume-stream (`dev cut-stream`).
	CutEpoch uint64    `json:"cut_epoch,omitempty"`
	CutUntil time.Time `json:"cut_until,omitzero"`
	// Recloned: a re-clone for "needs re-clone" (rule 8) finished; the
	// node still has to tell the cluster it is active again.
	Recloned bool `json:"recloned,omitempty"`
	// Removed explains why this node is no longer a member; such a node
	// never starts again.
	Removed string `json:"removed,omitempty"`
}

const agentFileFmt = 1

type stateStore struct {
	path string
	mu   sync.Mutex
	v    agentFile
}

func agentFilePath(dataDir string) string { return filepath.Join(dataDir, "agent.json") }

func loadStateStore(dataDir string) (*stateStore, error) {
	s := &stateStore{path: agentFilePath(dataDir), v: agentFile{Format: agentFileFmt}}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.v); err != nil {
		return nil, fmt.Errorf("%s: %w", s.path, err)
	}
	if s.v.Format != agentFileFmt {
		return nil, fmt.Errorf("%s: unknown format %d (made by a newer version?)", s.path, s.v.Format)
	}
	return s, nil
}

// get returns a copy.
func (s *stateStore) get() agentFile {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.v
	c.Advertise = slices.Clone(s.v.Advertise)
	c.AdvertiseRecorded = slices.Clone(s.v.AdvertiseRecorded)
	return c
}

// update applies f and writes the file durably if anything changed.
func (s *stateStore) update(f func(*agentFile)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.v
	next.Advertise = slices.Clone(s.v.Advertise)
	next.AdvertiseRecorded = slices.Clone(s.v.AdvertiseRecorded)
	f(&next)
	next.Format = agentFileFmt
	old, _ := json.Marshal(s.v)
	nb, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	if cmp, _ := json.Marshal(next); string(cmp) == string(old) {
		if _, err := os.Stat(s.path); err == nil {
			return nil
		}
	}
	if err := pg.WriteFileDurable(s.path, append(nb, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", s.path, err)
	}
	s.v = next
	return nil
}
