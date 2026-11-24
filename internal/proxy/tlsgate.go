package proxy

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// The TLS gate (Config.RequireTLS). The proxy still never parses or logs
// what a client sends after its first packet: it only looks at the very
// first protocol message, which says whether the client is about to start
// TLS, and passes everything (including that message) through unchanged.
//
// Allowed openings:
//   - SSLRequest (then the TLS handshake with Postgres, end to end);
//   - a TLS ClientHello straight away (sslnegotiation=direct, PG 17+);
//   - CancelRequest (carries no credentials, only a random key).
//
// A GSSENCRequest is answered "N" (not supported) here, which every client
// takes as "try the next method": it then sends SSLRequest or a plain
// startup. Anything else (a plain StartupMessage) gets a FATAL error that
// says what to change, and is closed: no password ever crosses the network
// in the clear, and Postgres' pg_hba never sees such a connection.

const (
	codeSSLRequest    = 80877103
	codeGSSENCRequest = 80877104
	codeCancelRequest = 80877102
	tlsHandshakeByte  = 0x16
	gateTimeout       = 10 * time.Second
)

var errPlaintext = errors.New("client did not start TLS")

// gate inspects the opening of client and returns a conn that replays it.
func gate(client net.Conn) (net.Conn, error) {
	_ = client.SetReadDeadline(time.Now().Add(gateTimeout))
	defer client.SetReadDeadline(time.Time{})
	for range 2 { // at most one GSSENCRequest before the real opening
		var first [1]byte
		if _, err := io.ReadFull(client, first[:]); err != nil {
			return nil, fmt.Errorf("read client opening: %w", err)
		}
		if first[0] == tlsHandshakeByte {
			return &replayConn{Conn: client, r: io.MultiReader(bytes.NewReader(first[:]), client)}, nil
		}
		var rest [7]byte
		if _, err := io.ReadFull(client, rest[:]); err != nil {
			return nil, fmt.Errorf("read client opening: %w", err)
		}
		head := append(first[:], rest[:]...)
		n := binary.BigEndian.Uint32(head[0:4])
		code := binary.BigEndian.Uint32(head[4:8])
		switch {
		case n == 8 && code == codeSSLRequest:
			return &replayConn{Conn: client, r: io.MultiReader(bytes.NewReader(head), client), sslRequest: true}, nil
		case n == 16 && code == codeCancelRequest:
			return &replayConn{Conn: client, r: io.MultiReader(bytes.NewReader(head), client)}, nil
		case n == 8 && code == codeGSSENCRequest:
			if _, err := client.Write([]byte{'N'}); err != nil {
				return nil, err
			}
			continue
		}
		_ = client.SetWriteDeadline(time.Now().Add(time.Second))
		_, _ = client.Write(fatalMessage("28000",
			"this server accepts only encrypted (TLS) connections: use sslmode=verify-full with the cluster's CA certificate (see `coyote conninfo`), or at least sslmode=require"))
		return nil, errPlaintext
	}
	return nil, errPlaintext
}

// fatalMessage is a protocol ErrorResponse a client shows as is.
func fatalMessage(sqlstate, msg string) []byte {
	var body bytes.Buffer
	for _, f := range []struct {
		t byte
		v string
	}{{'S', "FATAL"}, {'V', "FATAL"}, {'C', sqlstate}, {'M', msg}} {
		body.WriteByte(f.t)
		body.WriteString(f.v)
		body.WriteByte(0)
	}
	body.WriteByte(0)
	out := make([]byte, 5, 5+body.Len())
	out[0] = 'E'
	binary.BigEndian.PutUint32(out[1:5], uint32(4+body.Len()))
	return append(out, body.Bytes()...)
}

// replayConn yields the inspected opening first, then the client stream.
type replayConn struct {
	net.Conn
	r          io.Reader
	sslRequest bool // the opening was an SSLRequest
}

// errServerNoTLS: Postgres declined TLS (its ssl is off).
var errServerNoTLS = errors.New("proxy: the database server declined TLS; closing the client connection")

// mustTLS passes the upstream through after checking that its first byte,
// the answer to SSLRequest, is 'S'.
type mustTLS struct {
	r       io.Reader
	checked bool
}

func (m *mustTLS) Read(b []byte) (int, error) {
	if m.checked || len(b) == 0 {
		return m.r.Read(b)
	}
	n, err := m.r.Read(b[:1])
	if n == 1 {
		m.checked = true
		if b[0] != 'S' {
			return 0, errServerNoTLS
		}
	}
	return n, err
}

func (c *replayConn) Read(b []byte) (int, error) { return c.r.Read(b) }

// CloseWrite passes a half-close on to the TCP connection.
func (c *replayConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return errors.New("no half-close")
}
