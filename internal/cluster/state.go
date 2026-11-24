package cluster

import (
	"log/slog"
	"maps"
	"slices"
	"time"
)

// Role of a member.
type Role string

// Roles.
const (
	RoleData    Role = "data"    // runs Postgres
	RoleWitness Role = "witness" // votes only; any platform
)

// Suffrage of a member in the Raft configuration.
type Suffrage string

// Suffrage values.
const (
	Voter    Suffrage = "voter"
	Nonvoter Suffrage = "nonvoter"
)

// Member status values.
const (
	StatusActive       = "active"
	StatusNeedsReclone = "needs_reclone"
)

// Platform identifies what a data node's Postgres files are compatible with
// (safety rule 1).
//
// Two data nodes are alike when OS, Arch and PGBuildSHA are equal (the
// original rule 1). Nodes that are not alike may still share a cluster
// (docs/MIXED_OS.md, D37) when the cluster allows mixed systems and their
// Fingerprint and ProbeDigest are equal; see checkPlatform. For that
// comparison OS and Arch are for display only.
type Platform struct {
	OS         string `json:"os"`
	Arch       string `json:"arch"`
	PGBuildSHA string `json:"pg_build_sha"`
	// Fingerprint is the sha256 of the node's Postgres file-format
	// fingerprint (pg.Fingerprint.Hash): data-file layout, exact Postgres
	// version, build options, shipped time-zone and text-search files,
	// Unicode/ICU versions, available extensions. "" = not measured yet.
	Fingerprint string `json:"fingerprint,omitempty"`
	// FingerprintText is the compared content behind Fingerprint
	// ("key=value" lines, pg.Fingerprint.Canonical), so a refusal can say
	// which field differs. Not secret: versions and hashes of shipped files.
	FingerprintText string `json:"fingerprint_text,omitempty"`
	// ProbeDigest is the digest of the behaviour probe's must-match groups
	// (pg.ProbeResult.Digest); ProbeGroups lists "group=digest" for each of
	// them, so a refusal can name the group that answered differently.
	ProbeDigest string   `json:"probe_digest,omitempty"`
	ProbeGroups []string `json:"probe_groups,omitempty"`
}

// Measured reports whether the fingerprint and the probe are recorded.
func (p Platform) Measured() bool { return p.Fingerprint != "" && p.ProbeDigest != "" }

// Alike reports whether two platforms are the same kind of machine with
// the same Postgres build (the original safety rule 1).
func (p Platform) Alike(o Platform) bool {
	return p.OS == o.OS && p.Arch == o.Arch && p.PGBuildSHA == o.PGBuildSHA
}

// Equal compares every field (slices included).
func (p Platform) Equal(o Platform) bool {
	return p.Alike(o) && p.Fingerprint == o.Fingerprint && p.FingerprintText == o.FingerprintText &&
		p.ProbeDigest == o.ProbeDigest && slices.Equal(p.ProbeGroups, o.ProbeGroups)
}

// Guard is a data node's latest mixed-system guard results
// (docs/MIXED_OS.md conditions 2-7), written by the node itself. Times
// are the node's wall clock: they are shown to people and compared with
// a generous bound (MixedGuardMaxAge), never used for leases (rule 4).
type Guard struct {
	// AuditAt is when the portability audit last completed; AuditBlocking
	// whether it found something that forbids mixing machines.
	AuditAt       time.Time `json:"audit_at,omitzero"`
	AuditBlocking bool      `json:"audit_blocking,omitempty"`
	// VerifyAt is when the integrity check last completed; VerifyDamaged
	// whether it found damage.
	VerifyAt      time.Time `json:"verify_at,omitzero"`
	VerifyDamaged bool      `json:"verify_damaged,omitempty"`
	// Drift is set when this node's fingerprint or probe changed while it
	// was a member (an operating-system or program update) and the change
	// no longer matches the other data nodes: plain words, "" = none.
	Drift string `json:"drift,omitempty"`
	// Findings are the most important findings in plain words (at most
	// MaxGuardFindings).
	Findings []string `json:"findings,omitempty"`
}

// MaxGuardFindings caps Guard.Findings so the cluster state stays small.
const MaxGuardFindings = 8

// Blocked reports whether this record forbids mixing machines.
func (g *Guard) Blocked() bool {
	return g != nil && (g.AuditBlocking || g.VerifyDamaged || g.Drift != "")
}

// Member is one node of the cluster.
type Member struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Addresses []string  `json:"addresses"`
	Role      Role      `json:"role"`
	Suffrage  Suffrage  `json:"suffrage"` // as last recorded by the leader after a Raft config change
	Platform  Platform  `json:"platform"`
	JoinedAt  time.Time `json:"joined_at"`
	Status    string    `json:"status"`
	// ClientPort is the port this node's client proxy listens on (data
	// nodes; 0 = none announced yet). Connection strings list every data
	// node's address with its own client port.
	ClientPort int `json:"client_port,omitempty"`
	// AddressesPinned: an operator set Addresses (`node set-address`); the
	// node then no longer replaces them with what it detects itself.
	AddressesPinned bool `json:"addresses_pinned,omitempty"`
	// Guard is the node's latest mixed-system guard results (data nodes).
	Guard *Guard `json:"guard,omitempty"`
	// Removing: a `remove` or `leave` of this member has started (D59).
	// From then on nothing may add it to the sync set (applySetSyncSet
	// refuses), so the removal's hand-over out of the sync set cannot be
	// undone by the controller adding it back.
	Removing bool `json:"removing,omitempty"`
}

// Lease is the primary's lease record. It contains no wall-clock time
// (safety rule 4): the controller measures the lease with its own monotonic
// clock from when it observed Seq change.
type Lease struct {
	Holder   string        `json:"holder"`
	Epoch    uint64        `json:"epoch"`
	Seq      uint64        `json:"seq"`
	Duration time.Duration `json:"duration"`
}

// SyncSet is R and k (safety rule 3), tagged with the epoch it was set in.
type SyncSet struct {
	Members []string `json:"members"` // sorted
	K       int      `json:"k"`
	Epoch   uint64   `json:"epoch"`
}

// Single-data-node modes (safety rule 9).
const (
	ModeStrict       = "strict"
	ModeDegradeAfter = "degrade_after"
)

// Settings are cluster-wide knobs.
type Settings struct {
	SingleDataNodeMode string        `json:"single_data_node_mode"`
	DegradeAfter       time.Duration `json:"degrade_after"`
	LeaseDuration      time.Duration `json:"lease_duration"`
	LeaseRenewInterval time.Duration `json:"lease_renew_interval"`
	// GoneAfter (safety rule 8): a data member away longer than this has
	// its replication slot dropped and must re-clone when it returns.
	// Zero means DefaultGoneAfter (clusters made before M4 have no value).
	GoneAfter time.Duration `json:"gone_after,omitempty"`
	// MixedOS: data nodes of different operating systems or processors
	// may join (D29, D37), when their fingerprints and probes match. Off
	// until a joiner asks for it explicitly (`join --allow-mixed-os`).
	MixedOS bool `json:"mixed_os,omitempty"`
}

// DefaultGoneAfter is Settings.GoneAfter when unset.
const DefaultGoneAfter = 30 * time.Minute

// DefaultSettings are applied at bootstrap.
func DefaultSettings() Settings {
	return Settings{
		SingleDataNodeMode: ModeDegradeAfter,
		DegradeAfter:       30 * time.Second,
		// F1 (measured): a renewal every 1s, retried every 250ms while
		// unanswered; the main node starts stopping at 60% (6s) without a
		// renewal. That rides out a Raft leader dying (election ~1-3s) or
		// the leader's disk stalling up to ~5s, at ~4s more failover time.
		LeaseDuration:      10 * time.Second,
		LeaseRenewInterval: time.Second,
		GoneAfter:          DefaultGoneAfter,
	}
}

// EffectiveGoneAfter is GoneAfter, or its default when unset.
func (s Settings) EffectiveGoneAfter() time.Duration {
	if s.GoneAfter <= 0 {
		return DefaultGoneAfter
	}
	return s.GoneAfter
}

// JoinSecret is a one-time join secret, stored hashed.
type JoinSecret struct {
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Used      bool      `json:"used"`
	UsedBy    string    `json:"used_by,omitempty"`
	// UsedByKey is the hex sha256 of the joiner's public key (from its
	// CSR). With UsedBy it lets the same joiner retry a join whose response
	// was lost, while anyone else presenting the used secret is refused.
	UsedByKey string `json:"used_by_key,omitempty"`
}

// APIKey is an API key, stored hashed.
type APIKey struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// ForcedRecovery records a ForceNewCluster.
type ForcedRecovery struct {
	NodeID string    `json:"node_id"`
	Epoch  uint64    `json:"epoch"`
	At     time.Time `json:"at"`
}

// CAData is the cluster CA. The private key is replicated to every member
// so any leader can sign joins (members already hold all cluster data).
type CAData struct {
	CertDER []byte `json:"cert_der"`
	KeyDER  []byte `json:"key_der"`
}

// PGSecrets are the cluster's Postgres passwords (superuser, replication
// role, rewind role). The roles live in the replicated catalog, so every
// data node needs the same values; they travel inside Raft (over mTLS) so a
// joiner never copies them by hand. Never log them: String and LogValue
// redact, and nothing in this package prints State.
type PGSecrets struct {
	Superuser   string `json:"superuser"`
	Replication string `json:"replication"`
	Rewind      string `json:"rewind"`
	// App is the password of the application role (AppUser), the one
	// clients use. Unlike the others it can be changed (SetAppPassword).
	App string `json:"app,omitempty"`
}

// String redacts the passwords.
func (PGSecrets) String() string { return "PGSecrets{redacted}" }

// GoString redacts the passwords (%#v).
func (PGSecrets) GoString() string { return "PGSecrets{redacted}" }

// LogValue redacts the passwords in slog output.
func (PGSecrets) LogValue() slog.Value { return slog.StringValue("redacted") }

func (s PGSecrets) complete() bool {
	return s.Superuser != "" && s.Replication != "" && s.Rewind != ""
}

// State is the replicated cluster state. Values returned by Node.State are
// deep copies.
type State struct {
	ClusterID            string                `json:"cluster_id"`
	Epoch                uint64                `json:"epoch"`
	Members              map[string]Member     `json:"members"`
	PrimaryID            string                `json:"primary_id"`
	Lease                Lease                 `json:"lease"`
	Sync                 SyncSet               `json:"sync"`
	Settings             Settings              `json:"settings"`
	StandbyEligible      bool                  `json:"standby_eligible"`
	StandbyEligibleSince time.Time             `json:"standby_eligible_since"`
	JoinSecrets          map[string]JoinSecret `json:"join_secrets"` // key: hex sha256 of secret
	APIKeys              map[string]APIKey     `json:"api_keys"`     // key: hash as given by caller
	CA                   CAData                `json:"ca"`
	Forced               []ForcedRecovery      `json:"forced,omitempty"`
	// PGSecrets is set once, by the first primary (SetPGSecrets).
	PGSecrets *PGSecrets `json:"pg_secrets,omitempty"`
	// LastServed is the last node that renewed a primary lease, i.e. the
	// last primary that may have acknowledged writes. A node named primary
	// that never renewed (it died before promoting) has not served, so the
	// controller's rule 2 count is taken over R minus LastServed, not minus
	// the named primary.
	LastServed string `json:"last_served,omitempty"`
	// Backup is the backup settings (D63); nil = DefaultBackupSettings.
	Backup *BackupSettings `json:"backup,omitempty"`
	// AppliedIndex is the Raft index of the last applied command.
	AppliedIndex uint64 `json:"applied_index"`
}

func newState() *State {
	return &State{
		Members:     map[string]Member{},
		JoinSecrets: map[string]JoinSecret{},
		APIKeys:     map[string]APIKey{},
	}
}

// Bootstrapped reports whether the state has been initialised.
func (s State) Bootstrapped() bool { return s.ClusterID != "" }

// Clone returns a deep copy.
func (s *State) Clone() State {
	c := *s
	c.Members = make(map[string]Member, len(s.Members))
	for k, m := range s.Members {
		m.Addresses = slices.Clone(m.Addresses)
		m.Platform.ProbeGroups = slices.Clone(m.Platform.ProbeGroups)
		if m.Guard != nil {
			g := *m.Guard
			g.Findings = slices.Clone(g.Findings)
			m.Guard = &g
		}
		c.Members[k] = m
	}
	c.Sync.Members = slices.Clone(s.Sync.Members)
	c.JoinSecrets = maps.Clone(s.JoinSecrets)
	if c.JoinSecrets == nil {
		c.JoinSecrets = map[string]JoinSecret{}
	}
	c.APIKeys = maps.Clone(s.APIKeys)
	if c.APIKeys == nil {
		c.APIKeys = map[string]APIKey{}
	}
	c.CA.CertDER = slices.Clone(s.CA.CertDER)
	c.CA.KeyDER = slices.Clone(s.CA.KeyDER)
	c.Forced = slices.Clone(s.Forced)
	if s.PGSecrets != nil {
		v := *s.PGSecrets
		c.PGSecrets = &v
	}
	c.Backup = s.Backup.clone()
	return c
}

// DataMembers returns data members sorted by (JoinedAt, ID).
func (s *State) DataMembers() []Member {
	var out []Member
	for _, m := range s.Members {
		if m.Role == RoleData {
			out = append(out, m)
		}
	}
	sortMembers(out)
	return out
}

// SortedMembers returns all members sorted by (JoinedAt, ID).
func (s *State) SortedMembers() []Member {
	out := slices.Collect(maps.Values(s.Members))
	sortMembers(out)
	return out
}

func sortMembers(ms []Member) {
	slices.SortFunc(ms, func(a, b Member) int {
		if c := a.JoinedAt.Compare(b.JoinedAt); c != 0 {
			return c
		}
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
}
