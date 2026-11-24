package cluster

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/raft"
)

var t0 = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

var macPlat = Platform{OS: "darwin", Arch: "arm64", PGBuildSHA: "aaaaaaaaaaaaaaaaaaaaaaaa"}
var linuxPlat = Platform{OS: "linux", Arch: "amd64", PGBuildSHA: "bbbbbbbbbbbbbbbbbbbbbbbb"}

type fsmHarness struct {
	t   *testing.T
	f   *FSM
	idx uint64
	log [][]byte
}

func newHarness(t *testing.T) *fsmHarness {
	return &fsmHarness{t: t, f: NewFSM(nil)}
}

func (h *fsmHarness) applyRaw(data []byte) (Result, error) {
	h.idx++
	h.log = append(h.log, data)
	r := h.f.Apply(&raft.Log{Index: h.idx, Term: 1, Type: raft.LogCommand, Data: data}).(*applyResponse)
	return r.Result, r.Err
}

func (h *fsmHarness) apply(c Command) (Result, error) {
	h.t.Helper()
	data, err := EncodeCommand(c)
	if err != nil {
		h.t.Fatal(err)
	}
	return h.applyRaw(data)
}

func (h *fsmHarness) must(c Command) Result {
	h.t.Helper()
	r, err := h.apply(c)
	if err != nil {
		h.t.Fatalf("%T: %v", c, err)
	}
	return r
}

func (h *fsmHarness) mustFail(c Command, want error) {
	h.t.Helper()
	before := h.f.State()
	_, err := h.apply(c)
	if !errors.Is(err, want) {
		h.t.Fatalf("%T: want %v, got %v", c, want, err)
	}
	after := h.f.State()
	after.AppliedIndex = before.AppliedIndex
	if !reflect.DeepEqual(before, after) {
		h.t.Fatalf("%T: failed command changed state", c)
	}
}

func member(id, name string, role Role, p Platform) Member {
	return Member{ID: id, Name: name, Addresses: []string{"127.0.0.1:1"}, Role: role, Platform: p}
}

func bootstrapped(t *testing.T) *fsmHarness {
	h := newHarness(t)
	h.must(&BootstrapState{
		ClusterID: "c1", CA: CAData{CertDER: []byte{1}, KeyDER: []byte{2}},
		Member: member("n1", "one", RoleData, macPlat), Settings: DefaultSettings(), At: t0,
	})
	return h
}

func TestBootstrap(t *testing.T) {
	h := newHarness(t)
	h.mustFail(&AddMember{Member: member("n2", "two", RoleData, macPlat), At: t0}, ErrNotBootstrapped)
	h = bootstrapped(t)
	st := h.f.State()
	if st.Epoch != 1 || st.PrimaryID != "n1" || st.Members["n1"].Suffrage != Voter || st.Members["n1"].JoinedAt != t0 {
		t.Fatalf("state after bootstrap: %+v", st)
	}
	if st.Lease.Holder != "n1" || st.Lease.Epoch != 1 {
		t.Fatalf("lease %+v", st.Lease)
	}
	h.mustFail(&BootstrapState{ClusterID: "c2", CA: CAData{CertDER: []byte{1}, KeyDER: []byte{2}},
		Member: member("x", "x", RoleData, macPlat), Settings: DefaultSettings(), At: t0}, ErrAlreadyBootstrapped)
}

func TestMembers(t *testing.T) {
	h := bootstrapped(t)
	h.must(&AddMember{Member: member("n2", "two", RoleData, macPlat), At: t0.Add(time.Minute)})
	h.mustFail(&AddMember{Member: member("n2", "dup-id", RoleData, macPlat), At: t0}, ErrDuplicateMember)
	h.mustFail(&AddMember{Member: member("n3", "two", RoleData, macPlat), At: t0}, ErrDuplicateName)
	// Another kind of machine: refused while mixing is off (D37).
	h.mustFail(&AddMember{Member: member("n3", "three", RoleData, linuxPlat), At: t0}, ErrMixedOff)
	h.must(&AddMember{Member: member("w1", "witness", RoleWitness, linuxPlat), At: t0}) // witness: any platform
	h.mustFail(&AddMember{Member: Member{ID: "BAD ID", Name: "x", Addresses: []string{"a:1"}, Role: RoleData}, At: t0}, ErrInvalid)

	st := h.f.State()
	if m := st.Members["n2"]; m.Suffrage != Nonvoter || m.Status != StatusActive || m.JoinedAt != t0.Add(time.Minute) {
		t.Fatalf("defaults: %+v", m)
	}
	name := "deux"
	status := StatusNeedsReclone
	h.must(&UpdateMember{ID: "n2", Name: &name, Addresses: []string{"10.0.0.2:7481", "100.64.0.2:7481"}, Status: &status})
	m := h.f.State().Members["n2"]
	if m.Name != "deux" || len(m.Addresses) != 2 || m.Status != StatusNeedsReclone {
		t.Fatalf("update: %+v", m)
	}
	// Pinned addresses: a node's own announcement (UnlessPinned) leaves
	// them; an operator's (or an explicit --advertise) replaces them.
	yes := true
	h.must(&UpdateMember{ID: "n2", Addresses: []string{"db2.lan:7481"}, Pinned: &yes})
	h.must(&UpdateMember{ID: "n2", Addresses: []string{"10.9.9.9:7481"}, UnlessPinned: true})
	if m := h.f.State().Members["n2"]; !m.AddressesPinned || m.Addresses[0] != "db2.lan:7481" {
		t.Fatalf("pin undone by an announcement: %+v", m)
	}
	h.must(&UpdateMember{ID: "n2", Addresses: []string{"10.9.9.9:7481"}})
	if m := h.f.State().Members["n2"]; m.AddressesPinned || m.Addresses[0] != "10.9.9.9:7481" {
		t.Fatalf("explicit addresses did not replace the pin: %+v", m)
	}
	port := 6432
	h.must(&UpdateMember{ID: "n2", ClientPort: &port})
	if h.f.State().Members["n2"].ClientPort != 6432 {
		t.Fatal("client port not set")
	}
	one := "one"
	h.mustFail(&UpdateMember{ID: "n2", Name: &one}, ErrDuplicateName)
	h.mustFail(&UpdateMember{ID: "nope"}, ErrUnknownMember)
	h.mustFail(&UpdateMember{ID: "n2", Platform: &linuxPlat}, ErrMixedOff)
	bad := "weird"
	h.mustFail(&UpdateMember{ID: "n2", Status: &bad}, ErrInvalid)

	h.must(&SetSuffrage{ID: "n2", Suffrage: Voter})
	if h.f.State().Members["n2"].Suffrage != Voter {
		t.Fatal("suffrage not set")
	}
	h.mustFail(&SetSuffrage{ID: "n2", Suffrage: "maybe"}, ErrInvalid)

	// D59: a member being removed may stay in R, leaves it, and never
	// enters it again (the controller cannot undo a removal's hand-over).
	yes, no := true, false
	h.must(&SetSyncSet{ExpectedEpoch: 1, Members: []string{"n2"}, K: 1})
	h.must(&UpdateMember{ID: "n2", Removing: &yes})
	if !h.f.State().Members["n2"].Removing {
		t.Fatal("removing not recorded")
	}
	h.must(&SetSyncSet{ExpectedEpoch: 1, Members: []string{"n2"}, K: 1})
	h.must(&SetSyncSet{ExpectedEpoch: 1, Members: nil, K: 0})
	h.mustFail(&SetSyncSet{ExpectedEpoch: 1, Members: []string{"n2"}, K: 1}, ErrRemoving)
	h.must(&UpdateMember{ID: "n2", Removing: &no})
	h.must(&SetSyncSet{ExpectedEpoch: 1, Members: []string{"n2"}, K: 1})
	h.must(&SetSyncSet{ExpectedEpoch: 1, Members: nil, K: 0})

	h.mustFail(&RemoveMember{ID: "n1"}, ErrIsPrimary)
	h.must(&SetSyncSet{ExpectedEpoch: 1, Members: []string{"n2"}, K: 1})
	h.mustFail(&RemoveMember{ID: "n2"}, ErrInSyncSet)
	h.must(&SetSyncSet{ExpectedEpoch: 1, Members: nil, K: 0})
	h.must(&RemoveMember{ID: "n2"})
	h.mustFail(&RemoveMember{ID: "n2"}, ErrUnknownMember)
}

func TestJoinSecrets(t *testing.T) {
	h := bootstrapped(t)
	hash := strings.Repeat("ab", 32)
	h.must(&CreateJoinSecret{Hash: hash, CreatedAt: t0, ExpiresAt: t0.Add(time.Hour)})
	h.mustFail(&CreateJoinSecret{Hash: hash, CreatedAt: t0, ExpiresAt: t0.Add(time.Hour)}, ErrInvalid)
	h.mustFail(&CreateJoinSecret{Hash: "short", CreatedAt: t0, ExpiresAt: t0.Add(time.Hour)}, ErrInvalid)
	// Expired (judged by the command's own time).
	h.mustFail(&AddMember{Member: member("n2", "two", RoleData, macPlat), SecretHash: hash, At: t0.Add(time.Hour)}, ErrSecretExpired)
	h.mustFail(&AddMember{Member: member("n2", "two", RoleData, macPlat), SecretHash: strings.Repeat("cd", 32), At: t0}, ErrSecretUnknown)
	// A rejected join (platform) does not burn the secret.
	h.mustFail(&AddMember{Member: member("n2", "two", RoleData, linuxPlat), SecretHash: hash, At: t0}, ErrMixedOff)
	h.must(&AddMember{Member: member("n2", "two", RoleData, macPlat), SecretHash: hash, KeyHash: "kh2", At: t0.Add(time.Minute)})
	if s := h.f.State().JoinSecrets[hash]; !s.Used || s.UsedBy != "n2" || s.UsedByKey != "kh2" {
		t.Fatalf("secret not consumed: %+v", s)
	}
	h.mustFail(&AddMember{Member: member("n3", "three", RoleData, macPlat), SecretHash: hash, At: t0.Add(time.Minute)}, ErrSecretUsed)

	// Standalone consume.
	h2 := strings.Repeat("ef", 32)
	h.must(&CreateJoinSecret{Hash: h2, CreatedAt: t0, ExpiresAt: t0.Add(2 * time.Hour)})
	h.must(&ConsumeJoinSecret{Hash: h2, NodeID: "x", At: t0})
	h.mustFail(&ConsumeJoinSecret{Hash: h2, NodeID: "y", At: t0}, ErrSecretUsed)

	// Expire: secrets are kept SecretRetention past expiry, then removed:
	// the first (expired t0+1h) goes, the second (t0+2h) stays.
	h.must(&ExpireJoinSecrets{At: t0.Add(time.Hour)})
	if len(h.f.State().JoinSecrets) != 2 {
		t.Fatal("secret removed before its retention ended")
	}
	h.must(&ExpireJoinSecrets{At: t0.Add(time.Hour + SecretRetention)})
	st := h.f.State()
	if _, ok := st.JoinSecrets[hash]; ok {
		t.Fatal("expired secret kept")
	}
	if _, ok := st.JoinSecrets[h2]; !ok {
		t.Fatal("live secret removed")
	}
}

func TestPrimaryEpochCAS(t *testing.T) {
	h := bootstrapped(t)
	h.must(&AddMember{Member: member("n2", "two", RoleData, macPlat), At: t0})
	h.must(&AddMember{Member: member("w", "w", RoleWitness, linuxPlat), At: t0})

	// Two controllers race from epoch 1: exactly one wins.
	r := h.must(&SetPrimary{ExpectedEpoch: 1, NodeID: "n2", LeaseDuration: 6 * time.Second})
	if r.Epoch != 2 {
		t.Fatalf("epoch %d", r.Epoch)
	}
	h.mustFail(&SetPrimary{ExpectedEpoch: 1, NodeID: "n1"}, ErrEpochMismatch)
	st := h.f.State()
	if st.PrimaryID != "n2" || st.Lease.Holder != "n2" || st.Lease.Epoch != 2 || st.Lease.Seq != 0 {
		t.Fatalf("after set primary: %+v %+v", st.PrimaryID, st.Lease)
	}
	h.mustFail(&SetPrimary{ExpectedEpoch: 2, NodeID: "w"}, ErrNotEligible)
	h.mustFail(&SetPrimary{ExpectedEpoch: 2, NodeID: "ghost"}, ErrUnknownMember)

	// Lease renewals.
	if r := h.must(&RenewLease{NodeID: "n2", Epoch: 2}); r.LeaseSeq != 1 {
		t.Fatalf("seq %d", r.LeaseSeq)
	}
	h.mustFail(&RenewLease{NodeID: "n1", Epoch: 2}, ErrNotPrimary)
	h.mustFail(&RenewLease{NodeID: "n2", Epoch: 1}, ErrEpochMismatch)

	// Sync set: epoch-tagged CAS, data members only, k bounds.
	h.must(&SetSyncSet{ExpectedEpoch: 2, Members: []string{"n2", "n1"}, K: 1})
	if s := h.f.State().Sync; !reflect.DeepEqual(s.Members, []string{"n1", "n2"}) || s.K != 1 || s.Epoch != 2 {
		t.Fatalf("sync %+v", s)
	}
	h.mustFail(&SetSyncSet{ExpectedEpoch: 1, Members: []string{"n1"}, K: 1}, ErrEpochMismatch)
	h.mustFail(&SetSyncSet{ExpectedEpoch: 2, Members: []string{"n1"}, K: 2}, ErrInvalid)
	h.mustFail(&SetSyncSet{ExpectedEpoch: 2, Members: []string{"w"}, K: 0}, ErrInvalid)
	h.mustFail(&SetSyncSet{ExpectedEpoch: 2, Members: []string{"n1", "n1"}, K: 1}, ErrInvalid)

	// Only members of R may be promoted, unless forced.
	h.must(&SetSyncSet{ExpectedEpoch: 2, Members: []string{"n2"}, K: 1})
	h.mustFail(&SetPrimary{ExpectedEpoch: 2, NodeID: "n1"}, ErrNotEligible)
	if r := h.must(&SetPrimary{ExpectedEpoch: 2, NodeID: "n1", Forced: true}); r.Epoch != 3 {
		t.Fatalf("forced epoch %d", r.Epoch)
	}
	// Clearing the primary also bumps the epoch.
	if r := h.must(&SetPrimary{ExpectedEpoch: 3, NodeID: ""}); r.Epoch != 4 || h.f.State().PrimaryID != "" {
		t.Fatal("clear primary")
	}
}

func TestSettingsEligibilityAPIKeys(t *testing.T) {
	h := bootstrapped(t)
	s := DefaultSettings()
	s.SingleDataNodeMode = ModeStrict
	h.must(&SetSettings{Settings: s})
	if h.f.State().Settings.SingleDataNodeMode != ModeStrict {
		t.Fatal("settings")
	}
	bad := s
	bad.SingleDataNodeMode = "yolo"
	h.mustFail(&SetSettings{Settings: bad}, ErrInvalid)
	bad = s
	bad.LeaseDuration = bad.LeaseRenewInterval
	h.mustFail(&SetSettings{Settings: bad}, ErrInvalid)

	h.must(&SetStandbyEligible{ExpectedEpoch: 1, Eligible: true, At: t0.Add(time.Minute)})
	h.must(&SetStandbyEligible{ExpectedEpoch: 1, Eligible: true, At: t0.Add(2 * time.Minute)})
	st := h.f.State()
	if !st.StandbyEligible || !st.StandbyEligibleSince.Equal(t0.Add(time.Minute)) {
		t.Fatalf("since must not move while unchanged: %v", st.StandbyEligibleSince)
	}
	h.must(&SetStandbyEligible{ExpectedEpoch: 1, Eligible: false, At: t0.Add(3 * time.Minute)})
	if st := h.f.State(); st.StandbyEligible || !st.StandbyEligibleSince.Equal(t0.Add(3*time.Minute)) {
		t.Fatal("eligibility change")
	}
	h.mustFail(&SetStandbyEligible{ExpectedEpoch: 7, Eligible: true, At: t0}, ErrEpochMismatch)

	h.must(&AddAPIKey{Hash: "h1", Name: "laptop", At: t0})
	h.mustFail(&AddAPIKey{Hash: "h1", Name: "again", At: t0}, ErrInvalid)
	h.must(&RemoveAPIKey{Hash: "h1"})
	h.mustFail(&RemoveAPIKey{Hash: "h1"}, ErrInvalid)
}

func TestForceRecovered(t *testing.T) {
	h := bootstrapped(t)
	h.must(&AddMember{Member: member("n2", "two", RoleData, macPlat), At: t0})
	h.must(&AddMember{Member: member("n3", "three", RoleData, macPlat), At: t0})
	h.must(&SetSyncSet{ExpectedEpoch: 1, Members: []string{"n2", "n3"}, K: 1})
	r := h.must(&ForceRecovered{NodeID: "n3", At: t0.Add(time.Hour)})
	st := h.f.State()
	if r.Epoch != 2 || st.PrimaryID != "n3" || len(st.Members) != 1 || len(st.Sync.Members) != 0 || st.StandbyEligible {
		t.Fatalf("after force: %+v", st)
	}
	if len(st.Forced) != 1 || st.Forced[0].NodeID != "n3" || st.Forced[0].Epoch != 2 {
		t.Fatalf("marker %+v", st.Forced)
	}
}

func TestPGSecretsSetOnceAndRedacted(t *testing.T) {
	h := bootstrapped(t)
	s := PGSecrets{Superuser: "su-pw", Replication: "repl-pw", Rewind: "rw-pw"}
	h.mustFail(&SetPGSecrets{Secrets: PGSecrets{Superuser: "x"}}, ErrInvalid)
	h.must(&SetPGSecrets{Secrets: s})
	h.must(&SetPGSecrets{Secrets: s}) // same value: no-op
	other := s
	other.Rewind = "different"
	h.mustFail(&SetPGSecrets{Secrets: other}, ErrInvalid)
	st := h.f.State()
	if st.PGSecrets == nil || *st.PGSecrets != s {
		t.Fatalf("secrets not stored: %v", st.PGSecrets)
	}
	st.PGSecrets.Superuser = "changed"
	if h.f.State().PGSecrets.Superuser != "su-pw" {
		t.Fatal("State leaked the secrets pointer")
	}
	// Survives a snapshot round trip.
	b := NewFSM(nil)
	if err := b.Restore(io.NopCloser(bytes.NewReader(snapshotBytes(t, h.f)))); err != nil {
		t.Fatal(err)
	}
	if got := b.State().PGSecrets; got == nil || *got != s {
		t.Fatal("secrets lost in snapshot")
	}
	// Formatting and slog never show the passwords.
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("x", "s", s)
	for _, out := range []string{buf.String(), fmt.Sprint(s), fmt.Sprintf("%v %+v %#v", s, s, s)} {
		if strings.Contains(out, "pw") {
			t.Fatalf("secret printed: %s", out)
		}
	}
}

func TestUnknownCommandHalts(t *testing.T) {
	cases := map[string]string{
		"future version": `{"type":"set_primary","v":2,"body":{}}`,
		"unknown type":   `{"type":"launch_rockets","v":1,"body":{}}`,
		"unknown field":  `{"type":"remove_member","v":1,"body":{"id":"x","extra":1}}`,
		"garbage":        `not json`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			var halted error
			h := bootstrapped(t)
			h.f.onHalt = func(err error) { halted = err }
			_, err := h.applyRaw([]byte(raw))
			if !errors.Is(err, ErrUnknownCommand) {
				t.Fatalf("want ErrUnknownCommand, got %v", err)
			}
			if halted == nil || h.f.Halted() == nil {
				t.Fatal("FSM did not halt")
			}
			// Nothing is applied after a halt, not even valid commands.
			before := h.f.State()
			_, err = h.apply(&SetSettings{Settings: DefaultSettings()})
			if !errors.Is(err, ErrHalted) {
				t.Fatalf("after halt: %v", err)
			}
			if !reflect.DeepEqual(before, h.f.State()) {
				t.Fatal("state changed after halt")
			}
		})
	}
	if _, err := EncodeCommand(fakeCmd{}); !errors.Is(err, ErrUnknownCommand) {
		t.Fatalf("encode unknown: %v", err)
	}
}

type fakeCmd struct{}

func (fakeCmd) commandType() string { return "fake" }

// script exercises every command type.
func script() []Command {
	hash := strings.Repeat("ab", 32)
	name := "renamed"
	s := DefaultSettings()
	s.DegradeAfter = time.Minute
	return []Command{
		&BootstrapState{ClusterID: "c1", CA: CAData{CertDER: []byte{1, 2}, KeyDER: []byte{3}}, Member: member("n1", "one", RoleData, macPlat), Settings: DefaultSettings(), At: t0},
		&CreateJoinSecret{Hash: hash, CreatedAt: t0, ExpiresAt: t0.Add(time.Hour)},
		&AddMember{Member: member("n2", "two", RoleData, macPlat), SecretHash: hash, KeyHash: strings.Repeat("0f", 32), At: t0.Add(time.Second)},
		&AddMember{Member: member("w", "w", RoleWitness, linuxPlat), At: t0.Add(2 * time.Second)},
		&AddMember{Member: member("n3", "three", RoleData, linuxPlat), At: t0}, // fails: platform
		&SetSuffrage{ID: "n2", Suffrage: Voter},
		&UpdateMember{ID: "w", Name: &name, Addresses: []string{"10.1.1.1:7481"}},
		&SetSyncSet{ExpectedEpoch: 1, Members: []string{"n2"}, K: 1},
		&SetPrimary{ExpectedEpoch: 1, NodeID: "n2", LeaseDuration: 6 * time.Second},
		&SetPrimary{ExpectedEpoch: 1, NodeID: "n1"}, // fails: CAS
		&RenewLease{NodeID: "n2", Epoch: 2},
		&RenewLease{NodeID: "n2", Epoch: 2},
		&SetSettings{Settings: s},
		&SetStandbyEligible{ExpectedEpoch: 2, Eligible: true, At: t0.Add(time.Minute)},
		&AddAPIKey{Hash: "k1", Name: "ci", At: t0},
		&AddAPIKey{Hash: "k2", Name: "laptop", At: t0},
		&RemoveAPIKey{Hash: "k1"},
		&ConsumeJoinSecret{Hash: hash, NodeID: "z", At: t0}, // fails: used
		&ExpireJoinSecrets{At: t0.Add(2*time.Hour + SecretRetention)},
		&SetSyncSet{ExpectedEpoch: 2, Members: []string{"n1", "n2"}, K: 1},
		&RemoveMember{ID: "w"},
		&ForceRecovered{NodeID: "n1", At: t0.Add(3 * time.Hour)},
	}
}

func snapshotBytes(t *testing.T, f *FSM) []byte {
	t.Helper()
	s, err := f.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	sink := &memSink{}
	if err := s.Persist(sink); err != nil {
		t.Fatal(err)
	}
	s.Release()
	return sink.Bytes()
}

type memSink struct{ bytes.Buffer }

func (m *memSink) ID() string    { return "mem" }
func (m *memSink) Cancel() error { return nil }
func (m *memSink) Close() error  { return nil }

func TestDeterminism(t *testing.T) {
	a := newHarness(t)
	for _, c := range script() {
		a.apply(c)
	}
	b := newHarness(t)
	for _, raw := range a.log {
		b.applyRaw(raw)
	}
	sa, sb := snapshotBytes(t, a.f), snapshotBytes(t, b.f)
	if !bytes.Equal(sa, sb) {
		t.Fatalf("snapshots differ:\n%s\n%s", sa, sb)
	}
	st := a.f.State()
	if st.Epoch != 3 || st.PrimaryID != "n1" || len(st.APIKeys) != 1 || len(st.JoinSecrets) != 0 {
		t.Fatalf("unexpected end state: %+v", st)
	}
}

func TestSnapshotRestoreRoundTrip(t *testing.T) {
	a := newHarness(t)
	for _, c := range script()[:18] { // before ForceRecovered wipes members
		a.apply(c)
	}
	snap := snapshotBytes(t, a.f)
	b := NewFSM(nil)
	sub := b.Subscribe()
	defer sub.Close()
	if err := b.Restore(io.NopCloser(bytes.NewReader(snap))); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sub.C:
	default:
		t.Fatal("restore did not notify")
	}
	if !reflect.DeepEqual(a.f.State(), b.State()) {
		t.Fatalf("restored state differs:\n%+v\n%+v", a.f.State(), b.State())
	}
	if !bytes.Equal(snap, snapshotBytes(t, b)) {
		t.Fatal("re-snapshot differs")
	}
	// Every field survived: spot-check the less obvious ones.
	st := b.State()
	if len(st.CA.KeyDER) == 0 || st.Lease.Seq != 2 || !st.StandbyEligible || st.Settings.DegradeAfter != time.Minute || st.AppliedIndex != 18 {
		t.Fatalf("restored: %+v", st)
	}
	// Unknown snapshot format is refused.
	var m map[string]any
	json.Unmarshal(snap, &m)
	m["format"] = 99
	bad, _ := json.Marshal(m)
	if err := NewFSM(nil).Restore(io.NopCloser(bytes.NewReader(bad))); err == nil {
		t.Fatal("future snapshot format accepted")
	}
}

func TestStateIsACopy(t *testing.T) {
	h := bootstrapped(t)
	st := h.f.State()
	m := st.Members["n1"]
	m.Addresses[0] = "evil:1"
	st.Members["n1"] = m
	st.CA.KeyDER[0] = 99
	if h.f.State().Members["n1"].Addresses[0] == "evil:1" || h.f.State().CA.KeyDER[0] == 99 {
		t.Fatal("State leaked internal pointers")
	}
}

func TestSubscriptionCoalesces(t *testing.T) {
	h := bootstrapped(t)
	sub := h.f.Subscribe()
	for range 5 {
		h.must(&RenewLease{NodeID: "n1", Epoch: 1})
	}
	<-sub.C
	select {
	case <-sub.C:
		t.Fatal("signals should coalesce")
	default:
	}
	sub.Close()
	h.must(&RenewLease{NodeID: "n1", Epoch: 1})
	select {
	case <-sub.C:
		t.Fatal("closed subscription signalled")
	default:
	}
}

func TestErrorsCrossTheWire(t *testing.T) {
	err := errorf(ErrEpochMismatch, "expected 1, current 2")
	back := fromWire(toWire(err))
	if !errors.Is(back, ErrEpochMismatch) || back.Error() != err.Error() {
		t.Fatalf("got %v", back)
	}
	if errors.Is(back, ErrSecretUsed) {
		t.Fatal("matched wrong sentinel")
	}
}
