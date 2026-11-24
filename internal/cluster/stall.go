package cluster

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/raft"
)

// stallFaultEnv (tests only) makes this node's Raft log store slow, like
// a disk whose fsync stalls: while the file named by the variable holds a
// Unix time in nanoseconds that is still ahead, every log write waits
// until then. Unset in normal use, and then the store is not wrapped.
const stallFaultEnv = "COYOTE_TEST_RAFT_STALL_FILE"

// stallingLogs is a raft.LogStore whose writes can be held up by
// stallFaultEnv.
type stallingLogs struct {
	raft.LogStore
	file string
}

func withStallFault(s raft.LogStore, dir string) raft.LogStore {
	f := os.Getenv(stallFaultEnv)
	if f == "" {
		return s
	}
	if !filepath.IsAbs(f) {
		f = filepath.Join(dir, f)
	}
	return &stallingLogs{LogStore: s, file: f}
}

func (s *stallingLogs) wait() {
	b, err := os.ReadFile(s.file)
	if err != nil {
		return
	}
	ns, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return
	}
	if d := time.Until(time.Unix(0, ns)); d > 0 {
		time.Sleep(d)
	}
}

func (s *stallingLogs) StoreLog(l *raft.Log) error {
	s.wait()
	return s.LogStore.StoreLog(l)
}

func (s *stallingLogs) StoreLogs(ls []*raft.Log) error {
	s.wait()
	return s.LogStore.StoreLogs(ls)
}
