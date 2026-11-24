package agent

import (
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"coyotedb.com/coyote/internal/cluster"
)

func TestClientHostsAndAppURL(t *testing.T) {
	st := cluster.State{Members: map[string]cluster.Member{
		"s": {ID: "s", Name: "zeta", Role: cluster.RoleData, ClientPort: 5432, Addresses: []string{"100.64.0.9:7481", "10.0.0.9:7481"}},
		"x": {ID: "x", Name: "alpha", Role: cluster.RoleData, ClientPort: 6432, Addresses: []string{"[fd7a::1]:7481"}},
		"w": {ID: "w", Name: "wit", Role: cluster.RoleWitness, Addresses: []string{"10.0.0.7:7481"}},
		"n": {ID: "n", Name: "new", Role: cluster.RoleData, Addresses: []string{"10.0.0.8:7481"}}, // no client port yet
	}}
	got := clientHosts(st, "s")
	want := []string{"100.64.0.9:5432", "10.0.0.9:5432", "[fd7a::1]:6432"}
	if !slices.Equal(got, want) {
		t.Fatalf("hosts %v, want %v", got, want)
	}
	u := AppURL(got, "pw/+x", "/Library/Application Support/CoyoteDB/ca.crt")
	if !strings.Contains(u, "sslrootcert=%2FLibrary%2FApplication%20Support%2FCoyoteDB%2Fca.crt") {
		t.Fatalf("space must be %%20 (libpq does not decode '+'): %s", u)
	}
	pu, err := url.Parse(AppURL([]string{"10.0.0.9:5432"}, "pw/+x", "/ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	if pw, _ := pu.User.Password(); pw != "pw/+x" || pu.User.Username() != "app" || pu.Path != "/app" {
		t.Fatalf("user %v path %s", pu.User, pu.Path)
	}
	for _, q := range []string{"sslmode=verify-full", "target_session_attrs=read-write"} {
		if !strings.Contains(u, q) {
			t.Fatalf("missing %s: %s", q, u)
		}
	}
}

func TestPortInUseOffersNextFree(t *testing.T) {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	msg := portInUseMessage(port)
	if !strings.Contains(msg, strconv.Itoa(port)+" is already in use") || !strings.Contains(msg, "--client-port ") {
		t.Fatalf("message: %s", msg)
	}
}

func TestReadServiceRequest(t *testing.T) {
	dir := t.TempDir()
	if _, err := readServiceRequest(dir); !os.IsNotExist(err) {
		t.Fatalf("no file: %v", err)
	}
	for body, ok := range map[string]bool{
		`{"mode":"join","invite":"coyote-x"}`: true,
		`{"mode":"up"}`:                       true,
		`{"mode":"join"}`:                     false,
		`{"mode":"bootstrap"}`:                false,
		`nonsense`:                            false,
	} {
		if err := os.WriteFile(ServiceRequestPath(dir), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := readServiceRequest(dir)
		if (err == nil) != ok {
			t.Errorf("%s: err %v", body, err)
		}
		if _, err := os.Stat(filepath.Join(dir, ServiceRequestFile)); !os.IsNotExist(err) {
			t.Errorf("%s: the request (it may hold an invite code) was not removed", body)
		}
	}
}
