package svc

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeSys is a tiny simulated Unix machine: files, users, and the
// systemctl / launchctl / dscl / id commands the platforms use. It records
// every command so tests can assert exactly what would run.
type fakeSys struct {
	t        *testing.T
	root     bool
	systemd  bool
	files    map[string][]byte
	perms    map[string]fs.FileMode
	dirs     map[string]bool
	owner    map[string][2]int
	users    map[string][2]int // name -> uid, gid
	groups   map[string]int
	bins     map[string]bool // on PATH
	enabled  bool
	active   bool
	loaded   bool
	stopping int // launchctl: prints still "loaded" this many times after bootout
	calls    []string
	writes   int
	failCmd  string // a command prefix that fails
}

type exitErr struct{ code int }

func (e exitErr) Error() string { return "exit status " + strconv.Itoa(e.code) }
func (e exitErr) ExitCode() int { return e.code }

func newFakeSys(t *testing.T) *fakeSys {
	return &fakeSys{
		t: t, root: true, systemd: true,
		files: map[string][]byte{}, perms: map[string]fs.FileMode{}, dirs: map[string]bool{"/": true},
		owner: map[string][2]int{}, users: map[string][2]int{"root": {0, 0}, "_www": {70, 70}},
		groups: map[string]int{"wheel": 0, "admin": 80},
		bins:   map[string]bool{"useradd": true, "userdel": true, "groupdel": true},
	}
}

func (f *fakeSys) addFile(p string) {
	f.files[p] = []byte("binary")
	f.mkdirs(path.Dir(p))
}

func (f *fakeSys) mkdirs(p string) {
	for p != "/" && p != "." {
		f.dirs[p] = true
		p = path.Dir(p)
	}
}

type fakeInfo struct {
	name string
	dir  bool
}

func (i fakeInfo) Name() string       { return i.name }
func (i fakeInfo) Size() int64        { return 0 }
func (i fakeInfo) Mode() fs.FileMode  { return 0 }
func (i fakeInfo) ModTime() time.Time { return time.Time{} }
func (i fakeInfo) IsDir() bool        { return i.dir }
func (i fakeInfo) Sys() any           { return nil }

func (f *fakeSys) Elevated() bool { return f.root }

func (f *fakeSys) LookPath(name string) (string, error) {
	if f.bins[name] {
		return "/usr/sbin/" + name, nil
	}
	return "", errors.New("not found")
}

func (f *fakeSys) Stat(p string) (fs.FileInfo, error) {
	if p == "/run/systemd/system" && f.systemd {
		return fakeInfo{"system", true}, nil
	}
	if f.dirs[p] {
		return fakeInfo{path.Base(p), true}, nil
	}
	if _, ok := f.files[p]; ok {
		return fakeInfo{path.Base(p), false}, nil
	}
	return nil, fs.ErrNotExist
}

func (f *fakeSys) ReadFile(p string) ([]byte, error) {
	b, ok := f.files[p]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return b, nil
}

func (f *fakeSys) WriteFileAtomic(p string, data []byte, perm fs.FileMode) error {
	f.writes++
	f.files[p] = append([]byte(nil), data...)
	f.perms[p] = perm
	return nil
}

func (f *fakeSys) MkdirAll(p string, perm fs.FileMode) error {
	f.mkdirs(p)
	if _, ok := f.perms[p]; !ok {
		f.perms[p] = perm
	}
	return nil
}

func (f *fakeSys) Chmod(p string, perm fs.FileMode) error { f.perms[p] = perm; return nil }

func (f *fakeSys) ChownR(p string, uid, gid int) error {
	for d := range f.dirs {
		if d == p || strings.HasPrefix(d, p+"/") {
			f.owner[d] = [2]int{uid, gid}
		}
	}
	return nil
}

func (f *fakeSys) RemoveAll(p string) error {
	for d := range f.dirs {
		if d == p || strings.HasPrefix(d, p+"/") {
			delete(f.dirs, d)
		}
	}
	for n := range f.files {
		if strings.HasPrefix(n, p+"/") {
			delete(f.files, n)
		}
	}
	return nil
}

func (f *fakeSys) Remove(p string) error {
	if _, ok := f.files[p]; !ok {
		return fs.ErrNotExist
	}
	delete(f.files, p)
	return nil
}

func (f *fakeSys) Run(_ context.Context, name string, args ...string) (string, error) {
	line := strings.TrimSpace(name + " " + strings.Join(args, " "))
	f.calls = append(f.calls, line)
	if f.failCmd != "" && strings.HasPrefix(line, f.failCmd) {
		return "boom", exitErr{1}
	}
	switch name {
	case "id":
		u, ok := f.users[args[1]]
		if !ok {
			return "id: " + args[1] + ": no such user", exitErr{1}
		}
		if args[0] == "-u" {
			return strconv.Itoa(u[0]) + "\n", nil
		}
		return strconv.Itoa(u[1]) + "\n", nil
	case "getent":
		if _, ok := f.groups[args[1]]; ok {
			return args[1] + ":x:1:\n", nil
		}
		return "", exitErr{2}
	case "groupadd":
		f.groups[args[len(args)-1]] = 998
		return "", nil
	case "useradd":
		f.users[args[len(args)-1]] = [2]int{998, f.groups[args[len(args)-1]]}
		return "", nil
	case "userdel":
		delete(f.users, args[0])
		return "", nil
	case "groupdel":
		delete(f.groups, args[0])
		return "", nil
	case "systemctl":
		return f.systemctl(args)
	case "launchctl":
		return f.launchctl(args)
	case "dscl":
		return f.dscl(args)
	}
	f.t.Fatalf("unexpected command: %s", line)
	return "", nil
}

func (f *fakeSys) systemctl(args []string) (string, error) {
	installed := f.files[linuxUnitPath] != nil
	switch args[0] {
	case "daemon-reload", "reset-failed":
		return "", nil
	case "is-enabled":
		if f.enabled {
			return "enabled\n", nil
		}
		return "disabled\n", exitErr{1}
	case "enable":
		f.enabled = true
		return "", nil
	case "is-active":
		if f.active {
			return "active\n", nil
		}
		return "inactive\n", exitErr{3}
	case "start", "restart":
		f.active = true
		return "", nil
	case "stop":
		f.active = false
		return "", nil
	case "disable":
		f.enabled = false
		if slices(args, "--now") {
			f.active = false
		}
		return "", nil
	case "show":
		if !installed {
			return "LoadState=not-found\nActiveState=inactive\nSubState=dead\nMainPID=0\nWorkingDirectory=\nFragmentPath=\n", nil
		}
		st, sub, pid := "inactive", "dead", "0"
		if f.active {
			st, sub, pid = "active", "running", "4242"
		}
		wd := ""
		for _, l := range strings.Split(string(f.files[linuxUnitPath]), "\n") {
			if v, ok := strings.CutPrefix(l, "WorkingDirectory="); ok {
				wd = v
			}
		}
		return fmt.Sprintf("LoadState=loaded\nActiveState=%s\nSubState=%s\nMainPID=%s\nWorkingDirectory=%s\nFragmentPath=%s\n", st, sub, pid, wd, linuxUnitPath), nil
	}
	f.t.Fatalf("unexpected systemctl %v", args)
	return "", nil
}

func slices(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func (f *fakeSys) launchctl(args []string) (string, error) {
	switch args[0] {
	case "enable":
		return "", nil
	case "print":
		if f.stopping > 0 {
			f.stopping--
			return "system/com.coyotedb.coyote = {\n\tstate = stopping\n}\n", nil
		}
		if !f.loaded {
			return "Could not find service", exitErr{launchctlNotFound}
		}
		return "system/com.coyotedb.coyote = {\n\tactive count = 1\n\tstate = running\n\tpid = 777\n\tsubsystem = {\n\t\tpid = 1\n\t}\n}\n", nil
	case "bootstrap":
		if f.loaded {
			return "Bootstrap failed: 5: Input/output error", exitErr{5}
		}
		f.loaded = true
		return "", nil
	case "bootout":
		if !f.loaded {
			return "Boot-out failed: 3: No such process", exitErr{3}
		}
		f.loaded = false
		f.stopping = 2
		return "", nil
	case "kickstart":
		return "", nil
	}
	f.t.Fatalf("unexpected launchctl %v", args)
	return "", nil
}

func (f *fakeSys) dscl(args []string) (string, error) {
	switch args[1] {
	case "-list":
		var lines []string
		if args[2] == "/Users" {
			for n, u := range f.users {
				lines = append(lines, fmt.Sprintf("%-20s %d", n, u[0]))
			}
		} else {
			for n, g := range f.groups {
				lines = append(lines, fmt.Sprintf("%-20s %d", n, g))
			}
		}
		sort.Strings(lines)
		return strings.Join(lines, "\n") + "\n", nil
	case "-create":
		rec := args[2]
		name := path.Base(rec)
		if len(args) == 5 {
			n, _ := strconv.Atoi(args[4])
			switch args[3] {
			case "UniqueID":
				u := f.users[name]
				f.users[name] = [2]int{n, u[1]}
			case "PrimaryGroupID":
				if strings.HasPrefix(rec, "/Groups/") {
					f.groups[name] = n
				} else {
					u := f.users[name]
					f.users[name] = [2]int{u[0], n}
				}
			}
		}
		return "", nil
	case "-delete":
		name := path.Base(args[2])
		if strings.HasPrefix(args[2], "/Users/") {
			delete(f.users, name)
		} else {
			delete(f.groups, name)
		}
		return "", nil
	}
	f.t.Fatalf("unexpected dscl %v", args)
	return "", nil
}

func (f *fakeSys) ran(prefix string) bool {
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func (f *fakeSys) resetCalls() { f.calls = nil; f.writes = 0 }
