//go:build integration

package proxy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Real Postgres behind the proxy: two plain instances from the cached
// build (./.coyote-dev/cache, or COYOTE_PG_BIN=<dir with initdb>). B starts
// as a standby (standby.signal, no upstream) so the primary check must
// refuse it until it is promoted. Both use TLS, which must pass through.

const itestPassword = "proxy-itest-pw"

func pgBinDir(t *testing.T) string {
	t.Helper()
	exe := func(dir string) bool {
		name := "initdb"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		_, err := os.Stat(filepath.Join(dir, name))
		return err == nil
	}
	if d := os.Getenv("COYOTE_PG_BIN"); d != "" {
		if !exe(d) {
			t.Fatalf("COYOTE_PG_BIN=%s has no initdb", d)
		}
		return d
	}
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("repo root not found")
		}
		dir = parent
	}
	matches, _ := filepath.Glob(filepath.Join(dir, ".coyote-dev", "cache", "postgresql-*", "bin"))
	for _, m := range matches {
		if exe(m) {
			return m
		}
	}
	t.Fatal("no cached Postgres build: run `make itest` once (it downloads it) or set COYOTE_PG_BIN")
	return ""
}

type pgInst struct {
	bin, dir string
	port     int
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func run(t *testing.T, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = cleanEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", filepath.Base(name), args, err, out)
	}
}

func cleanEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if len(kv) >= 2 && kv[:2] == "PG" {
			continue
		}
		env = append(env, kv)
	}
	return env
}

func startPG(t *testing.T, bin string, standby bool) *pgInst {
	t.Helper()
	root := t.TempDir()
	p := &pgInst{bin: bin, dir: filepath.Join(root, "data"), port: freePort(t)}
	pw := filepath.Join(root, "pw")
	if err := os.WriteFile(pw, []byte(itestPassword+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, filepath.Join(bin, "initdb"), "-D", p.dir, "-U", "postgres", "--pwfile="+pw,
		"--auth=scram-sha-256", "--encoding=UTF8", "--locale-provider=builtin",
		"--builtin-locale=C.UTF-8", "--locale=C", "--no-sync")
	writeCert(t, p.dir)
	if standby {
		if err := os.WriteFile(filepath.Join(p.dir, "standby.signal"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	opts := "-c listen_addresses=127.0.0.1 -c port=" + strconv.Itoa(p.port) +
		" -c unix_socket_directories= -c ssl=on -c fsync=off"
	run(t, filepath.Join(bin, "pg_ctl"), "-D", p.dir, "-l", filepath.Join(root, "log"), "-w", "-t", "60", "-o", opts, "start")
	t.Cleanup(func() {
		cmd := exec.Command(filepath.Join(bin, "pg_ctl"), "-D", p.dir, "-m", "immediate", "-w", "stop")
		cmd.Env = cleanEnv()
		_ = cmd.Run()
	})
	return p
}

func (p *pgInst) promote(t *testing.T) {
	run(t, filepath.Join(p.bin, "pg_ctl"), "-D", p.dir, "-w", "promote")
}

func writeCert(t *testing.T, dir string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "server.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "server.key"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600); err != nil {
		t.Fatal(err)
	}
}

func clientConnect(ctx context.Context, proxyAddr string) (*pgx.Conn, error) {
	host, port, _ := net.SplitHostPort(proxyAddr)
	cfg, err := pgx.ParseConfig("host=" + host + " port=" + port + " user=postgres dbname=postgres sslmode=require connect_timeout=15")
	if err != nil {
		return nil, err
	}
	cfg.Password = itestPassword
	return pgx.ConnectConfig(ctx, cfg)
}

func backendPort(ctx context.Context, c *pgx.Conn) (int, bool, error) {
	var port int
	var ssl bool
	err := c.QueryRow(ctx, "SELECT current_setting('port')::int, ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()").Scan(&port, &ssl)
	return port, ssl, err
}

func TestIntegrationRealPostgres(t *testing.T) {
	checkLeaks(t)
	bin := pgBinDir(t)
	a := startPG(t, bin, false)
	b := startPG(t, bin, true)
	ports := map[string]int{"A": a.port, "B": b.port}

	r := newRouter(1, "A")
	r.keepContact(t, 100*time.Millisecond)
	p, err := New(Config{
		ListenAddrs: []string{"127.0.0.1:0"},
		Router:      r,
		Dial: func(ctx context.Context, node string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(ports[node])))
		},
		Verifier:       PGVerifier{User: "postgres", Password: itestPassword},
		Lease:          2 * time.Second,
		WaitForPrimary: 20 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	addr := p.Addrs()[0].String()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	c1, err := clientConnect(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close(context.Background())
	port, ssl, err := backendPort(ctx, c1)
	if err != nil || port != a.port || !ssl {
		t.Fatalf("first client: port=%d ssl=%v err=%v; want A (%d) over TLS", port, ssl, err, a.port)
	}

	// Fail over to B, which is still a standby.
	r.set(func(v *View) { v.Epoch, v.Primary = 2, "B" })
	t0 := time.Now()
	for {
		if _, _, err := backendPort(ctx, c1); err != nil {
			break
		}
		if time.Since(t0) > 2*time.Second {
			t.Fatal("client still connected to the old primary after the epoch change")
		}
		time.Sleep(20 * time.Millisecond)
	}

	type res struct {
		port int
		err  error
	}
	got := make(chan res, 1)
	go func() {
		c, err := clientConnect(ctx, addr)
		if err != nil {
			got <- res{0, err}
			return
		}
		defer c.Close(context.Background())
		port, _, err := backendPort(ctx, c)
		got <- res{port, err}
	}()
	defer func() { t.Logf("stats at end: %+v", p.Stats()) }()
	eventually(t, 10*time.Second, "B refused while in recovery", func() bool {
		st := p.Stats()
		return st.VerifyFailures >= 1 && st.Waiting >= 1
	})
	select {
	case r := <-got:
		t.Fatalf("client served while B was a standby: %+v", r)
	default:
	}
	b.promote(t)
	if r := <-got; r.err != nil || r.port != b.port {
		t.Fatalf("after promotion: %+v; want B (%d)", r, b.port)
	}

	// And back to A (as if B failed): new clients land on A.
	r.set(func(v *View) { v.Epoch, v.Primary = 3, "A" })
	c3, err := clientConnect(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c3.Close(context.Background())
	if port, _, err := backendPort(ctx, c3); err != nil || port != a.port {
		t.Fatalf("epoch 3: port=%d err=%v", port, err)
	}
	st := p.Stats()
	t.Logf("stats: %+v", st)
	if st.DroppedOnEpoch < 1 || st.Forwarded < 3 {
		t.Fatalf("stats: %+v", st)
	}
}
