package cluster

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/raft"

	"coyotedb.com/coyote/internal/pki"
)

// snapshotFormat is the version of the snapshot encoding.
const snapshotFormat = 1

type snapshotFile struct {
	Format int   `json:"format"`
	State  State `json:"state"`
}

// FSM is the Raft state machine holding State.
type FSM struct {
	mu     sync.RWMutex
	st     *State
	halted error

	subMu sync.Mutex
	subs  map[*Subscription]struct{}

	onHalt func(error)
}

// NewFSM returns an empty state machine. onHalt (may be nil) is called once,
// outside any lock, if an entry cannot be decoded.
func NewFSM(onHalt func(error)) *FSM {
	return &FSM{st: newState(), subs: map[*Subscription]struct{}{}, onHalt: onHalt}
}

type applyResponse struct {
	Result Result
	Err    error
}

// Apply implements raft.FSM.
func (f *FSM) Apply(l *raft.Log) any {
	if l.Type != raft.LogCommand {
		return nil
	}
	f.mu.Lock()
	if f.halted != nil {
		f.mu.Unlock()
		return &applyResponse{Err: f.halted}
	}
	cmd, err := DecodeCommand(l.Data)
	if err != nil {
		f.halted = errorf(ErrHalted, "entry %d: %v", l.Index, err)
		h := f.halted
		f.mu.Unlock()
		if f.onHalt != nil {
			f.onHalt(h)
		}
		f.notify()
		return &applyResponse{Err: err}
	}
	err = applyCommand(f.st, cmd)
	f.st.AppliedIndex = l.Index
	res := Result{Index: l.Index, Epoch: f.st.Epoch, LeaseSeq: f.st.Lease.Seq}
	f.mu.Unlock()
	f.notify()
	return &applyResponse{Result: res, Err: err}
}

// ApplyEntry applies one command at the given log index as Raft would.
// It lets packages built on the state machine test against the real FSM
// without running Raft.
func (f *FSM) ApplyEntry(index uint64, cmd Command) (Result, error) {
	data, err := EncodeCommand(cmd)
	if err != nil {
		return Result{}, err
	}
	r := f.Apply(&raft.Log{Index: index, Term: 1, Type: raft.LogCommand, Data: data}).(*applyResponse)
	return r.Result, r.Err
}

// Halted returns the error that stopped the state machine, or nil.
func (f *FSM) Halted() error {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.halted
}

// State returns a deep copy of the current state.
func (f *FSM) State() State {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.st.Clone()
}

// member returns one member and the applied index without a full copy.
func (f *FSM) member(id string) (Member, bool, uint64) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	m, ok := f.st.Members[id]
	m.Addresses = slices.Clone(m.Addresses)
	return m, ok, f.st.AppliedIndex
}

func (f *FSM) appliedIndex() uint64 {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.st.AppliedIndex
}

// Snapshot implements raft.FSM. The state is encoded right away (it is
// small) so Persist does not race with Apply.
func (f *FSM) Snapshot() (raft.FSMSnapshot, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	b, err := json.Marshal(snapshotFile{Format: snapshotFormat, State: *f.st})
	if err != nil {
		return nil, err
	}
	return &fsmSnapshot{data: b}, nil
}

// Restore implements raft.FSM.
func (f *FSM) Restore(rc io.ReadCloser) error {
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	st, err := decodeSnapshot(b)
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.st = st
	f.mu.Unlock()
	f.notify()
	return nil
}

func decodeSnapshot(b []byte) (*State, error) {
	var probe struct {
		Format int `json:"format"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return nil, fmt.Errorf("snapshot: %w", err)
	}
	if probe.Format != snapshotFormat {
		return nil, errorf(ErrUnknownCommand, "snapshot format %d (this node knows %d)", probe.Format, snapshotFormat)
	}
	var sf snapshotFile
	if err := strictUnmarshal(b, &sf); err != nil {
		return nil, fmt.Errorf("snapshot: %w", err)
	}
	st := sf.State.Clone()
	return &st, nil
}

type fsmSnapshot struct{ data []byte }

func (s *fsmSnapshot) Persist(sink raft.SnapshotSink) error {
	if _, err := sink.Write(s.data); err != nil {
		sink.Cancel()
		return err
	}
	return sink.Close()
}

func (s *fsmSnapshot) Release() {}

// Subscription delivers a signal on C after state changes. Signals coalesce:
// a receiver that is slow sees one signal and reads the latest State. It
// never blocks the state machine.
type Subscription struct {
	C <-chan struct{}
	c chan struct{}
	f *FSM
}

// Subscribe registers a change subscription. Call Close when done.
func (f *FSM) Subscribe() *Subscription {
	c := make(chan struct{}, 1)
	s := &Subscription{C: c, c: c, f: f}
	f.subMu.Lock()
	f.subs[s] = struct{}{}
	f.subMu.Unlock()
	return s
}

// Close unregisters the subscription.
func (s *Subscription) Close() {
	s.f.subMu.Lock()
	delete(s.f.subs, s)
	s.f.subMu.Unlock()
}

func (f *FSM) notify() {
	f.subMu.Lock()
	defer f.subMu.Unlock()
	for s := range f.subs {
		select {
		case s.c <- struct{}{}:
		default:
		}
	}
}

// ---- command semantics. Validate fully, then mutate. ----

func applyCommand(st *State, cmd Command) error {
	if _, ok := cmd.(*BootstrapState); !ok && !st.Bootstrapped() {
		return ErrNotBootstrapped
	}
	switch c := cmd.(type) {
	case *BootstrapState:
		return applyBootstrap(st, c)
	case *AddMember:
		return applyAddMember(st, c)
	case *UpdateMember:
		return applyUpdateMember(st, c)
	case *RemoveMember:
		return applyRemoveMember(st, c)
	case *SetSuffrage:
		m, ok := st.Members[c.ID]
		if !ok {
			return errorf(ErrUnknownMember, "%s", c.ID)
		}
		if c.Suffrage != Voter && c.Suffrage != Nonvoter {
			return errorf(ErrInvalid, "suffrage %q", c.Suffrage)
		}
		m.Suffrage = c.Suffrage
		st.Members[c.ID] = m
		return nil
	case *CreateJoinSecret:
		if len(c.Hash) != 64 {
			return errorf(ErrInvalid, "secret hash must be hex sha256")
		}
		if _, ok := st.JoinSecrets[c.Hash]; ok {
			return errorf(ErrInvalid, "secret already exists")
		}
		if !c.ExpiresAt.After(c.CreatedAt) {
			return errorf(ErrInvalid, "expiry must be after creation")
		}
		st.JoinSecrets[c.Hash] = JoinSecret{CreatedAt: c.CreatedAt, ExpiresAt: c.ExpiresAt}
		return nil
	case *ConsumeJoinSecret:
		if err := checkSecret(st, c.Hash, c.At); err != nil {
			return err
		}
		s := st.JoinSecrets[c.Hash]
		s.Used, s.UsedBy = true, c.NodeID
		st.JoinSecrets[c.Hash] = s
		return nil
	case *ExpireJoinSecrets:
		for h, s := range st.JoinSecrets {
			// Kept a while after expiry: a used secret so its joiner can
			// still retry a join whose response was lost, an unused one so
			// a late joiner hears "expired" rather than "unknown".
			if !c.At.Before(s.ExpiresAt.Add(SecretRetention)) {
				delete(st.JoinSecrets, h)
			}
		}
		return nil
	case *SetPrimary:
		return applySetPrimary(st, c)
	case *RenewLease:
		if c.Epoch != st.Epoch {
			return errorf(ErrEpochMismatch, "lease for epoch %d, current %d", c.Epoch, st.Epoch)
		}
		if st.PrimaryID == "" || c.NodeID != st.PrimaryID {
			return errorf(ErrNotPrimary, "%s (primary is %q)", c.NodeID, st.PrimaryID)
		}
		st.Lease.Seq++
		if c.Served {
			st.LastServed = c.NodeID
		}
		return nil
	case *SetSyncSet:
		return applySetSyncSet(st, c)
	case *SetSettings:
		if err := validSettings(c.Settings); err != nil {
			return err
		}
		st.Settings = c.Settings
		return nil
	case *SetStandbyEligible:
		if c.ExpectedEpoch != st.Epoch {
			return errorf(ErrEpochMismatch, "expected %d, current %d", c.ExpectedEpoch, st.Epoch)
		}
		if st.StandbyEligible != c.Eligible || st.StandbyEligibleSince.IsZero() {
			st.StandbyEligibleSince = c.At
		}
		st.StandbyEligible = c.Eligible
		return nil
	case *AddAPIKey:
		if c.Hash == "" || len(c.Hash) > 256 {
			return errorf(ErrInvalid, "api key hash")
		}
		if _, ok := st.APIKeys[c.Hash]; ok {
			return errorf(ErrInvalid, "api key already exists")
		}
		st.APIKeys[c.Hash] = APIKey{Name: c.Name, CreatedAt: c.At}
		return nil
	case *RemoveAPIKey:
		if _, ok := st.APIKeys[c.Hash]; !ok {
			return errorf(ErrInvalid, "no such api key")
		}
		delete(st.APIKeys, c.Hash)
		return nil
	case *ForceRecovered:
		return applyForceRecovered(st, c)
	case *Promote:
		return applyPromote(st, c)
	case *SetPGSecrets:
		if !c.Secrets.complete() {
			return errorf(ErrInvalid, "postgres secrets incomplete")
		}
		if st.PGSecrets != nil {
			if cur := *st.PGSecrets; cur.Superuser == c.Secrets.Superuser && cur.Replication == c.Secrets.Replication && cur.Rewind == c.Secrets.Rewind {
				return nil // App is managed by SetAppPassword
			}
			return errorf(ErrInvalid, "postgres secrets are already set")
		}
		v := c.Secrets
		st.PGSecrets = &v
		return nil
	case *SetAppPassword:
		if st.PGSecrets == nil {
			return errorf(ErrInvalid, "postgres secrets are not set yet")
		}
		if len(c.Password) < 16 {
			return errorf(ErrInvalid, "the application password must be at least 16 characters")
		}
		st.PGSecrets.App = c.Password
		return nil
	case *SetBackup:
		return applySetBackup(st, c)
	}
	return errorf(ErrUnknownCommand, "%T", cmd)
}

func applyBootstrap(st *State, c *BootstrapState) error {
	if st.Bootstrapped() {
		return ErrAlreadyBootstrapped
	}
	if err := pki.ValidID(c.ClusterID); err != nil {
		return errorf(ErrInvalid, "%v", err)
	}
	if len(c.CA.CertDER) == 0 || len(c.CA.KeyDER) == 0 {
		return errorf(ErrInvalid, "CA missing")
	}
	m := c.Member
	m.JoinedAt = c.At
	m.Suffrage = Voter
	if m.Status == "" {
		m.Status = StatusActive
	}
	if err := validMember(m); err != nil {
		return err
	}
	if err := validSettings(c.Settings); err != nil {
		return err
	}
	st.ClusterID = c.ClusterID
	st.CA = CAData{CertDER: slices.Clone(c.CA.CertDER), KeyDER: slices.Clone(c.CA.KeyDER)}
	st.Settings = c.Settings
	st.Members[m.ID] = m
	st.Epoch = 1
	if m.Role == RoleData {
		st.PrimaryID = m.ID
		st.Lease = Lease{Holder: m.ID, Epoch: 1, Duration: c.Settings.LeaseDuration}
	}
	st.StandbyEligible = false
	st.StandbyEligibleSince = c.At
	return nil
}

func checkSecret(st *State, hash string, at time.Time) error {
	s, ok := st.JoinSecrets[hash]
	if !ok {
		return ErrSecretUnknown
	}
	if s.Used {
		return ErrSecretUsed
	}
	if !at.Before(s.ExpiresAt) {
		return ErrSecretExpired
	}
	return nil
}

func applyAddMember(st *State, c *AddMember) error {
	m := c.Member
	m.JoinedAt = c.At
	if m.Status == "" {
		m.Status = StatusActive
	}
	if m.Suffrage == "" {
		m.Suffrage = Nonvoter
	}
	if err := validMember(m); err != nil {
		return err
	}
	if _, ok := st.Members[m.ID]; ok {
		return errorf(ErrDuplicateMember, "%s", m.ID)
	}
	for _, o := range st.Members {
		if o.Name == m.Name {
			return errorf(ErrDuplicateName, "%q is taken by node %s", m.Name, o.ID)
		}
	}
	mixed, err := checkPlatform(st, m, platformCheck{join: true, at: c.At, allowMixed: c.AllowMixedOS})
	if err != nil {
		return err
	}
	if c.SecretHash != "" {
		if err := checkSecret(st, c.SecretHash, c.At); err != nil {
			return err
		}
		s := st.JoinSecrets[c.SecretHash]
		s.Used, s.UsedBy, s.UsedByKey = true, m.ID, c.KeyHash
		st.JoinSecrets[c.SecretHash] = s
	}
	st.Members[m.ID] = m
	if mixed {
		// The joiner's explicit consent turns mixing on for the cluster.
		st.Settings.MixedOS = true
	}
	return nil
}

// MixedGuardMaxAge: a data node of another kind joins a mixed cluster only
// if some data node completed a portability audit at most this long
// before the join (by the join's time, which the leader sets). Generous,
// because the audit time is the auditing node's wall clock (it is never
// used for leases, rule 4); the join handler runs a fresh audit first, so
// in practice the record is seconds old.
const MixedGuardMaxAge = 30 * time.Minute

// platformCheck says what is being checked: a join (with the join's time
// and the joiner's explicit consent to mixing machines) or an update of a
// member's own platform record (a restart that measured itself again).
type platformCheck struct {
	join       bool
	at         time.Time
	allowMixed bool
}

// checkPlatform enforces safety rule 1 for data members, with its mixed
// extension (docs/MIXED_OS.md, D29, D37). It reports whether m makes or
// keeps the cluster mixed.
//
//  1. A data member alike to every other data member (same OS, CPU and
//     Postgres build) is accepted, as before.
//  2. Otherwise the cluster mixes machines, which needs all of:
//     a. the cluster allows it (Settings.MixedOS), or this join asks for
//     it explicitly (AddMember.AllowMixedOS);
//     b. no Windows node on either side (roll-out stage 1: Linux and
//     macOS; Windows only once it passes on its own, MIXED_OS.md item 9);
//     c. both sides measured, with equal fingerprints (condition 1: file
//     format, exact Postgres version, build options, shipped zone and
//     text-search files, Unicode/ICU versions, extensions) and equal
//     behaviour-probe digests (condition 1: the must-match calculations);
//     d. for a join: no data member's guard is blocked (conditions 2-5
//     audit findings, condition 7 damage, or a drifted fingerprint), and
//     some data member audited within MixedGuardMaxAge of the join.
func checkPlatform(st *State, m Member, pc platformCheck) (bool, error) {
	if m.Role != RoleData {
		return false, nil
	}
	var others, unlike []Member
	for _, o := range st.DataMembers() {
		if o.ID == m.ID {
			continue
		}
		others = append(others, o)
		if !o.Platform.Alike(m.Platform) {
			unlike = append(unlike, o)
		}
	}
	if len(unlike) == 0 {
		return false, nil
	}
	o := unlike[0]
	if !st.Settings.MixedOS && !(pc.join && pc.allowMixed) {
		return false, errorf(ErrMixedOff,
			"this machine is %s, but the cluster's data nodes (e.g. %q) are %s; mixing kinds of machines is off for this cluster",
			describePlatform(m.Platform), o.Name, describePlatform(o.Platform))
	}
	if m.Platform.OS == "windows" {
		return false, errorf(ErrPlatformMismatch,
			"this machine runs Windows, and Windows data nodes cannot be mixed with other kinds of machines yet; the cluster's data nodes (e.g. %q) are %s",
			o.Name, describePlatform(o.Platform))
	}
	for _, u := range unlike {
		if u.Platform.OS == "windows" {
			return false, errorf(ErrPlatformMismatch,
				"data node %q runs Windows, and Windows data nodes cannot be mixed with other kinds of machines yet (this machine is %s)",
				u.Name, describePlatform(m.Platform))
		}
	}
	if !m.Platform.Measured() {
		return false, errorf(ErrPlatformMismatch,
			"this machine's database fingerprint was not measured, so it cannot be compared with %q (%s)", o.Name, describePlatform(o.Platform))
	}
	for _, u := range unlike {
		if !u.Platform.Measured() {
			return false, errorf(ErrMixedNotReady,
				"data node %q has not measured its database fingerprint yet (it needs this version of the program and a running database)", u.Name)
		}
		if u.Platform.Fingerprint != m.Platform.Fingerprint {
			return false, errorf(ErrPlatformMismatch,
				"this machine's database fingerprint differs from %q (%s): %s",
				u.Name, describePlatform(u.Platform), fingerprintDiff(m.Platform.FingerprintText, u.Platform.FingerprintText))
		}
		if u.Platform.ProbeDigest != m.Platform.ProbeDigest {
			return false, errorf(ErrPlatformMismatch,
				"this machine's database gives different answers than %q (%s) in the behaviour check (%s)",
				u.Name, describePlatform(u.Platform), probeDiff(m.Platform.ProbeGroups, u.Platform.ProbeGroups))
		}
	}
	if !pc.join {
		return true, nil
	}
	fresh := false
	for _, d := range others {
		if d.Guard.Blocked() {
			return false, errorf(ErrMixedBlocked, "data node %q reports: %s", d.Name, guardSummary(d.Guard))
		}
		if d.Guard != nil && !d.Guard.AuditAt.IsZero() {
			age := pc.at.Sub(d.Guard.AuditAt)
			if age <= MixedGuardMaxAge && age >= -MixedGuardMaxAge {
				fresh = true
			}
		}
	}
	if !fresh {
		return false, errorf(ErrMixedNotReady,
			"no data node has checked the database for things that cannot be shared between kinds of machines in the last %s", MixedGuardMaxAge)
	}
	return true, nil
}

func describePlatform(p Platform) string {
	s := p.OS + "/" + p.Arch
	if p.PGBuildSHA != "" {
		s += " (Postgres build " + short(p.PGBuildSHA) + ")"
	}
	return s
}

// fingerprintDiff names the fields that differ between two canonical
// fingerprints ("key=value" lines), with both values.
func fingerprintDiff(here, there string) string {
	kv := func(s string) ([]string, map[string]string) {
		var keys []string
		m := map[string]string{}
		for _, l := range strings.Split(s, "\n") {
			k, v, ok := strings.Cut(l, "=")
			if !ok {
				continue
			}
			if _, seen := m[k]; !seen {
				keys = append(keys, k)
			}
			m[k] = v
		}
		return keys, m
	}
	hk, h := kv(here)
	tk, t := kv(there)
	var out []string
	for _, k := range hk {
		if h[k] != t[k] {
			out = append(out, fmt.Sprintf("%s is %s here, %s there", k, shortValue(h[k]), shortValue(t[k])))
		}
	}
	for _, k := range tk {
		if _, ok := h[k]; !ok {
			out = append(out, fmt.Sprintf("%s is missing here, %s there", k, shortValue(t[k])))
		}
	}
	if len(out) == 0 {
		return "the details were not recorded"
	}
	if len(out) > 6 {
		out = append(out[:6], fmt.Sprintf("and %d more", len(out)-6))
	}
	return strings.Join(out, "; ")
}

// probeDiff names the probe groups whose digests differ.
func probeDiff(here, there []string) string {
	t := map[string]string{}
	for _, g := range there {
		k, v, _ := strings.Cut(g, "=")
		t[k] = v
	}
	var out []string
	seen := map[string]bool{}
	for _, g := range here {
		k, v, _ := strings.Cut(g, "=")
		seen[k] = true
		if t[k] != v {
			out = append(out, k)
		}
	}
	for k := range t {
		if !seen[k] {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	if len(out) == 0 {
		return "the group details were not recorded"
	}
	return "different in: " + strings.Join(out, ", ")
}

func shortValue(v string) string {
	switch {
	case v == "":
		return "(empty)"
	case len(v) == 64:
		return v[:12]
	case len(v) > 40:
		return v[:37] + "..."
	}
	return v
}

func guardSummary(g *Guard) string {
	var parts []string
	if g.Drift != "" {
		parts = append(parts, g.Drift)
	}
	if g.VerifyDamaged {
		parts = append(parts, "the integrity check found damage")
	}
	if g.AuditBlocking {
		parts = append(parts, "the database uses something that cannot be shared between kinds of machines")
	}
	if len(g.Findings) > 0 {
		parts = append(parts, strings.Join(g.Findings, "; "))
	}
	return strings.Join(parts, "; ")
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	if s == "" {
		return "(none)"
	}
	return s
}

func applyUpdateMember(st *State, c *UpdateMember) error {
	if c.ExpectedEpoch != nil && *c.ExpectedEpoch != st.Epoch {
		return errorf(ErrEpochMismatch, "expected %d, current %d", *c.ExpectedEpoch, st.Epoch)
	}
	m, ok := st.Members[c.ID]
	if !ok {
		return errorf(ErrUnknownMember, "%s", c.ID)
	}
	if c.Name != nil {
		for _, o := range st.Members {
			if o.ID != c.ID && o.Name == *c.Name {
				return errorf(ErrDuplicateName, "%q", *c.Name)
			}
		}
		m.Name = *c.Name
	}
	if c.Addresses != nil && !(c.UnlessPinned && m.AddressesPinned) {
		m.Addresses = slices.Clone(c.Addresses)
		m.AddressesPinned = c.Pinned != nil && *c.Pinned
	}
	if c.ClientPort != nil {
		if *c.ClientPort < 0 || *c.ClientPort > 65535 {
			return errorf(ErrInvalid, "client port %d", *c.ClientPort)
		}
		m.ClientPort = *c.ClientPort
	}
	if c.Status != nil {
		m.Status = *c.Status
	}
	if c.Removing != nil {
		m.Removing = *c.Removing
	}
	if c.Platform != nil {
		m.Platform = *c.Platform
		m.Platform.ProbeGroups = slices.Clone(c.Platform.ProbeGroups)
		if _, err := checkPlatform(st, m, platformCheck{}); err != nil {
			return err
		}
	}
	if c.Guard != nil {
		if m.Role != RoleData {
			return errorf(ErrInvalid, "only data nodes keep guard results")
		}
		g := *c.Guard
		if len(g.Findings) > MaxGuardFindings {
			return errorf(ErrInvalid, "at most %d guard findings", MaxGuardFindings)
		}
		g.Findings = slices.Clone(g.Findings)
		m.Guard = &g
	}
	if err := validMember(m); err != nil {
		return err
	}
	st.Members[c.ID] = m
	return nil
}

func applyRemoveMember(st *State, c *RemoveMember) error {
	if _, ok := st.Members[c.ID]; !ok {
		return errorf(ErrUnknownMember, "%s", c.ID)
	}
	if st.PrimaryID == c.ID {
		return ErrIsPrimary
	}
	if slices.Contains(st.Sync.Members, c.ID) {
		return ErrInSyncSet
	}
	delete(st.Members, c.ID)
	return nil
}

func applySetPrimary(st *State, c *SetPrimary) error {
	if c.ExpectedEpoch != st.Epoch {
		return errorf(ErrEpochMismatch, "expected %d, current %d", c.ExpectedEpoch, st.Epoch)
	}
	if c.NodeID != "" {
		m, ok := st.Members[c.NodeID]
		if !ok {
			return errorf(ErrUnknownMember, "%s", c.NodeID)
		}
		if m.Role != RoleData {
			return errorf(ErrNotEligible, "%s is a witness", c.NodeID)
		}
		if !c.Forced && len(st.Sync.Members) > 0 && !slices.Contains(st.Sync.Members, c.NodeID) {
			return errorf(ErrNotEligible, "%s is not in the sync set", c.NodeID)
		}
	}
	if c.LeaseDuration < 0 {
		return errorf(ErrInvalid, "lease duration")
	}
	st.Epoch++
	st.PrimaryID = c.NodeID
	st.Lease = Lease{Holder: c.NodeID, Epoch: st.Epoch, Duration: c.LeaseDuration}
	return nil
}

func applyPromote(st *State, c *Promote) error {
	if c.ExpectedEpoch != st.Epoch {
		return errorf(ErrEpochMismatch, "expected %d, current %d", c.ExpectedEpoch, st.Epoch)
	}
	if c.ExpectedLeaseSeq != nil && (st.Lease.Epoch != st.Epoch || st.Lease.Seq != *c.ExpectedLeaseSeq) {
		return errorf(ErrLeaseRenewed, "lease seq %d at epoch %d, expected %d", st.Lease.Seq, st.Lease.Epoch, *c.ExpectedLeaseSeq)
	}
	m, ok := st.Members[c.NodeID]
	switch {
	case !ok:
		return errorf(ErrUnknownMember, "%s", c.NodeID)
	case m.Role != RoleData:
		return errorf(ErrNotEligible, "%s is a witness", c.NodeID)
	case m.Status != StatusActive:
		return errorf(ErrNotEligible, "%s needs a re-clone", c.NodeID)
	case c.NodeID == st.PrimaryID:
		return errorf(ErrInvalid, "%s is already the primary", c.NodeID)
	case len(st.Sync.Members) == 0 || !slices.Contains(st.Sync.Members, c.NodeID):
		return errorf(ErrNotEligible, "%s is not in the sync set", c.NodeID)
	case !st.StandbyEligible:
		return errorf(ErrNotEligible, "the copies may be missing writes (standby_eligible is false)")
	case c.LeaseDuration <= 0:
		return errorf(ErrInvalid, "lease duration")
	case c.OneCopy && len(c.AddToSync) > 0:
		return errorf(ErrInvalid, "one_copy and add_to_sync are exclusive")
	case c.OneCopy && c.At.IsZero():
		return errorf(ErrInvalid, "one_copy needs a time")
	}
	if c.OneCopy {
		st.Epoch++
		st.PrimaryID = c.NodeID
		st.Lease = Lease{Holder: c.NodeID, Epoch: st.Epoch, Duration: c.LeaseDuration}
		st.StandbyEligible = false
		st.StandbyEligibleSince = c.At
		st.Sync = SyncSet{Members: []string{}, K: 0, Epoch: st.Epoch}
		return nil
	}
	r := slices.Clone(st.Sync.Members)
	for _, id := range c.AddToSync {
		am, ok := st.Members[id]
		if !ok {
			return errorf(ErrUnknownMember, "%s", id)
		}
		if am.Role != RoleData {
			return errorf(ErrInvalid, "%s is a witness", id)
		}
		if !slices.Contains(r, id) {
			r = append(r, id)
		}
	}
	slices.Sort(r)
	st.Epoch++
	st.PrimaryID = c.NodeID
	st.Lease = Lease{Holder: c.NodeID, Epoch: st.Epoch, Duration: c.LeaseDuration}
	k := st.Sync.K
	if k < 1 {
		k = 1
	}
	st.Sync = SyncSet{Members: r, K: min(k, len(r)), Epoch: st.Epoch}
	return nil
}

func applySetSyncSet(st *State, c *SetSyncSet) error {
	if c.ExpectedEpoch != st.Epoch {
		return errorf(ErrEpochMismatch, "expected %d, current %d", c.ExpectedEpoch, st.Epoch)
	}
	ms := slices.Clone(c.Members)
	slices.Sort(ms)
	if len(slices.Compact(slices.Clone(ms))) != len(ms) {
		return errorf(ErrInvalid, "duplicate sync set member")
	}
	for _, id := range ms {
		m, ok := st.Members[id]
		if !ok {
			return errorf(ErrUnknownMember, "%s", id)
		}
		if m.Role != RoleData {
			return errorf(ErrInvalid, "%s is a witness", id)
		}
		// A member being removed may stay in R (it leaves R through the
		// hand-over) but never enters it (D59).
		if m.Removing && !slices.Contains(st.Sync.Members, id) {
			return errorf(ErrRemoving, "%s", m.Name)
		}
	}
	if c.K < 0 || c.K > len(ms) {
		return errorf(ErrInvalid, "k=%d with %d members", c.K, len(ms))
	}
	if ms == nil {
		ms = []string{}
	}
	st.Sync = SyncSet{Members: ms, K: c.K, Epoch: st.Epoch}
	return nil
}

func applyForceRecovered(st *State, c *ForceRecovered) error {
	m, ok := st.Members[c.NodeID]
	if !ok {
		return errorf(ErrUnknownMember, "%s", c.NodeID)
	}
	for id := range st.Members {
		if id != c.NodeID {
			delete(st.Members, id)
		}
	}
	m.Suffrage = Voter
	st.Members[c.NodeID] = m
	st.Epoch++
	if m.Role == RoleData {
		st.PrimaryID = c.NodeID
	} else {
		st.PrimaryID = ""
	}
	st.Lease = Lease{Holder: st.PrimaryID, Epoch: st.Epoch, Duration: st.Settings.LeaseDuration}
	st.Sync = SyncSet{Members: []string{}, K: 0, Epoch: st.Epoch}
	st.StandbyEligible = false
	st.StandbyEligibleSince = c.At
	st.Forced = append(st.Forced, ForcedRecovery{NodeID: c.NodeID, Epoch: st.Epoch, At: c.At})
	return nil
}

func validMember(m Member) error {
	if err := pki.ValidID(m.ID); err != nil {
		return errorf(ErrInvalid, "member id: %v", err)
	}
	if m.Name == "" || len(m.Name) > 64 {
		return errorf(ErrInvalid, "member name must be 1..64 characters")
	}
	if m.Role != RoleData && m.Role != RoleWitness {
		return errorf(ErrInvalid, "role %q", m.Role)
	}
	if m.Suffrage != Voter && m.Suffrage != Nonvoter {
		return errorf(ErrInvalid, "suffrage %q", m.Suffrage)
	}
	if len(m.Addresses) == 0 || len(m.Addresses) > 16 {
		return errorf(ErrInvalid, "member needs 1..16 addresses")
	}
	switch m.Status {
	case StatusActive, StatusNeedsReclone:
	default:
		return errorf(ErrInvalid, "status %q", m.Status)
	}
	return nil
}

func validSettings(s Settings) error {
	switch s.SingleDataNodeMode {
	case ModeStrict, ModeDegradeAfter:
	default:
		return errorf(ErrInvalid, "single data node mode %q", s.SingleDataNodeMode)
	}
	if s.DegradeAfter < 0 || s.LeaseDuration <= 0 || s.LeaseRenewInterval <= 0 {
		return errorf(ErrInvalid, "timeouts must be positive")
	}
	if s.LeaseDuration < 3*s.LeaseRenewInterval {
		return errorf(ErrInvalid, "lease must be at least 3x the renew interval (rule 4)")
	}
	if s.LeaseRenewInterval < MinLeaseRenewInterval {
		return errorf(ErrInvalid, "the lease renew interval must be at least %s", MinLeaseRenewInterval)
	}
	if s.GoneAfter < 0 {
		return errorf(ErrInvalid, "gone_after must not be negative")
	}
	return nil
}
