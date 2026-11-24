package migrate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// tool is one run of pg_dump or pg_restore.
type tool struct {
	Name string   // for messages
	Bin  string   // absolute path
	Args []string // never holds a password
	// Password, if set, is passed in PGPASSWORD (never on the command line).
	Password string
	// Log, if set, receives the raw output.
	Log io.Writer
	// OnLine sees every output line (progress, errors).
	OnLine func(string)
}

// toolEnv is this process's environment without libpq variables (they could
// point the tool at another server), plus a fixed C locale so the output we
// read is never translated.
func toolEnv(password string) []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(strings.ToUpper(k), "PG") || k == "LC_ALL" || k == "LANG" || k == "LC_MESSAGES" {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "LC_ALL=C", "LANG=C", "PGAPPNAME=coyote-migrate", "PGCONNECT_TIMEOUT=15")
	if password != "" {
		env = append(env, "PGPASSWORD="+password)
	}
	return env
}

// run runs the tool to completion. Cancelling ctx kills it and its
// children (pg_dump and pg_restore fork workers for parallel jobs on Unix).
// The tool runs in its own process group, so it never outlives this
// command (Linux also sets a parent-death signal). It returns the last
// lines of output for error messages.
func (t tool) run(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	out := &lines{raw: t.Log, onLine: t.OnLine}
	cmd := exec.CommandContext(ctx, t.Bin, t.Args...)
	cmd.Env = toolEnv(t.Password)
	cmd.Stdout = out
	cmd.Stderr = out
	setProcAttr(cmd)
	cmd.Cancel = func() error { killTree(cmd.Process); return nil }
	cmd.WaitDelay = 10 * time.Second
	err := cmd.Run()
	out.flush()
	if ctx.Err() != nil {
		return out.tail(), ctx.Err()
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return out.tail(), fmt.Errorf("%s failed (%s)", t.Name, ee.ProcessState.String())
		}
		return out.tail(), fmt.Errorf("%s: %w", t.Name, err)
	}
	return out.tail(), nil
}

// lines splits output into lines (also on '\r'), copies it raw to a log,
// and keeps the last lines.
type lines struct {
	mu     sync.Mutex
	raw    io.Writer
	onLine func(string)
	buf    bytes.Buffer
	last   []string
}

func (l *lines) Write(p []byte) (int, error) {
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

func (l *lines) emit() {
	s := strings.TrimRight(l.buf.String(), " \t")
	l.buf.Reset()
	if s == "" {
		return
	}
	if l.onLine != nil {
		l.onLine(s)
	}
	l.last = append(l.last, s)
	if len(l.last) > 20 {
		l.last = l.last[len(l.last)-20:]
	}
}

func (l *lines) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.emit()
}

func (l *lines) tail() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.last, "\n")
}
