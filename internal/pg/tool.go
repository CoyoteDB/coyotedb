package pg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// toolSpec is one run of a Postgres program that touches a data dir
// (pg_basebackup, pg_rewind, a single-user postgres for crash recovery).
type toolSpec struct {
	Name    string   // short name for logs
	Bin     string   // absolute path
	Args    []string // never contains a password
	Env     []string // full environment (use toolEnv)
	LogFile string   // raw output is appended here
	// OnLine, if set, sees every output line (for progress reports).
	OnLine func(string)
}

// toolEnv is cleanEnv plus the given variables and a fixed C locale so
// output we parse is never translated.
func toolEnv(extra ...string) []string {
	env := append(cleanEnv(), "LC_ALL=C", "LANG=C")
	return append(env, extra...)
}

// runTool runs a tool under the guard (safety rule 5: nothing we start may
// outlive us). On Unix the tool leads its own process group, which the
// watchdog kills if the agent dies (Linux also sets a parent-death signal);
// on Windows the Job Object covers it. Cancelling ctx kills the group.
// It returns the last lines of output for error messages.
func runTool(ctx context.Context, g *Guard, log *slog.Logger, sp toolSpec) (string, error) {
	if g == nil {
		return "", fmt.Errorf("refusing to run %s without a process guard (safety rule 5)", sp.Name)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var lf *os.File
	if sp.LogFile != "" {
		f, err := os.OpenFile(sp.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return "", err
		}
		defer f.Close()
		lf = f
	}
	out := &lineLog{log: log, tool: sp.Name, raw: lf, onLine: sp.OnLine}
	cmd := exec.Command(sp.Bin, sp.Args...)
	cmd.Env = sp.Env
	cmd.Stdout = out
	cmd.Stderr = out
	g.prepareTool(cmd)
	log.Info("running "+sp.Name, "bin", sp.Bin, "args", sp.Args)
	waitCh, err := startProcess(cmd)
	if err != nil {
		return "", fmt.Errorf("start %s: %w", sp.Name, err)
	}
	pid := cmd.Process.Pid
	if err := g.trackTool(pid); err != nil {
		killTool(cmd.Process)
		<-waitCh
		return "", fmt.Errorf("register %s with guard: %w", sp.Name, err)
	}
	defer g.untrackTool(pid)

	var werr error
	select {
	case werr = <-waitCh:
	case <-ctx.Done():
		log.Warn("cancelled; killing "+sp.Name, "pid", pid)
		killTool(cmd.Process)
		<-waitCh
		out.flush()
		return out.tail(), ctx.Err()
	}
	out.flush()
	if werr != nil {
		var ee *exec.ExitError
		if errors.As(werr, &ee) {
			return out.tail(), fmt.Errorf("%s failed (%s)", sp.Name, ee.ProcessState.String())
		}
		return out.tail(), fmt.Errorf("%s: %w", sp.Name, werr)
	}
	log.Info(sp.Name + " finished")
	return out.tail(), nil
}

// lineLog splits tool output on '\n' and '\r' (progress lines), logs each
// line, copies raw output to a file and keeps the last lines.
type lineLog struct {
	mu   sync.Mutex
	log  *slog.Logger
	tool string
	raw  *os.File
	// onLine is called with each line, under mu (keep it quick).
	onLine func(string)
	buf    bytes.Buffer
	last   []string
}

func (l *lineLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.raw != nil {
		_, _ = l.raw.Write(p)
	}
	for _, c := range p {
		if c == '\n' || c == '\r' {
			l.emit()
			continue
		}
		l.buf.WriteByte(c)
	}
	return len(p), nil
}

func (l *lineLog) emit() {
	s := strings.TrimSpace(l.buf.String())
	l.buf.Reset()
	if s == "" {
		return
	}
	l.log.Info(s, "tool", l.tool)
	if l.onLine != nil {
		l.onLine(s)
	}
	l.last = append(l.last, s)
	if len(l.last) > 20 {
		l.last = l.last[len(l.last)-20:]
	}
}

func (l *lineLog) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.emit()
}

func (l *lineLog) tail() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.last, "\n")
}
