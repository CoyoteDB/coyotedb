package netdetect

import (
	"net/netip"
	"os"
	"runtime"
	"strings"
)

// TestContainerEnv forces Container to report a container of that kind
// ("docker", ...), or none with "none". Integration tests only; unset in
// normal use.
const TestContainerEnv = "COYOTE_TEST_CONTAINER"

// Container reports whether this program runs inside a container (Docker,
// Podman, Kubernetes, LXC, systemd-nspawn) and of which kind. Inside one
// with its own network, the addresses this package finds belong to the
// container's private network, which other computers cannot reach.
func Container() (kind string, ok bool) {
	if v := os.Getenv(TestContainerEnv); v != "" {
		return v, v != "none"
	}
	if runtime.GOOS != "linux" {
		return "", false // Docker Desktop and the like run Linux containers
	}
	return containerFrom(func(p string) bool {
		_, err := os.Stat(p)
		return err == nil
	}, os.ReadFile, os.Getenv)
}

// containerFrom is Container's pure part.
func containerFrom(exists func(string) bool, read func(string) ([]byte, error), getenv func(string) string) (string, bool) {
	switch {
	case exists("/.dockerenv"):
		return "docker", true
	case exists("/run/.containerenv"):
		return "podman", true
	}
	// Podman, LXC and systemd-nspawn set "container" for PID 1's tree.
	if v := getenv("container"); v != "" {
		return v, true
	}
	if b, err := read("/proc/1/cgroup"); err == nil {
		s := string(b)
		for _, k := range []string{"kubepods", "docker", "libpod", "containerd", "lxc"} {
			if strings.Contains(s, k) {
				if k == "kubepods" {
					return "kubernetes", true
				}
				return k, true
			}
		}
	}
	// cgroup v2 hides the path ("0::/"); the container's own files show it.
	if b, err := read("/proc/self/mountinfo"); err == nil {
		s := string(b)
		switch {
		case strings.Contains(s, "/docker/containers/"):
			return "docker", true
		case strings.Contains(s, "/containers/storage/"):
			return "podman", true
		}
	}
	return "", false
}

// Container networks: Docker's bridges come from 172.16.0.0/12, Podman's
// default network is 10.88.0.0/16, rootless Podman's slirp/pasta uses
// 10.0.2.0/24.
var containerNets = []netip.Prefix{
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("10.88.0.0/16"),
	netip.MustParsePrefix("10.0.2.0/24"),
}

// ContainerPrivate reports an address that, inside a container, almost
// certainly belongs to the container's own network (unreachable from
// other computers).
func ContainerPrivate(a netip.Addr) bool {
	a = a.Unmap()
	for _, p := range containerNets {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
