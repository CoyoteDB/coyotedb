package netdetect

import (
	"errors"
	"io/fs"
	"net/netip"
	"testing"
)

func TestContainerFrom(t *testing.T) {
	none := func(string) ([]byte, error) { return nil, fs.ErrNotExist }
	files := func(m map[string]string) func(string) ([]byte, error) {
		return func(p string) ([]byte, error) {
			if v, ok := m[p]; ok {
				return []byte(v), nil
			}
			return nil, errors.New("no")
		}
	}
	exists := func(set ...string) func(string) bool {
		return func(p string) bool {
			for _, s := range set {
				if s == p {
					return true
				}
			}
			return false
		}
	}
	env := func(v string) func(string) string { return func(string) string { return v } }
	cases := []struct {
		name   string
		exists func(string) bool
		read   func(string) ([]byte, error)
		env    func(string) string
		kind   string
	}{
		{"dockerenv", exists("/.dockerenv"), none, env(""), "docker"},
		{"podman", exists("/run/.containerenv"), none, env(""), "podman"},
		{"nspawn env", exists(), none, env("systemd-nspawn"), "systemd-nspawn"},
		{"cgroup v1 docker", exists(), files(map[string]string{"/proc/1/cgroup": "12:pids:/docker/3f2a\n"}), env(""), "docker"},
		{"kubernetes", exists(), files(map[string]string{"/proc/1/cgroup": "0::/kubepods/besteffort/pod1\n"}), env(""), "kubernetes"},
		{"cgroup v2 + mountinfo", exists(), files(map[string]string{"/proc/1/cgroup": "0::/\n", "/proc/self/mountinfo": "1 2 0:1 /var/lib/docker/containers/abc/hostname /etc/hostname rw\n"}), env(""), "docker"},
		{"plain machine", exists(), files(map[string]string{"/proc/1/cgroup": "0::/init.scope\n", "/proc/self/mountinfo": "22 1 8:1 / / rw\n"}), env(""), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kind, ok := containerFrom(c.exists, c.read, c.env)
			if kind != c.kind || ok != (c.kind != "") {
				t.Fatalf("got %q %v, want %q", kind, ok, c.kind)
			}
		})
	}
}

func TestContainerPrivate(t *testing.T) {
	for s, want := range map[string]bool{
		"172.17.0.5": true, "172.31.255.1": true, "10.88.0.2": true, "10.0.2.100": true,
		"192.168.1.20": false, "10.1.2.3": false, "100.64.0.43": false, "172.32.0.1": false, "::ffff:172.17.0.2": true,
	} {
		if got := ContainerPrivate(netip.MustParseAddr(s)); got != want {
			t.Errorf("%s: %v, want %v", s, got, want)
		}
	}
}

func TestContainerTestHook(t *testing.T) {
	t.Setenv(TestContainerEnv, "docker")
	if k, ok := Container(); !ok || k != "docker" {
		t.Fatalf("hook: %q %v", k, ok)
	}
}
