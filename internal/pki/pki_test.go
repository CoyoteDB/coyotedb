package pki

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const testCluster = "c0ffee00c0ffee00c0ffee00c0ffee00"

func newNode(t *testing.T, ca *CA, nodeID string, notBefore, notAfter time.Time) *NodeTLS {
	t.Helper()
	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	cid, _ := ca.ClusterID()
	csr, err := NewCSR(key, cid, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	der, err := ca.SignCSR(csr, nodeID, notBefore, notAfter)
	if err != nil {
		t.Fatal(err)
	}
	n, err := NewNodeTLS(ca.Cert.Raw, der, key)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func newCA(t *testing.T, cluster string) *CA {
	t.Helper()
	ca, err := NewCA(cluster, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

func validNode(t *testing.T, ca *CA, id string) *NodeTLS {
	now := time.Now()
	return newNode(t, ca, id, now.Add(-time.Hour), now.Add(time.Hour))
}

// handshake runs a TLS handshake over loopback TCP and returns the client and
// server errors.
func handshake(t *testing.T, srv, cli *tls.Config) (serverErr, clientErr error, peerID string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	type res struct {
		err error
		id  string
	}
	ch := make(chan res, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			ch <- res{err: err}
			return
		}
		defer c.Close()
		tc := tls.Server(c, srv)
		tc.SetDeadline(time.Now().Add(10 * time.Second))
		err = tc.Handshake()
		if err == nil {
			// Read until the client closes so a client-side verification
			// failure (TLS 1.3 verifies the server after server Finished)
			// surfaces as an alert here.
			_, err = io.Copy(io.Discard, tc)
		}
		ch <- res{err: err, id: PeerNodeID(tc.ConnectionState())}
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	tc := tls.Client(c, cli)
	tc.SetDeadline(time.Now().Add(10 * time.Second))
	clientErr = tc.Handshake()
	if clientErr == nil {
		// Force a round trip so the server's verification of our cert
		// (which happens after the client's Finished) is observed.
		_, clientErr = tc.Write([]byte("x"))
	}
	tc.Close()
	r := <-ch
	return r.err, clientErr, r.id
}

func TestIssueAndMutualTLS(t *testing.T) {
	ca := newCA(t, testCluster)
	a := validNode(t, ca, "node-a")
	b := validNode(t, ca, "node-b")
	srvErr, cliErr, id := handshake(t, a.ServerConfig(), b.ClientConfig("node-a"))
	if srvErr != nil || cliErr != nil {
		t.Fatalf("server=%v client=%v", srvErr, cliErr)
	}
	if id != "node-b" {
		t.Fatalf("server saw peer %q", id)
	}
	cid, nid, err := Identity(a.Cert.Leaf)
	if err != nil || cid != testCluster || nid != "node-a" {
		t.Fatalf("identity = %q %q %v", cid, nid, err)
	}
	if got, err := VerifyNodeCert(a.Cert.Certificate[0], ca.Cert, testCluster, time.Now()); err != nil || got != "node-a" {
		t.Fatalf("VerifyNodeCert = %q %v", got, err)
	}
	if a.Cert.Leaf.SerialNumber.Cmp(b.Cert.Leaf.SerialNumber) == 0 {
		t.Fatal("serials must be random/unique")
	}
}

func TestWrongExpectedNode(t *testing.T) {
	ca := newCA(t, testCluster)
	a := validNode(t, ca, "node-a")
	b := validNode(t, ca, "node-b")
	_, cliErr, _ := handshake(t, a.ServerConfig(), b.ClientConfig("node-c"))
	if cliErr == nil || !strings.Contains(cliErr.Error(), "expected") {
		t.Fatalf("want identity mismatch, got %v", cliErr)
	}
}

func TestWrongCARejected(t *testing.T) {
	ca1 := newCA(t, testCluster)
	ca2 := newCA(t, testCluster) // same cluster id, different key
	a := validNode(t, ca1, "node-a")
	evil := validNode(t, ca2, "node-b")
	// Client from the wrong CA is refused by the server.
	srvErr, _, _ := handshake(t, a.ServerConfig(), evil.ClientConfig("node-a"))
	if srvErr == nil {
		t.Fatal("server accepted a client from another CA")
	}
	// Server from the wrong CA is refused by the client.
	_, cliErr, _ := handshake(t, evil.ServerConfig(), a.ClientConfig("node-b"))
	if cliErr == nil {
		t.Fatal("client accepted a server from another CA")
	}
	if _, err := VerifyNodeCert(evil.Cert.Certificate[0], ca1.Cert, testCluster, time.Now()); err == nil {
		t.Fatal("VerifyNodeCert accepted foreign cert")
	}
}

func TestWrongClusterIDInSAN(t *testing.T) {
	other := newCA(t, "bad00000bad00000bad00000bad00000")
	// A cert issued for another cluster. Even a verifier for testCluster
	// that (wrongly) trusted the other CA still rejects the SAN.
	n := validNode(t, other, "node-x")
	_, err := VerifyNodeCert(n.Cert.Certificate[0], other.Cert, testCluster, time.Now())
	if !errors.Is(err, ErrWrongCluster) {
		t.Fatalf("want ErrWrongCluster, got %v", err)
	}
	// And over TLS: a server configured for testCluster but trusting the
	// other CA must refuse the client.
	srv := &NodeTLS{ClusterID: testCluster, NodeID: "node-a", CA: other.Cert, Cert: validNode(t, other, "node-a").Cert}
	srvErr, _, _ := handshake(t, srv.ServerConfig(), n.ClientConfig("node-a"))
	if srvErr == nil || !strings.Contains(srvErr.Error(), "different cluster") && !strings.Contains(srvErr.Error(), "bad certificate") {
		t.Fatalf("server should reject wrong cluster SAN, got %v", srvErr)
	}
}

func TestExpiredRejected(t *testing.T) {
	ca := newCA(t, testCluster)
	a := validNode(t, ca, "node-a")
	now := time.Now()
	old := newNode(t, ca, "node-old", now.Add(-48*time.Hour), now.Add(-24*time.Hour))
	srvErr, _, _ := handshake(t, a.ServerConfig(), old.ClientConfig("node-a"))
	if srvErr == nil || !strings.Contains(srvErr.Error(), "expired") {
		t.Fatalf("want expired rejection, got %v", srvErr)
	}
	_, cliErr, _ := handshake(t, old.ServerConfig(), a.ClientConfig("node-old"))
	if cliErr == nil {
		t.Fatal("client accepted expired server cert")
	}
}

func TestTLS12Refused(t *testing.T) {
	ca := newCA(t, testCluster)
	a := validNode(t, ca, "node-a")
	b := validNode(t, ca, "node-b")
	cli := b.ClientConfig("node-a")
	cli.MinVersion = tls.VersionTLS12
	cli.MaxVersion = tls.VersionTLS12
	srvErr, cliErr, _ := handshake(t, a.ServerConfig(), cli)
	if srvErr == nil || cliErr == nil {
		t.Fatalf("TLS 1.2 must fail: server=%v client=%v", srvErr, cliErr)
	}
}

func TestAnonymousClientAllowedByTLSLayer(t *testing.T) {
	// The TLS layer lets a cert-less client through (the transport restricts
	// it to the join stream); PeerNodeID reports "".
	ca := newCA(t, testCluster)
	a := validNode(t, ca, "node-a")
	cli := JoinClientConfig(testCluster, ca.Fingerprint(), nil)
	srvErr, cliErr, id := handshake(t, a.ServerConfig(), cli)
	if srvErr != nil || cliErr != nil || id != "" {
		t.Fatalf("server=%v client=%v id=%q", srvErr, cliErr, id)
	}
	// Wrong fingerprint pin fails on the client.
	var bad [32]byte
	_, cliErr, _ = handshake(t, a.ServerConfig(), JoinClientConfig(testCluster, bad, nil))
	if cliErr == nil || !strings.Contains(cliErr.Error(), "fingerprint") {
		t.Fatalf("want fingerprint error, got %v", cliErr)
	}
	// Right fingerprint, wrong cluster id.
	_, cliErr, _ = handshake(t, a.ServerConfig(), JoinClientConfig("other", ca.Fingerprint(), nil))
	if cliErr == nil {
		t.Fatal("join client accepted wrong cluster id")
	}
}

func TestCSRIdentityComesFromSigner(t *testing.T) {
	ca := newCA(t, testCluster)
	key, _ := GenerateKey()
	csr, err := NewCSR(key, testCluster, "asked-for")
	if err != nil {
		t.Fatal(err)
	}
	der, err := ca.IssueNode(csr, "granted", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	id, err := VerifyNodeCert(der, ca.Cert, testCluster, time.Now())
	if err != nil || id != "granted" {
		t.Fatalf("got %q %v", id, err)
	}
	// Tampered CSR is refused.
	csr[len(csr)-3] ^= 0xff
	if _, err := ca.IssueNode(csr, "x", time.Now()); err == nil {
		t.Fatal("tampered CSR accepted")
	}
}

func TestFilesAndCARoundTrip(t *testing.T) {
	dir := t.TempDir()
	ca := newCA(t, testCluster)
	kp := filepath.Join(dir, "ca.key")
	if err := WriteKey(kp, ca.Key); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(kp)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("key mode %v", st.Mode().Perm())
		}
	}
	k2, err := ReadKey(kp)
	if err != nil || !k2.Equal(ca.Key) {
		t.Fatalf("read key: %v", err)
	}
	cp := filepath.Join(dir, "ca.crt")
	if err := WriteCert(cp, ca.Cert.Raw); err != nil {
		t.Fatal(err)
	}
	der, err := ReadCert(cp)
	if err != nil {
		t.Fatal(err)
	}
	kder, _ := ca.KeyDER()
	ca2, err := LoadCA(der, kder)
	if err != nil {
		t.Fatal(err)
	}
	if ca2.Fingerprint() != ca.Fingerprint() {
		t.Fatal("fingerprint changed")
	}
	if id, _ := ca2.ClusterID(); id != testCluster {
		t.Fatalf("cluster id %q", id)
	}
}

func TestValidID(t *testing.T) {
	for _, bad := range []string{"", "A", "a/b", "a.b", strings.Repeat("a", 65)} {
		if ValidID(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if ValidID("abc-123") != nil {
		t.Error("valid id rejected")
	}
}

// The Postgres server certificate verifies for every listed host against
// the cluster CA, and can never pass as a member's node certificate.
func TestPostgresServerCert(t *testing.T) {
	ca := newCA(t, testCluster)
	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	der, err := ca.IssuePostgresServer(&key.PublicKey, []string{"100.64.0.43", "db.example", "127.0.0.1"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	for _, h := range []string{"100.64.0.43", "db.example", "127.0.0.1"} {
		if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, DNSName: h}); err != nil {
			t.Fatalf("verify for %s: %v", h, err)
		}
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, DNSName: "10.0.0.9"}); err == nil {
		t.Fatal("verified for a host it does not name")
	}
	if _, err := VerifyNodeCert(der, ca.Cert, testCluster, time.Now()); err == nil {
		t.Fatal("the postgres certificate passed as a node certificate")
	}
}
