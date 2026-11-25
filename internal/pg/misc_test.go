package pg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestTargetFor(t *testing.T) {
	cases := map[string]string{
		"darwin/arm64":  "aarch64-apple-darwin",
		"darwin/amd64":  "x86_64-apple-darwin",
		"linux/amd64":   "x86_64-unknown-linux-gnu",
		"linux/arm64":   "aarch64-unknown-linux-gnu",
		"windows/amd64": "x86_64-pc-windows-msvc",
	}
	for in, want := range cases {
		goos, goarch, _ := strings.Cut(in, "/")
		got, err := TargetFor(goos, goarch)
		if err != nil || got != want {
			t.Errorf("TargetFor(%s) = %q, %v; want %q", in, got, err, want)
		}
		b, err := BuildFor(goos, goarch)
		if err != nil {
			t.Errorf("BuildFor(%s): %v", in, err)
			continue
		}
		wantURL := "https://github.com/theseus-rs/postgresql-binaries/releases/download/18.7.0/postgresql-18.7.0-" + want + ".tar.gz"
		if b.URL() != wantURL {
			t.Errorf("URL = %s", b.URL())
		}
	}
	for _, bad := range []string{"windows/arm64", "freebsd/amd64", "linux/386"} {
		goos, goarch, _ := strings.Cut(bad, "/")
		if _, err := TargetFor(goos, goarch); err == nil {
			t.Errorf("TargetFor(%s) should fail", bad)
		}
	}
}

func TestPinnedChecksumsWellFormed(t *testing.T) {
	hex64 := regexp.MustCompile(`^[0-9a-f]{64}$`)
	if len(pinnedSHA256) != 5 {
		t.Fatalf("want 5 pinned targets, have %d", len(pinnedSHA256))
	}
	seen := map[string]bool{}
	for target, sum := range pinnedSHA256 {
		if !hex64.MatchString(sum) {
			t.Errorf("%s: bad sha256 %q", target, sum)
		}
		if seen[sum] {
			t.Errorf("%s: duplicate sha256", target)
		}
		seen[sum] = true
	}
}

// TestKnownBuilds: the current release first, every earlier release of the
// same major with all five targets, every checksum distinct and well
// formed, versions strictly older than Version (D70).
func TestKnownBuilds(t *testing.T) {
	hex64 := regexp.MustCompile(`^[0-9a-f]{64}$`)
	kb := KnownBuilds()
	if want := 5 * (1 + len(earlierBuilds)); len(kb) != want {
		t.Fatalf("%d known builds, want %d", len(kb), want)
	}
	seen := map[string]bool{}
	for i, b := range kb {
		if (i < 5) != (b.Version == Version) {
			t.Errorf("build %d is %s; the current release comes first", i, b.Version)
		}
		if !hex64.MatchString(b.SHA256) || seen[b.SHA256] {
			t.Errorf("%s %s: bad or duplicate sha256 %q", b.Version, b.Target, b.SHA256)
		}
		seen[b.SHA256] = true
		if _, _, ok := GoPlatform(b.Target); !ok {
			t.Errorf("%s: no Go platform", b.Target)
		}
		if !strings.HasPrefix(b.Version, MajorVersion+".") {
			t.Errorf("%s is not PostgreSQL %s", b.Version, MajorVersion)
		}
	}
	prev := Version
	for _, e := range earlierBuilds {
		if len(e.SHA256) != 5 {
			t.Errorf("%s: %d targets, want 5", e.Version, len(e.SHA256))
		}
		if !versionLess(e.Version, prev) {
			t.Errorf("%s is listed after %s; newest first, all older than %s", e.Version, prev, Version)
		}
		prev = e.Version
	}
	if b, _ := CurrentBuild(); b.Version != Version {
		t.Errorf("CurrentBuild is %s, want %s", b.Version, Version)
	}
}

func versionLess(a, b string) bool {
	var x, y [3]int
	fmt.Sscanf(a, "%d.%d.%d", &x[0], &x[1], &x[2])
	fmt.Sscanf(b, "%d.%d.%d", &y[0], &y[1], &y[2])
	for i := range x {
		if x[i] != y[i] {
			return x[i] < y[i]
		}
	}
	return false
}

func TestParsePidFile(t *testing.T) {
	full := "4242\n/data/pg data\n1732100000\n7432\n\n127.0.0.1\n  1234567   65536\nready   \n"
	p, err := ParsePidFile(full)
	if err != nil {
		t.Fatal(err)
	}
	if p.PID != 4242 || p.DataDir != "/data/pg data" || p.StartTime != 1732100000 || p.Port != 7432 ||
		p.Listen != "127.0.0.1" || p.Status != "ready" {
		t.Fatalf("parsed %+v", p)
	}
	// Windows line endings, partial file (still starting).
	p, err = ParsePidFile("99\r\nC:/x\r\n")
	if err != nil || p.PID != 99 || p.Status != "" || p.Port != 0 {
		t.Fatalf("partial: %+v %v", p, err)
	}
	// Single-user backend writes a negative pid.
	if p, err := ParsePidFile("-77\n/d\n"); err != nil || p.PID != 77 {
		t.Fatalf("negative: %+v %v", p, err)
	}
	for _, bad := range []string{"", "\n", "abc\n", "0\n"} {
		if _, err := ParsePidFile(bad); err == nil {
			t.Errorf("ParsePidFile(%q) should fail", bad)
		}
	}
	// Missing file is not an error.
	if p, err := ReadPidFile(t.TempDir()); p != nil || err != nil {
		t.Fatalf("missing file: %v %v", p, err)
	}
}

func TestRenderConf(t *testing.T) {
	c := RenderConf(Config{Port: 7555, LogDir: `/var/x/it's logs`})
	for _, want := range []string{
		"listen_addresses = '127.0.0.1'\n",
		"port = 7555\n",
		"unix_socket_directories = ''\n",
		"password_encryption = 'scram-sha-256'\n",
		"wal_level = replica\n",
		"wal_log_hints = on\n",
		"full_page_writes = on\n",
		"max_wal_senders = 10\n",
		"max_replication_slots = 10\n",
		"hot_standby = on\n",
		"logging_collector = on\n",
		"log_directory = '/var/x/it''s logs'\n",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("coyote.conf missing %q\n%s", want, c)
		}
	}
	if strings.Contains(c, "0.0.0.0") || strings.Contains(c, "'*'") {
		t.Error("must never listen on all interfaces")
	}
	if got := quoteConf(`C:\a'b`); got != `'C:\\a''b'` {
		t.Errorf("quoteConf = %s", got)
	}
}

// pg_hba: loopback only; plain TCP admits only the internal roles; TLS
// (every client, through the proxy) rejects the internal roles before
// any password check and admits other roles with scram; nothing is trust.
func TestRenderHBA(t *testing.T) {
	h := RenderHBA()
	var rules [][]string
	for _, l := range strings.Split(h, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		f := strings.Fields(l)
		if len(f) != 5 || f[3] != "127.0.0.1/32" || (f[4] != "scram-sha-256" && f[4] != "reject") {
			t.Errorf("unexpected hba rule %q", l)
		}
		rules = append(rules, f)
	}
	internal := strings.Join(InternalRoles, ",")
	want := [][]string{
		{"hostnossl", "replication", ReplUser, "127.0.0.1/32", "scram-sha-256"},
		{"hostnossl", "all", internal, "127.0.0.1/32", "scram-sha-256"},
		{"hostnossl", "all", "all", "127.0.0.1/32", "reject"},
		{"hostssl", "all", internal, "127.0.0.1/32", "reject"},
		{"hostssl", "replication", "all", "127.0.0.1/32", "reject"},
		{"hostssl", "all", "all", "127.0.0.1/32", "scram-sha-256"},
	}
	if fmt.Sprint(rules) != fmt.Sprint(want) {
		t.Fatalf("rules\n%v\nwant\n%v", rules, want)
	}
	if strings.Contains(h, "trust") || strings.Contains(h, "0.0.0.0") {
		t.Fatal("trust or a wildcard address in pg_hba")
	}
}

func TestWithIncludeIdempotent(t *testing.T) {
	orig := []byte("# sample\nshared_buffers = 128MB\n")
	once, changed := withInclude(orig)
	if !changed || !strings.HasSuffix(string(once), includeLine+"\n") {
		t.Fatalf("include not appended:\n%s", once)
	}
	if !strings.HasPrefix(string(once), string(orig[:len(orig)-1])) {
		t.Fatal("original content must be kept")
	}
	twice, changed := withInclude(once)
	if changed || string(twice) != string(once) {
		t.Fatal("second call must be a no-op")
	}
	// Someone added a setting after our include: ours must come last again.
	edited := append(append([]byte{}, once...), []byte("port = 1\n")...)
	again, changed := withInclude(edited)
	if !changed || !strings.HasSuffix(string(again), includeLine+"\n") {
		t.Fatal("include must be re-appended to stay last")
	}
}

func TestWriteConfig(t *testing.T) {
	pgdata := t.TempDir()
	if err := os.WriteFile(filepath.Join(pgdata, "postgresql.conf"), []byte("max_connections = 100\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	logDir := filepath.Join(t.TempDir(), "logs")
	for range 2 {
		if err := WriteConfig(pgdata, Config{Port: 7432, LogDir: logDir}); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(filepath.Join(pgdata, "postgresql.conf"))
	if strings.Count(string(b), includeLine) != 1 {
		t.Fatalf("include line count wrong:\n%s", b)
	}
	if err := WriteConfig(pgdata, Config{Port: 0, LogDir: logDir}); err == nil {
		t.Fatal("port 0 must be rejected")
	}
	if err := WriteConfig(pgdata, Config{Port: 7432, LogDir: "rel"}); err == nil {
		t.Fatal("relative log dir must be rejected")
	}
}

func TestConnString(t *testing.T) {
	s := ConnString(7432, "p@ss/w:rd", true)
	if s != "postgresql://postgres:p%40ss%2Fw%3Ard@127.0.0.1:7432/postgres?sslmode=disable" {
		t.Fatalf("got %s", s)
	}
	s = ConnString(7432, "secret", false)
	if strings.Contains(s, "secret") {
		t.Fatal("password leaked")
	}
}

func TestLockExclusive(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	l1, err := AcquireLock(context.Background(), p, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	waited := false
	if _, err := AcquireLock(ctx, p, func() { waited = true }); !errors.Is(err, context.DeadlineExceeded) || !waited {
		t.Fatalf("second lock should block: %v waited=%v", err, waited)
	}
	if err := l1.Unlock(); err != nil {
		t.Fatal(err)
	}
	l2, err := AcquireLock(context.Background(), p, nil)
	if err != nil {
		t.Fatalf("lock after unlock: %v", err)
	}
	_ = l2.Unlock()
}

func TestStartRefusesWithoutGuard(t *testing.T) {
	n := Node{StateDir: t.TempDir()}
	if _, err := n.Start(context.Background(), StartOptions{Port: 7432}); err == nil {
		t.Fatal("Start without a guard must fail")
	}
}

func TestProcessAliveSelf(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Fatal("self should be alive")
	}
	if processAlive(0) || processAlive(-1) {
		t.Fatal("non-positive pids are never alive")
	}
	if ok, err := isPostgresProcess(os.Getpid()); err != nil || ok {
		t.Fatalf("test binary is not postgres: %v %v", ok, err)
	}
}

// Per-machine settings are always in coyote.conf (which is rewritten on
// every start and included last), and a value carried over in
// postgresql.auto.conf from another node is stripped.
func TestPerMachineSettingsNeverInherited(t *testing.T) {
	conf := RenderConf(Config{Port: 7432, LogDir: "/logs"})
	dsm, sync := platformSettings(runtime.GOOS)
	for _, want := range []string{
		"dynamic_shared_memory_type = " + dsm + "\n",
		"wal_sync_method = " + sync + "\n",
		"jit = off\n",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("coyote.conf lacks %q", want)
		}
	}
	for goos, want := range map[string][2]string{
		"darwin": {"posix", "fsync_writethrough"}, "windows": {"windows", "fsync"}, "linux": {"posix", "fdatasync"},
	} {
		if d, s := platformSettings(goos); d != want[0] || s != want[1] {
			t.Errorf("%s: %s %s", goos, d, s)
		}
	}
	auto := "# Do not edit\nwal_sync_method = 'open_datasync'\ndynamic_shared_memory_type = sysv\njit = on\nwork_mem = '64MB'\n"
	out, changed := StripAutoConf(auto)
	if !changed || out != "# Do not edit\nwork_mem = '64MB'\n" {
		t.Fatalf("stripped %q", out)
	}
}
