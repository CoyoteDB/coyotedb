// Package invite encodes and decodes invite codes: the one string a person
// copies from the first machine and pastes on a new one.
//
// A code looks like "coyote-" followed by lowercase Crockford base32. Decoding
// ignores whitespace, line breaks, dashes and case, and maps the look-alike
// letters o→0 and i/l→1. A CRC-32 over the binary payload catches every
// single mistyped character and any truncation, so a bad paste gives a
// friendly error before any network attempt.
//
// Binary layout (version 1, all integers big-endian):
//
//	u8      version (=1)
//	[16]    cluster id (the 32-hex-char id as raw bytes)
//	[32]    join secret
//	[32]    CA fingerprint (sha256 of the CA certificate DER)
//	uvarint expiry, unix seconds
//	u8      address count (1..16)
//	address*:
//	  u8 kind: 4 = IPv4 ([4] ip, u16 port)
//	           6 = IPv6 ([16] ip, u16 port)
//	           1 = hostname (u8 len, [len] ascii, u16 port)
//	[4]     CRC-32 (IEEE) of everything above
package invite

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"coyotedb.com/coyote/internal/brand"
)

// Prefix starts every invite code.
const Prefix = brand.Command + "-"

// Version is the binary format version this package writes.
const Version = 1

// MaxAddrs is the most seed addresses a code can carry.
const MaxAddrs = 16

const (
	kindIPv4 = 4
	kindIPv6 = 6
	kindHost = 1
)

var (
	// ErrNotInvite means the text does not look like an invite code at all.
	ErrNotInvite = errors.New("this is not an invite code (it should start with \"" + Prefix + "\")")
	// ErrCorrupt means the code was mistyped or cut short.
	ErrCorrupt = errors.New("the invite code is incomplete or has a typo; copy the whole code again")
	// ErrExpired means the invite is past its expiry time.
	ErrExpired = errors.New("the invite code has expired; create a new one on a machine that is already in the cluster")
	// ErrVersion means the code was made by a newer version of the program.
	ErrVersion = errors.New("the invite code was made by a newer version; update this machine first")
)

// Invite is the decoded content of an invite code.
type Invite struct {
	ClusterID     string   // 32 lowercase hex chars
	Addrs         []string // seed addresses, host:port, tried in order
	Secret        [32]byte // one-time join secret
	CAFingerprint [32]byte // sha256 of the cluster CA cert DER
	Expires       time.Time
}

// NewSecret returns a fresh random join secret.
func NewSecret() ([32]byte, error) {
	var s [32]byte
	if _, err := rand.Read(s[:]); err != nil {
		return s, fmt.Errorf("random: %w", err)
	}
	return s, nil
}

// Check returns ErrExpired if the invite has expired at now.
func (inv *Invite) Check(now time.Time) error {
	if !now.Before(inv.Expires) {
		return fmt.Errorf("%w (expired %s)", ErrExpired, inv.Expires.UTC().Format(time.RFC3339))
	}
	return nil
}

// Encode returns the invite code string.
func (inv *Invite) Encode() (string, error) {
	cid, err := hex.DecodeString(inv.ClusterID)
	if err != nil || len(cid) != 16 || strings.ToLower(inv.ClusterID) != inv.ClusterID {
		return "", fmt.Errorf("cluster id %q must be 32 lowercase hex chars", inv.ClusterID)
	}
	if len(inv.Addrs) == 0 || len(inv.Addrs) > MaxAddrs {
		return "", fmt.Errorf("need 1..%d seed addresses, got %d", MaxAddrs, len(inv.Addrs))
	}
	if inv.Expires.Unix() <= 0 {
		return "", errors.New("expiry not set")
	}
	b := make([]byte, 0, 128)
	b = append(b, Version)
	b = append(b, cid...)
	b = append(b, inv.Secret[:]...)
	b = append(b, inv.CAFingerprint[:]...)
	b = binary.AppendUvarint(b, uint64(inv.Expires.Unix()))
	b = append(b, byte(len(inv.Addrs)))
	for _, a := range inv.Addrs {
		b, err = appendAddr(b, a)
		if err != nil {
			return "", err
		}
	}
	b = binary.BigEndian.AppendUint32(b, crc32.ChecksumIEEE(b))
	return Prefix + encode32(b), nil
}

func appendAddr(b []byte, addr string) ([]byte, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("address %q: %w", addr, err)
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil || port == 0 {
		return nil, fmt.Errorf("address %q: bad port", addr)
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if ip.Zone() != "" {
			return nil, fmt.Errorf("address %q: zoned IPv6 not supported", addr)
		}
		if ip.Is4() || ip.Is4In6() {
			a := ip.Unmap().As4()
			b = append(b, kindIPv4)
			b = append(b, a[:]...)
		} else {
			a := ip.As16()
			b = append(b, kindIPv6)
			b = append(b, a[:]...)
		}
	} else {
		if err := validHost(host); err != nil {
			return nil, fmt.Errorf("address %q: %w", addr, err)
		}
		b = append(b, kindHost, byte(len(host)))
		b = append(b, host...)
	}
	return binary.BigEndian.AppendUint16(b, uint16(port)), nil
}

func validHost(h string) error {
	if len(h) == 0 || len(h) > 253 {
		return errors.New("host name length must be 1..253")
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_') {
			return fmt.Errorf("host name has invalid character %q", c)
		}
	}
	return nil
}

// Normalize strips what pasting tends to add: whitespace, line breaks,
// surrounding quotes, and case. It does not validate.
func Normalize(s string) string {
	var sb strings.Builder
	for _, r := range s {
		switch r {
		case ' ', '\t', '\r', '\n', ' ', '"', '\'', '`', '<', '>':
			continue
		}
		sb.WriteRune(r)
	}
	return strings.ToLower(sb.String())
}

// Decode parses an invite code. It does not check expiry (see Check).
func Decode(code string) (*Invite, error) {
	s := Normalize(code)
	body, ok := strings.CutPrefix(s, Prefix)
	if !ok {
		return nil, ErrNotInvite
	}
	body = strings.ReplaceAll(body, "-", "")
	b, err := decode32(body)
	if err != nil {
		return nil, ErrCorrupt
	}
	if len(b) < 5 {
		return nil, ErrCorrupt
	}
	payload, sum := b[:len(b)-4], binary.BigEndian.Uint32(b[len(b)-4:])
	if crc32.ChecksumIEEE(payload) != sum {
		return nil, ErrCorrupt
	}
	if payload[0] != Version {
		return nil, fmt.Errorf("%w (format %d)", ErrVersion, payload[0])
	}
	r := reader{b: payload[1:]}
	inv := &Invite{}
	inv.ClusterID = hex.EncodeToString(r.take(16))
	copy(inv.Secret[:], r.take(32))
	copy(inv.CAFingerprint[:], r.take(32))
	exp := r.uvarint()
	if exp > 1<<40 {
		r.fail()
	}
	inv.Expires = time.Unix(int64(exp), 0).UTC()
	n := int(r.u8())
	if n == 0 || n > MaxAddrs {
		r.fail()
	}
	for i := 0; i < n && !r.bad; i++ {
		var host string
		switch r.u8() {
		case kindIPv4:
			a, _ := netip.AddrFromSlice(r.take(4))
			host = a.String()
		case kindIPv6:
			a, _ := netip.AddrFromSlice(r.take(16))
			host = a.String()
		case kindHost:
			l := int(r.u8())
			host = string(r.take(l))
			if validHost(host) != nil {
				r.fail()
			}
		default:
			r.fail()
		}
		port := r.u16()
		inv.Addrs = append(inv.Addrs, net.JoinHostPort(host, strconv.Itoa(int(port))))
	}
	if r.bad || len(r.b) != 0 {
		// The CRC matched, so this is a malformed code from a buggy
		// encoder rather than a typo; still report it plainly.
		return nil, ErrCorrupt
	}
	return inv, nil
}

type reader struct {
	b   []byte
	bad bool
}

func (r *reader) fail() { r.bad = true }

func (r *reader) take(n int) []byte {
	if r.bad || len(r.b) < n {
		r.bad = true
		return make([]byte, n)
	}
	v := r.b[:n]
	r.b = r.b[n:]
	return v
}

func (r *reader) u8() byte { return r.take(1)[0] }

func (r *reader) u16() uint16 { return binary.BigEndian.Uint16(r.take(2)) }

func (r *reader) uvarint() uint64 {
	if r.bad {
		return 0
	}
	v, n := binary.Uvarint(r.b)
	if n <= 0 {
		r.bad = true
		return 0
	}
	r.b = r.b[n:]
	return v
}

// ---- Crockford base32 ----

const alphabet = "0123456789abcdefghjkmnpqrstvwxyz"

var decodeMap = func() [256]int8 {
	var m [256]int8
	for i := range m {
		m[i] = -1
	}
	for i := 0; i < len(alphabet); i++ {
		c := alphabet[i]
		m[c] = int8(i)
		if c >= 'a' && c <= 'z' {
			m[c-'a'+'A'] = int8(i)
		}
	}
	m['o'], m['O'] = 0, 0
	m['i'], m['I'], m['l'], m['L'] = 1, 1, 1, 1
	return m
}()

func encode32(b []byte) string {
	var sb strings.Builder
	var acc uint32
	bits := 0
	for _, c := range b {
		acc = acc<<8 | uint32(c)
		bits += 8
		for bits >= 5 {
			bits -= 5
			sb.WriteByte(alphabet[(acc>>bits)&31])
		}
	}
	if bits > 0 {
		sb.WriteByte(alphabet[(acc<<(5-bits))&31])
	}
	return sb.String()
}

func decode32(s string) ([]byte, error) {
	out := make([]byte, 0, len(s)*5/8)
	var acc uint32
	bits := 0
	for i := 0; i < len(s); i++ {
		v := decodeMap[s[i]]
		if v < 0 {
			return nil, fmt.Errorf("invalid character %q", s[i])
		}
		acc = acc<<5 | uint32(v)
		bits += 5
		if bits >= 8 {
			bits -= 8
			out = append(out, byte(acc>>bits))
		}
	}
	// Leftover bits are padding: there must be fewer than 5 and they must be
	// zero, otherwise the length or the last character is wrong.
	if bits >= 5 || acc&(1<<bits-1) != 0 {
		return nil, errors.New("bad length or padding")
	}
	return out, nil
}
