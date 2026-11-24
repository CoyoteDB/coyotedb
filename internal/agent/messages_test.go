package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"coyotedb.com/coyote/internal/cluster"
	"coyotedb.com/coyote/internal/invite"
)

func TestFriendlyJoinError(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("join: %w", cluster.ErrPlatformMismatch), "`coyote witness` with an invite code"},
		{fmt.Errorf("join: %w", cluster.ErrMixedOff), "--allow-mixed-os"},
		{cluster.ErrMixedOff, "Linux is the recommended system"},
		{cluster.ErrMixedBlocked, "coyote status"},
		{cluster.ErrMixedNotReady, "run the same command again"},
		{cluster.ErrSecretUsed, "already used"},
		{fmt.Errorf("x: %w", invite.ErrExpired), "expired"},
		{cluster.ErrSecretExpired, "expired"},
		{invite.ErrCorrupt, "typo"},
		{cluster.ErrDuplicateName, "--name"},
		{errors.New("could not reach the cluster after 6 attempts: via 10.0.0.1:7481: connection refused"), "run the same command again"},
	} {
		if got := friendlyJoinError(tc.err); !strings.Contains(got, tc.want) {
			t.Errorf("%v: %q lacks %q", tc.err, got, tc.want)
		}
	}
}

// A specific listen host is advertised as is (nil: the cluster layer uses
// the listener); an unspecified one is detected.
func TestAdvertiseForSpecificHost(t *testing.T) {
	for _, l := range []string{"127.0.0.1:7481", "10.0.0.5:7481", "[fd00::1]:7481"} {
		adv, rep, err := advertiseFor(context.Background(), l)
		if err != nil || adv != nil || rep != nil {
			t.Fatalf("%s: %v %v %v", l, adv, rep, err)
		}
	}
	if _, _, err := advertiseFor(context.Background(), "nonsense"); err == nil {
		t.Fatal("bad listen address accepted")
	}
}

func TestSyncNamesFor(t *testing.T) {
	st := cluster.State{
		Members: map[string]cluster.Member{
			"a": {ID: "a", Name: "node-a"}, "b": {ID: "b", Name: "node-b"}, "c": {ID: "c", Name: "node-c"},
		},
		Sync: cluster.SyncSet{Members: []string{"c", "a", "b"}, K: 5},
	}
	names, k := syncNamesFor(st, "a")
	if !slices.Equal(names, []string{"node-b", "node-c"}) || k != 2 {
		t.Fatalf("got %v %d", names, k)
	}
	st.Sync = cluster.SyncSet{Members: []string{"a"}, K: 1}
	if names, k := syncNamesFor(st, "a"); names != nil || k != 0 {
		t.Fatalf("only self in R: got %v %d", names, k)
	}
}
