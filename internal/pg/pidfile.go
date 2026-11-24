package pg

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// PidFile is the parsed content of a Postgres postmaster.pid.
//
// Layout (src/include/utils/pidfile.h):
//
//	1 PID   2 DATADIR   3 START_TIME   4 PORT   5 SOCKET_DIR
//	6 LISTEN_ADDR   7 SHMEM_KEY   8 STATUS ("starting", "stopping",
//	"ready", "standby")
//
// Lines after the first may be missing while Postgres is starting.
type PidFile struct {
	PID       int
	DataDir   string
	StartTime int64
	Port      int
	SocketDir string
	Listen    string
	Status    string
}

// PostmasterPidPath is the location of postmaster.pid in a data dir.
func PostmasterPidPath(pgdata string) string { return filepath.Join(pgdata, "postmaster.pid") }

// ReadPidFile reads and parses postmaster.pid. It returns (nil, nil) if the
// file does not exist.
func ReadPidFile(pgdata string) (*PidFile, error) {
	b, err := os.ReadFile(PostmasterPidPath(pgdata))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ParsePidFile(string(b))
}

// ParsePidFile parses postmaster.pid content.
func ParsePidFile(s string) (*PidFile, error) {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	get := func(i int) string {
		if i < len(lines) {
			return strings.TrimSpace(lines[i])
		}
		return ""
	}
	pidStr := get(0)
	if pidStr == "" {
		return nil, errors.New("postmaster.pid is empty")
	}
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		return nil, fmt.Errorf("postmaster.pid: bad pid %q", pidStr)
	}
	// A negative pid means a single-user backend owns the directory.
	if pid < 0 {
		pid = -pid
	}
	if pid == 0 {
		return nil, errors.New("postmaster.pid: pid is zero")
	}
	p := &PidFile{PID: pid, DataDir: get(1), SocketDir: get(4), Listen: get(5), Status: get(7)}
	if v := get(2); v != "" {
		p.StartTime, _ = strconv.ParseInt(v, 10, 64)
	}
	if v := get(3); v != "" {
		p.Port, _ = strconv.Atoi(v)
	}
	return p, nil
}
