package agent

import (
	"errors"
	"fmt"
	"strings"

	"coyotedb.com/coyote/internal/brand"
	"coyotedb.com/coyote/internal/cluster"
	"coyotedb.com/coyote/internal/invite"
	"coyotedb.com/coyote/internal/pki"
)

// friendlyJoinError turns a join failure into a sentence a person can act
// on. The full error goes to the log.
func friendlyJoinError(err error) string {
	switch {
	case errors.Is(err, invite.ErrNotInvite), errors.Is(err, invite.ErrCorrupt), errors.Is(err, invite.ErrVersion):
		return "That invite code does not work: " + baseMessage(err) + "."
	case errors.Is(err, invite.ErrExpired), errors.Is(err, cluster.ErrSecretExpired):
		return fmt.Sprintf("That invite code has expired. On a machine that is already in the cluster, run `%s invite` and use the new code.", brand.Command)
	case errors.Is(err, cluster.ErrSecretUsed):
		return fmt.Sprintf("That invite code was already used by another machine. Each code works once: run `%s invite` on a machine in the cluster for a new one.", brand.Command)
	case errors.Is(err, cluster.ErrSecretUnknown):
		return fmt.Sprintf("The cluster does not recognise that invite code (it may have been cancelled). Run `%s invite` on a machine in the cluster for a new one.", brand.Command)
	case errors.Is(err, cluster.ErrJoinerTooOld):
		return upperFirst(clusterDetail(err)) + "."
	case errors.Is(err, cluster.ErrMixedOff):
		return fmt.Sprintf("This machine is a different kind of computer (operating system or processor) than the cluster's data nodes (%s). "+
			"Mixing kinds of computers is new and off unless you ask for it: run the same command again with --allow-mixed-os. "+
			"The cluster then checks that this machine reads and writes the data exactly like the others, and refuses it if not. "+
			"It can always help the cluster as a witness (a voter that keeps no data): `%s witness` with an invite code. %s",
			clusterDetail(err), brand.Command, linuxRecommended)
	case errors.Is(err, cluster.ErrMixedBlocked):
		return fmt.Sprintf("The cluster cannot take a different kind of computer right now: %s. "+
			"Fix that first (see `%s status` on a machine in the cluster), then run the same command again.", clusterDetail(err), brand.Command)
	case errors.Is(err, cluster.ErrMixedNotReady):
		return fmt.Sprintf("The cluster could not check this machine yet: %s. Progress is saved: wait a minute and run the same command again.", clusterDetail(err))
	case errors.Is(err, cluster.ErrPlatformMismatch):
		return fmt.Sprintf("This machine cannot hold a copy of the data: %s. "+
			"Copies need database programs that store and compare data exactly like the other data nodes'. "+
			"It can still help the cluster as a witness (a voter that keeps no data): `%s witness` with an invite code.", clusterDetail(err), brand.Command)
	case errors.Is(err, cluster.ErrDuplicateName):
		return "Another node in the cluster already has this name. Choose a different one with --name."
	case errors.Is(err, pki.ErrFingerprint):
		return "The machine that answered is not the cluster this invite was made for, so nothing was sent to it. Check the address, or create a new invite."
	}
	var ce *cluster.Error
	if errors.As(err, &ce) {
		return fmt.Sprintf("The cluster did not let this machine join: %s. Fix that and run the same command again; the details are in this node's log.", clusterDetail(err))
	}
	return fmt.Sprintf("Could not reach the cluster. Is the machine that made the invite running and reachable from here? "+
		"Progress is saved: run the same command again to continue. (%v)", err)
}

// clusterDetail is the detail of a cluster error (the part after its
// sentinel's own words), or the innermost message.
func clusterDetail(err error) string {
	var ce *cluster.Error
	if errors.As(err, &ce) {
		for _, s := range []*cluster.Error{cluster.ErrJoinerTooOld, cluster.ErrMixedOff, cluster.ErrMixedBlocked, cluster.ErrMixedNotReady, cluster.ErrPlatformMismatch} {
			if d, ok := strings.CutPrefix(ce.Msg, s.Msg+": "); ok {
				return trimDot(d)
			}
		}
		return trimDot(ce.Msg)
	}
	return baseMessage(err)
}

// baseMessage is the innermost error's message, without its wrappers.
func baseMessage(err error) string {
	for {
		u := errors.Unwrap(err)
		if u == nil {
			return trimDot(err.Error())
		}
		err = u
	}
}

func trimDot(s string) string {
	if n := len(s); n > 0 && s[n-1] == '.' {
		return s[:n-1]
	}
	return s
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
