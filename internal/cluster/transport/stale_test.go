package transport

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"coyotedb.com/coyote/internal/pki/pkitest"
)

// A connection dialed to an address the peer no longer has is closed
// (D62); one to an address it still has is kept.
func TestDropStaleClosesConnectionsToOldAddress(t *testing.T) {
	ca := pkitest.CA(t)
	m := newMembers()
	a := start(t, pkitest.Node(t, ca, "a"), m)
	b := start(t, pkitest.Node(t, ca, "b"), m)
	a.Handle(StreamRPC, echoHandler)
	c, err := b.Dial(context.Background(), "a", StreamRPC)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if n := b.DropStale(); n != 0 {
		t.Fatalf("dropped %d while the address is current", n)
	}
	m.set("a", true, "127.0.0.1:1", a.Addr().String()) // still listed: kept
	if n := b.DropStale(); n != 0 {
		t.Fatalf("dropped %d while the address is still listed", n)
	}
	m.set("a", true, "127.0.0.1:1") // a moved
	if n := b.DropStale(); n != 1 {
		t.Fatalf("dropped %d, want 1", n)
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("connection to the old address still open")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("connection not closed (timed out instead)")
	}
	// Inbound connections (dialed by the other side) are never touched.
	if n := a.DropStale(); n != 0 {
		t.Fatalf("inbound side dropped %d", n)
	}
}

// A connection whose local address this computer no longer has is dead
// (its packets go nowhere and TCP only gives up after many minutes), so
// it is closed, on both the dialing and the accepting side (D62).
func TestDropLocalGoneClosesConnectionsOnLostAddress(t *testing.T) {
	ca := pkitest.CA(t)
	m := newMembers()
	a := start(t, pkitest.Node(t, ca, "a"), m)
	b := start(t, pkitest.Node(t, ca, "b"), m)
	a.Handle(StreamRPC, echoHandler)
	c, err := b.Dial(context.Background(), "a", StreamRPC)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	all := func(netip.Addr) bool { return true }
	if n := b.DropLocalGone(all); n != 0 {
		t.Fatalf("dropped %d while every address is present", n)
	}
	if n := a.DropLocalGone(all); n != 0 {
		t.Fatalf("accepting side dropped %d while every address is present", n)
	}
	gone := func(ip netip.Addr) bool { return ip != netip.MustParseAddr("127.0.0.1") }
	if n := b.DropLocalGone(gone); n != 1 {
		t.Fatalf("dialing side dropped %d, want 1", n)
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("connection on the lost address still open")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("connection not closed (timed out instead)")
	}
	c2, err := b.Dial(context.Background(), "a", StreamRPC)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	// The accepting side tracks the connection once its handshake is
	// done there: retry until it has one to drop.
	deadline := time.Now().Add(5 * time.Second)
	for a.DropLocalGone(gone) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("accepting side never dropped the connection on the lost address")
		}
		time.Sleep(10 * time.Millisecond)
	}
	c2.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c2.Read(make([]byte, 1)); err == nil {
		t.Fatal("connection closed by the accepting side still open")
	}
}
