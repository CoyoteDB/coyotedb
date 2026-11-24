//go:build windows

package svc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"coyotedb.com/coyote/internal/brand"
	"coyotedb.com/coyote/internal/paths"
)

type winPlatform struct{ log *slog.Logger }

func newPlatform(log *slog.Logger) platform { return &winPlatform{log: log} }

func (w *winPlatform) defaultDataDir() string {
	if d, err := paths.SystemDataDir(); err == nil {
		return d
	}
	return filepath.Join(`C:\ProgramData`, brand.DirName)
}

// elevated reports whether this process can manage services, the firewall
// and the data folder's ACL: an elevated token that also holds an enabled
// Administrators group. Checking IsElevated alone is not enough: a token
// restricted by a launcher (Administrators deny-only) still reports
// elevated, and install then failed half-way with "Access is denied"
// instead of the plain "needs administrator rights" (found on a Windows test PC).
func elevated() bool {
	if !windows.GetCurrentProcessToken().IsElevated() {
		return false
	}
	adm, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return false
	}
	member, err := windows.Token(0).IsMember(adm) // effective token, deny-only groups do not count
	return err == nil && member
}

// netsh runs %SystemRoot%\System32\netsh.exe with an exact command line
// (see netshCmdLine); no shell is involved.
func netsh(ctx context.Context, args []string) (string, error) {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	exe := filepath.Join(root, "System32", "netsh.exe")
	line, err := netshCmdLine(exe, args)
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, exe)
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: line, HideWindow: true}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("netsh %s: %w: %s", args[2], err, out)
	}
	return string(out), nil
}

func (w *winPlatform) serviceConfig(o Options) mgr.Config {
	return mgr.Config{
		ServiceType:      windows.SERVICE_WIN32_OWN_PROCESS,
		StartType:        mgr.StartAutomatic,
		ErrorControl:     mgr.ErrorNormal,
		BinaryPathName:   winBinaryPath(o.Executable, o.Args),
		ServiceStartName: winAccount,
		DisplayName:      DisplayName,
		Description:      Description + ". Data: " + o.DataDir,
		SidType:          windows.SERVICE_SID_TYPE_UNRESTRICTED,
	}
}

type preshutdownInfo struct{ timeout uint32 }

func (w *winPlatform) install(ctx context.Context, o Options) (Report, error) {
	rep := Report{DataDir: o.DataDir}
	if !elevated() {
		return rep, elevationError("windows", "install")
	}
	if _, err := os.Stat(o.Executable); err != nil {
		return rep, fmt.Errorf("program %s not found: %w", o.Executable, err)
	}
	m, err := mgr.Connect()
	if err != nil {
		return rep, fmt.Errorf("connect to the service manager: %w", err)
	}
	defer m.Disconnect()

	cfg := w.serviceConfig(o)
	s, err := m.OpenService(Name)
	switch {
	case err == nil:
		cur, cerr := s.Config()
		if cerr != nil {
			s.Close()
			return rep, cerr
		}
		if cur.BinaryPathName != cfg.BinaryPathName || cur.ServiceStartName != cfg.ServiceStartName ||
			cur.StartType != cfg.StartType || cur.SidType != cfg.SidType || cur.Description != cfg.Description ||
			cur.DisplayName != cfg.DisplayName {
			if err := s.UpdateConfig(cfg); err != nil {
				s.Close()
				return rep, fmt.Errorf("update the service: %w", err)
			}
			rep.step("Updated the service %q", Name)
		}
	case errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST):
		s, err = m.CreateService(Name, o.Executable, cfg, o.Args...)
		if err != nil {
			return rep, fmt.Errorf("create the service: %w", err)
		}
		rep.step("Created the service %q, running as %s (not an administrator), starting with Windows", Name, winAccount)
	default:
		return rep, fmt.Errorf("open the service: %w", err)
	}
	defer s.Close()

	actions := make([]mgr.RecoveryAction, len(winRecovery))
	for i, d := range winRecovery {
		actions[i] = mgr.RecoveryAction{Type: mgr.ServiceRestart, Delay: d}
	}
	if err := s.SetRecoveryActions(actions, uint32(winResetPeriod/time.Second)); err != nil {
		return rep, fmt.Errorf("set restart-on-failure: %w", err)
	}
	if err := s.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return rep, fmt.Errorf("set restart-on-failure: %w", err)
	}
	info := preshutdownInfo{timeout: StopTimeoutSeconds * 1000}
	if err := windows.ChangeServiceConfig2(s.Handle, windows.SERVICE_CONFIG_PRESHUTDOWN_INFO, (*byte)(unsafe.Pointer(&info))); err != nil {
		return rep, fmt.Errorf("set the shutdown grace period: %w", err)
	}

	created := false
	if _, err := os.Stat(o.DataDir); errors.Is(err, os.ErrNotExist) {
		created = true
	}
	if err := os.MkdirAll(o.DataDir, 0o700); err != nil {
		return rep, fmt.Errorf("create %s: %w", o.DataDir, err)
	}
	if err := setDirACL(o.DataDir, dataDirSDDL(Name)); err != nil {
		return rep, fmt.Errorf("protect %s: %w", o.DataDir, err)
	}
	if created {
		rep.step("Created the folder %s (only the service, SYSTEM and administrators can open it)", o.DataDir)
	}

	if _, err := netsh(ctx, firewallShowArgs()); err == nil {
		if _, err := netsh(ctx, firewallDeleteArgs()); err != nil {
			return rep, err
		}
	}
	if _, err := netsh(ctx, firewallAddArgs(o.Executable, o.FirewallPorts)); err != nil {
		return rep, err
	}
	rep.step("Allowed %s through Windows Firewall on ports %s (private and work networks only)", brand.Command, joinPorts(o.FirewallPorts))

	if o.Start {
		if err := startService(s); err != nil {
			return rep, err
		}
		rep.step("Started the service")
	}
	rep.note("Data folder: %s", o.DataDir)
	return rep, nil
}

func setDirACL(dir, sddl string) error {
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		owner, nil, dacl, nil)
}

func startService(s *mgr.Service) error {
	if err := s.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
		return fmt.Errorf("start the service: %w", err)
	}
	return nil
}

// stopService asks the service to stop and waits for it (bounded).
func stopService(ctx context.Context, s *mgr.Service) error {
	st, err := s.Control(svc.Stop)
	if err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
			return nil
		}
		return fmt.Errorf("stop the service: %w", err)
	}
	deadline := time.Now().Add((StopTimeoutSeconds + 30) * time.Second)
	for st.State != svc.Stopped {
		if time.Now().After(deadline) {
			return fmt.Errorf("the service did not stop within %d seconds", StopTimeoutSeconds+30)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
		if st, err = s.Query(); err != nil {
			return err
		}
	}
	return nil
}

func (w *winPlatform) open() (*mgr.Mgr, *mgr.Service, error) {
	m, err := mgr.Connect()
	if err != nil {
		return nil, nil, fmt.Errorf("connect to the service manager: %w", err)
	}
	s, err := m.OpenService(Name)
	if err != nil {
		m.Disconnect()
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return nil, nil, ErrNotInstalled
		}
		return nil, nil, err
	}
	return m, s, nil
}

func (w *winPlatform) start(ctx context.Context) error {
	if !elevated() {
		return elevationError("windows", "start")
	}
	m, s, err := w.open()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	defer s.Close()
	return startService(s)
}

func (w *winPlatform) stop(ctx context.Context) error {
	if !elevated() {
		return elevationError("windows", "stop")
	}
	m, s, err := w.open()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	defer s.Close()
	return stopService(ctx, s)
}

// openQuery opens the service with query rights only, which every user
// has, so status works without elevation.
func openQuery() (*mgr.Mgr, *mgr.Service, error) {
	h, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return nil, nil, fmt.Errorf("connect to the service manager: %w", err)
	}
	m := &mgr.Mgr{Handle: h}
	name, err := windows.UTF16PtrFromString(Name)
	if err != nil {
		m.Disconnect()
		return nil, nil, err
	}
	sh, err := windows.OpenService(h, name, windows.SERVICE_QUERY_STATUS|windows.SERVICE_QUERY_CONFIG)
	if err != nil {
		m.Disconnect()
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return nil, nil, ErrNotInstalled
		}
		return nil, nil, err
	}
	return m, &mgr.Service{Name: Name, Handle: sh}, nil
}

func (w *winPlatform) status(ctx context.Context) (Status, error) {
	m, s, err := openQuery()
	if errors.Is(err, ErrNotInstalled) {
		return Status{State: "not installed"}, nil
	}
	if err != nil {
		return Status{}, err
	}
	defer m.Disconnect()
	defer s.Close()
	q, err := s.Query()
	if err != nil {
		return Status{}, err
	}
	st := Status{Installed: true, Definition: `HKLM\SYSTEM\CurrentControlSet\Services\` + Name, PID: int(q.ProcessId)}
	st.State = map[svc.State]string{
		svc.Stopped: "stopped", svc.StartPending: "starting", svc.StopPending: "stopping",
		svc.Running: "running", svc.ContinuePending: "continuing", svc.PausePending: "pausing", svc.Paused: "paused",
	}[q.State]
	st.Running = q.State == svc.Running || q.State == svc.StopPending
	if cfg, err := s.Config(); err == nil {
		if args, err := windows.DecomposeCommandLine(cfg.BinaryPathName); err == nil {
			st.DataDir = dataDirFromArgs(args)
		}
	}
	return st, nil
}

func (w *winPlatform) uninstall(ctx context.Context, o UninstallOptions) (Report, error) {
	rep := Report{}
	if !elevated() {
		return rep, elevationError("windows", "uninstall")
	}
	dataDir := w.defaultDataDir()
	m, s, err := w.open()
	switch {
	case err == nil:
		if cfg, err := s.Config(); err == nil {
			if args, err := windows.DecomposeCommandLine(cfg.BinaryPathName); err == nil {
				if d := dataDirFromArgs(args); d != "" {
					dataDir = d
				}
			}
		}
		if err := stopService(ctx, s); err != nil {
			s.Close()
			m.Disconnect()
			return rep, err
		}
		err = s.Delete()
		s.Close()
		m.Disconnect()
		if err != nil && !errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
			return rep, fmt.Errorf("remove the service: %w", err)
		}
		rep.step("Stopped and removed the service %q", Name)
	case errors.Is(err, ErrNotInstalled):
		rep.step("The background service was not installed")
	default:
		return rep, err
	}
	if _, err := netsh(ctx, firewallShowArgs()); err == nil {
		if _, err := netsh(ctx, firewallDeleteArgs()); err != nil {
			return rep, err
		}
		rep.step("Removed the Windows Firewall rule %q", winFirewallRule)
	}
	rep.DataDir = dataDir
	_, statErr := os.Stat(dataDir)
	if !o.Purge {
		if statErr == nil {
			rep.note("Your data is still in %s. To delete it for good, run as administrator: %s service uninstall --purge", dataDir, brand.Command)
		}
		return rep, nil
	}
	if err := safeToPurge(dataDir); err != nil {
		return rep, err
	}
	if statErr == nil {
		if err := os.RemoveAll(dataDir); err != nil {
			return rep, fmt.Errorf("delete %s: %w", dataDir, err)
		}
		rep.step("Deleted the data folder %s", dataDir)
	}
	return rep, nil
}
