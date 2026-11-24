package pg

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func ensureFromArchive(t *testing.T, cache, arch string, b Build, o FetchOptions) (Install, error) {
	t.Helper()
	o.CacheDir, o.Build, o.Archive = cache, b, arch
	return Ensure(context.Background(), o)
}

func installed(t *testing.T) (b Build, cache, arch string, inst Install) {
	t.Helper()
	b, data := fakeBuild(t)
	dir := t.TempDir()
	arch = filepath.Join(dir, "pg.tar.gz")
	if err := os.WriteFile(arch, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cache = filepath.Join(dir, "cache")
	inst, err := ensureFromArchive(t, cache, arch, b, FetchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckInstall(inst, true); err != nil {
		t.Fatalf("fresh install: %v", err)
	}
	return b, cache, arch, inst
}

// A program cut short (an install that was never synced, then a power
// cut) is found by the size check every start does, and replaced.
func TestInstallTruncatedBinaryHealed(t *testing.T) {
	b, cache, arch, inst := installed(t)
	if err := os.Truncate(inst.Bin("postgres"), 1); err != nil {
		t.Fatal(err)
	}
	err := CheckInstall(inst, false)
	if err == nil || !strings.Contains(err.Error(), "cut short") {
		t.Fatalf("size check: %v", err)
	}
	var said []string
	inst2, err := ensureFromArchive(t, cache, arch, b, FetchOptions{Status: func(m string) { said = append(said, m) }})
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckInstall(inst2, true); err != nil {
		t.Fatalf("after healing: %v", err)
	}
	if len(said) != 1 || !strings.Contains(said[0], "damaged") {
		t.Fatalf("status %q", said)
	}
}

// Same size, other bytes: only the full check (run after a crash on
// start) sees it, and Full heals it.
func TestInstallCorruptedContentHealedByFullCheck(t *testing.T) {
	b, cache, arch, inst := installed(t)
	data, err := os.ReadFile(inst.Bin("postgres"))
	if err != nil {
		t.Fatal(err)
	}
	data[0] ^= 0xff
	if err := os.WriteFile(inst.Bin("postgres"), data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CheckInstall(inst, false); err != nil {
		t.Fatalf("size check should pass: %v", err)
	}
	if err := CheckInstall(inst, true); err == nil || !strings.Contains(err.Error(), "corrupted") {
		t.Fatalf("full check: %v", err)
	}
	inst2, err := ensureFromArchive(t, cache, arch, b, FetchOptions{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckInstall(inst2, true); err != nil {
		t.Fatalf("after healing: %v", err)
	}
}

func TestInstallCorruptedManifestHealed(t *testing.T) {
	b, cache, arch, inst := installed(t)
	for _, bad := range []string{"", "{", `{"archive_sha256":"` + b.SHA256 + `","files":[]}`, b.SHA256 + "\n",
		`{"archive_sha256":"` + b.SHA256 + `","files":[{"path":"../x","size":1}]}`} {
		if err := os.WriteFile(filepath.Join(inst.Root, verifiedMarker), []byte(bad), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := CheckInstall(inst, false); err == nil {
			t.Fatalf("manifest %q accepted", bad)
		}
		inst2, err := ensureFromArchive(t, cache, arch, b, FetchOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := CheckInstall(inst2, true); err != nil {
			t.Fatalf("after healing %q: %v", bad, err)
		}
	}
}

const unpackKillEnv = "COYOTE_TEST_UNPACK_KILL"

// TestUnpackKillHelper is the child process of TestUnpackKilledAtEachStep:
// it installs and dies (no cleanup runs) at the named step.
func TestUnpackKillHelper(t *testing.T) {
	spec := os.Getenv(unpackKillEnv)
	if spec == "" {
		t.Skip("helper process only")
	}
	f := strings.SplitN(spec, "|", 4)
	b := Build{Version: "0.0.1", Target: "test-target", SHA256: f[3]}
	_, _ = Ensure(context.Background(), FetchOptions{CacheDir: f[1], Archive: f[2], Build: b, fault: func(p string) {
		if p == f[0] {
			os.Exit(3) // like kill -9: no deferred cleanup
		}
	}})
	os.Exit(4) // the step was never reached
}

// The process dies at every step of an install, over a damaged earlier
// one: afterwards the install is either absent/failing its check or fully
// verified, never half there with a marker; the next start heals it.
func TestUnpackKilledAtEachStep(t *testing.T) {
	for _, point := range []string{"extracted", "synced", "marked", "removed-old", "renamed"} {
		t.Run(point, func(t *testing.T) {
			b, cache, arch, inst := installed(t)
			if err := os.Truncate(inst.Bin("postgres"), 1); err != nil { // damaged earlier install
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestUnpackKillHelper$")
			cmd.Env = append(os.Environ(), unpackKillEnv+"="+strings.Join([]string{point, cache, arch, b.SHA256}, "|"))
			err := cmd.Run()
			if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 3 {
				t.Fatalf("helper did not die at %s: %v", point, err)
			}
			_, markErr := os.Stat(filepath.Join(inst.Root, verifiedMarker))
			check := CheckInstall(inst, true)
			if markErr == nil && check != nil && point == "renamed" {
				t.Fatalf("published but not fully verified: %v", check)
			}
			if point == "renamed" && check != nil {
				t.Fatalf("after the rename the install must be complete: %v", check)
			}
			if point != "renamed" && check == nil {
				t.Fatalf("killed at %s, yet the damaged install passes", point)
			}
			inst2, err := ensureFromArchive(t, cache, arch, b, FetchOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if err := CheckInstall(inst2, true); err != nil {
				t.Fatalf("next start did not heal: %v", err)
			}
			// A folder left by the dead process is removed by the next
			// install under the lock; after "renamed" it is empty (the
			// install moved out of it) and nothing needs installing.
			left, _ := filepath.Glob(filepath.Join(cache, ".extract-*"))
			for _, d := range left {
				if es, _ := os.ReadDir(d); point != "renamed" || len(es) != 0 {
					t.Fatalf("leftover extract folder %s (%d entries)", d, len(es))
				}
			}
		})
	}
}
