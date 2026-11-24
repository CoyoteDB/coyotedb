package cluster

import (
	"reflect"
	"testing"
	"time"
)

func TestPromoteIsStrictCAS(t *testing.T) {
	h := bootstrapped(t)
	h.must(&AddMember{Member: member("n2", "two", RoleData, macPlat), At: t0})
	h.must(&AddMember{Member: member("n3", "three", RoleData, macPlat), At: t0})
	h.must(&AddMember{Member: member("w", "w", RoleWitness, linuxPlat), At: t0})
	lease := 6 * time.Second
	seq := func(v uint64) *uint64 { return &v }

	// LastServed follows renewals.
	h.must(&RenewLease{NodeID: "n1", Epoch: 1})
	if st := h.f.State(); st.LastServed != "" {
		t.Fatal("a renewal without Served must not record LastServed")
	}
	h.must(&RenewLease{NodeID: "n1", Epoch: 1, Served: true})
	if st := h.f.State(); st.LastServed != "n1" || st.Lease.Seq != 2 {
		t.Fatalf("after renew: served %q seq %d", st.LastServed, st.Lease.Seq)
	}

	// Empty R, or not eligible: refused.
	h.mustFail(&Promote{ExpectedEpoch: 1, NodeID: "n2", LeaseDuration: lease}, ErrNotEligible)
	h.must(&SetSyncSet{ExpectedEpoch: 1, Members: []string{"n2", "n3"}, K: 1})
	h.mustFail(&Promote{ExpectedEpoch: 1, NodeID: "n2", LeaseDuration: lease}, ErrNotEligible)
	h.must(&SetStandbyEligible{ExpectedEpoch: 1, Eligible: true, At: t0})

	// Witness, unknown, already primary, not in R, needs re-clone.
	h.mustFail(&Promote{ExpectedEpoch: 1, NodeID: "w", LeaseDuration: lease}, ErrNotEligible)
	h.mustFail(&Promote{ExpectedEpoch: 1, NodeID: "ghost", LeaseDuration: lease}, ErrUnknownMember)
	h.mustFail(&Promote{ExpectedEpoch: 1, NodeID: "n1", LeaseDuration: lease}, ErrInvalid)
	h.must(&SetSyncSet{ExpectedEpoch: 1, Members: []string{"n1", "n2", "n3"}, K: 1})
	h.mustFail(&Promote{ExpectedEpoch: 1, NodeID: "n1", LeaseDuration: lease}, ErrInvalid)
	recl := StatusNeedsReclone
	h.must(&UpdateMember{ID: "n3", Status: &recl})
	h.mustFail(&Promote{ExpectedEpoch: 1, NodeID: "n3", LeaseDuration: lease}, ErrNotEligible)

	// The lease must be the one that expired.
	h.mustFail(&Promote{ExpectedEpoch: 1, ExpectedLeaseSeq: seq(1), NodeID: "n2", LeaseDuration: lease}, ErrLeaseRenewed)
	h.mustFail(&Promote{ExpectedEpoch: 0, ExpectedLeaseSeq: seq(2), NodeID: "n2", LeaseDuration: lease}, ErrEpochMismatch)
	h.mustFail(&Promote{ExpectedEpoch: 1, ExpectedLeaseSeq: seq(2), NodeID: "n2"}, ErrInvalid)

	r := h.must(&Promote{ExpectedEpoch: 1, ExpectedLeaseSeq: seq(2), NodeID: "n2", LeaseDuration: lease, AddToSync: []string{"n1"}})
	st := h.f.State()
	if r.Epoch != 2 || st.PrimaryID != "n2" || st.Lease != (Lease{Holder: "n2", Epoch: 2, Duration: lease}) {
		t.Fatalf("after promote: epoch %d primary %s lease %+v", r.Epoch, st.PrimaryID, st.Lease)
	}
	if !reflect.DeepEqual(st.Sync.Members, []string{"n1", "n2", "n3"}) || st.Sync.Epoch != 2 || st.Sync.K != 1 {
		t.Fatalf("sync after promote: %+v", st.Sync)
	}
	// n2 has not renewed yet: n1 is still the last node that served.
	if st.LastServed != "n1" {
		t.Fatalf("last served %q", st.LastServed)
	}
	// A deposed controller's late write fails.
	h.mustFail(&Promote{ExpectedEpoch: 1, NodeID: "n2", LeaseDuration: lease}, ErrEpochMismatch)
	h.must(&RenewLease{NodeID: "n2", Epoch: 2})
	if st := h.f.State(); st.LastServed != "n1" {
		t.Fatalf("not yet served: last served %q", st.LastServed)
	}
	h.must(&RenewLease{NodeID: "n2", Epoch: 2, Served: true})
	if st := h.f.State(); st.LastServed != "n2" {
		t.Fatalf("last served %q", st.LastServed)
	}
	h.mustFail(&Promote{ExpectedEpoch: 2, NodeID: "n1", LeaseDuration: lease, AddToSync: []string{"w"}}, ErrInvalid)
	e := uint64(1)
	h.mustFail(&UpdateMember{ExpectedEpoch: &e, ID: "n3", Status: &recl}, ErrEpochMismatch)
	e = 2
	act := StatusActive
	h.must(&UpdateMember{ExpectedEpoch: &e, ID: "n3", Status: &act})
}

func TestSettingsChecks(t *testing.T) {
	h := bootstrapped(t)
	s := DefaultSettings()
	s.LeaseRenewInterval = 100 * time.Millisecond
	s.LeaseDuration = time.Second
	h.mustFail(&SetSettings{Settings: s}, ErrInvalid)
	s = DefaultSettings()
	s.GoneAfter = -time.Second
	h.mustFail(&SetSettings{Settings: s}, ErrInvalid)
	if (Settings{}).EffectiveGoneAfter() != DefaultGoneAfter {
		t.Fatal("gone_after default")
	}

	if err := CheckLeaseTiming(DefaultSettings(), DefaultTiming()); err != nil {
		t.Fatalf("defaults must pass: %v", err)
	}
	short := DefaultSettings()
	short.LeaseDuration, short.LeaseRenewInterval = 3*time.Second, time.Second
	if err := CheckLeaseTiming(short, DefaultTiming()); err == nil {
		t.Fatal("a 3s lease cannot cover a 2s election at half lease")
	}
	tight := DefaultTiming()
	tight.LeaderLeaseTimeout = DefaultSettings().LeaseDuration - 500*time.Millisecond
	if err := CheckLeaseTiming(DefaultSettings(), tight); err == nil {
		t.Fatal("lease must exceed the raft leader lease plus margin")
	}
}

// OneCopy: the target has no copy to wait for. In one entry it becomes
// primary, eligibility is recorded false (with the time) and R is cleared.
func TestPromoteOneCopy(t *testing.T) {
	h := bootstrapped(t)
	h.must(&AddMember{Member: member("n2", "two", RoleData, macPlat), At: t0})
	h.must(&AddMember{Member: member("w", "w", RoleWitness, linuxPlat), At: t0})
	h.must(&SetSyncSet{ExpectedEpoch: 1, Members: []string{"n2"}, K: 1})
	h.must(&SetStandbyEligible{ExpectedEpoch: 1, Eligible: true, At: t0})
	lease := 6 * time.Second
	at := t0.Add(time.Minute)

	h.mustFail(&Promote{ExpectedEpoch: 1, NodeID: "n2", LeaseDuration: lease, OneCopy: true}, ErrInvalid)
	h.mustFail(&Promote{ExpectedEpoch: 1, NodeID: "n2", LeaseDuration: lease, OneCopy: true, At: at, AddToSync: []string{"n1"}}, ErrInvalid)
	h.must(&Promote{ExpectedEpoch: 1, NodeID: "n2", LeaseDuration: lease, OneCopy: true, At: at})
	st := h.f.State()
	if st.Epoch != 2 || st.PrimaryID != "n2" || st.StandbyEligible || !st.StandbyEligibleSince.Equal(at) ||
		len(st.Sync.Members) != 0 || st.Sync.K != 0 || st.Lease.Holder != "n2" || st.Lease.Epoch != 2 {
		t.Fatalf("after one-copy promote: %+v", st)
	}
	// Not eligible: no further failover until a copy is eligible again.
	h.mustFail(&Promote{ExpectedEpoch: 2, NodeID: "n1", LeaseDuration: lease, OneCopy: true, At: at}, ErrNotEligible)
}
