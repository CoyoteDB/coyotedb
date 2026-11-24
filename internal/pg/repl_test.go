package pg

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLSN(t *testing.T) {
	for _, c := range []struct {
		in   string
		want LSN
		out  string
	}{
		{"0/0", 0, "0/0"},
		{"0/3000148", 0x3000148, "0/3000148"},
		{"16/b374d848", 0x16B374D848, "16/B374D848"},
		{"FFFFFFFF/FFFFFFFF", 0xFFFFFFFFFFFFFFFF, "FFFFFFFF/FFFFFFFF"},
	} {
		got, err := ParseLSN(c.in)
		if err != nil || got != c.want || got.String() != c.out {
			t.Errorf("ParseLSN(%q) = %v (%s), %v", c.in, uint64(got), got, err)
		}
	}
	for _, bad := range []string{"", "0", "/1", "1/", "x/1", "1/2/3", "100000000/0", "-1/0", "1 /2"} {
		if _, err := ParseLSN(bad); err == nil {
			t.Errorf("ParseLSN(%q) should fail", bad)
		}
	}
	a, _ := ParseLSN("1/0")
	b, _ := ParseLSN("0/FFFFFF00")
	if a.Compare(b) != 1 || b.Compare(a) != -1 || a.Compare(a) != 0 {
		t.Error("Compare")
	}
	if a.Sub(b) != 0x100 || b.Sub(a) != -0x100 {
		t.Errorf("Sub = %d", a.Sub(b))
	}
	if Lag(a, b) != 0x100 || Lag(b, a) != 0 {
		t.Error("Lag")
	}
	j, _ := json.Marshal(struct{ L LSN }{a})
	if string(j) != `{"L":"1/0"}` {
		t.Errorf("json %s", j)
	}
	var back struct{ L LSN }
	if err := json.Unmarshal(j, &back); err != nil || back.L != a {
		t.Errorf("unmarshal %v %v", back, err)
	}
	var s LSN
	if err := s.Scan("0/10"); err != nil || s != 0x10 {
		t.Error("Scan")
	}
	if got := WALFileName(1, 0x3000148); got != "000000010000000000000003" {
		t.Errorf("WALFileName = %s", got)
	}
	if got := WALFileName(2, 0x1_2F000000); got != "00000002000000010000002F" {
		t.Errorf("WALFileName = %s", got)
	}
}

func TestConninfo(t *testing.T) {
	got := Conninfo(ConnParams{
		Upstream: Upstream{Host: "127.0.0.1", Port: 7433},
		User:     ReplUser,
		AppName:  "n2",
		Passfile: `/data/node one/it's\pass`,
	})
	want := `host=127.0.0.1 port=7433 user=coyote_repl passfile='/data/node one/it\'s\\pass' application_name=n2 sslmode=disable connect_timeout=10` +
		` keepalives=1 keepalives_idle=10 keepalives_interval=5 keepalives_count=3 tcp_user_timeout=30000`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if strings.Contains(got, "password") {
		t.Fatal("conninfo must never carry a password")
	}
	// As a postgresql.conf value: conf quoting on top of conninfo quoting.
	conf := RenderConf(Config{Port: 1, LogDir: "/l", PrimaryConninfo: got, PrimarySlot: "coyote_n2"})
	if !strings.Contains(conf, `primary_conninfo = 'host=127.0.0.1 port=7433 user=coyote_repl passfile=''/data/node one/it\\''s\\\\pass''`) {
		t.Fatalf("conf rendering:\n%s", conf)
	}
	if quoteConninfo("") != "''" || quoteConninfo("a=b") != "'a=b'" || quoteConninfo("plain") != "plain" {
		t.Fatal("quoteConninfo")
	}
}

func TestUpstream(t *testing.T) {
	u, err := ParseUpstream("127.0.0.1:7433")
	if err != nil || u != (Upstream{"127.0.0.1", 7433}) || u.String() != "127.0.0.1:7433" {
		t.Fatalf("%v %v", u, err)
	}
	if u, err := ParseUpstream("[::1]:5"); err != nil || u.Host != "::1" || u.String() != "[::1]:5" {
		t.Fatalf("ipv6: %v %v", u, err)
	}
	for _, bad := range []string{"", "host", "h:0", "h:70000", "h:x", "a b:1", "h'x:1"} {
		if _, err := ParseUpstream(bad); err == nil {
			t.Errorf("ParseUpstream(%q) should fail", bad)
		}
	}
}

func TestRenderPassfile(t *testing.T) {
	got := RenderPassfile([]PassEntry{
		{"127.0.0.1", "7433", "replication", "coyote_repl", `p:w\d`},
		{"::1", "5", "*", "u", "x"},
	})
	want := "127.0.0.1:7433:replication:coyote_repl:p\\:w\\\\d\n\\:\\:1:5:*:u:x\n"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	e := upstreamPassEntries(Upstream{"127.0.0.1", 9}, Secrets{Superuser: "su", Replication: "rp", Rewind: "rw"})
	for _, x := range e {
		if x.Password == "su" {
			t.Fatal("superuser password must not go to the upstream passfile")
		}
	}
}

func TestNames(t *testing.T) {
	for _, ok := range []string{"n1", "a", "node-2", "x9", strings.Repeat("a", 48)} {
		if err := ValidName(ok); err != nil {
			t.Errorf("ValidName(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-a", "a-", "A", "a_b", "a.b", "é", strings.Repeat("a", 49), `a"b`} {
		if ValidName(bad) == nil {
			t.Errorf("ValidName(%q) should fail", bad)
		}
	}
	for in, want := range map[string]string{
		"Alexs-MacBook-Pro.local": "alexs-macbook-pro",
		"Alex's  laptop":          "alex-s-laptop",
		"--x--":                   "x",
		"":                        "node",
		"日本":                      "node",
		strings.Repeat("ab", 40):  strings.Repeat("ab", 24),
	} {
		got := SanitizeName(in)
		if got != want || ValidName(got) != nil {
			t.Errorf("SanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
	if SlotName("node-2") != "coyote_node_2" {
		t.Error("SlotName")
	}
}

func TestRenderSyncNames(t *testing.T) {
	got, err := RenderSyncNames(1, []string{"n2", "n3"})
	if err != nil || got != `ANY 1 ("n2", "n3")` {
		t.Fatalf("%q %v", got, err)
	}
	if got, err := RenderSyncNames(0, nil); err != nil || got != "" {
		t.Fatalf("empty: %q %v", got, err)
	}
	// Odd names are taken literally; inside the conf file single quotes
	// are doubled on top.
	got, err = RenderSyncNames(2, []string{`we"ird`, "1st", "select", "it's"})
	if err != nil || got != `ANY 2 ("we""ird", "1st", "select", "it's")` {
		t.Fatalf("%q %v", got, err)
	}
	conf := RenderConf(Config{Port: 1, LogDir: "/l", SyncStandbyNames: got})
	if !strings.Contains(conf, `synchronous_standby_names = 'ANY 2 ("we""ird", "1st", "select", "it''s")'`) {
		t.Fatalf("conf:\n%s", conf)
	}
	for _, c := range []struct {
		k     int
		names []string
	}{{0, []string{"a"}}, {2, []string{"a"}}, {1, nil}, {1, []string{"a", "A"}}, {1, []string{""}}, {1, []string{"a\nb"}}} {
		if _, err := RenderSyncNames(c.k, c.names); err == nil {
			t.Errorf("RenderSyncNames(%d, %q) should fail", c.k, c.names)
		}
	}
}

func TestRenderConfReplication(t *testing.T) {
	c := RenderConf(Config{Port: 7, LogDir: "/l"})
	for _, want := range []string{
		"wal_keep_size = '512MB'\n",
		"max_slot_wal_keep_size = '8GB'\n",
		"recovery_target_timeline = 'latest'\n",
		"primary_conninfo = ''\n",
		"primary_slot_name = ''\n",
		"synchronous_standby_names = ''\n",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("missing %q", want)
		}
	}
	h := RenderHBA()
	if !strings.Contains(h, "hostnossl replication  coyote_repl  127.0.0.1/32   scram-sha-256") {
		t.Fatalf("hba:\n%s", h)
	}
	if strings.Contains(h, "replication  all   127.0.0.1/32   scram") {
		t.Fatal("only the replication role may replicate")
	}
}

func TestStripAutoConf(t *testing.T) {
	in := "# Do not edit this file manually!\n" +
		"# It will be overwritten by the ALTER SYSTEM command.\n" +
		"work_mem = '64MB'\n" +
		"primary_conninfo = 'host=10.0.0.9 port=5432 user=x'\n" +
		"Primary_Slot_Name='old'\n" +
		"listen_addresses = '*'\n" +
		"wal_level minimal\n" +
		"restore_command = 'cp /a/%f %p'\n" +
		"synchronous_standby_names = '*'\n" +
		"primary_conninfox = 'kept: not our key'\n" +
		"shared_buffers = '1GB'"
	got, changed := StripAutoConf(in)
	want := "# Do not edit this file manually!\n" +
		"# It will be overwritten by the ALTER SYSTEM command.\n" +
		"work_mem = '64MB'\n" +
		"primary_conninfox = 'kept: not our key'\n" +
		"shared_buffers = '1GB'"
	if !changed || got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if again, changed := StripAutoConf(got); changed || again != got {
		t.Fatal("second strip must be a no-op")
	}

	// On disk, through WriteConfig.
	pgdata := t.TempDir()
	_ = os.WriteFile(filepath.Join(pgdata, "postgresql.conf"), []byte("x = 1\n"), 0o600)
	_ = os.WriteFile(filepath.Join(pgdata, autoConfFile), []byte(in), 0o600)
	if err := WriteConfig(pgdata, Config{Port: 7, LogDir: "/l"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(pgdata, autoConfFile))
	if string(b) != want {
		t.Fatalf("on disk:\n%s", b)
	}
}

func TestSecrets(t *testing.T) {
	s := Secrets{Superuser: "su-pw", Replication: "repl-pw", Rewind: "rw-pw"}
	got, err := ParseSecrets([]byte(RenderSecrets(s)))
	if err != nil || got != s {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := ParseSecrets([]byte("superuser=a\nreplication=b\n")); err == nil {
		t.Fatal("incomplete must fail")
	}
	if _, err := ParseSecrets([]byte("superuser=a\nreplication=b\nrewind=c\nextra=d\n")); err == nil {
		t.Fatal("unknown key must fail")
	}
	n := Node{StateDir: t.TempDir()}
	if err := os.WriteFile(n.PasswordFile(), []byte("su\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s1, err := n.EnsureSecrets()
	if err != nil || s1.Superuser != "su" || s1.Replication == "" || s1.Rewind == "" || s1.Replication == s1.Rewind {
		t.Fatalf("%v", err)
	}
	s2, err := n.EnsureSecrets()
	if err != nil || s2 != s1 {
		t.Fatal("EnsureSecrets must be stable")
	}
	fi, _ := os.Stat(n.SecretsPath())
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
}

func TestPlanStart(t *testing.T) {
	demoted := &Marker{Reason: ReasonDemoted}
	cloning := &Marker{Reason: ReasonCloning}
	rewinding := &Marker{Reason: ReasonRewinding}
	for _, c := range []struct {
		name                 string
		init, secrets, clone bool
		m                    *Marker
		want                 StartPlan
		wantErr, wantMarkErr bool
	}{
		{"fresh", false, false, false, nil, PlanInit, false, false},
		{"fresh clone", false, false, true, nil, PlanClone, false, false},
		{"resume", true, true, false, nil, PlanResume, false, false},
		{"resume ignores clone flag", true, true, true, nil, PlanResume, false, false},
		{"lost pgdata of a member", false, true, false, nil, 0, true, false},
		{"lost pgdata, clone ok", false, true, true, nil, PlanClone, false, false},
		{"demoted refuses", true, true, false, demoted, 0, true, true},
		{"demoted refuses even with clone flag", true, true, true, demoted, 0, true, true},
		{"rewinding refuses", true, true, false, rewinding, 0, true, true},
		{"interrupted clone needs flag", false, true, false, cloning, 0, true, true},
		{"interrupted clone retried", false, true, true, cloning, PlanClone, false, false},
		{"interrupted clone after rename", true, true, true, cloning, PlanClone, false, false},
	} {
		got, err := PlanStart(c.init, c.secrets, c.m, c.clone)
		if (err != nil) != c.wantErr || (err == nil && got != c.want) {
			t.Errorf("%s: got %v, %v", c.name, got, err)
		}
		var me MarkerError
		if errors.As(err, &me) != c.wantMarkErr {
			t.Errorf("%s: marker error = %v", c.name, err)
		}
	}
	if !RewindFirst(nil) || !RewindFirst(demoted) || RewindFirst(rewinding) || RewindFirst(cloning) || RewindFirst(&Marker{Reason: ReasonRecloning}) {
		t.Error("RewindFirst")
	}
}

func TestMarkerFile(t *testing.T) {
	n := Node{StateDir: t.TempDir()}
	if m, err := n.ReadMarker(); m != nil || err != nil {
		t.Fatal("no marker expected")
	}
	if err := n.WriteMarker(Marker{Reason: ReasonRewinding, Timeline: 3}); err != nil {
		t.Fatal(err)
	}
	// ensureMarker never downgrades an existing marker.
	if err := n.ensureMarker(Marker{Reason: ReasonDemoted}); err != nil {
		t.Fatal(err)
	}
	m, _ := n.ReadMarker()
	if m == nil || m.Reason != ReasonRewinding || m.Timeline != 3 || m.Since.IsZero() {
		t.Fatalf("%+v", m)
	}
	// A damaged marker still blocks a primary start.
	_ = os.WriteFile(n.MarkerPath(), []byte("{garbage"), 0o600)
	m, _ = n.ReadMarker()
	if m == nil || m.Reason != ReasonDemoted {
		t.Fatalf("damaged marker: %+v", m)
	}
	if err := n.ClearMarker(); err != nil {
		t.Fatal(err)
	}
	if err := n.ClearMarker(); err != nil {
		t.Fatal("clearing twice is fine")
	}
	if m, _ := n.ReadMarker(); m != nil {
		t.Fatal("marker not cleared")
	}
}

func TestReplStateRoundTrip(t *testing.T) {
	n := Node{StateDir: t.TempDir()}
	if s, err := n.ReadReplState(); err != nil || s.Upstream != nil {
		t.Fatal("empty state expected")
	}
	in := ReplState{Upstream: &Upstream{"127.0.0.1", 5}, Slot: "coyote_n2", SyncCount: 1, SyncNames: []string{"n2"}}
	if err := n.WriteReplState(in); err != nil {
		t.Fatal(err)
	}
	out, err := n.ReadReplState()
	if err != nil || *out.Upstream != *in.Upstream || out.Slot != in.Slot || out.SyncCount != 1 || out.SyncNames[0] != "n2" {
		t.Fatalf("%+v %v", out, err)
	}
	cfg, err := n.configFor(7433, "n2", out)
	if err != nil || !strings.Contains(cfg.PrimaryConninfo, "application_name=n2") || cfg.SyncStandbyNames != `ANY 1 ("n2")` {
		t.Fatalf("%+v %v", cfg, err)
	}
}

func TestParseControlData(t *testing.T) {
	out := `pg_control version number:            1800
Database system identifier:           7400000000000000001
Database cluster state:               shut down in recovery
Latest checkpoint location:           0/3000148
Latest checkpoint's REDO location:    0/3000110
Latest checkpoint's TimeLineID:       2
Latest checkpoint's PrevTimeLineID:   2
`
	c, err := ParseControlData(out)
	if err != nil || c.State != "shut down in recovery" || c.Timeline != 2 || c.CheckpointLSN != 0x3000148 || c.SystemID != "7400000000000000001" || !c.CleanlyShutDown() {
		t.Fatalf("%+v %v", c, err)
	}
	c.State = "in production"
	if c.CleanlyShutDown() {
		t.Fatal("in production is not clean")
	}
	if _, err := ParseControlData("garbage"); err == nil {
		t.Fatal("garbage must fail")
	}
}

func TestLooksLikePGData(t *testing.T) {
	d := t.TempDir()
	if looksLikePGData(d) {
		t.Fatal("empty dir")
	}
	_ = os.WriteFile(filepath.Join(d, "PG_VERSION"), []byte("18\n"), 0o600)
	_ = os.MkdirAll(filepath.Join(d, "global"), 0o700)
	_ = os.WriteFile(filepath.Join(d, "global", "pg_control"), nil, 0o600)
	if !looksLikePGData(d) {
		t.Fatal("pgdata not recognised")
	}
}

func TestCloneRefusesWithoutGuardOrOverData(t *testing.T) {
	n := Node{StateDir: t.TempDir()}
	o := CloneOptions{Source: Upstream{"127.0.0.1", 5}, Secrets: Secrets{Superuser: "a", Replication: "b", Rewind: "c"}, Name: "n2", Port: 7}
	if err := n.Clone(t.Context(), o); err == nil || !strings.Contains(err.Error(), "guard") {
		t.Fatalf("no guard: %v", err)
	}
	if err := n.Rewind(t.Context(), RewindOptions{Source: o.Source}); err == nil || !strings.Contains(err.Error(), "guard") {
		t.Fatalf("no guard: %v", err)
	}
}

func TestParseBasebackupProgress(t *testing.T) {
	for _, tc := range []struct {
		line string
		pct  int
		ok   bool
	}{
		{"24199/24199 kB (100%), 1/1 tablespace", 100, true},
		{"    0/24199 kB (0%), 0/1 tablespace (...data.clone-tmp/backup_label )", 0, true},
		{"12000/24199 kB (49%), 0/1 tablespace (x (y))", 49, true},
		{"pg_basebackup: base backup completed", 0, false},
		{"weird (abc%)", 0, false},
		{"over (101%)", 0, false},
	} {
		pct, ok := ParseBasebackupProgress(tc.line)
		if pct != tc.pct || ok != tc.ok {
			t.Errorf("%q: got %d %v, want %d %v", tc.line, pct, ok, tc.pct, tc.ok)
		}
	}
}

func TestParseSyncNames(t *testing.T) {
	for _, names := range [][]string{nil, {"a"}, {"b", "a"}, {`we"ird`, "x y"}} {
		k := min(1, len(names))
		s, err := RenderSyncNames(k, names)
		if err != nil {
			t.Fatal(err)
		}
		gk, gn, err := ParseSyncNames(s)
		if err != nil || gk != k || !slices.Equal(gn, names) {
			t.Fatalf("%q: got %d %q %v", s, gk, gn, err)
		}
	}
	if k, n, err := ParseSyncNames("FIRST 2 (a, b, c)"); err != nil || k != 2 || len(n) != 3 {
		t.Fatalf("first: %d %v %v", k, n, err)
	}
	if k, n, err := ParseSyncNames("a, b"); err != nil || k != 1 || len(n) != 2 {
		t.Fatalf("plain: %d %v %v", k, n, err)
	}
	for _, bad := range []string{"ANY x (a)", "ANY 1 (a", `ANY 1 ("a)`, "ANY 1 (a,,b)"} {
		if _, _, err := ParseSyncNames(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}
