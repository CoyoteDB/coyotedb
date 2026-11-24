package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"coyotedb.com/coyote/internal/cluster"
)

func TestStorageMessage(t *testing.T) {
	raftDir := filepath.Join("data", "node", "cluster", "raft")
	wrap := func(errno error) error {
		pe := &fs.PathError{Op: "write", Path: filepath.Join(raftDir, "raft.db"), Err: errno}
		return fmt.Errorf("%w: %w", cluster.ErrStorage, fmt.Errorf("save the Raft term 7: %w", pe))
	}
	for _, tc := range []struct {
		name string
		err  error
		want []string
	}{
		{"read-only", wrap(readOnlyErrno()), []string{"stopped accepting writes", raftDir + ": read-only file system", "Fix the disk"}},
		{"full", wrap(syscall.ENOSPC), []string{"The disk that holds " + raftDir + " is full", "Free some space"}},
		{"other", wrap(syscall.EIO), []string{"refused a write in " + raftDir, "Check the disk"}},
		{"no path", fmt.Errorf("%w: %w", cluster.ErrStorage, errors.New("boom")), []string{"refused a write in fallback (boom)"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !errors.Is(tc.err, cluster.ErrStorage) {
				t.Fatal("test error does not match ErrStorage")
			}
			msg := storageMessage(tc.err, "fallback")
			for _, w := range append(tc.want, "This node stopped to keep the data safe", "start this node again.") {
				if !strings.Contains(msg, w) {
					t.Errorf("message %q lacks %q", msg, w)
				}
			}
			for _, bad := range []string{"panic", "goroutine", "Raft", "raft.db"} {
				if strings.Contains(msg, bad) {
					t.Errorf("message %q has %q", msg, bad)
				}
			}
		})
	}
}

func TestNextStepReadOnly(t *testing.T) {
	err := fmt.Errorf("raft bootstrap: %w", &fs.PathError{Op: "write", Path: "x/raft.db", Err: readOnlyErrno()})
	if got := NextStep(err, "/srv/coyote"); !strings.Contains(got, "/srv/coyote does not accept writes (it is read-only)") {
		t.Fatalf("NextStep = %q", got)
	}
}
