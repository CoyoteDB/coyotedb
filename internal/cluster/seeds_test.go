package cluster

import (
	"context"
	"net"
	"slices"
	"testing"

	"coyotedb.com/coyote/internal/cluster/transport"
)

// Seeds that answer come first; the rest keep the invite's order after
// them.
func TestOrderSeeds(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	dead1, dead2 := deadAddr(t), deadAddr(t)
	live := ln.Addr().String()
	got := orderSeeds(context.Background(), []string{dead1, live, dead2}, transport.Timing{})
	if want := []string{live, dead1, dead2}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if one := orderSeeds(context.Background(), []string{dead1}, transport.Timing{}); !slices.Equal(one, []string{dead1}) {
		t.Fatalf("single seed: %v", one)
	}
}

func deadAddr(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := ln.Addr().String()
	ln.Close()
	return a
}
