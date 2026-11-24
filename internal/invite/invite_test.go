package invite

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"reflect"
	"strings"
	"testing"
	"time"
)

func sample() *Invite {
	inv := &Invite{
		ClusterID: "0123456789abcdef0123456789abcdef",
		Addrs:     []string{"100.64.0.43:7481", "192.168.1.20:7481", "[fd7a:115c:a1e0::1]:7481", "node-1.example.ts.net:7481"},
		Expires:   time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC),
	}
	for i := range inv.Secret {
		inv.Secret[i] = byte(i * 7)
		inv.CAFingerprint[i] = byte(255 - i)
	}
	return inv
}

func TestRoundTrip(t *testing.T) {
	inv := sample()
	code, err := inv.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(code, "coyote-") || strings.ToLower(code) != code {
		t.Fatalf("code %q", code)
	}
	t.Logf("sample code (%d chars): %s", len(code), code)
	got, err := Decode(code)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, inv) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", got, inv)
	}
	// One IPv4 address: the common case stays short.
	inv.Addrs = inv.Addrs[:1]
	code, _ = inv.Encode()
	if len(code) > 170 {
		t.Fatalf("single-address code too long: %d", len(code))
	}
}

func TestPasteTolerance(t *testing.T) {
	inv := sample()
	code, _ := inv.Encode()
	body := strings.TrimPrefix(code, Prefix)
	var wrapped strings.Builder
	for i := 0; i < len(body); i += 20 {
		end := min(i+20, len(body))
		wrapped.WriteString(body[i:end])
		wrapped.WriteString("\r\n  ")
	}
	variants := []string{
		"  " + code + "\n",
		strings.ToUpper(code),
		"\"" + code + "\"",
		Prefix + wrapped.String(),
		Prefix + strings.Join(chunks(body, 5), "-"),
		strings.ReplaceAll(strings.ReplaceAll(code[len(Prefix):], "0", "O"), "1", "l"), // no prefix: must fail
	}
	for i, v := range variants[:5] {
		got, err := Decode(v)
		if err != nil {
			t.Fatalf("variant %d: %v", i, err)
		}
		if !reflect.DeepEqual(got, inv) {
			t.Fatalf("variant %d decoded differently", i)
		}
	}
	if _, err := Decode(variants[5]); !errors.Is(err, ErrNotInvite) {
		t.Fatalf("missing prefix: %v", err)
	}
	// Look-alike letters decode to the digits they resemble.
	alias := Prefix + strings.NewReplacer("0", "O", "1", "I").Replace(body)
	if got, err := Decode(alias); err != nil || !reflect.DeepEqual(got, inv) {
		t.Fatalf("aliases: %v", err)
	}
}

func chunks(s string, n int) []string {
	var out []string
	for len(s) > n {
		out = append(out, s[:n])
		s = s[n:]
	}
	return append(out, s)
}

func TestEverySingleCharCorruptionDetected(t *testing.T) {
	code, _ := sample().Encode()
	body := code[len(Prefix):]
	tried := 0
	for i := 0; i < len(body); i++ {
		orig := decodeMap[body[i]]
		for c := 0; c < 256; c++ {
			v := decodeMap[byte(c)]
			if v == orig || byte(c) == '-' || strings.ContainsRune(" \t\r\n\"'`<>", rune(c)) {
				continue // same value (alias/case) or a character Normalize strips
			}
			bad := Prefix + body[:i] + string([]byte{byte(c)}) + body[i+1:]
			tried++
			if _, err := Decode(bad); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("pos %d byte %#x: want ErrCorrupt, got %v", i, c, err)
			}
		}
	}
	if tried < len(body)*31 {
		t.Fatalf("only tried %d corruptions", tried)
	}
	// Truncation at every length, dropping and doubling a character.
	for n := 0; n < len(body); n++ {
		if _, err := Decode(Prefix + body[:n]); err == nil {
			t.Fatalf("truncated to %d chars accepted", n)
		}
	}
	for i := 0; i < len(body); i++ {
		if _, err := Decode(Prefix + body[:i] + body[i+1:]); err == nil {
			t.Fatalf("deleted char %d accepted", i)
		}
		if _, err := Decode(Prefix + body[:i] + body[i:i+1] + body[i:]); err == nil {
			t.Fatalf("doubled char %d accepted", i)
		}
	}
	// Prefix typos.
	if _, err := Decode("hwol-" + body); !errors.Is(err, ErrNotInvite) {
		t.Fatalf("prefix typo: %v", err)
	}
	if _, err := Decode(""); !errors.Is(err, ErrNotInvite) {
		t.Fatalf("empty: %v", err)
	}
}

func TestExpiry(t *testing.T) {
	inv := sample()
	if err := inv.Check(inv.Expires.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := inv.Check(inv.Expires); !errors.Is(err, ErrExpired) {
		t.Fatalf("at expiry: %v", err)
	}
	if err := inv.Check(inv.Expires.Add(time.Hour)); !errors.Is(err, ErrExpired) {
		t.Fatalf("after expiry: %v", err)
	}
}

func TestFutureVersion(t *testing.T) {
	inv := sample()
	code, _ := inv.Encode()
	b, err := decode32(code[len(Prefix):])
	if err != nil {
		t.Fatal(err)
	}
	b = b[:len(b)-4]
	b[0] = 9
	b = appendCRC(b)
	if _, err := Decode(Prefix + encode32(b)); !errors.Is(err, ErrVersion) {
		t.Fatalf("want ErrVersion, got %v", err)
	}
}

func TestEncodeValidation(t *testing.T) {
	inv := sample()
	inv.Addrs = nil
	if _, err := inv.Encode(); err == nil {
		t.Fatal("no addresses accepted")
	}
	inv = sample()
	inv.ClusterID = "xyz"
	if _, err := inv.Encode(); err == nil {
		t.Fatal("bad cluster id accepted")
	}
	inv = sample()
	inv.Addrs = []string{"no-port"}
	if _, err := inv.Encode(); err == nil {
		t.Fatal("address without port accepted")
	}
}

func TestBase32(t *testing.T) {
	for n := 0; n < 40; n++ {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(i*37 + n)
		}
		got, err := decode32(encode32(b))
		if err != nil || string(got) != string(b) {
			t.Fatalf("n=%d: %v", n, err)
		}
	}
}

func appendCRC(b []byte) []byte {
	return binary.BigEndian.AppendUint32(b, crc32.ChecksumIEEE(b))
}
