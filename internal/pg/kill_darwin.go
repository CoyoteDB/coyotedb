//go:build darwin

package pg

import "golang.org/x/sys/unix"

type procRec struct {
	ppid  int
	start int64
}

func startOf(k *unix.KinfoProc) int64 {
	t := k.Proc.P_starttime
	return int64(t.Sec)*1_000_000 + int64(t.Usec)
}

func procInfo(pid int) (procRec, error) {
	k, err := kinfo(pid)
	if err != nil {
		return procRec{}, errNoProc
	}
	return procRec{ppid: int(k.Eproc.Ppid), start: startOf(k)}, nil
}

// childProcs lists the processes whose parent is ppid.
func childProcs(ppid int) ([]procID, error) {
	all, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	var out []procID
	for i := range all {
		k := &all[i]
		if int(k.Eproc.Ppid) == ppid && int(k.Proc.P_pid) != ppid {
			out = append(out, procID{pid: int(k.Proc.P_pid), start: startOf(k)})
		}
	}
	return out, nil
}
