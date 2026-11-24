package pg

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

type tarEntry struct {
	name     string
	typ      byte
	body     string
	linkname string
	mode     int64
}

func makeTarGz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		mode := e.mode
		if mode == 0 {
			mode = 0o644
			if e.typ == tar.TypeDir {
				mode = 0o755
			}
		}
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: mode, Linkname: e.linkname}
		if e.typ == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractGoodArchive(t *testing.T) {
	data := makeTarGz(t, []tarEntry{
		{name: "root/", typ: tar.TypeDir},
		{name: "root/bin/", typ: tar.TypeDir},
		{name: "root/bin/postgres", typ: tar.TypeReg, body: "binary", mode: 0o4755},
		{name: "root/lib/libpq.so.5.18", typ: tar.TypeReg, body: "lib"},
		{name: "root/lib/libpq.so.5", typ: tar.TypeSymlink, linkname: "libpq.so.5.18"},
		{name: "root/share/tz/GMT", typ: tar.TypeReg, body: "tz"},
		{name: "root/share/tz/Zulu", typ: tar.TypeLink, linkname: "root/share/tz/GMT"},
	})
	dir := t.TempDir()
	if err := extractTarGz(bytes.NewReader(data), dir); err != nil {
		t.Fatalf("extract: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "root", "bin", "postgres"))
	if err != nil || string(b) != "binary" {
		t.Fatalf("postgres = %q, %v", b, err)
	}
	fi, _ := os.Stat(filepath.Join(dir, "root", "bin", "postgres"))
	if runtime.GOOS != "windows" && fi.Mode()&os.ModeSetuid != 0 {
		t.Fatal("setuid bit must be dropped")
	}
	b, err = os.ReadFile(filepath.Join(dir, "root", "lib", "libpq.so.5"))
	if err != nil || string(b) != "lib" {
		t.Fatalf("symlink read = %q, %v", b, err)
	}
	b, err = os.ReadFile(filepath.Join(dir, "root", "share", "tz", "Zulu"))
	if err != nil || string(b) != "tz" {
		t.Fatalf("hard link = %q, %v", b, err)
	}
}

func TestExtractRejectsUnsafeEntries(t *testing.T) {
	cases := map[string][]tarEntry{
		"dotdot":            {{name: "root/../../evil", typ: tar.TypeReg, body: "x"}},
		"leading dotdot":    {{name: "../evil", typ: tar.TypeReg, body: "x"}},
		"absolute":          {{name: "/tmp/evil", typ: tar.TypeReg, body: "x"}},
		"backslash":         {{name: `root\..\evil`, typ: tar.TypeReg, body: "x"}},
		"drive letter":      {{name: "C:/evil", typ: tar.TypeReg, body: "x"}},
		"symlink escapes":   {{name: "root/", typ: tar.TypeDir}, {name: "root/link", typ: tar.TypeSymlink, linkname: "../../etc/passwd"}},
		"symlink absolute":  {{name: "root/link", typ: tar.TypeSymlink, linkname: "/etc/passwd"}},
		"symlink to parent": {{name: "root/link", typ: tar.TypeSymlink, linkname: ".."}},
		"write through symlink": {
			{name: "root/sub/", typ: tar.TypeDir},
			{name: "root/a", typ: tar.TypeSymlink, linkname: "sub"},
			{name: "root/a/x", typ: tar.TypeReg, body: "x"},
		},
		"hardlink outside":   {{name: "root/h", typ: tar.TypeLink, linkname: "../outside"}},
		"hardlink missing":   {{name: "root/h", typ: tar.TypeLink, linkname: "root/nope"}},
		"device file":        {{name: "root/dev", typ: tar.TypeChar}},
		"fifo":               {{name: "root/fifo", typ: tar.TypeFifo}},
		"hardlink absolute":  {{name: "root/h", typ: tar.TypeLink, linkname: "/etc/passwd"}},
		"dotdot in the tail": {{name: "root/a/..", typ: tar.TypeDir}},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			dir := filepath.Join(parent, "dest")
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			err := extractTarGz(bytes.NewReader(makeTarGz(t, entries)), dir)
			if !errors.Is(err, errUnsafeArchive) {
				t.Fatalf("want errUnsafeArchive, got %v", err)
			}
			if _, err := os.Lstat(filepath.Join(parent, "evil")); err == nil {
				t.Fatal("file escaped the destination")
			}
		})
	}
}

func TestCleanEntryName(t *testing.T) {
	ok := map[string]string{
		"a/b/c":   "a/b/c",
		"./a/b":   "a/b",
		"a//b/":   "a/b",
		"./":      "",
		"a/./b/c": "a/b/c",
	}
	for in, want := range ok {
		got, err := cleanEntryName(in)
		if err != nil || got != want {
			t.Errorf("cleanEntryName(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "..", "a/../../b", "/a", `a\b`, "c:x", "a\x00b"} {
		if _, err := cleanEntryName(in); err == nil {
			t.Errorf("cleanEntryName(%q) should fail", in)
		}
	}
}
