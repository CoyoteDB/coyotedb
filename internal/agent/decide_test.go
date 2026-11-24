package agent

import "testing"

func TestDecideTable(t *testing.T) {
	base := observed{Self: "a", Bootstrapped: true, Member: true, Primary: "a", Epoch: 3, HasSecrets: true, Initialized: true}
	with := func(f func(*observed)) observed { o := base; f(&o); return o }
	cases := []struct {
		name string
		o    observed
		want actKind
	}{
		{"no state yet", with(func(o *observed) { o.Bootstrapped = false }), actWait},
		{"not a member yet: confirm first", with(func(o *observed) { o.Member = false; o.Confirmed = false }), actConfirm},
		{"removed (confirmed)", with(func(o *observed) { o.Member = false; o.Confirmed = true }), actRemoved},
		{"removed (seen before)", with(func(o *observed) { o.Member = false; o.Confirmed = false; o.SeenMember = true }), actRemoved},

		// Fresh cluster.
		{"fresh primary needs confirm", with(func(o *observed) { o.Initialized = false; o.FreshPrimary = true }), actConfirm},
		{"fresh primary confirmed", with(func(o *observed) { o.Initialized = false; o.FreshPrimary = true; o.Confirmed = true }), actInitPrimary},
		{"primary lost its data: never initdb again", with(func(o *observed) { o.Initialized = false; o.Confirmed = true }), actWait},
		{"joiner clones", with(func(o *observed) { o.Initialized = false; o.Primary = "b" }), actClone},
		{"joiner waits for secrets", with(func(o *observed) { o.Initialized = false; o.Primary = "b"; o.HasSecrets = false }), actWait},
		{"joiner waits for a primary", with(func(o *observed) { o.Initialized = false; o.Primary = "" }), actWait},

		// Markers (rule 6).
		{"interrupted first clone retried", with(func(o *observed) { o.Initialized = false; o.Primary = "b"; o.Marker = "cloning" }), actClone},
		{"demoted marker rejoins", with(func(o *observed) { o.Primary = "b"; o.Marker = "demoted" }), actRejoin},
		{"interrupted rewind rejoins", with(func(o *observed) { o.Primary = "b"; o.Marker = "rewinding" }), actRejoin},
		{"marker but named primary: never start", with(func(o *observed) { o.Marker = "demoted"; o.Confirmed = true; o.LastPrimaryEpoch = 3 }), actWait},
		{"marker and no primary", with(func(o *observed) { o.Primary = ""; o.Marker = "demoted" }), actWait},

		// Primary data, stopped.
		{"primary data needs confirm", with(func(o *observed) { o.LastPrimaryEpoch = 3 }), actConfirm},
		{"confirmed same epoch starts primary", with(func(o *observed) { o.LastPrimaryEpoch = 3; o.Confirmed = true }), actStartPrimary},
		{"epoch moved on while primary: fence", with(func(o *observed) { o.LastPrimaryEpoch = 2; o.Confirmed = true }), actFence},
		{"forced recovery survivor starts", with(func(o *observed) { o.LastPrimaryEpoch = 2; o.Confirmed = true; o.ForcedSelf = true }), actStartPrimary},

		// D56: a promotion of this node cut short after Postgres removed
		// standby.signal: start as a standby again, never as a primary.
		{"promotion cut short needs confirm", with(func(o *observed) { o.LastPrimaryEpoch, o.PromotingEpoch = 3, 3 }), actConfirm},
		{"promotion cut short: resume as standby", with(func(o *observed) { o.LastPrimaryEpoch, o.PromotingEpoch = 3, 3; o.Confirmed = true }), actResume},
		{"promotion cut short, someone else is primary: rejoin", with(func(o *observed) {
			o.Primary = "b"
			o.LastPrimaryEpoch, o.PromotingEpoch, o.Confirmed = 3, 3, true
		}), actRejoin},
		{"promotion cut short, then the epoch moved on: fence", with(func(o *observed) {
			o.Epoch = 4
			o.LastPrimaryEpoch, o.PromotingEpoch, o.Confirmed = 3, 3, true
		}), actFence},
		{"promotion cut short before standby.signal was removed: standby", with(func(o *observed) {
			o.StandbyDir = true
			o.LastPrimaryEpoch, o.PromotingEpoch = 3, 3
		}), actStartStandby},
		{"resumed standby promotes again", with(func(o *observed) {
			o.PGRunning, o.PGInRecovery = true, true
			o.LastPrimaryEpoch, o.PromotingEpoch, o.Confirmed = 3, 3, true
		}), actPromote},
		{"someone else is primary: rejoin", with(func(o *observed) { o.Primary = "b"; o.LastPrimaryEpoch = 2; o.Confirmed = true }), actRejoin},
		{"no primary: stay stopped", with(func(o *observed) { o.Primary = ""; o.Confirmed = true }), actWait},

		// Standby data, stopped: always safe to start read-only.
		{"standby data starts as standby", with(func(o *observed) { o.Primary = "b"; o.StandbyDir = true }), actStartStandby},
		{"standby data named primary: start as standby first", with(func(o *observed) { o.StandbyDir = true }), actStartStandby},

		// Running standby.
		{"standby steady", with(func(o *observed) { o.Primary = "b"; o.PGRunning = true; o.PGInRecovery = true }), actNone},
		{"named primary needs confirm", with(func(o *observed) { o.PGRunning = true; o.PGInRecovery = true }), actConfirm},
		{"named primary promotes", with(func(o *observed) { o.PGRunning = true; o.PGInRecovery = true; o.Confirmed = true }), actPromote},
		{"cut during a switch stays cut", with(func(o *observed) {
			o.Primary = "b"
			o.PGRunning, o.PGInRecovery, o.StreamCut, o.CutEpoch = true, true, true, 3
		}), actNone},
		{"cut and epoch moved: repoint", with(func(o *observed) {
			o.Primary = "c"
			o.PGRunning, o.PGInRecovery, o.StreamCut, o.CutEpoch = true, true, true, 2
		}), actRepoint},
		{"cut hold expired with the epoch unchanged: resume", with(func(o *observed) {
			o.Primary = "b"
			o.PGRunning, o.PGInRecovery, o.StreamCut, o.CutEpoch, o.CutExpired = true, true, true, 3, true
		}), actRepoint},

		// Running primary.
		{"primary steady", with(func(o *observed) { o.PGRunning = true; o.LastPrimaryEpoch = 3 }), actNone},
		{"another node became primary: rejoin", with(func(o *observed) { o.PGRunning = true; o.Primary = "b"; o.Epoch = 4; o.LastPrimaryEpoch = 3 }), actRejoin},
		{"stale local view: wait", with(func(o *observed) { o.PGRunning = true; o.Primary = "b"; o.Epoch = 2; o.LastPrimaryEpoch = 3 }), actNone},
		{"primary cleared: fence", with(func(o *observed) { o.PGRunning = true; o.Primary = ""; o.Epoch = 4; o.LastPrimaryEpoch = 3 }), actFence},

		// Rule 4: the lease fence, and recovering in place.
		{"fencing: wait", with(func(o *observed) { o.PGRunning = true; o.Fencing = true }), actWait},
		{"fenced, still primary at that epoch: confirm", with(func(o *observed) {
			o.Marker, o.MarkerFencedEpoch, o.LastPrimaryEpoch = "demoted", 3, 3
		}), actConfirm},
		{"fenced, confirmed still primary at that epoch: clear", with(func(o *observed) {
			o.Marker, o.MarkerFencedEpoch, o.LastPrimaryEpoch, o.Confirmed = "demoted", 3, 3, true
		}), actClearFence},
		{"fenced, epoch moved on: rejoin", with(func(o *observed) {
			o.Primary, o.Epoch, o.Marker, o.MarkerFencedEpoch, o.LastPrimaryEpoch, o.Confirmed = "b", 4, "demoted", 3, 3, true
		}), actRejoin},
		{"fenced at an older epoch but named primary again: never start", with(func(o *observed) {
			o.Epoch, o.Marker, o.MarkerFencedEpoch, o.LastPrimaryEpoch, o.Confirmed = 4, "demoted", 3, 3, true
		}), actWait},

		// Rule 8: needs re-clone.
		{"needs re-clone", with(func(o *observed) { o.Primary = "b"; o.NeedsReclone = true; o.PGRunning = true; o.PGInRecovery = true }), actReclone},
		{"re-cloned: mark active", with(func(o *observed) { o.Primary = "b"; o.NeedsReclone = true; o.Recloned = true }), actMarkActive},
		{"needs re-clone, no primary", with(func(o *observed) { o.Primary = ""; o.NeedsReclone = true }), actWait},
	}
	for _, c := range cases {
		if got := decide(c.o); got.kind != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got.kind, c.want)
		}
	}
}

// Whatever the inputs, primary data that is not running never starts as
// a primary without a confirmed view naming this node at its own epoch.
func TestDecideNeverStartsUnconfirmedPrimary(t *testing.T) {
	for _, primary := range []string{"a", "b", ""} {
		for _, confirmed := range []bool{false, true} {
			for _, last := range []uint64{0, 2, 3} {
				for _, marker := range []string{"", "demoted", "rewinding"} {
					for _, fenced := range []uint64{0, 2, 3} {
						if fenced != 0 && marker != "demoted" {
							continue
						}
						o := observed{Self: "a", Bootstrapped: true, Member: true, Primary: primary, Epoch: 3,
							HasSecrets: true, Initialized: true, Confirmed: confirmed, LastPrimaryEpoch: last, Marker: marker,
							MarkerFencedEpoch: fenced}
						for _, promoting := range []uint64{0, 2, 3} {
							o.PromotingEpoch = promoting
							got := decide(o).kind
							// D56: never as a primary while a promotion at this epoch is unfinished.
							ok := confirmed && primary == "a" && last == 3 && marker == "" && promoting != 3
							if (got == actStartPrimary) != ok {
								t.Errorf("%+v: got %s", o, got)
							}
						}
						o.PromotingEpoch = 0
						got := decide(o).kind
						clear := confirmed && primary == "a" && last == 3 && fenced == 3
						if (got == actClearFence) != clear {
							t.Errorf("%+v: got %s", o, got)
						}
					}
				}
			}
		}
	}
}
