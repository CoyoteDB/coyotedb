//go:build !windows

package svc

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"coyotedb.com/coyote/internal/durable"
)

type osHost struct{}

func (osHost) Elevated() bool { return os.Geteuid() == 0 }

func (osHost) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "SYSTEMD_PAGER=", "SYSTEMD_COLORS=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), &runError{cmd: name + " " + strings.Join(args, " "), out: strings.TrimSpace(string(out)), err: err}
	}
	return string(out), nil
}

func (osHost) LookPath(name string) (string, error) { return exec.LookPath(name) }

func (osHost) Stat(p string) (fs.FileInfo, error) { return os.Stat(p) }

func (osHost) ReadFile(p string) ([]byte, error) { return os.ReadFile(p) }

func (osHost) WriteFileAtomic(p string, data []byte, perm fs.FileMode) error {
	return durable.WriteFile(p, data, perm)
}

func (osHost) MkdirAll(p string, perm fs.FileMode) error { return os.MkdirAll(p, perm) }

func (osHost) Chmod(p string, perm fs.FileMode) error { return os.Chmod(p, perm) }

func (osHost) ChownR(root string, uid, gid int) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(p, uid, gid)
	})
}

func (osHost) RemoveAll(p string) error { return os.RemoveAll(p) }

func (osHost) Remove(p string) error { return os.Remove(p) }

// runError keeps the exit code reachable and the output in the message.
type runError struct {
	cmd, out string
	err      error
}

func (e *runError) Error() string {
	if e.out == "" {
		return fmt.Sprintf("%s: %v", e.cmd, e.err)
	}
	return fmt.Sprintf("%s: %v: %s", e.cmd, e.err, e.out)
}

func (e *runError) Unwrap() error { return e.err }
