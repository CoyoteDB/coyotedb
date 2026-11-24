package svc

import (
	"context"
	"errors"
	"io/fs"
)

// host is the operating system as the Unix platforms use it. The real one
// is in host_unix.go; tests use a fake that records every call.
type host interface {
	Elevated() bool
	// Run executes name with args (never through a shell) and returns
	// its combined output. A non-zero exit is an error with ExitCode().
	Run(ctx context.Context, name string, args ...string) (string, error)
	LookPath(name string) (string, error)
	Stat(path string) (fs.FileInfo, error)
	ReadFile(path string) ([]byte, error)
	// WriteFileAtomic writes via a temp file + rename, owned by root.
	WriteFileAtomic(path string, data []byte, perm fs.FileMode) error
	MkdirAll(path string, perm fs.FileMode) error
	Chmod(path string, perm fs.FileMode) error
	// ChownR changes owner of path and everything under it, never
	// following symlinks.
	ChownR(path string, uid, gid int) error
	RemoveAll(path string) error
	Remove(path string) error
}

// exitCode returns the exit code of a failed Run, or -1.
func exitCode(err error) int {
	var ec interface{ ExitCode() int }
	if errors.As(err, &ec) {
		return ec.ExitCode()
	}
	return -1
}

func exists(h host, path string) bool {
	_, err := h.Stat(path)
	return err == nil
}

func isDir(h host, path string) bool {
	fi, err := h.Stat(path)
	return err == nil && fi.IsDir()
}
