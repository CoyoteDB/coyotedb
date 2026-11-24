package cluster

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"coyotedb.com/coyote/internal/invite"
	"coyotedb.com/coyote/internal/pki"
)

// Mixed-system rule (safety rule 1 extension, docs/MIXED_OS.md, D37):
// table of FSM decisions. The FSM is the authority: the leader's dry run
// and every follower apply the same checks to the same entry.

const (
	fpA = "1111111111111111111111111111111111111111111111111111111111111111"
	fpB = "2222222222222222222222222222222222222222222222222222222222222222"
)

func measured(p Platform, fp, probe string) Platform {
	p.Fingerprint = fp
	p.FingerprintText = "format=1\npg_version=18.7\nicu_collator_version=" + fp[:4] + "\n"
	p.ProbeDigest = probe
	p.ProbeGroups = []string{"float=" + probe[:4], "text=aaaa"}
	return p
}

var (
	macM   = measured(macPlat, fpA, "p1p1p1p1")
	linuxM = measured(linuxPlat, fpA, "p1p1p1p1")
	winM   = measured(Platform{OS: "windows", Arch: "amd64", PGBuildSHA: "cccccccccccccccccccccccc"}, fpA, "p1p1p1p1")
)

// mixedHarness: n1 (macOS, measured, a fresh clean audit at t0).
func mixedHarness(t *testing.T) *fsmHarness {
	h := newHarness(t)
	h.must(&BootstrapState{
		ClusterID: "c1", CA: CAData{CertDER: []byte{1}, KeyDER: []byte{2}},
		Member: member("n1", "mac-1", RoleData, macM), Settings: DefaultSettings(), At: t0,
	})
	h.must(&UpdateMember{ID: "n1", Guard: &Guard{AuditAt: t0}})
	return h
}

func TestMixedJoinAccepted(t *testing.T) {
	h := mixedHarness(t)
	at := t0.Add(5 * time.Minute)
	// Without consent: refused, and nothing changes.
	h.mustFail(&AddMember{Member: member("n2", "lin-1", RoleData, linuxM), At: at}, ErrMixedOff)
	// With consent, equal fingerprint and probe, clean fresh audit: in,
	// and the cluster now allows mixing.
	h.must(&AddMember{Member: member("n2", "lin-1", RoleData, linuxM), At: at, AllowMixedOS: true})
	st := h.f.State()
	if !st.Settings.MixedOS {
		t.Fatal("MixedOS not turned on by the accepted mixed join")
	}
	if got := st.Members["n2"].Platform; !got.Equal(linuxM) {
		t.Fatalf("platform stored %+v", got)
	}
	// Once on, a later mixed joiner needs no flag (it still needs every check).
	h.must(&AddMember{Member: member("n3", "lin-2", RoleData, linuxM), At: at})
	// A machine alike to an existing one also passes every check, because
	// it joins a mixed cluster (it is unlike the Linux nodes).
	h.must(&AddMember{Member: member("n4", "mac-2", RoleData, macM), At: at})
	// Witnesses: any platform, measured or not, as before.
	h.must(&AddMember{Member: member("w", "w", RoleWitness, Platform{OS: "windows", Arch: "arm64"}), At: at})
}

func TestMixedJoinRefusals(t *testing.T) {
	at := t0.Add(time.Minute)
	type tc struct {
		name  string
		setup func(h *fsmHarness)
		join  Platform
		want  error
		text  string
	}
	cases := []tc{
		{name: "joiner not measured", join: linuxPlat, want: ErrPlatformMismatch, text: "not measured"},
		{name: "fingerprint differs", join: measured(linuxPlat, fpB, "p1p1p1p1"), want: ErrPlatformMismatch, text: "icu_collator_version is 2222 here, 1111 there"},
		{name: "probe differs", join: measured(linuxPlat, fpA, "p2p2p2p2"), want: ErrPlatformMismatch, text: "different in: float"},
		{name: "windows joiner", join: winM, want: ErrPlatformMismatch, text: "Windows"},
		{name: "member not measured", join: linuxM, want: ErrMixedNotReady, text: "has not measured",
			setup: func(h *fsmHarness) { h.must(&UpdateMember{ID: "n1", Platform: &macPlat}) }},
		{name: "no audit yet", join: linuxM, want: ErrMixedNotReady, text: "in the last",
			setup: func(h *fsmHarness) { h.must(&UpdateMember{ID: "n1", Guard: &Guard{}}) }},
		{name: "audit too old", join: linuxM, want: ErrMixedNotReady,
			setup: func(h *fsmHarness) {
				h.must(&UpdateMember{ID: "n1", Guard: &Guard{AuditAt: at.Add(-MixedGuardMaxAge - time.Second)}})
			}},
		{name: "audit from a clock far ahead", join: linuxM, want: ErrMixedNotReady,
			setup: func(h *fsmHarness) {
				h.must(&UpdateMember{ID: "n1", Guard: &Guard{AuditAt: at.Add(MixedGuardMaxAge + time.Second)}})
			}},
		{name: "audit blocking", join: linuxM, want: ErrMixedBlocked, text: "libm in index",
			setup: func(h *fsmHarness) {
				h.must(&UpdateMember{ID: "n1", Guard: &Guard{AuditAt: at, AuditBlocking: true, Findings: []string{"libm in index"}}})
			}},
		{name: "damage found", join: linuxM, want: ErrMixedBlocked, text: "damage",
			setup: func(h *fsmHarness) {
				h.must(&UpdateMember{ID: "n1", Guard: &Guard{AuditAt: at, VerifyAt: at, VerifyDamaged: true}})
			}},
		{name: "drifted member", join: linuxM, want: ErrMixedBlocked, text: "system update",
			setup: func(h *fsmHarness) {
				h.must(&UpdateMember{ID: "n1", Guard: &Guard{AuditAt: at, Drift: "changed after a system update"}})
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := mixedHarness(t)
			hash := strings.Repeat("ab", 32)
			h.must(&CreateJoinSecret{Hash: hash, CreatedAt: t0, ExpiresAt: t0.Add(time.Hour)})
			if c.setup != nil {
				c.setup(h)
			}
			cmd := &AddMember{Member: member("n2", "lin-1", RoleData, c.join), SecretHash: hash, At: at, AllowMixedOS: true}
			h.mustFail(cmd, c.want) // also checks: no state change, the secret is not burned
			_, err := h.apply(cmd)
			if c.text != "" && !strings.Contains(err.Error(), c.text) {
				t.Fatalf("message %q lacks %q", err, c.text)
			}
		})
	}
}

// Windows on the existing side refuses too (roll-out stage 1).
func TestMixedJoinWindowsMember(t *testing.T) {
	h := newHarness(t)
	h.must(&BootstrapState{ClusterID: "c1", CA: CAData{CertDER: []byte{1}, KeyDER: []byte{2}},
		Member: member("n1", "win-1", RoleData, winM), Settings: DefaultSettings(), At: t0})
	h.must(&UpdateMember{ID: "n1", Guard: &Guard{AuditAt: t0}})
	h.mustFail(&AddMember{Member: member("n2", "lin-1", RoleData, linuxM), At: t0, AllowMixedOS: true}, ErrPlatformMismatch)
	// Windows with Windows of the same build: rule 1 as before.
	h.must(&AddMember{Member: member("n2", "win-2", RoleData, winM), At: t0})
}

// A fingerprint that changes while the node is a member (an OS or
// program update): the node's own record update is refused when it no
// longer matches a data node of another kind, so the cluster keeps the
// last good record; the node then records the drift, which blocks new
// mixed joins.
func TestMixedFingerprintDriftMidLife(t *testing.T) {
	h := mixedHarness(t)
	h.must(&AddMember{Member: member("n2", "lin-1", RoleData, linuxM), At: t0, AllowMixedOS: true})
	drifted := measured(linuxPlat, fpB, "p1p1p1p1")
	h.mustFail(&UpdateMember{ID: "n2", Platform: &drifted}, ErrPlatformMismatch)
	probeDrift := measured(linuxPlat, fpA, "p9p9p9p9")
	h.mustFail(&UpdateMember{ID: "n2", Platform: &probeDrift}, ErrPlatformMismatch)
	if got := h.f.State().Members["n2"].Platform; !got.Equal(linuxM) {
		t.Fatalf("record changed: %+v", got)
	}
	// The same, re-measured unchanged: accepted (every restart does this).
	same := linuxM
	h.must(&UpdateMember{ID: "n2", Platform: &same})
	h.must(&UpdateMember{ID: "n2", Guard: &Guard{AuditAt: t0, Drift: "this machine's database programs changed (a system update?)"}})
	h.mustFail(&AddMember{Member: member("n3", "lin-2", RoleData, linuxM), At: t0, AllowMixedOS: true}, ErrMixedBlocked)
	// Drift cleared (the node was put back): joins pass again.
	h.must(&UpdateMember{ID: "n2", Guard: &Guard{AuditAt: t0}})
	h.must(&AddMember{Member: member("n3", "lin-2", RoleData, linuxM), At: t0})
}

// Alike machines keep rule 1 exactly as before: a fingerprint difference
// between two machines with the same OS, CPU and Postgres build is not a
// refusal (the build is the same; the guard reports it instead).
func TestAlikeUnchanged(t *testing.T) {
	h := mixedHarness(t)
	other := measured(macPlat, fpB, "p2p2p2p2")
	h.must(&AddMember{Member: member("n2", "mac-2", RoleData, other), At: t0})
	h.must(&AddMember{Member: member("n3", "mac-3", RoleData, macPlat), At: t0}) // unmeasured, alike
	if h.f.State().Settings.MixedOS {
		t.Fatal("alike joins must not turn mixing on")
	}
}

// A join racing a failover: the decision does not depend on which node
// is primary, and a fresh audit from the node that just died still
// counts (it is about the shared catalog, which did not change by the
// promotion).
func TestMixedJoinRacingFailover(t *testing.T) {
	h := mixedHarness(t)
	h.must(&AddMember{Member: member("n2", "mac-2", RoleData, macM), At: t0})
	h.must(&SetSyncSet{ExpectedEpoch: 1, Members: []string{"n2"}, K: 1})
	h.must(&SetStandbyEligible{ExpectedEpoch: 1, Eligible: true, At: t0})
	h.must(&Promote{ExpectedEpoch: 1, NodeID: "n2", LeaseDuration: 10 * time.Second, At: t0})
	if st := h.f.State(); st.PrimaryID != "n2" || st.Epoch != 2 {
		t.Fatalf("promote: %+v", st)
	}
	h.must(&AddMember{Member: member("n3", "lin-1", RoleData, linuxM), At: t0.Add(time.Minute), AllowMixedOS: true})
	// The new primary's audit then finds a problem (a guard failing after
	// failover): from then on mixed joins are refused.
	h.must(&UpdateMember{ID: "n2", Guard: &Guard{AuditAt: t0.Add(2 * time.Minute), AuditBlocking: true, Findings: []string{"x"}}})
	h.mustFail(&AddMember{Member: member("n4", "lin-2", RoleData, linuxM), At: t0.Add(3 * time.Minute)}, ErrMixedBlocked)
}

func TestGuardRecordValidation(t *testing.T) {
	h := mixedHarness(t)
	h.must(&AddMember{Member: member("w", "w", RoleWitness, linuxPlat), At: t0})
	h.mustFail(&UpdateMember{ID: "w", Guard: &Guard{AuditAt: t0}}, ErrInvalid)
	many := make([]string, MaxGuardFindings+1)
	h.mustFail(&UpdateMember{ID: "n1", Guard: &Guard{Findings: many}}, ErrInvalid)
	// Round trip through the strict decoder, every new field set.
	cmd := &UpdateMember{ID: "n1", Platform: &macM, Guard: &Guard{AuditAt: t0, AuditBlocking: true, VerifyAt: t0,
		VerifyDamaged: true, Drift: "d", Findings: []string{"f"}}}
	data, err := EncodeCommand(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeCommand(data); err != nil {
		t.Fatal(err)
	}
	add := &AddMember{Member: member("x", "x", RoleData, linuxM), At: t0, AllowMixedOS: true}
	if data, err = EncodeCommand(add); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeCommand(data); err != nil {
		t.Fatal(err)
	}
	// Clone is deep for the new slices.
	h.must(&UpdateMember{ID: "n1", Guard: &Guard{AuditAt: t0, Findings: []string{"a"}}})
	st := h.f.State()
	st.Members["n1"].Guard.Findings[0] = "changed"
	st.Members["n1"].Platform.ProbeGroups[0] = "changed"
	if again := h.f.State(); again.Members["n1"].Guard.Findings[0] != "a" || again.Members["n1"].Platform.ProbeGroups[0] == "changed" {
		t.Fatal("State() shares guard or probe slices")
	}
}

// Over real Raft and the join RPC: the leader runs MixedJoinCheck before
// admitting a mixed joiner (its error refuses the join), the refusal text
// comes from ExplainPlatform, and an accepted mixed join turns mixing on.
func TestMixedJoinOverRaft(t *testing.T) {
	h := newClusterHarness(t)
	ctx := ctxT(t)
	var checks atomic.Int32
	var failCheck atomic.Bool
	acfg := h.cfg("a", RoleData, macM)
	acfg.ExplainPlatform = func(j, m Platform) []string {
		return []string{"The ICU sort rules version is different (" + j.Fingerprint[:4] + " here, " + m.Fingerprint[:4] + " there)."}
	}
	var oldMember atomic.Bool // a member that does not know the new fields
	acfg.MixedFieldsOK = func(context.Context) bool { return !oldMember.Load() }
	var a *Node
	acfg.MixedJoinCheck = func(ctx context.Context, joiner Member) error {
		checks.Add(1)
		if failCheck.Load() {
			return errors.New("the main node could not be reached to check the database")
		}
		// What the agent does: a data node audits now and records it.
		_, err := a.Apply(ctx, &UpdateMember{ID: a.ID(), Guard: &Guard{AuditAt: time.Now()}})
		return err
	}
	a = h.bootstrap(acfg)

	// Mixing off and no consent: refused before the check runs.
	_, err := Join(ctx, h.cfg("l0", RoleData, linuxM), h.invite(a))
	if !errors.Is(err, ErrMixedOff) || checks.Load() != 0 {
		t.Fatalf("no consent: %v (checks %d)", err, checks.Load())
	}
	// Rolling upgrade: while some member cannot decode the new fields,
	// the leader strips them, so a mixed joiner is refused as unmeasured
	// and nothing an older node cannot read is written.
	oldMember.Store(true)
	up := h.cfg("l9", RoleData, linuxM)
	up.AllowMixedOS = true
	_, err = Join(ctx, up, h.invite(a))
	if !errors.Is(err, ErrMixedOff) {
		t.Fatalf("old member present: %v", err)
	}
	alike := h.track(mustJoin(t, h.invite(a), h.cfg("m2", RoleData, macM)))
	if p := a.State().Members[alike.ID()].Platform; p.Fingerprint != "" || p.ProbeDigest != "" {
		t.Fatalf("new fields written while an old member is present: %+v", p)
	}
	oldMember.Store(false)
	// After the upgrade, the alike member announces its measurement (the
	// agent does this after its next Postgres start); until then a mixed
	// join is "not ready" because m2 is unmeasured, checked below.
	pendingM2 := func() {
		if _, err := alike.Apply(ctx, &UpdateMember{ID: alike.ID(), Platform: &macM}); err != nil {
			t.Fatal(err)
		}
	}
	// Fingerprint differs: explained in plain words.
	bad := h.cfg("l1", RoleData, measured(linuxPlat, fpB, "p1p1p1p1"))
	bad.AllowMixedOS = true
	_, err = Join(ctx, bad, h.invite(a))
	if !errors.Is(err, ErrPlatformMismatch) || !strings.Contains(err.Error(), "ICU sort rules version is different (2222 here, 1111 there)") {
		t.Fatalf("fingerprint refusal: %v", err)
	}
	pendingM2()
	// The check fails (no data node can audit): not ready, the invite is kept.
	failCheck.Store(true)
	good := h.cfg("l2", RoleData, linuxM)
	good.AllowMixedOS = true
	good.Timing.JoinAttempts = 1
	code := h.invite(a)
	_, err = Join(ctx, good, code)
	if !errors.Is(err, ErrMixedNotReady) || !strings.Contains(err.Error(), "could not be reached") {
		t.Fatalf("check failed: %v", err)
	}
	// Resumed with the same code once the check passes.
	failCheck.Store(false)
	l2, err := Join(ctx, good, code)
	if err != nil {
		t.Fatalf("mixed join: %v", err)
	}
	h.track(l2)
	waitUntil(t, "mixing on everywhere", func() bool {
		return a.State().Settings.MixedOS && l2.State().Settings.MixedOS
	})
	if g := a.State().Members[a.ID()].Guard; g == nil || g.AuditAt.IsZero() {
		t.Fatalf("audit not recorded: %+v", g)
	}
}

// A joiner whose program predates the mixed-system fields (no protocol in
// its request) is refused with a plain message before anything is
// written: its strict state machine would halt on those fields and leave a
// dead member. The cluster stays healthy and the invite stays usable.
func TestOldProtocolJoinerRefused(t *testing.T) {
	h := newClusterHarness(t)
	ctx := ctxT(t)
	acfg := h.cfg("a", RoleData, macM)
	acfg.Version = "v9.9.9"
	acfg.MixedFieldsOK = func(context.Context) bool { return true }
	a := h.bootstrap(acfg)
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
	old := joinRequest{ClusterID: inv.ClusterID, Secret: inv.Secret[:], CSR: csr,
		Member: Member{ID: "oldnode1", Name: "old", Addresses: []string{"127.0.0.1:1"}, Role: RoleData, Platform: macPlat}}
	tm := FastTiming()
	_, err = joinVia(ctx, inv, old, tm, tm.transport())
	if !errors.Is(err, ErrJoinerTooOld) || !strings.Contains(err.Error(), "Update coyote on this machine to v9.9.9") {
		t.Fatalf("old joiner: %v", err)
	}
	st := a.State()
	if len(st.Members) != 1 {
		t.Fatalf("members after refusal: %d", len(st.Members))
	}
	if _, err := a.Apply(ctx, &UpdateMember{ID: a.ID(), Guard: &Guard{AuditAt: time.Now()}}); err != nil {
		t.Fatalf("cluster not healthy after the refusal: %v", err)
	}
	if a.Err() != nil {
		t.Fatalf("leader halted: %v", a.Err())
	}
	// The same invite still works for an up-to-date joiner.
	h.track(mustJoin(t, code, h.cfg("b", RoleData, macM)))
}
