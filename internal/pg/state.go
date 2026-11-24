package pg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"coyotedb.com/coyote/internal/brand"
)

// Files in the node state dir added in M2 (next to the M1 ones in node.go):
//
//	cluster.secrets    superuser, replication and rewind passwords (0600).
//	                   Cluster-wide: the roles live in the replicated
//	                   catalog, so every member needs the same passwords.
//	upstream.pgpass    libpq passfile for the current upstream (0600)
//	replication.json   this node's replication settings (upstream, slot,
//	                   sync list); coyote.conf is rendered from it
//	rejoin.json        the "must rejoin as standby" marker (see Marker)
//	config.lock        serialises writers of coyote.conf / replication.json
func (n Node) SecretsPath() string   { return filepath.Join(n.StateDir, "cluster.secrets") }
func (n Node) PassfilePath() string  { return filepath.Join(n.StateDir, "upstream.pgpass") }
func (n Node) ReplStatePath() string { return filepath.Join(n.StateDir, "replication.json") }
func (n Node) MarkerPath() string    { return filepath.Join(n.StateDir, "rejoin.json") }
func (n Node) ConfigLockPath() string {
	return filepath.Join(n.StateDir, "config.lock")
}

// ---- secrets ----

// Secrets are the cluster-wide passwords. They are never logged or printed.
type Secrets struct {
	Superuser   string
	Replication string
	Rewind      string
	App         string // application role; may be empty until the cluster has set it
}

func (s Secrets) complete() error {
	if s.Superuser == "" || s.Replication == "" || s.Rewind == "" {
		return errors.New("secrets file is incomplete (needs superuser, replication and rewind)")
	}
	return nil
}

// RenderSecrets renders the secrets file.
func RenderSecrets(s Secrets) string {
	out := fmt.Sprintf("# %s cluster secrets. Keep private: anyone with this file can read all data.\nsuperuser=%s\nreplication=%s\nrewind=%s\n",
		brand.Product, s.Superuser, s.Replication, s.Rewind)
	if s.App != "" {
		out += "app=" + s.App + "\n"
	}
	return out
}

// ParseSecrets parses a secrets file. Unknown keys are an error so a wrong
// file is not silently accepted.
func ParseSecrets(b []byte) (Secrets, error) {
	var s Secrets
	for i, line := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || v == "" {
			return Secrets{}, fmt.Errorf("secrets file line %d is not key=value", i+1)
		}
		switch k {
		case "superuser":
			s.Superuser = v
		case "replication":
			s.Replication = v
		case "rewind":
			s.Rewind = v
		case "app":
			s.App = v
		default:
			return Secrets{}, fmt.Errorf("secrets file line %d: unknown key %q", i+1, k)
		}
	}
	return s, s.complete()
}

// ReadSecretsFile reads a secrets file from any path (for example the
// upstream's, handed over with --upstream-password-file).
func ReadSecretsFile(path string) (Secrets, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Secrets{}, fmt.Errorf("read secrets: %w", err)
	}
	return ParseSecrets(b)
}

// Secrets reads this node's secrets.
func (n Node) Secrets() (Secrets, error) { return ReadSecretsFile(n.SecretsPath()) }

// WriteSecrets stores s (0600) and keeps superuser.password in step.
func (n Node) WriteSecrets(s Secrets) error {
	if err := s.complete(); err != nil {
		return err
	}
	if err := writeFileAtomic(n.SecretsPath(), []byte(RenderSecrets(s)), 0o600); err != nil {
		return fmt.Errorf("write secrets: %w", err)
	}
	return writeFileAtomic(n.PasswordFile(), []byte(s.Superuser+"\n"), 0o600)
}

// EnsureSecrets returns the node's secrets, creating the replication and
// rewind passwords if this node has none yet (a node made by initdb).
func (n Node) EnsureSecrets() (Secrets, error) {
	s, err := n.Secrets()
	if err == nil {
		return s, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return Secrets{}, err
	}
	su, err := n.Password()
	if err != nil {
		return Secrets{}, err
	}
	s = Secrets{Superuser: su}
	if s.Replication, err = generatePassword(); err != nil {
		return Secrets{}, err
	}
	if s.Rewind, err = generatePassword(); err != nil {
		return Secrets{}, err
	}
	return s, n.WriteSecrets(s)
}

// writePassfile writes the libpq passfile for up.
func (n Node) writePassfile(up Upstream, s Secrets) error {
	return writeFileAtomic(n.PassfilePath(), []byte(RenderPassfile(upstreamPassEntries(up, s))), 0o600)
}

// ---- replication state ----

// ReplState is the persisted replication setup of one node. coyote.conf is
// rendered from it on every start and on every change, so a change made
// while running survives a restart (a cut stream stays cut).
type ReplState struct {
	// Upstream is where this node streams from when it is a standby. nil
	// means "no upstream" (a primary, or a standby whose stream was cut).
	Upstream *Upstream `json:"upstream,omitempty"`
	// Slot is primary_slot_name (normally SlotName(node name)).
	Slot string `json:"slot,omitempty"`
	// SyncCount and SyncNames render synchronous_standby_names.
	SyncCount int      `json:"sync_count,omitempty"`
	SyncNames []string `json:"sync_names,omitempty"`
}

// ReadReplState loads replication.json (zero value if missing).
func (n Node) ReadReplState() (ReplState, error) {
	var s ReplState
	b, err := os.ReadFile(n.ReplStatePath())
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("%s: %w", n.ReplStatePath(), err)
	}
	return s, nil
}

// WriteReplState stores replication.json.
func (n Node) WriteReplState(s ReplState) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(n.ReplStatePath(), append(b, '\n'), 0o600)
}

// ---- the "must rejoin as standby" marker (safety rule 6) ----

// Marker reasons. While any marker exists, `up` never starts Postgres as a
// primary (it refuses to start at all, except to retry an interrupted
// clone). Only a finished rewind or (re)clone clears it.
const (
	// ReasonDemoted: an operator (M2) or the controller (M3+) said another
	// node is primary now. The data may hold writes nobody else has.
	ReasonDemoted = "demoted"
	// ReasonRewinding: pg_rewind was started. If it did not finish, the
	// data dir is not trustworthy and must be re-cloned.
	ReasonRewinding = "rewinding"
	// ReasonCloning: the first clone of this node has not finished.
	ReasonCloning = "cloning"
	// ReasonRecloning: a wipe-and-clone has not finished.
	ReasonRecloning = "recloning"
)

// Marker is rejoin.json.
type Marker struct {
	Reason   string    `json:"reason"`
	Timeline uint32    `json:"timeline,omitempty"` // timeline of the data when written, if known
	Since    time.Time `json:"since"`              // informational only
	Detail   string    `json:"detail,omitempty"`
	// FencedEpoch is set when the node stopped its own Postgres because
	// its primary lease ran out (safety rule 4) at this cluster epoch. Such
	// a node may start as the primary again only if a confirmed read of the
	// cluster still names it primary at exactly this epoch (no one else can
	// have been promoted); otherwise it rejoins like any demoted node.
	FencedEpoch uint64 `json:"fenced_epoch,omitempty"`
}

// ReadMarker returns the marker, or nil if there is none.
func (n Node) ReadMarker() (*Marker, error) {
	b, err := os.ReadFile(n.MarkerPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m Marker
	if err := json.Unmarshal(b, &m); err != nil || m.Reason == "" {
		// A damaged marker still means "do not start as primary".
		return &Marker{Reason: ReasonDemoted, Detail: "unreadable marker file"}, nil
	}
	return &m, nil
}

// WriteMarker stores the marker durably (temp file, fsync, rename).
func (n Node) WriteMarker(m Marker) error {
	if m.Since.IsZero() {
		m.Since = time.Now().UTC()
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(n.MarkerPath(), append(b, '\n'), 0o600); err != nil {
		return err
	}
	return syncDir(n.StateDir)
}

// ClearMarker removes the marker.
func (n Node) ClearMarker() error {
	err := os.Remove(n.MarkerPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return syncDir(n.StateDir)
}

// StartPlan is what `up` should do with a data dir.
type StartPlan int

const (
	PlanInit   StartPlan = iota // new node: initdb, run as primary
	PlanClone                   // new standby: clone from the upstream
	PlanResume                  // start whatever the data dir is
)

// PlanStart decides what `up` does. It is pure so the rules are unit-tested.
//
//   - Any marker blocks a normal start: the node must not come up as a
//     primary. Only an interrupted first clone may be retried by `up`.
//   - A missing pgdata on a node that has cluster secrets means it was a
//     cluster member; initdb would silently make a new, empty, separate
//     primary, so that is refused too.
func PlanStart(initialized, hasSecrets bool, m *Marker, cloneRequested bool) (StartPlan, error) {
	if m != nil {
		if m.Reason == ReasonCloning && cloneRequested {
			return PlanClone, nil
		}
		return 0, MarkerError{M: *m}
	}
	if initialized {
		return PlanResume, nil
	}
	if cloneRequested {
		return PlanClone, nil
	}
	if hasSecrets {
		return 0, errors.New("this node's database folder is missing but the node was part of a cluster; " +
			"re-clone it from the primary (" + brand.Command + " dev reclone) instead of creating a new, empty database")
	}
	return PlanInit, nil
}

// MarkerError explains why a node with a marker will not start.
type MarkerError struct{ M Marker }

func (e MarkerError) Error() string {
	switch e.M.Reason {
	case ReasonCloning:
		return "the first copy of this node was interrupted; run `up` again with --clone-from to retry it"
	case ReasonRewinding, ReasonRecloning:
		return "a rewind or re-clone of this node was interrupted; run `" + brand.Command + " dev rewind --from <primary>` (it will re-clone)"
	default:
		return "this node was a primary and another node has taken over; it must rejoin as a standby first: `" +
			brand.Command + " dev rewind --from <new primary>`"
	}
}

// RewindFirst reports whether a rewind is worth trying for a node with this
// marker; if not, only a re-clone is safe.
func RewindFirst(m *Marker) bool {
	return m == nil || m.Reason == ReasonDemoted
}

// ---- locks ----

// LockConfig takes the config lock, which serialises everyone who writes
// coyote.conf, replication.json or the passfile: the agent while starting,
// and one-shot commands acting on a running node.
func (n Node) LockConfig(ctx context.Context) (*FileLock, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return AcquireLock(ctx, n.ConfigLockPath(), nil)
}

// LockNode takes the node lock without waiting long: for one-shot commands
// that need the agent to be stopped (rewind, reclone, demote).
func (n Node) LockNode(ctx context.Context, wait time.Duration) (*FileLock, error) {
	if err := n.Prepare(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	l, err := AcquireLock(ctx, n.LockPath(), nil)
	if err != nil {
		return nil, fmt.Errorf("the %s agent for %s is running (or still stopping); stop it first: %w", brand.Command, n.StateDir, err)
	}
	return l, nil
}
