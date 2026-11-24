package proxy

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func packet(code uint32, extra int) []byte {
	b := make([]byte, 8+extra)
	binary.BigEndian.PutUint32(b[0:4], uint32(8+extra))
	binary.BigEndian.PutUint32(b[4:8], code)
	return b
}

// startup is a plain StartupMessage (protocol 3.0) for user u.
func startup(u string) []byte {
	body := []byte("user\x00" + u + "\x00\x00")
	return append(packet(196608, 0)[:8:8], body...)
}

func gated(t *testing.T) *harness { return gatedTo(t, "A") }

// gatedTo: the backend's greeting is name + "\n", so a backend called "S"
// answers an SSLRequest the way a Postgres with ssl=on does.
func gatedTo(t *testing.T, name string) *harness {
	a := newBackend(t, name)
	h := start(t, newRouter(1, name), newNet(a), newVerifier(), func(c *Config) { c.RequireTLS = true })
	h.r.keepContact(t, 100*time.Millisecond)
	return h
}

// An SSLRequest (then anything) is passed through byte for byte: the
// backend sees exactly what the client sent, the opening included.
func TestGateSSLRequestPassesThrough(t *testing.T) {
	for name, open := range map[string][]byte{
		"ssl request": packet(codeSSLRequest, 0),
		"direct tls":  {0x16, 0x03, 0x01, 0x00, 0x05, 1, 2, 3, 4, 5},
		"cancel":      packet(codeCancelRequest, 8),
	} {
		t.Run(name, func(t *testing.T) {
			h := gatedTo(t, "S")
			c, err := net.Dial("tcp", h.addr())
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if _, err := c.Write(open); err != nil {
				t.Fatal(err)
			}
			_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
			if line, err := readLine(c); err != nil || line != "S" {
				t.Fatalf("greeting %q %v", line, err)
			}
			echo := make([]byte, len(open))
			if _, err := io.ReadFull(c, echo); err != nil || !bytes.Equal(echo, open) {
				t.Fatalf("backend got %x, want %x (%v)", echo, open, err)
			}
		})
	}
}

// A plain startup never reaches the backend: the client gets a FATAL
// ErrorResponse naming TLS, and is closed.
func TestGatePlainStartupRefused(t *testing.T) {
	h := gated(t)
	c, err := net.Dial("tcp", h.addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write(startup("postgres")); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	b, _ := io.ReadAll(c)
	if len(b) < 6 || b[0] != 'E' || !strings.Contains(string(b), "28000") || !strings.Contains(string(b), "TLS") {
		t.Fatalf("got %q", b)
	}
	if n := int(binary.BigEndian.Uint32(b[1:5])); n != len(b)-1 {
		t.Fatalf("length %d for %d bytes", n, len(b))
	}
	// The only backend connections are the proxy's own primary checks.
	eventually(t, 2*time.Second, "no client bytes upstream", func() bool {
		return h.net.backends["A"].n.Load() == int64(h.v.count("A"))
	})
	eventually(t, 2*time.Second, "counted", func() bool { return h.p.Stats().RefusedPlain == 1 })
}

// A Postgres that declines TLS ('N': ssl off) never gets to talk to a
// client that asked for it: the proxy closes the session instead of
// passing the 'N' on (the client could go on in plaintext).
func TestGateServerWithoutTLSClosed(t *testing.T) {
	h := gatedTo(t, "N")
	c, err := net.Dial("tcp", h.addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write(packet(codeSSLRequest, 0)); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if b, _ := io.ReadAll(c); len(b) != 0 {
		t.Fatalf("client got %q from a server without TLS", b)
	}
}

// GSSENCRequest is declined with 'N'; the client may then ask for TLS.
func TestGateGSSThenSSL(t *testing.T) {
	h := gatedTo(t, "S")
	c, err := net.Dial("tcp", h.addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write(packet(codeGSSENCRequest, 0)); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	var n [1]byte
	if _, err := io.ReadFull(c, n[:]); err != nil || n[0] != 'N' {
		t.Fatalf("want N, got %q %v", n, err)
	}
	ssl := packet(codeSSLRequest, 0)
	if _, err := c.Write(ssl); err != nil {
		t.Fatal(err)
	}
	if line, err := readLine(c); err != nil || line != "S" {
		t.Fatalf("greeting %q %v", line, err)
	}
	echo := make([]byte, 8)
	if _, err := io.ReadFull(c, echo); err != nil || !bytes.Equal(echo, ssl) {
		t.Fatalf("backend got %x", echo)
	}
}

// A second GSSENCRequest, or plain after GSS, is refused.
func TestGateGSSThenPlainRefused(t *testing.T) {
	h := gated(t)
	c, err := net.Dial("tcp", h.addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = c.Write(packet(codeGSSENCRequest, 0))
	_, _ = c.Write(startup("app"))
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	b, _ := io.ReadAll(c)
	if len(b) < 2 || b[0] != 'N' || b[1] != 'E' {
		t.Fatalf("got %q", b)
	}
}
