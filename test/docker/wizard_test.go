//go:build docker

package docker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"coyotedb.com/coyote/test/testports"
)

// dockerSetupLine is the Docker join command naming the image (edge, or a
// release's own version: brand.DockerImage, set at build time).
var dockerSetupLine = regexp.MustCompile(`coyotedb/coyote:[0-9a-z.-]+ setup`)

// The setup page from Docker (D68): the container is started with
// `setup` and no address, as docker/README.md "Easiest: the setup page"
// says; the one-time sign-in link is read from `docker logs`, and the
// page's API is driven over the published 127.0.0.1 port with that
// sign-in, exactly as the browser does.

var linkRE = regexp.MustCompile(`http://127\.0\.0\.1:(\d+)/login#t=([A-Za-z0-9_-]+)`)

// panel is a browser session on one container's published control port.
type panel struct {
	t    *testing.T
	base string
	c    *http.Client
	csrf string
}

// wizardStart runs a node exactly like the README's setup command, with
// this test's ports: the control port published on 127.0.0.1 only.
func (r *rig) wizardStart(n node, ctlPort int) {
	r.t.Helper()
	args := []string{"run", "-d", "--name", n.container(), "--hostname", n.name,
		"--restart", "unless-stopped", "--stop-timeout", "120", "--shm-size", "256m",
		"--add-host", "host.docker.internal:host-gateway",
		"-p", fmt.Sprintf("127.0.0.1:%d:%d", ctlPort, ctlPort),
		"-p", fmt.Sprintf("%s:%d:%d", r.bind, n.clusterPort, n.clusterPort),
		"-v", n.volume() + ":/data"}
	if !n.witness {
		args = append(args, "-p", fmt.Sprintf("%s:%d:%d", r.bind, n.clientPort, n.clientPort))
	}
	// The image's COYOTE_DASHBOARD_LISTEN=0.0.0.0:7480 takes the port of
	// --control-port; nothing about the dashboard is given here.
	args = append(args, r.img, "setup",
		"--listen", fmt.Sprintf("0.0.0.0:%d", n.clusterPort),
		"--control-port", strconv.Itoa(ctlPort))
	if !n.witness {
		args = append(args, "--client-port", strconv.Itoa(n.clientPort))
	}
	dockerT(r.t, 2*time.Minute, args...)
}

// linkFromLogs waits for the sign-in link the container prints.
func (r *rig) linkFromLogs(n node, ctlPort int) string {
	r.t.Helper()
	var tok string
	waitFor(r.t, "a sign-in link in docker logs "+n.name, 2*time.Minute, func() error {
		out := dockerT(r.t, time.Minute, "logs", n.container())
		m := linkRE.FindStringSubmatch(out)
		if m == nil {
			return fmt.Errorf("no link yet")
		}
		if m[1] != strconv.Itoa(ctlPort) {
			return fmt.Errorf("link names port %s, want %d", m[1], ctlPort)
		}
		if !strings.Contains(out, "docker exec CONTAINER coyote dashboard") {
			return fmt.Errorf("the logs do not say how to get a new link")
		}
		tok = m[2]
		return nil
	})
	return tok
}

// linkFromExec is `docker exec <c> coyote dashboard`'s link.
func (r *rig) linkFromExec(n node) string {
	r.t.Helper()
	out := dockerT(r.t, time.Minute, "exec", n.container(), "coyote", "dashboard")
	m := linkRE.FindStringSubmatch(out)
	if m == nil || !strings.Contains(out, "computer that runs this container") {
		r.t.Fatalf("coyote dashboard in the container printed no link")
	}
	return m[2]
}

func signIn(t *testing.T, ctlPort int, tok string) *panel {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	p := &panel{t: t, base: fmt.Sprintf("http://127.0.0.1:%d", ctlPort), c: &http.Client{Jar: jar, Timeout: 30 * time.Second}}
	var sb struct {
		CSRF string `json:"csrf_token"`
	}
	if code, b := p.do("POST", "/v1/session", map[string]string{"token": tok}, &sb); code != 200 || sb.CSRF == "" {
		t.Fatalf("sign in on %s: %d %s", p.base, code, b)
	}
	p.csrf = sb.CSRF
	return p
}

// do sends a request as the dashboard page does (Origin, CSRF).
func (p *panel) do(method, path string, body, out any) (int, string) {
	return p.doHdr(method, path, body, out, nil)
}

func (p *panel) doHdr(method, path string, body, out any, hdr map[string]string) (int, string) {
	p.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, p.base+path, rd)
	if err != nil {
		p.t.Fatal(err)
	}
	req.Header.Set("Origin", p.base)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if p.csrf != "" && method != "GET" {
		req.Header.Set("X-Coyote-CSRF", p.csrf)
	}
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := p.c.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode < 300 {
		_ = json.Unmarshal(b, out)
	}
	return resp.StatusCode, string(b)
}

// checkAddressStep: the page is told it runs in a container and is not
// offered any of Docker's private addresses; such an address is refused.
func (p *panel) checkAddressStep() {
	p.t.Helper()
	var av struct {
		Addresses []struct {
			Addr string `json:"addr"`
		} `json:"addresses"`
		Default   *string `json:"default"`
		Container *struct {
			Kind              string `json:"kind"`
			HostAddressNeeded bool   `json:"host_address_needed"`
		} `json:"container"`
	}
	if code, b := p.do("GET", "/v1/addresses", nil, &av); code != 200 || av.Container == nil || av.Container.Kind != "docker" || !av.Container.HostAddressNeeded {
		p.t.Fatalf("addresses: %d %s", code, b)
	}
	if len(av.Addresses) != 0 || av.Default != nil {
		p.t.Fatalf("the page offers container addresses: %+v", av)
	}
}

func TestDockerWizard(t *testing.T) {
	img := image(t)
	r := newRig(t, img)
	n1, n2, w := r.node("n1"), r.node("n2"), r.node("w")
	ctl := map[string]int{}
	for _, n := range []node{n1, n2, w} {
		p, err := testports.Free()
		if err != nil {
			t.Fatal(err)
		}
		ctl[n.name] = p
	}
	t.Logf("control ports on 127.0.0.1: %v; cluster/client ports on %s", ctl, r.bind)

	// 1. First container: setup page, sign-in link from docker logs.
	r.wizardStart(n1, ctl["n1"])
	tok1 := r.linkFromLogs(n1, ctl["n1"])
	p1 := signIn(t, ctl["n1"], tok1)
	// The link went to docker logs only, never to coyote.log (D68).
	if lg := dockerT(t, time.Minute, "exec", n1.container(), "cat", "/data/coyote/logs/coyote.log"); strings.Contains(lg, tok1) {
		t.Fatal("the sign-in token is in coyote.log")
	}
	// Used once: the same link does not sign in again.
	again := &panel{t: t, base: p1.base, c: &http.Client{Timeout: 30 * time.Second}}
	if code, _ := again.do("POST", "/v1/session", map[string]string{"token": tok1}, nil); code != 401 {
		t.Fatalf("second use of the link: %d", code)
	}
	// Waiting for the page is healthy: the image's health check passes.
	if out := dockerT(t, time.Minute, "exec", n1.container(), "coyote", "status", "--json"); !strings.Contains(out, `"set_up": false`) {
		t.Fatalf("status while waiting: %s", out)
	}
	if code, b := p1.do("GET", "/v1/setup", nil, nil); code != 200 || !strings.Contains(b, `"fresh"`) {
		t.Fatalf("setup view: %d %s", code, b)
	}
	// Security (D52): no session, no entry; the CLI's bearer path and a
	// non-loopback Host are refused from outside the container.
	anon := &panel{t: t, base: p1.base, c: &http.Client{Timeout: 30 * time.Second}}
	if code, b := anon.do("GET", "/v1/setup", nil, nil); code != 401 {
		t.Fatalf("without a session: %d %s", code, b)
	}
	if code, b := anon.doHdr("POST", "/v1/session/links", map[string]string{}, nil, map[string]string{"Authorization": "Bearer x"}); code != 403 {
		t.Fatalf("bearer from the host: %d %s", code, b)
	}
	if code, b := p1.doHdr("GET", "/v1/setup", nil, nil, map[string]string{"Host": fmt.Sprintf("192.168.1.10:%d", ctl["n1"])}); code != 403 {
		t.Fatalf("LAN host name: %d %s", code, b)
	}
	p1.checkAddressStep()
	// Loopback and the container's own Docker address only reach the
	// container: refused before anything starts.
	own := strings.Fields(dockerT(t, time.Minute, "inspect", "--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}", n1.container()))
	for _, bad := range append([]string{"127.0.0.1"}, own...) {
		if code, b := p1.do("POST", "/v1/setup/create", map[string]string{"name": "n1", "advertise": bad}, nil); code != 422 || !strings.Contains(b, "container_address") {
			t.Fatalf("create with %s: %d %s", bad, code, b)
		}
	}
	// Create with the host's address (host.docker.internal stands in for
	// the host's LAN address in this one-computer test).
	if code, b := p1.do("POST", "/v1/setup/create", map[string]string{"name": "n1", "advertise": "host.docker.internal"}, nil); code != 202 {
		t.Fatalf("create: %d %s", code, b)
	}
	d := r.waitHealthy(n1, "n1", nil, 5*time.Minute)
	t.Logf("n1 created from the setup page; main node after %s", d.Round(time.Second))
	// The same port and session now serve the whole control panel.
	waitFor(t, "the panel's status on the same session", time.Minute, func() error {
		if code, b := p1.do("GET", "/v1/status", nil, nil); code != 200 {
			return fmt.Errorf("%d %s", code, b)
		}
		return nil
	})

	// 2. Restart: the same `setup` command just starts the node.
	dockerT(t, 3*time.Minute, "restart", n1.container())
	d = r.waitHealthy(n1, "n1", nil, 5*time.Minute)
	t.Logf("n1 main again %s after docker restart", d.Round(time.Second))
	if out := dockerT(t, time.Minute, "logs", "--since", "3m", n1.container()); !strings.Contains(out, "Control panel: http://127.0.0.1:") {
		t.Fatalf("restart logs do not say where the panel is:\n%s", tail(out, 20))
	}
	p1 = signIn(t, ctl["n1"], r.linkFromExec(n1))
	if code, b := p1.do("GET", "/v1/status", nil, nil); code != 200 {
		t.Fatalf("panel after restart: %d %s", code, b)
	}

	// The panel knows it runs in Docker (no download in the image) and the
	// CLI's invite tells the next machine the Docker commands (D68).
	var vi struct {
		Container string `json:"container"`
	}
	if code, b := p1.do("GET", "/v1/version", nil, &vi); code != 200 || vi.Container != "docker" {
		t.Fatalf("version: %d %s", code, b)
	}
	if out := dockerT(t, time.Minute, "exec", n1.container(), "coyote", "invite"); !dockerSetupLine.MatchString(out) || !strings.Contains(out, "COYOTE_INVITE=PASTE-THE-CODE") || strings.Contains(out, "run `coyote join`") {
		t.Fatalf("coyote invite in a container does not give the Docker commands")
	}

	// 3. Second data container and a witness join through their own
	// setup pages, each with an invite made in n1's panel.
	for _, j := range []node{n2, w} {
		role := "data"
		if j.witness {
			role = "witness"
		}
		var inv struct {
			Code   string `json:"code"`
			Docker *struct {
				SetupCommand string `json:"setup_command"`
				CLICommand   string `json:"cli_command"`
			} `json:"docker"`
		}
		if code, _ := p1.do("POST", "/v1/invites", map[string]string{"role": role}, &inv); (code != 200 && code != 201) || inv.Code == "" {
			t.Fatalf("invite from the panel: %d", code) // the body holds the code: not logged
		}
		if inv.Docker == nil || !dockerSetupLine.MatchString(inv.Docker.SetupCommand) || !strings.Contains(inv.Docker.CLICommand, " "+map[string]string{"data": "join", "witness": "witness"}[role]+" --name ") {
			t.Fatalf("invite from a container lacks the Docker join commands")
		}
		r.wizardStart(j, ctl[j.name])
		pj := signIn(t, ctl[j.name], r.linkFromLogs(j, ctl[j.name]))
		pj.checkAddressStep()
		var chk struct {
			Valid bool `json:"valid"`
			Probe *struct {
				Reachable bool `json:"reachable"`
			} `json:"probe"`
		}
		if code, b := pj.do("POST", "/v1/setup/check-invite", map[string]any{"code": inv.Code, "probe": true}, &chk); code != 200 || !chk.Valid || chk.Probe == nil || !chk.Probe.Reachable {
			t.Fatalf("check invite from %s: %d %s", j.name, code, b)
		}
		if code, b := pj.do("POST", "/v1/setup/join", map[string]string{"code": inv.Code, "name": j.name, "advertise": "host.docker.internal", "role": role}, nil); code != 202 {
			t.Fatalf("join %s: %d %s", j.name, code, b)
		}
		if !j.witness {
			d := r.waitHealthy(n1, "n1", []string{j.name}, 10*time.Minute)
			t.Logf("%s joined from its setup page; follows live after %s", j.name, d.Round(time.Second))
		}
	}
	waitFor(t, "witness in the cluster", 5*time.Minute, func() error {
		s, err := r.status(n1)
		if err != nil {
			return err
		}
		if st, _ := s.state("w"); st != "witness" {
			return fmt.Errorf("witness is %q", st)
		}
		if len(s.Cluster.SyncSet) == 0 {
			return fmt.Errorf("no sync copy yet")
		}
		return nil
	})
	r.waitHealthy(n1, "n1", []string{"n2"}, 5*time.Minute)

	// 4. Acknowledged rows, then kill the main container: n2 takes over
	// by itself and has every row.
	if _, err := r.admin(n1, "CREATE TABLE wiz(id int PRIMARY KEY)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	// 30 commits one by one (each docker exec is slow), then the rest in
	// one; every one acknowledged before the kill.
	const rows = 300
	for i := 1; i <= 30; i++ {
		if _, err := r.admin(n1, fmt.Sprintf("INSERT INTO wiz VALUES (%d)", i)); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	if _, err := r.admin(n1, fmt.Sprintf("INSERT INTO wiz SELECT generate_series(31, %d)", rows)); err != nil {
		t.Fatalf("insert: %v", err)
	}
	dockerT(t, time.Minute, "kill", n1.container())
	d = waitFor(t, "n2 the main node", 3*time.Minute, func() error {
		s, err := r.status(n2)
		if err != nil {
			return err
		}
		if s.Cluster.PrimaryName != "n2" {
			return fmt.Errorf("main node is %q", s.Cluster.PrimaryName)
		}
		if st, _ := s.state("n2"); st != "primary" {
			return fmt.Errorf("n2 is %q", st)
		}
		return nil
	})
	t.Logf("n2 took over %s after n1 was killed", d.Round(100*time.Millisecond))
	waitFor(t, "every acknowledged row on n2", 2*time.Minute, func() error {
		out, err := r.admin(n2, "SELECT count(*) FROM wiz")
		if err != nil {
			return err
		}
		if out != strconv.Itoa(rows) {
			return fmt.Errorf("n2 has %s rows, want %d", out, rows)
		}
		return nil
	})
	// n2's panel works on its own port too.
	p2 := signIn(t, ctl["n2"], r.linkFromExec(n2))
	var st status
	if code, b := p2.do("GET", "/v1/status", nil, &st); code != 200 || st.Cluster.PrimaryName != "n2" {
		t.Fatalf("n2 panel: %d %s", code, tail(b, 5))
	}
	// The killed container comes back (restart policy is not applied to
	// docker kill) and rejoins as a copy.
	dockerT(t, time.Minute, "start", n1.container())
	d = r.waitHealthy(n2, "n2", []string{"n1"}, 10*time.Minute)
	t.Logf("n1 back as a copy after %s", d.Round(time.Second))
	r.noZombies(n1)
}
