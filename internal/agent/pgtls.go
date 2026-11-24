package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"coyotedb.com/coyote/internal/cluster"
	"coyotedb.com/coyote/internal/pg"
	"coyotedb.com/coyote/internal/pki"
)

// Client TLS for Postgres (see pg.RenderHBA). Every data node presents a
// certificate from the cluster CA naming every member's addresses, because
// a client may dial any node's proxy and the bytes are passed through to
// whichever node is the main node. The key is this node's own and never
// leaves it; the files live outside pgdata.

// pgTLSHosts are the names and addresses clients may dial.
func pgTLSHosts(st cluster.State) []string {
	hosts := []string{"127.0.0.1", "::1", "localhost"}
	for _, m := range st.Members {
		for _, a := range m.Addresses {
			if h, _, err := net.SplitHostPort(a); err == nil && h != "" {
				hosts = append(hosts, h)
			}
		}
	}
	slices.Sort(hosts)
	return slices.Compact(hosts)
}

// renewBefore: a certificate this close to expiry is replaced.
const renewBefore = 30 * 24 * time.Hour

// ensurePGTLS writes (or refreshes) the Postgres server certificate and
// key. It reports whether the files changed.
func (a *Agent) ensurePGTLS(st cluster.State) (bool, error) {
	if len(st.CA.CertDER) == 0 {
		return false, errors.New("the cluster state has no CA yet")
	}
	if err := a.writeCAFile(st); err != nil {
		return false, fmt.Errorf("write the CA certificate: %w", err)
	}
	certPath, keyPath := a.node.TLSFiles()
	hosts := pgTLSHosts(st)
	seen := strings.Join(hosts, ",")
	if a.pgTLSSeen == seen && time.Since(a.pgTLSChecked) < time.Hour {
		return false, nil
	}
	if certCurrent(certPath, st.CA.CertDER, hosts, time.Now()) {
		a.pgTLSSeen, a.pgTLSChecked = seen, time.Now()
		return false, nil
	}
	ca, err := pki.LoadCA(st.CA.CertDER, st.CA.KeyDER)
	if err != nil {
		return false, fmt.Errorf("load the cluster CA: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(certPath), 0o700); err != nil {
		return false, err
	}
	key, err := loadOrCreateKey(keyPath)
	if err != nil {
		return false, err
	}
	der, err := ca.IssuePostgresServer(&key.PublicKey, hosts, time.Now())
	if err != nil {
		return false, err
	}
	out := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := pg.WriteFileDurable(certPath, out, 0o600); err != nil {
		return false, err
	}
	a.log.Info("postgres TLS certificate issued", "hosts", len(hosts))
	a.pgTLSSeen, a.pgTLSChecked = seen, time.Now()
	return true, nil
}

// certCurrent: the certificate exists, was issued by this CA, names every
// host and is not close to expiry.
func certCurrent(path string, caDER []byte, hosts []string, now time.Time) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return false
	}
	cert, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return false
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil || cert.CheckSignatureFrom(caCert) != nil || now.Add(renewBefore).After(cert.NotAfter) {
		return false
	}
	var have []string
	for _, ip := range cert.IPAddresses {
		have = append(have, ip.String())
	}
	have = append(have, cert.DNSNames...)
	slices.Sort(have)
	want := slices.Clone(hosts)
	for i, h := range want {
		if ip := net.ParseIP(h); ip != nil {
			want[i] = ip.String()
		}
	}
	slices.Sort(want)
	return slices.Equal(slices.Compact(have), slices.Compact(want))
}

func loadOrCreateKey(path string) (*ecdsa.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		if blk, _ := pem.Decode(b); blk != nil {
			if k, err := x509.ParseECPrivateKey(blk.Bytes); err == nil {
				return k, nil
			}
		}
		return nil, fmt.Errorf("%s is not a valid key; remove it to make a new one", path)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	k, err := pki.GenerateKey()
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		return nil, err
	}
	if err := pg.WriteFileDurable(path, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		return nil, err
	}
	return k, nil
}

// refreshPGTLS re-issues the certificate when the members' addresses
// changed and makes a running Postgres load it (ssl files are re-read on
// reload).
func (a *Agent) refreshPGTLS(ctx context.Context, st cluster.State) error {
	changed, err := a.ensurePGTLS(st)
	if err != nil || !changed {
		return err
	}
	if srv, _ := a.currentPG(); srv == nil {
		return nil
	}
	c, err := a.node.Connect(ctx)
	if err != nil {
		return err
	}
	defer c.Close(context.Background())
	_, err = c.Exec(ctx, "SELECT pg_reload_conf()")
	return err
}
