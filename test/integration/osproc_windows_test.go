//go:build integration && windows

// OS-specific process helpers for the integration tests (Windows). See
// osproc_unix_test.go for the contract. Run notes: docs/TESTING.md
// (Windows section).
package integration

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

const exeSuffix = ".exe"

// prepareAgent starts coyote in its own process group so interrupt can
// send it Ctrl+Break without hitting the test (Go maps Ctrl+Break to
// os.Interrupt, as Ctrl+C in a terminal).
func prepareAgent(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
}

func interrupt(p *os.Process) error {
	return windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(p.Pid))
}

func killPID(pid int) error {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return windows.TerminateProcess(h, 1)
}

var (
	ntdll           = windows.NewLazySystemDLL("ntdll.dll")
	ntSuspendProces = ntdll.NewProc("NtSuspendProcess")
	ntResumeProcess = ntdll.NewProc("NtResumeProcess")
)

// freezePID is SIGSTOP: every thread of the process is suspended.
func freezePID(pid int) error { return suspendResume(pid, ntSuspendProces) }

func thawPID(pid int) error { return suspendResume(pid, ntResumeProcess) }

func suspendResume(pid int, p *windows.LazyProc) error {
	h, err := windows.OpenProcess(windows.PROCESS_SUSPEND_RESUME, false, uint32(pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	if r, _, _ := p.Call(uintptr(h)); r != 0 {
		return windows.NTStatus(r)
	}
	return nil
}

// quitPostgres: Windows has no SIGQUIT; terminate the postmaster and its
// children (what an immediate shutdown amounts to for a stray server).
func quitPostgres(pid int) {
	_ = killPID(pid)
	for _, p := range snapshot() {
		if p.ppid == pid && strings.EqualFold(p.name, "postgres.exe") {
			_ = killPID(p.pid)
		}
	}
}

func alive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return true
	}
	return code == 259 // STILL_ACTIVE
}

// postmasterGroupGone: no process groups here; the postmaster and every
// postgres.exe it started (Windows keeps the parent pid) have exited.
func postmasterGroupGone(t *testing.T, pid int) bool {
	if alive(pid) {
		return false
	}
	for _, p := range snapshot() {
		if p.ppid == pid && strings.EqualFold(p.name, "postgres.exe") {
			return false
		}
	}
	return true
}

// argForm is how Go's os/exec writes an argument into a command line.
func argForm(s string) string { return syscall.EscapeArg(s) }

// isPostmasterCmd matches "…\bin\postgres.exe … -D <pgdata>". The agent
// writes pgdata as given (quoted when it has spaces); pg_ctl writes it with
// forward slashes. Windows paths are case-insensitive.
func isPostmasterCmd(cmdline, pgdata string) bool {
	norm := func(s string) string {
		return strings.ReplaceAll(strings.ReplaceAll(strings.ToLower(s), "/", `\`), `"`, "")
	}
	c := norm(cmdline)
	return strings.HasSuffix(c, " -d "+norm(pgdata)) && strings.Contains(c, `\bin\postgres.exe`)
}

type proc struct {
	pid  int
	cmd  string
	pgid int // the parent pid (Windows has no process groups)
}

type snap struct {
	pid, ppid int
	name      string
}

func snapshot() []snap {
	s, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(s)
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	var out []snap
	for err = windows.Process32First(s, &e); err == nil; err = windows.Process32Next(s, &e) {
		out = append(out, snap{pid: int(e.ProcessID), ppid: int(e.ParentProcessID), name: windows.UTF16ToString(e.ExeFile[:])})
	}
	return out
}

// listProcs lists every process whose command line can be read (ours and
// our children's; others are skipped).
func listProcs(t *testing.T) []proc {
	t.Helper()
	var ps []proc
	for _, p := range snapshot() {
		if cmd, ok := cmdline(p.pid); ok {
			ps = append(ps, proc{pid: p.pid, cmd: cmd, pgid: p.ppid})
		}
	}
	return ps
}

func cmdline(pid int) (string, bool) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", false
	}
	defer windows.CloseHandle(h)
	buf := make([]byte, 64*1024)
	var n uint32
	if err := windows.NtQueryInformationProcess(h, windows.ProcessCommandLineInformation, unsafe.Pointer(&buf[0]), uint32(len(buf)), &n); err != nil {
		return "", false
	}
	return strings.TrimSpace((*windows.NTUnicodeString)(unsafe.Pointer(&buf[0])).String()), true
}

// privateFile: only SYSTEM, Administrators and the user running the test
// (who runs the agent) have any access (Windows ignores mode bits).
func privateFile(path string) error {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	if dacl == nil {
		return fmt.Errorf("no DACL: everyone has access")
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	system, _ := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	admins, _ := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	for i := range uint32(dacl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return err
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			continue
		}
		s := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !s.Equals(user.User.Sid) && !s.Equals(system) && !s.Equals(admins) {
			return fmt.Errorf("%s has access", s)
		}
	}
	return nil
}
