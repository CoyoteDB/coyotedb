package svc

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strconv"
	"strings"

	"coyotedb.com/coyote/internal/brand"
	"coyotedb.com/coyote/internal/paths"
)

// Linux / systemd. The logic here is OS-independent (it drives a host),
// so it is unit-tested on every OS; platform_linux.go wires the real host.

const (
	linuxUser     = brand.DirNameLower // "coyote"
	linuxUnitPath = "/etc/systemd/system/" + Name + ".service"
	linuxDataDir  = paths.LinuxSystemDataDir
	unitName      = Name + ".service"
	// StopTimeout: Postgres' stop escalation (fast 30 s, immediate 10 s,
	// kill 10 s; internal/pg DefaultStopTimeouts) plus the agent's own
	// shutdown (Raft, up to 10 s), with margin. Also used for launchd
	// ExitTimeOut and the Windows preshutdown timeout.
	StopTimeoutSeconds = 120
	fileLimit          = 65536
)

var errNoSystemd = errors.New("this Linux system does not run systemd, so " + brand.Command +
	" cannot set up the background service by itself. Start it from your init system with: " +
	brand.Command + " service run --data-dir " + linuxDataDir + " (as a dedicated non-root user)")

type linuxPlatform struct {
	h   host
	log *slog.Logger
}

func (l *linuxPlatform) defaultDataDir() string { return linuxDataDir }

// systemdQuote quotes one ExecStart word: always double quotes, with
// backslash and quote escaped, and % / $ doubled so systemd does not
// expand specifiers or variables.
func systemdQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`, `$`, `$$`)
	return `"` + r.Replace(s) + `"`
}

// systemdPath escapes a path value (specifiers only).
func systemdPath(s string) string { return strings.ReplaceAll(s, "%", "%%") }

func unescapeSystemdPath(s string) string { return strings.ReplaceAll(s, "%%", "%") }

// renderUnit renders the systemd unit. Hardening choices (see the report
// in docs): everything here is compatible with a child Postgres.
//
//	NoNewPrivileges, ProtectSystem=full (/usr, /boot, /efi, /etc read-only),
//	ProtectHome, PrivateTmp (Postgres uses no Unix sockets, D15),
//	ProtectKernel*, ProtectControlGroups, RestrictSUIDSGID,
//	RestrictRealtime, LockPersonality, an empty capability set (all ports
//	are above 1024).
//
// Left out on purpose: MemoryDenyWriteExecute (breaks Postgres' LLVM JIT),
// PrivateDevices (Postgres' POSIX shared memory lives in /dev/shm),
// ProtectSystem=strict (would need every writable path listed; a moved
// data folder would then fail at start), SystemCallFilter and
// ProtectProc (too easy to break across distributions and Postgres
// versions for little gain).
func renderUnit(o Options) string {
	words := make([]string, 0, len(o.Args)+1)
	words = append(words, systemdQuote(o.Executable))
	for _, a := range o.Args {
		words = append(words, systemdQuote(a))
	}
	var b strings.Builder
	w := func(s string) { b.WriteString(s); b.WriteByte('\n') }
	w("# Installed by `" + brand.Command + " service install`; reinstalling overwrites this file.")
	w("[Unit]")
	w("Description=" + Description)
	w("Wants=network-online.target")
	w("After=network-online.target")
	w("StartLimitIntervalSec=0")
	w("")
	w("[Service]")
	w("Type=simple")
	w("User=" + linuxUser)
	w("Group=" + linuxUser)
	w("ExecStart=" + strings.Join(words, " "))
	w("WorkingDirectory=" + systemdPath(o.DataDir))
	w("UMask=0077")
	w("KillMode=mixed")
	w("KillSignal=SIGTERM")
	w("TimeoutStopSec=" + strconv.Itoa(StopTimeoutSeconds))
	w("Restart=on-failure")
	w("RestartSec=5")
	w("RestartSteps=6")
	w("RestartMaxDelaySec=120")
	w("RestartPreventExitStatus=" + strconv.Itoa(ExitNoRestart))
	w("LimitNOFILE=" + strconv.Itoa(fileLimit))
	w("NoNewPrivileges=yes")
	w("ProtectSystem=full")
	w("ProtectHome=yes")
	w("PrivateTmp=yes")
	w("ProtectKernelTunables=yes")
	w("ProtectKernelModules=yes")
	w("ProtectKernelLogs=yes")
	w("ProtectControlGroups=yes")
	w("RestrictSUIDSGID=yes")
	w("RestrictRealtime=yes")
	w("LockPersonality=yes")
	w("CapabilityBoundingSet=")
	w("AmbientCapabilities=")
	w("")
	w("[Install]")
	w("WantedBy=multi-user.target")
	return b.String()
}

// unixPathChecks rejects locations the service user cannot use.
func unixPathChecks(o Options, goos string) error {
	if homeLike(o.Executable) {
		return fmt.Errorf("the program is inside a home folder (%s), which the background service cannot use. "+
			"Install it first (the installer puts it in /usr/local/bin), then run %s service install from there", o.Executable, brand.Command)
	}
	if homeLike(o.DataDir) {
		def := linuxDataDir
		if goos == "darwin" {
			def = darwinDataDir
		}
		return fmt.Errorf("the data folder %s is inside a home folder, which the background service cannot use; pick one outside, like %s", o.DataDir, def)
	}
	if goos == "linux" {
		for _, p := range []string{"/usr/", "/etc/", "/boot/", "/efi/"} {
			if strings.HasPrefix(o.DataDir+"/", p) {
				return fmt.Errorf("the data folder %s is in a system folder that the service sees read-only; use %s", o.DataDir, linuxDataDir)
			}
		}
	}
	return nil
}

func (l *linuxPlatform) systemd() bool { return isDir(l.h, "/run/systemd/system") }

func (l *linuxPlatform) install(ctx context.Context, o Options) (Report, error) {
	rep := Report{DataDir: o.DataDir}
	if !l.h.Elevated() {
		return rep, elevationError("linux", "install")
	}
	if !l.systemd() {
		return rep, errNoSystemd
	}
	if err := unixPathChecks(o, "linux"); err != nil {
		return rep, err
	}
	if !exists(l.h, o.Executable) {
		return rep, fmt.Errorf("program %s not found", o.Executable)
	}
	uid, gid, err := l.ensureUser(ctx, o.DataDir, &rep)
	if err != nil {
		return rep, err
	}
	if err := prepareDir(l.h, o.DataDir, uid, gid, 0o700, &rep); err != nil {
		return rep, err
	}
	unit := renderUnit(o)
	old, _ := l.h.ReadFile(linuxUnitPath)
	changed := string(old) != unit
	if changed {
		if err := l.h.WriteFileAtomic(linuxUnitPath, []byte(unit), 0o644); err != nil {
			return rep, fmt.Errorf("write %s: %w", linuxUnitPath, err)
		}
		if old == nil {
			rep.step("Created the service definition %s", linuxUnitPath)
		} else {
			rep.step("Updated the service definition %s", linuxUnitPath)
		}
		if _, err := l.h.Run(ctx, "systemctl", "daemon-reload"); err != nil {
			return rep, err
		}
	}
	if out, _ := l.h.Run(ctx, "systemctl", "is-enabled", unitName); strings.TrimSpace(out) != "enabled" {
		if _, err := l.h.Run(ctx, "systemctl", "enable", unitName); err != nil {
			return rep, err
		}
		rep.step("Set the service to start when the computer starts")
	}
	active := l.active(ctx)
	switch {
	case o.Start && active && changed:
		if _, err := l.h.Run(ctx, "systemctl", "restart", unitName); err != nil {
			return rep, err
		}
		rep.step("Restarted the service with the new definition")
	case o.Start && !active:
		if _, err := l.h.Run(ctx, "systemctl", "start", unitName); err != nil {
			return rep, err
		}
		rep.step("Started the service")
	case !o.Start && active && changed:
		rep.note("The service is running with the old definition; restart it to apply: sudo %s service stop, then sudo %s service start", brand.Command, brand.Command)
	}
	if len(rep.Steps) == 0 {
		rep.step("Already installed; nothing to change")
	}
	rep.note("Data folder: %s", o.DataDir)
	return rep, nil
}

func (l *linuxPlatform) active(ctx context.Context) bool {
	out, _ := l.h.Run(ctx, "systemctl", "is-active", unitName)
	s := strings.TrimSpace(out)
	return s == "active" || s == "activating" || s == "reloading"
}

func (l *linuxPlatform) nologin() string {
	for _, p := range []string{"/usr/sbin/nologin", "/sbin/nologin", "/bin/false"} {
		if exists(l.h, p) {
			return p
		}
	}
	return "/bin/false"
}

// lookupIDs returns the uid and gid of name via id(1), which also sees
// users from NSS / directory services.
func lookupIDs(ctx context.Context, h host, name string) (uid, gid int, ok bool, err error) {
	out, err := h.Run(ctx, "id", "-u", name)
	if err != nil {
		if exitCode(err) > 0 {
			return 0, 0, false, nil
		}
		return 0, 0, false, err
	}
	if uid, err = strconv.Atoi(strings.TrimSpace(out)); err != nil {
		return 0, 0, false, fmt.Errorf("id -u %s: %q", name, out)
	}
	out, err = h.Run(ctx, "id", "-g", name)
	if err != nil {
		return 0, 0, false, err
	}
	if gid, err = strconv.Atoi(strings.TrimSpace(out)); err != nil {
		return 0, 0, false, fmt.Errorf("id -g %s: %q", name, out)
	}
	return uid, gid, true, nil
}

func (l *linuxPlatform) ensureUser(ctx context.Context, home string, rep *Report) (int, int, error) {
	uid, gid, ok, err := lookupIDs(ctx, l.h, linuxUser)
	if err != nil {
		return 0, 0, err
	}
	if ok {
		if uid == 0 {
			return 0, 0, fmt.Errorf("user %q exists but is root; refusing to run the database as root", linuxUser)
		}
		return uid, gid, nil
	}
	shell := l.nologin()
	groupExists := false
	if _, err := l.h.Run(ctx, "getent", "group", linuxUser); err == nil {
		groupExists = true
	}
	if _, err := l.h.LookPath("useradd"); err == nil {
		if !groupExists {
			if _, err := l.h.Run(ctx, "groupadd", "--system", linuxUser); err != nil {
				return 0, 0, err
			}
		}
		if _, err := l.h.Run(ctx, "useradd", "--system", "--gid", linuxUser, "--home-dir", home,
			"--no-create-home", "--shell", shell, "--comment", brand.Product+" service", linuxUser); err != nil {
			return 0, 0, err
		}
	} else if _, err := l.h.LookPath("adduser"); err == nil { // BusyBox (Alpine)
		if !groupExists {
			if _, err := l.h.Run(ctx, "addgroup", "-S", linuxUser); err != nil {
				return 0, 0, err
			}
		}
		if _, err := l.h.Run(ctx, "adduser", "-S", "-D", "-H", "-h", home, "-s", shell, "-G", linuxUser, linuxUser); err != nil {
			return 0, 0, err
		}
	} else {
		return 0, 0, fmt.Errorf("cannot create the %q user: neither useradd nor adduser is installed", linuxUser)
	}
	rep.step("Created the system user %q (no login, no password)", linuxUser)
	uid, gid, ok, err = lookupIDs(ctx, l.h, linuxUser)
	if err != nil {
		return 0, 0, err
	}
	if !ok {
		return 0, 0, fmt.Errorf("user %q was created but cannot be found", linuxUser)
	}
	return uid, gid, nil
}

// prepareDir creates dir (if needed) and gives it to uid:gid with perm.
func prepareDir(h host, dir string, uid, gid int, perm fs.FileMode, rep *Report) error {
	created := !exists(h, dir)
	if err := h.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	if err := h.ChownR(dir, uid, gid); err != nil {
		return fmt.Errorf("give %s to the service user: %w", dir, err)
	}
	if err := h.Chmod(dir, perm); err != nil {
		return err
	}
	if created {
		rep.step("Created the folder %s (only the service user can open it)", dir)
	}
	return nil
}

type systemdShow map[string]string

func (l *linuxPlatform) show(ctx context.Context) systemdShow {
	out, _ := l.h.Run(ctx, "systemctl", "show", unitName, "--property=LoadState,ActiveState,SubState,MainPID,WorkingDirectory,FragmentPath")
	m := systemdShow{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			m[k] = v
		}
	}
	return m
}

func (l *linuxPlatform) status(ctx context.Context) (Status, error) {
	if !l.systemd() {
		return Status{}, errNoSystemd
	}
	m := l.show(ctx)
	st := Status{State: m["ActiveState"]}
	if m["LoadState"] == "" || m["LoadState"] == "not-found" {
		return Status{State: "not installed"}, nil
	}
	st.Installed = true
	st.Definition = m["FragmentPath"]
	st.DataDir = unescapeSystemdPath(m["WorkingDirectory"])
	if s := m["SubState"]; s != "" {
		st.State += " (" + s + ")"
	}
	st.Running = m["ActiveState"] == "active" || m["ActiveState"] == "reloading" || m["ActiveState"] == "deactivating"
	st.PID, _ = strconv.Atoi(m["MainPID"])
	return st, nil
}

func (l *linuxPlatform) control(ctx context.Context, verb, action string) error {
	if !l.h.Elevated() {
		return elevationError("linux", action)
	}
	st, err := l.status(ctx)
	if err != nil {
		return err
	}
	if !st.Installed {
		return ErrNotInstalled
	}
	_, err = l.h.Run(ctx, "systemctl", verb, unitName)
	return err
}

func (l *linuxPlatform) start(ctx context.Context) error { return l.control(ctx, "start", "start") }

// stop blocks until systemd reports the unit stopped (bounded by
// TimeoutStopSec).
func (l *linuxPlatform) stop(ctx context.Context) error { return l.control(ctx, "stop", "stop") }

func (l *linuxPlatform) uninstall(ctx context.Context, o UninstallOptions) (Report, error) {
	rep := Report{}
	if !l.h.Elevated() {
		return rep, elevationError("linux", "uninstall")
	}
	dataDir := linuxDataDir
	installed := exists(l.h, linuxUnitPath)
	if l.systemd() {
		if st, err := l.status(ctx); err == nil && st.Installed {
			installed = true
			if st.DataDir != "" {
				dataDir = st.DataDir
			}
			if st.Running {
				rep.step("Stopping the service (this waits for the database to shut down cleanly)")
			}
			if _, err := l.h.Run(ctx, "systemctl", "disable", "--now", unitName); err != nil {
				return rep, err
			}
			rep.step("Stopped the service and removed it from start-up")
		}
	}
	if exists(l.h, linuxUnitPath) {
		if err := l.h.Remove(linuxUnitPath); err != nil {
			return rep, err
		}
		rep.step("Removed %s", linuxUnitPath)
		if l.systemd() {
			if _, err := l.h.Run(ctx, "systemctl", "daemon-reload"); err != nil {
				return rep, err
			}
			_, _ = l.h.Run(ctx, "systemctl", "reset-failed", unitName)
		}
	}
	if !installed {
		rep.step("The background service was not installed")
	}
	rep.DataDir = dataDir
	if !o.Purge {
		if exists(l.h, dataDir) {
			rep.note("Your data is still in %s. To delete it for good: sudo %s service uninstall --purge", dataDir, brand.Command)
		}
		return rep, nil
	}
	if err := purgeDir(l.h, dataDir, &rep); err != nil {
		return rep, err
	}
	if _, _, ok, err := lookupIDs(ctx, l.h, linuxUser); err == nil && ok {
		if _, err := l.h.LookPath("userdel"); err == nil {
			if _, err := l.h.Run(ctx, "userdel", linuxUser); err != nil {
				return rep, err
			}
		} else if _, err := l.h.Run(ctx, "deluser", linuxUser); err != nil {
			return rep, err
		}
		rep.step("Removed the system user %q", linuxUser)
	}
	if _, err := l.h.Run(ctx, "getent", "group", linuxUser); err == nil {
		if _, err := l.h.LookPath("groupdel"); err == nil {
			_, _ = l.h.Run(ctx, "groupdel", linuxUser)
		} else {
			_, _ = l.h.Run(ctx, "delgroup", linuxUser)
		}
	}
	return rep, nil
}

func purgeDir(h host, dir string, rep *Report) error {
	if err := safeToPurge(dir); err != nil {
		return err
	}
	if !exists(h, dir) {
		return nil
	}
	if err := h.RemoveAll(dir); err != nil {
		return fmt.Errorf("delete %s: %w", dir, err)
	}
	rep.step("Deleted the data folder %s", dir)
	return nil
}
