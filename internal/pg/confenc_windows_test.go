//go:build windows

package pg

import (
	"bytes"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// coyote.conf is written in the ANSI code page Postgres reads paths in,
// and a folder name that page cannot hold is refused, never best-fitted.
func TestConfFileUsesANSICodePage(t *testing.T) {
	acp := windows.GetACP()
	b, err := encodeConfFile("ssl_key_file = 'D:/J\u00f6rg/k'\n")
	if err != nil {
		t.Fatal(err)
	}
	switch acp {
	case 1252:
		if !bytes.Contains(b, []byte("J\xf6rg")) {
			t.Fatalf("not code page 1252: %q", b)
		}
		if _, err := encodeConfFile("D:/\u5f20"); err == nil {
			t.Fatal("a character outside code page 1252 was accepted")
		}
		if err := checkPostgresPath("D:\\data\\c\u00f8d\u00e9"); err != nil {
			t.Fatalf("cp1252 letters refused: %v", err)
		}
		if err := checkPostgresPath("D:\\data\\\u5f20\u4e09"); err == nil {
			t.Fatal("CJK folder accepted on code page 1252")
		}
		// No best fit: o-slash must not become a plain "o".
		if _, ok, _ := toANSI("\u0153"); !ok { // oe ligature is in cp1252 (0x9c)
			t.Fatal("oe ligature should be in cp1252")
		}
		if _, ok, _ := toANSI("\u0101"); ok { // a-macron is not in cp1252; best fit would give "a"
			t.Fatal("a-macron best-fitted instead of refused")
		}
	case cpUTF8:
		if !bytes.Contains(b, []byte("J\u00f6rg")) {
			t.Fatalf("UTF-8 system: %q", b)
		}
	default:
		t.Logf("code page %d: only the round trip of ASCII is checked", acp)
	}
	if err := checkPostgresPath(`C:\ProgramData\CoyoteDB`); err != nil {
		t.Fatal(err)
	}
}

// 165 characters worked on a Windows test PC, 180 did not: the cap is 160.
func TestDataDirLengthCap(t *testing.T) {
	ok := `D:\` + strings.Repeat("x", maxDataDirLen-3)
	if err := checkPostgresPath(ok); err != nil {
		t.Fatalf("%d characters refused: %v", len(ok), err)
	}
	if err := checkPostgresPath(ok + "y"); err == nil {
		t.Fatalf("%d characters accepted", len(ok)+1)
	}
}
