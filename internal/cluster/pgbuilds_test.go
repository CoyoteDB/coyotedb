package cluster

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"coyotedb.com/coyote/internal/invite"
	"coyotedb.com/coyote/internal/pki"
)

// PostgreSQL minor updates (D70, safety rule 1): table of state machine
// decisions. Every member applies the same entries to the same state, so
// the decision depends only on the cluster's table of pinned builds.

func shaOf(c byte) string { return strings.Repeat(string(c), 64) }

var (
	mac186   = Platform{OS: "darwin", Arch: "arm64", PGBuildSHA: shaOf('1')}
	mac187   = Platform{OS: "darwin", Arch: "arm64", PGBuildSHA: shaOf('2')}
	mac188   = Platform{OS: "darwin", Arch: "arm64", PGBuildSHA: shaOf('3')}
	mac190   = Platform{OS: "darwin", Arch: "arm64", PGBuildSHA: shaOf('4')} // another major
	macX     = Platform{OS: "darwin", Arch: "arm64", PGBuildSHA: shaOf('5')} // not pinned
	macIntel = Platform{OS: "darwin", Arch: "amd64", PGBuildSHA: shaOf('6')} // 18.7, other processor
	lin187   = Platform{OS: "linux", Arch: "arm64", PGBuildSHA: shaOf('7')}  // 18.7, other OS
	// macLies: a darwin record naming a build the table has for Linux.
	macLies = Platform{OS: "darwin", Arch: "arm64", PGBuildSHA: shaOf('7')}
)

var testBuilds = map[string]PGBuild{
	shaOf('1'): {Version: "18.6.0", OS: "darwin", Arch: "arm64"},
	shaOf('2'): {Version: "18.7.0", OS: "darwin", Arch: "arm64"},
	shaOf('3'): {Version: "18.8.0", OS: "darwin", Arch: "arm64"},
	shaOf('4'): {Version: "19.0.0", OS: "darwin", Arch: "arm64"},
	shaOf('6'): {Version: "18.7.0", OS: "darwin", Arch: "amd64"},
	shaOf('7'): {Version: "18.7.0", OS: "linux", Arch: "arm64"},
}

// minorHarness: n1 (main), n2, n3 on 18.6, and a witness.
func minorHarness(t *testing.T, register bool) *fsmHarness {
	h := newHarness(t)
	h.must(&BootstrapState{
		ClusterID: "c1", CA: CAData{CertDER: []byte{1}, KeyDER: []byte{2}},
		Member: member("n1", "n1", RoleData, mac186), Settings: DefaultSettings(), At: t0,
	})
	h.must(&AddMember{Member: member("n2", "n2", RoleData, mac186), At: t0})
	h.must(&AddMember{Member: member("n3", "n3", RoleData, mac186), At: t0})
	h.must(&AddMember{Member: member("w", "w", RoleWitness, Platform{OS: "linux", Arch: "amd64"}), At: t0})
	if register {
		h.must(&RegisterPGBuilds{Builds: testBuilds})
	}
	return h
}

func setPlat(id string, p Platform) *UpdateMember {
	return &UpdateMember{ID: id, Platform: &p}
}

func joinPlat(id string, p Platform) *AddMember {
	return &AddMember{Member: member(id, id, RoleData, p), At: t0}
}

// Without the table (some member's program does not know it, or no
// member registered yet) rule 1 is exactly as before: an updated node's
// record does not move and a joiner on the new build is refused. This is
// what 0.1.0 does, and what the cluster keeps doing while a 0.1.0 member
// remains.
func TestPGMinorWithoutTableUnchanged(t *testing.T) {
	h := minorHarness(t, false)
	h.mustFail(setPlat("n2", mac187), ErrMixedOff)
	h.mustFail(joinPlat("n4", mac187), ErrMixedOff)
	h.must(joinPlat("n4", mac186)) // alike: as before
}

func TestPGMinorForwardUpdate(t *testing.T) {
	h := minorHarness(t, true)
	// Copies first, one at a time, then the old main node.
	h.must(setPlat("n2", mac187))
	h.must(setPlat("n3", mac187))
	h.must(setPlat("n1", mac187))
	st := h.f.State()
	for _, id := range []string{"n1", "n2", "n3"} {
		if got := st.Members[id].Platform; !got.Equal(mac187) {
			t.Fatalf("%s recorded %+v, want 18.7", id, got)
		}
	}
	// A measured record (fingerprint, probe) moves too.
	m := measured(mac188, fpA, "p1p1p1p1")
	h.must(setPlat("n2", m))
	if got := h.f.State().Members["n2"].Platform; !got.Equal(m) {
		t.Fatalf("n2 recorded %+v", got)
	}
}

func TestPGMinorRefusals(t *testing.T) {
	cases := []struct {
		name string
		prep func(h *fsmHarness)
		cmd  Command
		text string
	}{
		{"downgrade while another node is newer", func(h *fsmHarness) {
			h.must(setPlat("n2", mac187))
			h.must(setPlat("n3", mac187))
		}, setPlat("n3", mac186), "older than the PostgreSQL 18.7 it ran before"},
		{"other major", nil, setPlat("n2", mac190), "mixing kinds of machines is off"},
		{"other processor", nil, setPlat("n2", macIntel), "mixing kinds of machines is off"},
		{"other OS", nil, setPlat("n2", lin187), "mixing kinds of machines is off"},
		{"unpinned build", nil, setPlat("n2", macX), "mixing kinds of machines is off"},
		{"record and table disagree on the OS", nil, setPlat("n2", macLies), "mixing kinds of machines is off"},
		{"joiner below the newest node", func(h *fsmHarness) {
			h.must(setPlat("n2", mac187))
		}, joinPlat("n4", mac186), "older than data node \"n2\" (PostgreSQL 18.7)"},
		{"joiner below the main node", func(h *fsmHarness) {
			h.must(setPlat("n2", mac188))
			h.must(setPlat("n3", mac188))
			h.must(setPlat("n1", mac188))
		}, joinPlat("n4", mac187), "a new data node must run the newest PostgreSQL in the cluster"},
		{"joiner on another major", nil, joinPlat("n4", mac190), "mixing kinds of machines is off"},
		{"joiner unpinned", nil, joinPlat("n4", macX), "mixing kinds of machines is off"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := minorHarness(t, true)
			if c.prep != nil {
				c.prep(h)
			}
			before := h.f.State()
			_, err := h.apply(c.cmd)
			if err == nil || !strings.Contains(err.Error(), c.text) {
				t.Fatalf("want a refusal with %q, got %v", c.text, err)
			}
			if !errors.Is(err, ErrPlatformMismatch) && !errors.Is(err, ErrMixedOff) {
				t.Fatalf("refusal code: %v", err)
			}
			after := h.f.State()
			after.AppliedIndex = before.AppliedIndex
			if !reflect.DeepEqual(before, after) {
				t.Fatal("a refused command changed the state")
			}
		})
	}
}

func TestPGMinorJoins(t *testing.T) {
	h := minorHarness(t, true)
	// Mid-roll: n2 on 18.7, n1 and n3 still 18.6. A joiner on the newest
	// build (at or above every record) is accepted; on a newer one too.
	h.must(setPlat("n2", mac187))
	h.must(joinPlat("n4", mac187))
	h.must(joinPlat("n5", mac188))
	// Now 18.8 is the newest record: 18.7 is too old.
	h.mustFail(joinPlat("n6", mac187), ErrPlatformMismatch)
	// Witnesses: any platform, as before.
	h.must(&AddMember{Member: member("w2", "w2", RoleWitness, macX), At: t0})
}

// Going back to the build every other data node runs is "alike" and is
// accepted as before D70 (undoing an update that went wrong).
func TestPGMinorUndoToAlike(t *testing.T) {
	h := minorHarness(t, true)
	h.must(setPlat("n2", mac187))
	h.must(setPlat("n2", mac186))
	if got := h.f.State().Members["n2"].Platform; !got.Equal(mac186) {
		t.Fatalf("n2 recorded %+v", got)
	}
}

func TestRegisterPGBuildsValidation(t *testing.T) {
	h := minorHarness(t, false)
	h.mustFail(&RegisterPGBuilds{}, ErrInvalid)
	h.mustFail(&RegisterPGBuilds{Builds: map[string]PGBuild{"abc": {Version: "18.7.0", OS: "darwin", Arch: "arm64"}}}, ErrInvalid)
	h.mustFail(&RegisterPGBuilds{Builds: map[string]PGBuild{strings.ToUpper(shaOf('a')): {Version: "18.7.0", OS: "darwin", Arch: "arm64"}}}, ErrInvalid)
	for _, v := range []string{"", "18.7", "18.07.0", "x.7.0", "0.1.0", "1.2.3.4", "-1.7.0"} {
		h.mustFail(&RegisterPGBuilds{Builds: map[string]PGBuild{shaOf('a'): {Version: v, OS: "darwin", Arch: "arm64"}}}, ErrInvalid)
	}
	h.mustFail(&RegisterPGBuilds{Builds: map[string]PGBuild{shaOf('a'): {Version: "18.7.0", Arch: "arm64"}}}, ErrInvalid)
	h.must(&RegisterPGBuilds{Builds: testBuilds})
	// Registering again (every node registers its own table): no change.
	h.must(&RegisterPGBuilds{Builds: map[string]PGBuild{shaOf('2'): testBuilds[shaOf('2')]}})
	// A sha256 names one archive: a different description refuses the
	// whole command, also its new entries.
	h.mustFail(&RegisterPGBuilds{Builds: map[string]PGBuild{
		shaOf('2'): {Version: "18.9.0", OS: "darwin", Arch: "arm64"},
		shaOf('b'): {Version: "18.9.0", OS: "darwin", Arch: "arm64"},
	}}, ErrInvalid)
	// The size cap.
	big := map[string]PGBuild{}
	for i := 0; len(big) < MaxPGBuilds; i++ {
		sha := strings.Repeat("0", 56) + strings.Repeat("c", 8)
		b := []byte(sha)
		for j, x := 0, i; j < 8; j, x = j+1, x/16 {
			b[63-j] = "0123456789abcdef"[x%16]
		}
		big[string(b)] = PGBuild{Version: "18.1.0", OS: "linux", Arch: "amd64"}
	}
	h.mustFail(&RegisterPGBuilds{Builds: big}, ErrInvalid)
}

// The table survives a snapshot and a restore unchanged, and the state's
// clone does not share it.
func TestPGBuildsSnapshotAndClone(t *testing.T) {
	h := minorHarness(t, true)
	h.must(setPlat("n2", mac187))
	st := h.f.State()
	c := st.Clone()
	c.PGBuilds[shaOf('9')] = PGBuild{}
	if _, ok := h.f.State().PGBuilds[shaOf('9')]; ok {
		t.Fatal("Clone shares the build table")
	}
	g := newHarness(t)
	for _, e := range h.log {
		if _, err := g.applyRaw(e); err != nil {
			t.Fatal(err)
		}
	}
	a, b := h.f.State(), g.f.State()
	if !reflect.DeepEqual(a, b) {
		t.Fatal("replaying the same log gave another state")
	}
	snapRestore(t, h.f)
}

func TestParsePGVersion(t *testing.T) {
	v, ok := ParsePGVersion("18.10.2")
	if !ok || v != (PGVersion{18, 10, 2}) || v.Short() != "18.10" {
		t.Fatalf("%+v %v", v, ok)
	}
	a, _ := ParsePGVersion("18.9.0")
	if a.Compare(v) != -1 || v.Compare(a) != 1 || a.Compare(a) != 0 {
		t.Fatal("compare")
	}
	r1, _ := ParsePGVersion("18.7.0")
	r2, _ := ParsePGVersion("18.7.1")
	if r1.Compare(r2) != -1 {
		t.Fatal("revision")
	}
}

// Over Raft: the leader refuses an old-protocol joiner once the build
// table exists, records a newer joiner's own build, and leaves the table
// alone while it does not exist.
func TestPGBuildsOverRaft(t *testing.T) {
	h := newClusterHarness(t)
	ctx := ctxT(t)
	acfg := h.cfg("a", RoleData, mac186)
	acfg.Version = "v9.9.9"
	a := h.bootstrap(acfg)

	// No table yet: a joiner's build is not written (an older member
	// could not decode it), and a joiner on another build is refused as
	// before.
	newer := h.cfg("b", RoleData, mac187)
	newer.PGBuilds = map[string]PGBuild{shaOf('2'): testBuilds[shaOf('2')]}
	newer.Timing.JoinAttempts = 1
	if _, err := Join(ctx, newer, h.invite(a)); !errors.Is(err, ErrMixedOff) {
		t.Fatalf("join before the table: %v", err)
	}
	if n := len(a.State().PGBuilds); n != 0 {
		t.Fatalf("table written by a join before it existed: %d", n)
	}
	// An old-protocol joiner is still accepted before the table exists.
	oldJoin := func() error {
		code := h.invite(a)
		inv, err := invite.Decode(code)
		if err != nil {
			t.Fatal(err)
		}
		key, err := pki.GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		csr, err := pki.NewCSR(key, inv.ClusterID, "oldnode1")
		if err != nil {
			t.Fatal(err)
		}
		req := joinRequest{ClusterID: inv.ClusterID, Secret: inv.Secret[:], CSR: csr, Protocol: joinProtocolMixed,
			Member: Member{ID: "oldnode1", Name: "old", Addresses: []string{"127.0.0.1:1"}, Role: RoleWitness, Platform: mac186}}
		tm := FastTiming()
		_, err = joinVia(ctx, inv, req, tm, tm.transport())
		return err
	}

	// The members register their table (what each agent does once every
	// member answers caps "pg_builds").
	if _, err := a.Apply(ctx, &RegisterPGBuilds{Builds: map[string]PGBuild{shaOf('1'): testBuilds[shaOf('1')]}}); err != nil {
		t.Fatal(err)
	}
	if err := oldJoin(); !errors.Is(err, ErrJoinerTooOld) {
		t.Fatalf("old-protocol joiner once the table exists: %v", err)
	}
	// Without a valid invite nothing is written to the table.
	{
		code := h.invite(a)
		inv, err := invite.Decode(code)
		if err != nil {
			t.Fatal(err)
		}
		key, err := pki.GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		csr, err := pki.NewCSR(key, inv.ClusterID, "intruder")
		if err != nil {
			t.Fatal(err)
		}
		bad := inv
		bad.Secret[0] ^= 0xff
		fake := PGBuild{Version: "18.9.0", OS: "darwin", Arch: "arm64"}
		req := joinRequest{ClusterID: inv.ClusterID, Secret: bad.Secret[:], CSR: csr, Protocol: JoinProtocol, PGBuild: &fake,
			Member: Member{ID: "intruder", Name: "intruder", Addresses: []string{"127.0.0.1:1"}, Role: RoleData, Platform: Platform{OS: "darwin", Arch: "arm64", PGBuildSHA: shaOf('8')}}}
		tm := FastTiming()
		if _, err := joinVia(ctx, inv, req, tm, tm.transport()); err == nil {
			t.Fatal("a join with a wrong secret was accepted")
		}
		if _, ok := a.State().PGBuilds[shaOf('8')]; ok {
			t.Fatal("a joiner without a valid invite wrote to the build table")
		}
	}
	// A joiner on a newer build than any member's program knows: the
	// leader records the joiner's own build, then admits it.
	newer.Timing.JoinAttempts = 0
	b := h.track(mustJoin(t, h.invite(a), newer))
	if got := a.State().PGBuilds[shaOf('2')]; got != testBuilds[shaOf('2')] {
		t.Fatalf("joiner's build recorded as %+v", got)
	}
	waitUntil(t, "b sees itself and the table", func() bool {
		st := b.State()
		_, ok := st.Members[b.ID()]
		return ok && len(st.PGBuilds) == 2
	})
	// The old main node a moves forward too; a downgrade of b is refused.
	if _, err := a.Apply(ctx, setPlat(a.ID(), mac187)); err != nil {
		t.Fatalf("a to 18.7: %v", err)
	}
	if _, err := b.Apply(ctx, setPlat(b.ID(), mac186)); !errors.Is(err, ErrPlatformMismatch) {
		t.Fatalf("b back to 18.6 while a runs 18.7: %v", err)
	}
}

// snapRestore checks that a snapshot of f restores to the same state.
func snapRestore(t *testing.T, f *FSM) {
	t.Helper()
	before := f.State()
	g := NewFSM(nil)
	if err := g.Restore(io.NopCloser(bytes.NewReader(snapshotBytes(t, f)))); err != nil {
		t.Fatal(err)
	}
	if after := g.State(); !reflect.DeepEqual(before, after) {
		t.Fatalf("restored state differs:\n%+v\n%+v", before.PGBuilds, after.PGBuilds)
	}
}
