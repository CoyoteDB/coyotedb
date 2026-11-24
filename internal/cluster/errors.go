package cluster

import (
	"errors"
	"fmt"
)

// Error is a cluster error with a stable code. Codes cross the wire (a
// command forwarded to the leader returns the same sentinel to the caller),
// so match with errors.Is against the sentinels below.
type Error struct {
	Code string
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

// Is matches by code, so a decoded remote error matches its sentinel.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}

func newErr(code, msg string) *Error { return &Error{Code: code, Msg: msg} }

// Sentinel errors returned by commands and lifecycle calls.
var (
	ErrEpochMismatch       = newErr("epoch_mismatch", "epoch changed: someone else acted first")
	ErrNotBootstrapped     = newErr("not_bootstrapped", "cluster is not bootstrapped")
	ErrAlreadyBootstrapped = newErr("already_bootstrapped", "cluster is already bootstrapped")
	ErrUnknownMember       = newErr("unknown_member", "no such member")
	ErrDuplicateMember     = newErr("duplicate_member", "a member with this id already exists")
	ErrDuplicateName       = newErr("duplicate_name", "a member with this name already exists")
	ErrPlatformMismatch    = newErr("platform_mismatch", "this machine's platform does not match the cluster's data nodes")
	ErrMixedOff            = newErr("mixed_os_off", "this machine is a different kind than the cluster's data nodes, and mixing is not turned on")
	ErrMixedNotReady       = newErr("mixed_os_not_ready", "the cluster cannot check a different kind of machine right now")
	ErrMixedBlocked        = newErr("mixed_os_blocked", "the cluster's checks forbid adding a different kind of machine right now")
	ErrJoinerTooOld        = newErr("joiner_too_old", "this machine runs an older version than the cluster")
	ErrSecretUnknown       = newErr("secret_unknown", "the invite code is not valid for this cluster")
	ErrSecretUsed          = newErr("secret_used", "the invite code was already used; create a new one")
	ErrSecretExpired       = newErr("secret_expired", "the invite code has expired; create a new one")
	ErrNotPrimary          = newErr("not_primary", "node is not the current primary")
	ErrNotEligible         = newErr("not_eligible", "node may not become primary")
	ErrIsPrimary           = newErr("is_primary", "member is the current primary; move the primary first")
	ErrInSyncSet           = newErr("in_sync_set", "member is in the sync set; remove it from the sync set first")
	ErrRemoving            = newErr("member_removing", "member is being removed from the cluster; it cannot join the sync set")
	ErrInvalid             = newErr("invalid", "invalid command")
	ErrUnknownCommand      = newErr("unknown_command", "unknown command type or version")
	ErrHalted              = newErr("fsm_halted", "state machine halted after an unknown command; upgrade this node")
	ErrNotLeader           = newErr("not_leader", "this node is not the leader")
	ErrNoLeader            = newErr("no_leader", "the cluster has no leader right now")
	ErrNotConfirmed        = newErr("not_confirmed", "confirmation phrase does not match")
	ErrLastVoter           = newErr("last_voter", "cannot remove the only voting member")
	ErrLeaseRenewed        = newErr("lease_renewed", "the primary renewed its lease")
	ErrRemoved             = newErr("removed", "this node is no longer a cluster member (the others refuse its certificate)")
	ErrStorage             = newErr("storage_failed", "this node's disk refused a write that Raft needs; Raft on this node stopped")
)

var sentinels = func() map[string]*Error {
	m := map[string]*Error{}
	for _, e := range []*Error{
		ErrEpochMismatch, ErrNotBootstrapped, ErrAlreadyBootstrapped, ErrUnknownMember,
		ErrDuplicateMember, ErrDuplicateName, ErrPlatformMismatch, ErrSecretUnknown,
		ErrSecretUsed, ErrSecretExpired, ErrNotPrimary, ErrNotEligible, ErrIsPrimary,
		ErrInSyncSet, ErrInvalid, ErrUnknownCommand, ErrHalted, ErrNotLeader, ErrNoLeader,
		ErrNotConfirmed, ErrLastVoter, ErrLeaseRenewed, ErrRemoved,
		ErrMixedOff, ErrMixedNotReady, ErrMixedBlocked, ErrJoinerTooOld, ErrStorage, ErrRemoving,
	} {
		m[e.Code] = e
	}
	return m
}()

// errorf wraps a sentinel with detail; errors.Is(result, sentinel) holds.
func errorf(sentinel *Error, format string, args ...any) error {
	return &Error{Code: sentinel.Code, Msg: sentinel.Msg + ": " + fmt.Sprintf(format, args...)}
}

// wireError is how errors travel in RPC responses.
type wireError struct {
	Code string `json:"code,omitempty"`
	Msg  string `json:"msg"`
}

func toWire(err error) *wireError {
	if err == nil {
		return nil
	}
	var ce *Error
	if errors.As(err, &ce) {
		return &wireError{Code: ce.Code, Msg: err.Error()}
	}
	return &wireError{Msg: err.Error()}
}

func fromWire(w *wireError) error {
	if w == nil {
		return nil
	}
	if _, ok := sentinels[w.Code]; ok {
		return &Error{Code: w.Code, Msg: w.Msg}
	}
	return errors.New(w.Msg)
}
