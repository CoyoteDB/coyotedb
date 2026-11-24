package pg

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeExtension builds an extension archive for b with the given extra
// entries (relative to the archive's top folder) and returns the
// extension, the folder holding the archive, and the archive bytes.
func fakeExtension(t *testing.T, b Build, version string, extra ...tarEntry) (Extension, string, []byte) {
	t.Helper()
	e := Extension{Project: "fakevec", Name: "fakevec", Version: version, Build: b}
	top := e.ArchiveName() + "/"
	entries := []tarEntry{
		{name: top, typ: tar.TypeDir},
		{name: top + "build-info.json", typ: tar.TypeReg, body: "{}"},
		{name: top + "lib/", typ: tar.TypeDir},
		{name: top + "lib/fakevec" + libSuffix(b.Target), typ: tar.TypeReg, body: "library " + version, mode: 0o755},
		{name: top + "share/", typ: tar.TypeDir},
		{name: top + "share/extension/", typ: tar.TypeDir},
		{name: top + "share/extension/fakevec.control", typ: tar.TypeReg, body: "default_version = '" + version + "'"},
		{name: top + "share/extension/fakevec--" + version + ".sql", typ: tar.TypeReg, body: "CREATE TYPE fakevec;"},
		{name: top + "share/extension/fakevec--0.1--" + version + ".sql", typ: tar.TypeReg, body: "-- upgrade"},
		{name: top + "dependency-notices/fakevec/LICENSE", typ: tar.TypeReg, body: "licence"},
	}
	for _, x := range extra {
		x.name = top + x.name
		entries = append(entries, x)
	}
	data := makeTarGz(t, entries)
	sum := sha256.Sum256(data)
	e.SHA256 = hex.EncodeToString(sum[:])
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, e.FileName()), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return e, dir, data
}

func extFile(inst Install, rel string) string {
	return filepath.Join(inst.Root, filepath.FromSlash(rel))
}

func ensureExt(t *testing.T, cache, arch string, b Build, e Extension, from string, o FetchOptions) Install {
	t.Helper()
	o.CacheDir, o.Build, o.Archive, o.ExtensionsFrom, o.extensions = cache, b, arch, from, []Extension{e}
	inst, err := Ensure(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

func TestExtensionInstalledFromFolder(t *testing.T) {
	b, cache, arch, _ := installed(t)
	e, dir, _ := fakeExtension(t, b, "1.0")
	inst := ensureExt(t, cache, arch, b, e, dir, FetchOptions{})
	if err := CheckExtension(inst, e, true); err != nil {
		t.Fatalf("after install: %v", err)
	}
	for _, rel := range []string{e.Library(), e.Control(), "share/extension/fakevec--1.0.sql", "share/extension/fakevec--0.1--1.0.sql", "dependency-notices/fakevec/LICENSE"} {
		if _, err := os.Stat(extFile(inst, rel)); err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
	}
	if _, err := os.Stat(extFile(inst, "build-info.json")); err == nil {
		t.Fatal("build-info.json must not be installed")
	}
	if err := CheckInstall(inst, true); err != nil {
		t.Fatalf("the PostgreSQL install must still check out: %v", err)
	}
	// Reused without the archive.
	ensureExt(t, cache, arch, b, e, t.TempDir(), FetchOptions{})
	if err := CheckExtension(inst, e, true); err != nil {
		t.Fatal(err)
	}
	if left, _ := filepath.Glob(filepath.Join(cache, ".extract-*")); len(left) != 0 {
		t.Fatalf("leftover %v", left)
	}
}

func TestExtensionFromHTTP(t *testing.T) {
	b, cache, arch, _ := installed(t)
	e, _, data := fakeExtension(t, b, "1.0")
	var asked string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Path
		if r.URL.Path != "/builds/"+e.FileName() {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
	}))
	defer srv.Close()
	inst := ensureExt(t, cache, arch, b, e, srv.URL+"/builds/", FetchOptions{Client: srv.Client()})
	if err := CheckExtension(inst, e, true); err != nil {
		t.Fatalf("after install (server asked for %s): %v", asked, err)
	}
	if left, _ := filepath.Glob(filepath.Join(cache, "*.partial")); len(left) != 0 {
		t.Fatalf("download left behind: %v", left)
	}
}

func TestExtensionTamperedArchiveNotInstalled(t *testing.T) {
	b, cache, arch, _ := installed(t)
	e, dir, data := fakeExtension(t, b, "1.0")
	data[len(data)-5] ^= 0xff
	if err := os.WriteFile(filepath.Join(dir, e.FileName()), data, 0o644); err != nil {
		t.Fatal(err)
	}
	var said []string
	inst := ensureExt(t, cache, arch, b, e, dir, FetchOptions{Status: func(m string) { said = append(said, m) }})
	err := EnsureExtensions(context.Background(), FetchOptions{ExtensionsFrom: dir, extensions: []Extension{e}}, inst)
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("want checksum mismatch, got %v", err)
	}
	if _, err := os.Stat(extFile(inst, e.Library())); err == nil {
		t.Fatal("tampered archive was installed")
	}
	if len(said) != 1 || !strings.Contains(said[0], "runs without it") {
		t.Fatalf("status %q", said)
	}
}

func TestExtensionNotConfigured(t *testing.T) {
	b, cache, arch, _ := installed(t)
	e, _, _ := fakeExtension(t, b, "1.0")
	t.Setenv(ExtensionsFromEnv, "")
	inst := ensureExt(t, cache, arch, b, e, "", FetchOptions{}) // Ensure itself does not fail
	err := EnsureExtensions(context.Background(), FetchOptions{extensions: []Extension{e}}, inst)
	if !errors.Is(err, ErrExtensionsNotConfigured) {
		t.Fatalf("got %v", err)
	}
	// The environment variable is the default location.
	e2, dir, _ := fakeExtension(t, b, "1.0")
	t.Setenv(ExtensionsFromEnv, dir)
	ensureExt(t, cache, arch, b, e2, "", FetchOptions{})
	if err := CheckExtension(inst, e2, true); err != nil {
		t.Fatalf("from %s: %v", ExtensionsFromEnv, err)
	}
	e3, _, _ := fakeExtension(t, b, "3.0")
	if err := EnsureExtensions(context.Background(), FetchOptions{ExtensionsFrom: "ftp://x/", extensions: []Extension{e3}}, inst); err == nil || !strings.Contains(err.Error(), "only a folder") {
		t.Fatalf("ftp location: %v", err)
	}
}

func TestExtensionArchiveMayOnlyAddItsOwnFiles(t *testing.T) {
	b, cache, arch, _ := installed(t)
	exe := "postgres"
	if strings.Contains(b.Target, "windows") {
		exe += ".exe"
	}
	for _, bad := range []tarEntry{
		{name: "bin/" + exe, typ: tar.TypeReg, body: "evil"}, // a PostgreSQL file
		{name: "bin/other", typ: tar.TypeReg, body: "x"},
		{name: "lib/other" + libSuffix(b.Target), typ: tar.TypeReg, body: "x"},
		{name: "lib/sub/fakevec.so", typ: tar.TypeReg, body: "x"},
		{name: "share/extension/other.control", typ: tar.TypeReg, body: "x"},
		{name: "share/extension/fakevec--9.txt", typ: tar.TypeReg, body: "x"},
		{name: "share/extension/.hidden", typ: tar.TypeReg, body: "x"},
		{name: ".coyote-verified", typ: tar.TypeReg, body: "x"},
		{name: "lib/fakevec2.so", typ: tar.TypeSymlink, linkname: "fakevec" + libSuffix(b.Target)},
	} {
		t.Run(bad.name, func(t *testing.T) {
			e, dir, _ := fakeExtension(t, b, "1.0", bad)
			inst := ensureExt(t, cache, arch, b, e, dir, FetchOptions{})
			err := EnsureExtensions(context.Background(), FetchOptions{ExtensionsFrom: dir, extensions: []Extension{e}}, inst)
			if err == nil {
				t.Fatal("accepted")
			}
			if _, err := os.Stat(extFile(inst, e.Control())); err == nil {
				t.Fatal("something was installed")
			}
			if err := CheckInstall(inst, true); err != nil {
				t.Fatalf("PostgreSQL files touched: %v", err)
			}
		})
	}
	// An archive without its library or control file.
	e := Extension{Project: "fakevec", Name: "fakevec", Version: "1.0", Build: b}
	data := makeTarGz(t, []tarEntry{{name: e.ArchiveName() + "/", typ: tar.TypeDir},
		{name: e.ArchiveName() + "/share/extension/fakevec.control", typ: tar.TypeReg, body: "x"}})
	sum := sha256.Sum256(data)
	e.SHA256 = hex.EncodeToString(sum[:])
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, e.FileName()), data, 0o644); err != nil {
		t.Fatal(err)
	}
	inst := ensureExt(t, cache, arch, b, e, dir, FetchOptions{})
	if err := EnsureExtensions(context.Background(), FetchOptions{ExtensionsFrom: dir, extensions: []Extension{e}}, inst); err == nil {
		t.Fatal("archive without a library accepted")
	}
}

func TestExtensionDamagedReinstalledOrRemoved(t *testing.T) {
	b, cache, arch, _ := installed(t)
	e, dir, _ := fakeExtension(t, b, "1.0")
	inst := ensureExt(t, cache, arch, b, e, dir, FetchOptions{})

	// Cut short: the size check sees it; with the archive at hand it is
	// installed again.
	if err := os.Truncate(extFile(inst, e.Library()), 1); err != nil {
		t.Fatal(err)
	}
	if err := CheckExtension(inst, e, false); err == nil || !strings.Contains(err.Error(), "cut short") {
		t.Fatalf("size check: %v", err)
	}
	ensureExt(t, cache, arch, b, e, dir, FetchOptions{})
	if err := CheckExtension(inst, e, true); err != nil {
		t.Fatalf("not healed: %v", err)
	}

	// Same size, other bytes: only the full check sees it.
	data, _ := os.ReadFile(extFile(inst, e.Library()))
	data[0] ^= 0xff
	if err := os.WriteFile(extFile(inst, e.Library()), data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CheckExtension(inst, e, false); err != nil {
		t.Fatalf("size check should pass: %v", err)
	}
	ensureExt(t, cache, arch, b, e, dir, FetchOptions{Full: true})
	if err := CheckExtension(inst, e, true); err != nil {
		t.Fatalf("not healed by the full check: %v", err)
	}

	// Damaged with no copy to be had: removed, control file included,
	// so Postgres no longer offers it; the PostgreSQL files stay.
	t.Setenv(ExtensionsFromEnv, "")
	if err := os.Truncate(extFile(inst, e.Library()), 1); err != nil {
		t.Fatal(err)
	}
	var said []string
	ensureExt(t, cache, arch, b, e, "", FetchOptions{Status: func(m string) { said = append(said, m) }})
	for _, rel := range []string{e.Control(), e.Library(), "share/extension/fakevec--1.0.sql", e.marker()} {
		if _, err := os.Stat(extFile(inst, rel)); err == nil {
			t.Fatalf("%s left after removing a damaged extension", rel)
		}
	}
	if len(said) == 0 || !strings.Contains(said[0], "damaged") {
		t.Fatalf("status %q", said)
	}
	if err := CheckInstall(inst, true); err != nil {
		t.Fatal(err)
	}
}

// coyote upgraded to a newer pinned build that is not available here: the
// older one, intact, stays (databases may use it); with the archive it is
// replaced.
func TestExtensionOlderVersionKeptUntilAvailable(t *testing.T) {
	b, cache, arch, _ := installed(t)
	old, oldDir, _ := fakeExtension(t, b, "1.0")
	inst := ensureExt(t, cache, arch, b, old, oldDir, FetchOptions{})
	e, dir, _ := fakeExtension(t, b, "2.0")
	t.Setenv(ExtensionsFromEnv, "")
	ensureExt(t, cache, arch, b, e, "", FetchOptions{})
	if err := CheckExtension(inst, old, true); err != nil {
		t.Fatalf("older build not kept: %v", err)
	}
	ensureExt(t, cache, arch, b, e, dir, FetchOptions{})
	if err := CheckExtension(inst, e, true); err != nil {
		t.Fatalf("not upgraded: %v", err)
	}
	if b, _ := os.ReadFile(extFile(inst, e.Library())); string(b) != "library 2.0" {
		t.Fatalf("library %q", b)
	}
}

const extKillEnv = "COYOTE_TEST_EXT_KILL"

// TestExtensionKillHelper is the child of TestExtensionKilledAtEachStep:
// it installs the extension and dies (no cleanup) at the named step.
func TestExtensionKillHelper(t *testing.T) {
	spec := os.Getenv(extKillEnv)
	if spec == "" {
		t.Skip("helper process only")
	}
	f := strings.Split(spec, "|")
	b := Build{Version: "0.0.1", Target: "test-target", SHA256: f[3]}
	e := Extension{Project: "fakevec", Name: "fakevec", Version: f[5], Build: b, SHA256: f[4]}
	_, _ = Ensure(context.Background(), FetchOptions{CacheDir: f[1], Archive: f[2], Build: b, ExtensionsFrom: f[6],
		extensions: []Extension{e}, fault: func(p string) {
			if p == f[0] {
				os.Exit(3) // like kill -9
			}
		}})
	os.Exit(4)
}

// The process dies at every step of an extension install, over an
// installed older version: afterwards there is never a marker for files
// that are not all there, Postgres never sees a control file without its
// library, and the next start completes the install.
func TestExtensionKilledAtEachStep(t *testing.T) {
	for _, point := range []string{"ext-staged", "ext-first-moved", "ext-moved", "ext-marked"} {
		t.Run(point, func(t *testing.T) {
			b, cache, arch, _ := installed(t)
			old, oldDir, _ := fakeExtension(t, b, "1.0")
			inst := ensureExt(t, cache, arch, b, old, oldDir, FetchOptions{})
			e, dir, _ := fakeExtension(t, b, "2.0")
			cmd := exec.Command(os.Args[0], "-test.run=^TestExtensionKillHelper$")
			cmd.Env = append(os.Environ(), extKillEnv+"="+strings.Join([]string{point, cache, arch, b.SHA256, e.SHA256, e.Version, dir}, "|"))
			err := cmd.Run()
			if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 3 {
				t.Fatalf("helper did not die at %s: %v", point, err)
			}
			check := CheckExtension(inst, e, true)
			if point == "ext-marked" && check != nil {
				t.Fatalf("marked but not complete: %v", check)
			}
			if point != "ext-marked" && check == nil {
				t.Fatalf("killed at %s, yet the new build checks out", point)
			}
			if _, err := os.Stat(extFile(inst, e.marker())); err == nil && point != "ext-staged" && point != "ext-marked" {
				t.Fatalf("killed at %s with files half moved, yet a marker is there", point)
			}
			if _, err := os.Stat(extFile(inst, e.Control())); err == nil {
				if _, err := os.Stat(extFile(inst, e.Library())); err != nil {
					t.Fatal("control file without its library")
				}
			}
			ensureExt(t, cache, arch, b, e, dir, FetchOptions{})
			if err := CheckExtension(inst, e, true); err != nil {
				t.Fatalf("next start did not complete it: %v", err)
			}
			if err := CheckInstall(inst, true); err != nil {
				t.Fatal(err)
			}
			// After the marker nothing needs installing, so the dead
			// process's (emptied) folder waits for the next install.
			if left, _ := filepath.Glob(filepath.Join(cache, ".extract-*")); len(left) != 0 && point != "ext-marked" {
				t.Fatalf("leftover %v", left)
			}
		})
	}
}

// A PostgreSQL install found damaged is replaced as a whole; the
// extension, which lived inside it, is installed again right after.
func TestExtensionReinstalledWithPostgres(t *testing.T) {
	b, cache, arch, inst := installed(t)
	e, dir, _ := fakeExtension(t, b, "1.0")
	ensureExt(t, cache, arch, b, e, dir, FetchOptions{})
	if err := os.Truncate(inst.Bin("postgres"), 1); err != nil {
		t.Fatal(err)
	}
	ensureExt(t, cache, arch, b, e, dir, FetchOptions{})
	if err := CheckInstall(inst, true); err != nil {
		t.Fatal(err)
	}
	if err := CheckExtension(inst, e, true); err != nil {
		t.Fatal(err)
	}
}

func TestExtensionPins(t *testing.T) {
	for _, p := range pinnedExtensions {
		for target, sum := range p.sha256 {
			if _, ok := pinnedSHA256[target]; !ok {
				t.Errorf("%s: pinned for unknown target %s", p.project, target)
			}
			if len(sum) != 64 || strings.Trim(sum, "0123456789abcdef") != "" {
				t.Errorf("%s %s: bad sha256 %q", p.project, target, sum)
			}
		}
	}
	for target := range pinnedSHA256 {
		b := Build{Version: Version, Target: target}
		if got := ExtensionsFor(b); len(got) != len(pinnedExtensions) {
			t.Errorf("%s: %d extensions pinned, want %d", target, len(got), len(pinnedExtensions))
		}
	}
	if got := ExtensionsFor(Build{Version: "0.0.1", Target: "x86_64-unknown-linux-gnu"}); got != nil {
		t.Errorf("extensions offered for another PostgreSQL version: %v", got)
	}
}
