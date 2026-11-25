package cluster

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// Command is a typed, versioned state change. Commands are deterministic:
// every time and id they need is a field, filled in by the proposer; the FSM
// never reads the clock, randomness or the environment.
//
// Wire form (the Raft log entry data):
//
//	{"type":"set_primary","v":1,"body":{...}}
//
// A node that does not know a (type, v) pair halts its state machine rather
// than skipping the entry (see FSM.Halted).
type Command interface {
	commandType() string
}

// commandVersion is the single version every current command type is
// written with. A body change must bump the version of that type.
const commandVersion = 1

type envelope struct {
	Type string          `json:"type"`
	V    int             `json:"v"`
	Body json.RawMessage `json:"body"`
}

var commandTypes = map[string]func() Command{
	"bootstrap":            func() Command { return &BootstrapState{} },
	"add_member":           func() Command { return &AddMember{} },
	"update_member":        func() Command { return &UpdateMember{} },
	"remove_member":        func() Command { return &RemoveMember{} },
	"set_suffrage":         func() Command { return &SetSuffrage{} },
	"create_join_secret":   func() Command { return &CreateJoinSecret{} },
	"consume_join_secret":  func() Command { return &ConsumeJoinSecret{} },
	"expire_join_secrets":  func() Command { return &ExpireJoinSecrets{} },
	"set_primary":          func() Command { return &SetPrimary{} },
	"renew_lease":          func() Command { return &RenewLease{} },
	"set_sync_set":         func() Command { return &SetSyncSet{} },
	"set_settings":         func() Command { return &SetSettings{} },
	"set_standby_eligible": func() Command { return &SetStandbyEligible{} },
	"add_api_key":          func() Command { return &AddAPIKey{} },
	"remove_api_key":       func() Command { return &RemoveAPIKey{} },
	"force_recovered":      func() Command { return &ForceRecovered{} },
	"set_pg_secrets":       func() Command { return &SetPGSecrets{} },
	"set_app_password":     func() Command { return &SetAppPassword{} },
	"promote":              func() Command { return &Promote{} },
	"set_backup":           func() Command { return &SetBackup{} },
	"register_pg_builds":   func() Command { return &RegisterPGBuilds{} },
}

// EncodeCommand serialises c for the Raft log.
func EncodeCommand(c Command) ([]byte, error) {
	if _, ok := commandTypes[c.commandType()]; !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownCommand, c.commandType())
	}
	body, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{Type: c.commandType(), V: commandVersion, Body: body})
}

// DecodeCommand parses a Raft log entry. Unknown types or versions, and
// unknown fields, are errors.
func DecodeCommand(data []byte) (Command, error) {
	var env envelope
	if err := strictUnmarshal(data, &env); err != nil {
		return nil, errorf(ErrUnknownCommand, "bad envelope: %v", err)
	}
	mk, ok := commandTypes[env.Type]
	if !ok {
		return nil, errorf(ErrUnknownCommand, "type %q", env.Type)
	}
	if env.V != commandVersion {
		return nil, errorf(ErrUnknownCommand, "type %q version %d (this node knows %d)", env.Type, env.V, commandVersion)
	}
	c := mk()
	if err := strictUnmarshal(env.Body, c); err != nil {
		return nil, errorf(ErrUnknownCommand, "type %q body: %v", env.Type, err)
	}
	return c, nil
}

func strictUnmarshal(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	return d.Decode(v)
}

// Result is what a successfully applied command returns to its proposer.
type Result struct {
	Index    uint64 `json:"index"`     // Raft index of the entry
	Epoch    uint64 `json:"epoch"`     // epoch after the command
	LeaseSeq uint64 `json:"lease_seq"` // lease sequence after the command
}

// BootstrapState initialises an empty state. Epoch becomes 1 and the first member
// (if a data node) becomes primary.
type BootstrapState struct {
	ClusterID string    `json:"cluster_id"`
	CA        CAData    `json:"ca"`
	Member    Member    `json:"member"`
	Settings  Settings  `json:"settings"`
	At        time.Time `json:"at"`
}

// AddMember adds a member. If SecretHash is set, that join secret is
// consumed in the same entry (fails with ErrSecretUsed / ErrSecretExpired /
// ErrSecretUnknown, judged against At). Data members must match the
// platform of existing data members (rule 1), or pass the mixed-system
// check (checkPlatform). Names and ids are unique.
type AddMember struct {
	Member     Member    `json:"member"`
	SecretHash string    `json:"secret_hash,omitempty"`
	KeyHash    string    `json:"key_hash,omitempty"` // joiner's public key hash, recorded on the consumed secret
	At         time.Time `json:"at"`
	// AllowMixedOS: the joiner asked explicitly to mix kinds of machines
	// (`join --allow-mixed-os`); accepted, it turns Settings.MixedOS on.
	AllowMixedOS bool `json:"allow_mixed_os,omitempty"`
}

// SecretRetention is how long a join secret is kept after its expiry: a
// used one so its joiner can still retry (see JoinSecret.UsedByKey), an
// unused one so a late joiner gets ErrSecretExpired, not ErrSecretUnknown.
const SecretRetention = 24 * time.Hour

// UpdateMember changes selected fields; nil means unchanged.
type UpdateMember struct {
	// ExpectedEpoch, when set, makes the update a compare-and-set on the
	// epoch (the controller's writes).
	ExpectedEpoch *uint64   `json:"expected_epoch,omitempty"`
	ID            string    `json:"id"`
	Name          *string   `json:"name,omitempty"`
	Addresses     []string  `json:"addresses,omitempty"`
	Status        *string   `json:"status,omitempty"`
	Platform      *Platform `json:"platform,omitempty"`
	ClientPort    *int      `json:"client_port,omitempty"`
	// Guard replaces the member's guard results (data nodes only).
	Guard *Guard `json:"guard,omitempty"`
	// Pinned, with Addresses, records whether an operator set them.
	Pinned *bool `json:"pinned,omitempty"`
	// UnlessPinned: a node announcing what it detected; the addresses are
	// left alone if an operator pinned them. Decided when the entry is
	// applied, so a node whose own copy of the state is still behind
	// (right after a restart) cannot undo a pin it has not seen yet.
	UnlessPinned bool `json:"unless_pinned,omitempty"`
	// Removing sets or clears Member.Removing (D59).
	Removing *bool `json:"removing,omitempty"`
}

// RemoveMember removes a member. The primary and members of the sync set
// cannot be removed (move the primary / shrink R first, rule 3).
type RemoveMember struct {
	ID string `json:"id"`
}

// SetSuffrage records a member's Raft suffrage after a config change.
type SetSuffrage struct {
	ID       string   `json:"id"`
	Suffrage Suffrage `json:"suffrage"`
}

// CreateJoinSecret stores a hashed one-time join secret.
type CreateJoinSecret struct {
	Hash      string    `json:"hash"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ConsumeJoinSecret marks a secret used.
type ConsumeJoinSecret struct {
	Hash   string    `json:"hash"`
	NodeID string    `json:"node_id"`
	At     time.Time `json:"at"`
}

// ExpireJoinSecrets deletes secrets whose expiry plus SecretRetention is at
// or before At.
type ExpireJoinSecrets struct {
	At time.Time `json:"at"`
}

// SetPrimary makes NodeID the primary ("" clears it) and bumps the epoch.
// It is a compare-and-set: it fails with ErrEpochMismatch unless the current
// epoch equals ExpectedEpoch, so two controllers cannot both win. NodeID must
// be a data member, and a member of the sync set R when R is non-empty
// (rule 3), unless Forced (manual operation).
type SetPrimary struct {
	ExpectedEpoch uint64        `json:"expected_epoch"`
	NodeID        string        `json:"node_id"`
	LeaseDuration time.Duration `json:"lease_duration"`
	Forced        bool          `json:"forced,omitempty"`
}

// RenewLease bumps the lease sequence. NodeID must be the primary and Epoch
// the current epoch. With Served it also records NodeID as
// State.LastServed: the primary sets it once its sync standbys hold all the
// WAL it had when it started serving (so every write acknowledged in any
// earlier epoch is on k members of R other than itself).
type RenewLease struct {
	NodeID string `json:"node_id"`
	Epoch  uint64 `json:"epoch"`
	Served bool   `json:"served,omitempty"`
}

// SetSyncSet replaces R and k, tagged with the current epoch (CAS).
type SetSyncSet struct {
	ExpectedEpoch uint64   `json:"expected_epoch"`
	Members       []string `json:"members"`
	K             int      `json:"k"`
}

// SetSettings replaces the settings.
type SetSettings struct {
	Settings Settings `json:"settings"`
}

// SetStandbyEligible records rule 9's standby_eligible flag (CAS on epoch).
// Since changes only when the value changes.
type SetStandbyEligible struct {
	ExpectedEpoch uint64    `json:"expected_epoch"`
	Eligible      bool      `json:"eligible"`
	At            time.Time `json:"at"`
}

// AddAPIKey stores an API key hash.
type AddAPIKey struct {
	Hash string    `json:"hash"`
	Name string    `json:"name"`
	At   time.Time `json:"at"`
}

// RemoveAPIKey deletes an API key hash.
type RemoveAPIKey struct {
	Hash string `json:"hash"`
}

// ForceRecovered is written by ForceNewCluster on the lone survivor: every
// other member is removed, the epoch is bumped, the survivor becomes primary
// if it is a data node, R is cleared and standby_eligible is false. A
// ForcedRecovery marker is appended.
type ForceRecovered struct {
	NodeID string    `json:"node_id"`
	At     time.Time `json:"at"`
}

// SetPGSecrets stores the cluster's Postgres passwords. It is set once:
// the same value again is a no-op, a different one is ErrInvalid (the
// passwords are baked into the replicated catalog).
type SetPGSecrets struct {
	Secrets PGSecrets `json:"secrets"`
}

// SetAppPassword sets (or changes) the application role's password. It
// needs the other secrets to be set already. The primary applies it to the
// role; every data node copies it into its local secrets file.
type SetAppPassword struct {
	Password string `json:"password"`
}

// Promote is the controller's compare-and-set of the primary, used by
// automatic failover and the planned switch. It is stricter than
// SetPrimary (which `dev switch` and forced operations keep using):
//
//   - the epoch must equal ExpectedEpoch;
//   - if ExpectedLeaseSeq is set, the lease must still be the one the
//     controller watched expire (same epoch and sequence), so a primary
//     that renewed in the meantime is never replaced by a failover;
//   - the target must be an active data member, a member of a non-empty
//     sync set R, and standby_eligible must be true (rules 3 and 9);
//   - the target must not already be the primary.
//
// It bumps the epoch, names the target, resets the lease, and adds
// AddToSync (data members, e.g. the old primary when the target would
// otherwise have no copy to wait for) to R.
//
// OneCopy (rule 9, failover only) is for a target that has no copy it
// could wait for: in the same entry it records standby_eligible=false
// (since At) and clears R, so the new primary starts with an empty sync
// list. Eligibility is false before any primary of the new epoch has a
// sync list, which keeps rule 9's order; no primary of the new epoch has an
// S yet, so clearing R cannot leave an S outside R (rule 3). OneCopy and
// AddToSync are exclusive.
type Promote struct {
	ExpectedEpoch    uint64        `json:"expected_epoch"`
	ExpectedLeaseSeq *uint64       `json:"expected_lease_seq,omitempty"`
	NodeID           string        `json:"node_id"`
	LeaseDuration    time.Duration `json:"lease_duration"`
	AddToSync        []string      `json:"add_to_sync,omitempty"`
	OneCopy          bool          `json:"one_copy,omitempty"`
	At               time.Time     `json:"at,omitzero"`
}

func (*BootstrapState) commandType() string     { return "bootstrap" }
func (*AddMember) commandType() string          { return "add_member" }
func (*UpdateMember) commandType() string       { return "update_member" }
func (*RemoveMember) commandType() string       { return "remove_member" }
func (*SetSuffrage) commandType() string        { return "set_suffrage" }
func (*CreateJoinSecret) commandType() string   { return "create_join_secret" }
func (*ConsumeJoinSecret) commandType() string  { return "consume_join_secret" }
func (*ExpireJoinSecrets) commandType() string  { return "expire_join_secrets" }
func (*SetPrimary) commandType() string         { return "set_primary" }
func (*RenewLease) commandType() string         { return "renew_lease" }
func (*SetSyncSet) commandType() string         { return "set_sync_set" }
func (*SetSettings) commandType() string        { return "set_settings" }
func (*SetStandbyEligible) commandType() string { return "set_standby_eligible" }
func (*AddAPIKey) commandType() string          { return "add_api_key" }
func (*RemoveAPIKey) commandType() string       { return "remove_api_key" }
func (*ForceRecovered) commandType() string     { return "force_recovered" }
func (*SetPGSecrets) commandType() string       { return "set_pg_secrets" }
func (*SetAppPassword) commandType() string     { return "set_app_password" }
func (*Promote) commandType() string            { return "promote" }
