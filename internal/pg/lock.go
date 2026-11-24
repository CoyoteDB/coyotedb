package pg

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

// errLocked is returned by tryLock when another process holds the lock.
var errLocked = errors.New("locked by another process")

// FileLock is an exclusive, advisory, cross-process lock on a file. The lock
// is tied to the open file: it is released when every copy of the handle is
// closed (including copies inherited by child processes on Unix).
type FileLock struct {
	f *os.File
}

// File returns the underlying file (used to hand the lock to the watchdog).
func (l *FileLock) File() *os.File { return l.f }

// Unlock releases the lock.
func (l *FileLock) Unlock() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// AcquireLock takes an exclusive lock on path, polling until ctx is done.
// onWait is called once if the lock is not immediately available.
func AcquireLock(ctx context.Context, path string, onWait func()) (*FileLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	waited := false
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		err := tryLock(f)
		if err == nil {
			return &FileLock{f: f}, nil
		}
		if !errors.Is(err, errLocked) {
			f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		if !waited && onWait != nil {
			onWait()
		}
		waited = true
		select {
		case <-ctx.Done():
			f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, ctx.Err())
		case <-tick.C:
		}
	}
}
