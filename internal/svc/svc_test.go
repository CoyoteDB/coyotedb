package svc

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

var ctx = context.Background()

func linuxOpts() Options {
	o := Options{Executable: "/usr/local/bin/coyote", DataDir: linuxDataDir, FirewallPorts: DefaultFirewallPorts}
	o.Args = DefaultArgs(o.DataDir)
	return o
}

func newLinux(t *testing.T) (*linuxPlatform, *fakeSys) {
	f := newFakeSys(t)
	f.addFile("/usr/local/bin/coyote")
	f.addFile("/usr/sbin/nologin")
	return &linuxPlatform{h: f, log: slog.New(slog.DiscardHandler)}, f
}

func hasLine(s, line string) bool {
	for _, l := range strings.Split(s, "\n") {
		if l == line {
			return true
		}
	}
	return false
}

func TestRenderUnit(t *testing.T) {
	o := Options{Executable: "/opt/my apps/coyote", DataDir: "/srv/db%1", Args: []string{"service", "run", "--data-dir", "/srv/db%1", `we"ird$HOME\x`}}
	u := renderUnit(o)
	for _, want := range []string{
		`ExecStart="/opt/my apps/coyote" "service" "run" "--data-dir" "/srv/db%%1" "we\"ird$$HOME\\x"`,
		"WorkingDirectory=/srv/db%%1",
		"User=coyote", "Group=coyote",
		"KillMode=mixed", "TimeoutStopSec=120", "Restart=on-failure", "RestartSec=5",
		"LimitNOFILE=65536", "NoNewPrivileges=yes", "ProtectSystem=full", "PrivateTmp=yes",
		"ProtectHome=yes", "RestartPreventExitStatus=78", "WantedBy=multi-user.target",
		"After=network-online.target", "StartLimitIntervalSec=0", "UMask=0077",
	} {
		if !hasLine(u, want) {
			t.Errorf("unit lacks %q\n%s", want, u)
		}
	}
	for _, bad := range []string{"MemoryDenyWriteExecute", "PrivateDevices", "ProtectSystem=strict"} {
		if strings.Contains(u, bad) {
			t.Errorf("unit has %s, which breaks Postgres", bad)
		}
	}
	if unescapeSystemdPath("/srv/db%%1") != "/srv/db%1" {
		t.Fatal("path unescape")
	}
}

func TestLinuxInstallNeedsRoot(t *testing.T) {
	l, f := newLinux(t)
	f.root = false
	_, err := l.install(ctx, linuxOpts())
	if !IsElevationError(err) || !strings.Contains(err.Error(), "sudo coyote service install") {
		t.Fatalf("err = %v", err)
	}
	if len(f.calls) != 0 || f.writes != 0 {
		t.Fatalf("did something without root: %v", f.calls)
	}
	for _, fn := range []func() error{
		func() error { return l.start(ctx) },
		func() error { return l.stop(ctx) },
		func() error { _, err := l.uninstall(ctx, UninstallOptions{}); return err },
	} {
		if err := fn(); !IsElevationError(err) {
			t.Fatalf("err = %v", err)
		}
	}
}

func TestLinuxNoSystemd(t *testing.T) {
	l, f := newLinux(t)
	f.systemd = false
	if _, err := l.install(ctx, linuxOpts()); !errors.Is(err, errNoSystemd) {
		t.Fatalf("err = %v", err)
	}
	if _, err := l.status(ctx); !errors.Is(err, errNoSystemd) {
		t.Fatalf("status err = %v", err)
	}
}

func TestLinuxRejectsHomeAndSystemPaths(t *testing.T) {
	l, f := newLinux(t)
	f.addFile("/home/alex/coyote")
	o := linuxOpts()
	o.Executable = "/home/alex/coyote"
	if _, err := l.install(ctx, o); err == nil || !strings.Contains(err.Error(), "home folder") {
		t.Fatalf("err = %v", err)
	}
	o = linuxOpts()
	o.DataDir = "/etc/coyote"
	if _, err := l.install(ctx, o); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("err = %v", err)
	}
	o = linuxOpts()
	o.Executable = "/usr/local/bin/missing"
	if _, err := l.install(ctx, o); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v", err)
	}
}

func TestLinuxInstallIdempotentAndReversible(t *testing.T) {
	l, f := newLinux(t)
	rep, err := l.install(ctx, linuxOpts())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"groupadd --system coyote",
		"useradd --system --gid coyote --home-dir /var/lib/coyote --no-create-home --shell /usr/sbin/nologin --comment CoyoteDB service coyote",
		"systemctl daemon-reload", "systemctl enable coyote.service",
	} {
		if !f.ran(want) {
			t.Errorf("did not run %q; ran %q", want, f.calls)
		}
	}
	if f.ran("systemctl start") {
		t.Error("started without Start")
	}
	if f.perms[linuxUnitPath] != 0o644 || f.perms[linuxDataDir] != 0o700 || f.owner[linuxDataDir] != [2]int{998, 998} {
		t.Errorf("perms %o %o owner %v", f.perms[linuxUnitPath], f.perms[linuxDataDir], f.owner[linuxDataDir])
	}
	if len(rep.Steps) < 3 || !strings.Contains(strings.Join(rep.Notes, "\n"), linuxDataDir) {
		t.Errorf("report: %+v", rep)
	}

	// Again: nothing changes.
	f.resetCalls()
	rep, err = l.install(ctx, linuxOpts())
	if err != nil {
		t.Fatal(err)
	}
	if f.writes != 0 || f.ran("systemctl daemon-reload") || f.ran("useradd") || f.ran("systemctl enable") {
		t.Errorf("second install changed things: %q", f.calls)
	}
	if len(rep.Steps) != 1 || !strings.Contains(rep.Steps[0], "nothing to change") {
		t.Errorf("report: %+v", rep)
	}

	// With Start: starts. Then a changed definition restarts it.
	o := linuxOpts()
	o.Start = true
	if _, err := l.install(ctx, o); err != nil || !f.active || !f.ran("systemctl start coyote.service") {
		t.Fatalf("start: %v %v", err, f.calls)
	}
	f.addFile("/opt/coyote/coyote")
	o.Executable = "/opt/coyote/coyote"
	f.resetCalls()
	if _, err := l.install(ctx, o); err != nil || !f.ran("systemctl restart coyote.service") {
		t.Fatalf("restart on change: %v %v", err, f.calls)
	}
	// Changed while running, without Start: says to restart.
	o.Start = false
	o.Executable = "/usr/local/bin/coyote"
	rep, err = l.install(ctx, o)
	if err != nil || !strings.Contains(strings.Join(rep.Notes, " "), "restart it") {
		t.Fatalf("note: %v %+v", err, rep)
	}

	st, err := l.status(ctx)
	if err != nil || !st.Installed || !st.Running || st.PID != 4242 || st.DataDir != linuxDataDir {
		t.Fatalf("status: %+v %v", st, err)
	}

	// Uninstall keeps the data and says where it is.
	f.resetCalls()
	rep, err = l.uninstall(ctx, UninstallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !f.ran("systemctl disable --now coyote.service") || f.files[linuxUnitPath] != nil || f.active {
		t.Fatalf("uninstall: %q", f.calls)
	}
	if !f.dirs[linuxDataDir] || !strings.Contains(strings.Join(rep.Notes, " "), "still in /var/lib/coyote") || !strings.Contains(strings.Join(rep.Notes, " "), "--purge") {
		t.Fatalf("data handling: %+v", rep)
	}
	if _, ok := f.users["coyote"]; !ok {
		t.Fatal("user removed without purge")
	}
	// Uninstall again: fine.
	rep, err = l.uninstall(ctx, UninstallOptions{})
	if err != nil || !strings.Contains(rep.Steps[0], "not installed") {
		t.Fatalf("second uninstall: %v %+v", err, rep)
	}
	if st, _ := l.status(ctx); st.Installed {
		t.Fatal("still installed")
	}
	if err := l.start(ctx); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("start after uninstall: %v", err)
	}

	// Purge removes data and the user.
	if _, err := l.install(ctx, linuxOpts()); err != nil {
		t.Fatal(err)
	}
	if _, err := l.uninstall(ctx, UninstallOptions{Purge: true}); err != nil {
		t.Fatal(err)
	}
	if f.dirs[linuxDataDir] || f.ran("userdel coyote") == false {
		t.Fatalf("purge: dirs=%v calls=%q", f.dirs[linuxDataDir], f.calls)
	}
	if _, ok := f.users["coyote"]; ok {
		t.Fatal("user kept after purge")
	}
}

func TestLinuxExistingRootUserRefused(t *testing.T) {
	l, f := newLinux(t)
	f.users["coyote"] = [2]int{0, 0}
	if _, err := l.install(ctx, linuxOpts()); err == nil || !strings.Contains(err.Error(), "root") {
		t.Fatalf("err = %v", err)
	}
}

func TestLinuxBusyBoxUser(t *testing.T) {
	l, f := newLinux(t)
	f.bins = map[string]bool{"adduser": true} // BusyBox: no useradd
	l.h = &busyBox{fakeSys: f}
	if _, err := l.install(ctx, linuxOpts()); err != nil {
		t.Fatal(err)
	}
	if !f.ran("addgroup -S coyote") || !f.ran("adduser -S -D -H -h /var/lib/coyote -s /usr/sbin/nologin -G coyote coyote") {
		t.Fatalf("calls: %q", f.calls)
	}
}

type busyBox struct{ *fakeSys }

func (b *busyBox) Run(c context.Context, name string, args ...string) (string, error) {
	switch name {
	case "addgroup":
		b.calls = append(b.calls, name+" "+strings.Join(args, " "))
		b.groups["coyote"] = 101
		return "", nil
	case "adduser":
		b.calls = append(b.calls, name+" "+strings.Join(args, " "))
		b.users["coyote"] = [2]int{100, 101}
		return "", nil
	}
	return b.fakeSys.Run(c, name, args...)
}

func TestSafeToPurge(t *testing.T) {
	for _, bad := range []string{"/", "/var", "C:\\", `C:\ProgramData`, "relative/dir", ""} {
		if safeToPurge(bad) == nil {
			t.Errorf("would purge %q", bad)
		}
	}
	for _, ok := range []string{"/var/lib/coyote", "/Library/Application Support/CoyoteDB", `C:\ProgramData\CoyoteDB`} {
		if err := safeToPurge(ok); err != nil {
			t.Errorf("refused %q: %v", ok, err)
		}
	}
}

func TestValidate(t *testing.T) {
	good := linuxOpts()
	if err := validate(good); err != nil {
		t.Fatal(err)
	}
	for _, mod := range []func(*Options){
		func(o *Options) { o.Executable = "coyote" },
		func(o *Options) { o.DataDir = "data" },
		func(o *Options) { o.DataDir = "/var/lib/co\nyote" },
		func(o *Options) { o.FirewallPorts = []int{70000} },
	} {
		o := linuxOpts()
		mod(&o)
		if validate(o) == nil {
			t.Errorf("accepted %+v", o)
		}
	}
	if !isAbs(`C:\Program Files\CoyoteDB\coyote.exe`) {
		t.Fatal("windows path not absolute")
	}
}

func TestManagerDefaults(t *testing.T) {
	m := &Manager{p: &linuxPlatform{h: newFakeSys(t)}}
	o := Options{Executable: "/usr/local/bin/coyote"}
	if err := m.normalize(&o); err != nil {
		t.Fatal(err)
	}
	if o.DataDir != linuxDataDir || strings.Join(o.Args, " ") != "service run --data-dir /var/lib/coyote" || len(o.FirewallPorts) != 2 {
		t.Fatalf("%+v", o)
	}
}

// ---- macOS ----

func darwinOpts() Options {
	o := Options{Executable: "/usr/local/bin/coyote", DataDir: darwinDataDir}
	o.Args = DefaultArgs(o.DataDir)
	return o
}

func newDarwin(t *testing.T) (*darwinPlatform, *fakeSys) {
	f := newFakeSys(t)
	f.addFile("/usr/local/bin/coyote")
	f.users["_taken"] = [2]int{400, 400}
	return &darwinPlatform{h: f, log: slog.New(slog.DiscardHandler), sleep: func(time.Duration) {}}, f
}

func TestRenderPlist(t *testing.T) {
	o := darwinOpts()
	o.Args = append(o.Args, "--name", "a&b<c>")
	p := renderPlist(o)
	// Well-formed XML.
	dec := xml.NewDecoder(strings.NewReader(p))
	dec.Strict = true
	for {
		_, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("plist is not valid XML: %v\n%s", err, p)
		}
	}
	for _, want := range []string{
		"<string>com.coyotedb.coyote</string>",
		"<string>/Library/Application Support/CoyoteDB</string>",
		"<string>a&amp;b&lt;c&gt;</string>",
		"<key>UserName</key>\n\t<string>_coyote</string>",
		"<key>SuccessfulExit</key>\n\t\t<false/>",
		"<key>ExitTimeOut</key>\n\t<integer>120</integer>",
		"<key>Umask</key>\n\t<integer>63</integer>",
		"<string>/Library/Logs/CoyoteDB/coyote.err.log</string>",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("plist lacks %q", want)
		}
	}
	if got := plistString([]byte(p), "WorkingDirectory"); got != darwinDataDir {
		t.Fatalf("WorkingDirectory = %q", got)
	}
	if got := plistString([]byte(p), "UserName"); got != "_coyote" {
		t.Fatalf("UserName = %q", got)
	}
	if got := plistString([]byte(p), "Missing"); got != "" {
		t.Fatalf("Missing = %q", got)
	}
}

func TestDarwinInstallLifecycle(t *testing.T) {
	d, f := newDarwin(t)
	f.root = false
	if _, err := d.install(ctx, darwinOpts()); !IsElevationError(err) || !strings.Contains(err.Error(), "sudo") {
		t.Fatalf("err = %v", err)
	}
	f.root = true

	rep, err := d.install(ctx, darwinOpts())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"dscl . -create /Groups/_coyote PrimaryGroupID 401",
		"dscl . -create /Users/_coyote UniqueID 401",
		"dscl . -create /Users/_coyote UserShell /usr/bin/false",
		"dscl . -create /Users/_coyote IsHidden 1",
		"launchctl enable system/com.coyotedb.coyote",
	} {
		if !f.ran(want) {
			t.Errorf("did not run %q", want)
		}
	}
	if f.ran("launchctl load") || f.ran("launchctl unload") {
		t.Fatal("used deprecated load/unload")
	}
	if f.loaded || f.ran("launchctl bootstrap") {
		t.Fatal("bootstrapped without Start")
	}
	if f.perms[darwinPlist] != 0o644 || f.perms[darwinDataDir] != 0o700 || f.owner[darwinDataDir] != [2]int{401, 401} || f.owner[darwinLogDir] != [2]int{401, darwinAdminGID} {
		t.Fatalf("perms/owners: %o %o %v %v", f.perms[darwinPlist], f.perms[darwinDataDir], f.owner[darwinDataDir], f.owner[darwinLogDir])
	}
	if !strings.Contains(strings.Join(rep.Notes, " "), darwinDataDir) {
		t.Fatalf("report %+v", rep)
	}

	// Idempotent.
	f.resetCalls()
	if _, err := d.install(ctx, darwinOpts()); err != nil || f.writes != 0 || f.ran("dscl . -create") {
		t.Fatalf("second install: %v %q", err, f.calls)
	}

	// Start.
	if err := d.start(ctx); err != nil || !f.loaded || !f.ran("launchctl bootstrap system "+darwinPlist) {
		t.Fatalf("start: %v %q", err, f.calls)
	}
	st, err := d.status(ctx)
	if err != nil || !st.Running || st.PID != 777 || st.DataDir != darwinDataDir {
		t.Fatalf("status: %+v %v", st, err)
	}
	f.resetCalls()
	if err := d.start(ctx); err != nil || !f.ran("launchctl kickstart system/com.coyotedb.coyote") {
		t.Fatalf("start when loaded: %v %q", err, f.calls)
	}

	// Changed definition with Start: bootout, wait, bootstrap.
	o := darwinOpts()
	o.Start = true
	f.addFile("/opt/coyote/coyote")
	o.Executable = "/opt/coyote/coyote"
	f.resetCalls()
	if _, err := d.install(ctx, o); err != nil {
		t.Fatal(err)
	}
	if !f.ran("launchctl bootout system/com.coyotedb.coyote") || !f.loaded {
		t.Fatalf("reload: %q", f.calls)
	}

	// Stop: bootout and wait until launchd lets go.
	if err := d.stop(ctx); err != nil || f.loaded {
		t.Fatalf("stop: %v", err)
	}
	if st, _ := d.status(ctx); st.Running || !st.Installed {
		t.Fatalf("status after stop: %+v", st)
	}

	// Uninstall keeps data; purge deletes data, logs and user.
	rep, err = d.uninstall(ctx, UninstallOptions{})
	if err != nil || f.files[darwinPlist] != nil || !f.dirs[darwinDataDir] {
		t.Fatalf("uninstall: %v %+v", err, rep)
	}
	if !strings.Contains(strings.Join(rep.Notes, " "), "still in "+darwinDataDir) {
		t.Fatalf("notes %+v", rep.Notes)
	}
	if _, err := d.uninstall(ctx, UninstallOptions{Purge: true}); err != nil {
		t.Fatal(err)
	}
	if f.dirs[darwinDataDir] || f.dirs[darwinLogDir] || !f.ran("dscl . -delete /Users/_coyote") {
		t.Fatalf("purge: %q", f.calls)
	}
	if err := d.stop(ctx); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("stop after uninstall: %v", err)
	}
}

func TestDarwinRejectsUsersFolder(t *testing.T) {
	d, f := newDarwin(t)
	f.addFile("/Users/alex/Downloads/coyote")
	o := darwinOpts()
	o.Executable = "/Users/alex/Downloads/coyote"
	if _, err := d.install(ctx, o); err == nil || !strings.Contains(err.Error(), "home folder") {
		t.Fatalf("err = %v", err)
	}
}

// ---- Windows rules ----

func TestServiceSID(t *testing.T) {
	// Well-known: the TrustedInstaller service SID.
	if got, want := serviceSID("TrustedInstaller"), "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"; got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	if serviceSID("coyote") != serviceSID("COYOTE") {
		t.Fatal("service SID must be case-insensitive")
	}
	sddl := dataDirSDDL("coyote")
	if !strings.HasPrefix(sddl, "O:BAG:SYD:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;S-1-5-80-") || strings.Count(sddl, "(A;") != 3 {
		t.Fatalf("sddl %s", sddl)
	}
}

func TestEscapeArg(t *testing.T) {
	for in, want := range map[string]string{
		"":                                     `""`,
		"run":                                  "run",
		`C:\Program Files\CoyoteDB\coyote.exe`: `"C:\Program Files\CoyoteDB\coyote.exe"`,
		`C:\ProgramData\CoyoteDB`:              `C:\ProgramData\CoyoteDB`,
		`a"b`:                                  `a\"b`,
		`C:\dir with space\`:                   `"C:\dir with space\\"`,
	} {
		if got := escapeArg(in); got != want {
			t.Errorf("escapeArg(%q) = %s, want %s", in, got, want)
		}
	}
	bp := winBinaryPath(`C:\Program Files\CoyoteDB\coyote.exe`, DefaultArgs(`C:\ProgramData\CoyoteDB`))
	if bp != `"C:\Program Files\CoyoteDB\coyote.exe" service run --data-dir C:\ProgramData\CoyoteDB` {
		t.Fatal(bp)
	}
	if d := dataDirFromArgs([]string{"x", "service", "run", "--data-dir", `D:\db`}); d != `D:\db` {
		t.Fatal(d)
	}
	if d := dataDirFromArgs([]string{"x", "--data-dir=D:\\db"}); d != `D:\db` {
		t.Fatal(d)
	}
}

func TestFirewallCmdLine(t *testing.T) {
	exe := `C:\Program Files\CoyoteDB\coyote.exe`
	line, err := netshCmdLine(`C:\Windows\System32\netsh.exe`, firewallAddArgs(exe, DefaultFirewallPorts))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`C:\Windows\System32\netsh.exe advfirewall firewall add rule name=CoyoteDB `,
		`program="C:\Program Files\CoyoteDB\coyote.exe"`,
		` localport=7481,5432 `,
		` profile=private,domain `,
		` dir=in action=allow protocol=TCP `,
		`description="CoyoteDB cluster and database ports (added by coyote service install)"`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("netsh line lacks %q:\n%s", want, line)
		}
	}
	if strings.Contains(line, "public") {
		t.Error("rule must not cover public networks")
	}
	if _, err := netshCmdLine("netsh", []string{`program=C:\a"b`}); err == nil {
		t.Fatal("quote accepted")
	}
	del, _ := netshCmdLine("netsh", firewallDeleteArgs())
	if del != "netsh advfirewall firewall delete rule name=CoyoteDB" {
		t.Fatal(del)
	}
}

func TestElevationMessages(t *testing.T) {
	if e := elevationError("windows", "install").Error(); !strings.Contains(e, "Run as administrator") || strings.Contains(e, "sudo") {
		t.Fatal(e)
	}
	if e := elevationError("linux", "uninstall").Error(); !strings.Contains(e, "sudo coyote service uninstall") {
		t.Fatal(e)
	}
}
