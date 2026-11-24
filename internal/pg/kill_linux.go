//go:build linux

package pg

import (
	"bytes"
	"errors"
	"os"
	"strconv"
)

// statFields returns the fields of /proc/pid/stat after "(comm)".
func statFields(pid int) ([][]byte, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if errors.Is(err, os.ErrNotExist) {
		return nil, errNoProc
	}
	if err != nil {
		return nil, err
	}
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return nil, errors.New("unreadable /proc stat")
	}
	f := bytes.Fields(b[i+1:])
	if len(f) < 20 {
		return nil, errors.New("short /proc stat")
	}
	return f, nil // f[0] state, f[1] ppid, f[19] starttime
}

type procRec struct {
	ppid  int
	start int64
}

func procInfo(pid int) (procRec, error) {
	f, err := statFields(pid)
	if err != nil {
		return procRec{}, err
	}
	ppid, err1 := strconv.Atoi(string(f[1]))
	start, err2 := strconv.ParseInt(string(f[19]), 10, 64)
	if err1 != nil || err2 != nil {
		return procRec{}, errors.New("unreadable /proc stat")
	}
	return procRec{ppid: ppid, start: start}, nil
}

// childProcs lists the live processes whose parent is ppid.
func childProcs(ppid int) ([]procID, error) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var out []procID
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == ppid {
			continue
		}
		p, err := procInfo(pid)
		if err != nil || p.ppid != ppid {
			continue
		}
		out = append(out, procID{pid: pid, start: p.start})
	}
	return out, nil
}
