// Package pki holds the cluster certificate authority, node certificates and
// the TLS settings used for all agent-to-agent traffic.
//
// Identity is carried in a URI SAN, never in hostnames or IP addresses, because
// nodes move between Tailscale and LAN addresses:
//
//	CA:   coyote://<cluster-id>
//	node: coyote://<cluster-id>/node/<node-id>
//
// All TLS is 1.3 only, with mutual authentication. Verification checks the
// chain against the cluster CA and the SAN scheme; the peer's address is
// ignored.
package pki

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"coyotedb.com/coyote/internal/brand"
	"coyotedb.com/coyote/internal/durable"
)

// Lifetimes. Node certificates are long-lived on purpose: revocation is done
// by cluster membership (a removed node's valid cert is refused), and there
// is no renewal flow yet.
const (
	CAValidity   = 20 * 365 * 24 * time.Hour
	NodeValidity = 10 * 365 * 24 * time.Hour
	// clockSkew backdates NotBefore so a peer whose clock is a little behind
	// still accepts a fresh certificate.
	clockSkew = time.Hour
)

// URIScheme is the scheme of identity SANs.
const URIScheme = brand.Command

var (
	// ErrWrongCluster means the peer's certificate is for another cluster.
	ErrWrongCluster = errors.New("certificate belongs to a different cluster")
	// ErrNoIdentity means the certificate has no usable coyote:// URI SAN.
	ErrNoIdentity = errors.New("certificate has no node identity")
	// ErrFingerprint means no presented certificate matched the pinned CA fingerprint.
	ErrFingerprint = errors.New("server is not part of the invited cluster (CA fingerprint mismatch)")
)

// CA is the cluster certificate authority.
type CA struct {
	Cert *x509.Certificate
	Key  *ecdsa.PrivateKey
}

// NewCA creates a self-signed ECDSA P-256 CA for clusterID.
func NewCA(clusterID string, now time.Time) (*CA, error) {
	if err := validID(clusterID); err != nil {
		return nil, fmt.Errorf("cluster id: %w", err)
	}
	key, err := GenerateKey()
	if err != nil {
		return nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: brand.Product + " cluster CA " + clusterID},
		NotBefore:             now.Add(-clockSkew),
		NotAfter:              now.Add(CAValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
		URIs:                  []*url.URL{clusterURI(clusterID)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("create CA certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &CA{Cert: cert, Key: key}, nil
}

// LoadCA parses a CA from its certificate DER and PKCS#8 key DER.
func LoadCA(certDER, keyDER []byte) (*CA, error) {
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, fmt.Errorf("parse CA certificate: %w", err)
	}
	if !cert.IsCA {
		return nil, errors.New("certificate is not a CA")
	}
	k, err := x509.ParsePKCS8PrivateKey(keyDER)
	if err != nil {
		return nil, fmt.Errorf("parse CA key: %w", err)
	}
	key, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("CA key is not ECDSA")
	}
	if !key.PublicKey.Equal(cert.PublicKey) {
		return nil, errors.New("CA key does not match CA certificate")
	}
	return &CA{Cert: cert, Key: key}, nil
}

// KeyDER returns the CA private key as PKCS#8 DER.
func (c *CA) KeyDER() ([]byte, error) { return x509.MarshalPKCS8PrivateKey(c.Key) }

// Fingerprint returns sha256 of the CA certificate DER.
func (c *CA) Fingerprint() [32]byte { return Fingerprint(c.Cert.Raw) }

// ClusterID returns the cluster id from the CA's URI SAN.
func (c *CA) ClusterID() (string, error) {
	for _, u := range c.Cert.URIs {
		if u.Scheme == URIScheme && u.Path == "" && validID(u.Host) == nil {
			return u.Host, nil
		}
	}
	return "", ErrNoIdentity
}

// Fingerprint returns sha256 of a certificate's DER bytes.
func Fingerprint(der []byte) [32]byte { return sha256.Sum256(der) }

// FingerprintString renders a fingerprint as lowercase hex.
func FingerprintString(fp [32]byte) string { return hex.EncodeToString(fp[:]) }

// GenerateKey creates a new ECDSA P-256 key.
func GenerateKey() (*ecdsa.PrivateKey, error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	return k, nil
}

// NewCSR creates a certificate request for nodeID in clusterID.
func NewCSR(key *ecdsa.PrivateKey, clusterID, nodeID string) ([]byte, error) {
	if err := validID(clusterID); err != nil {
		return nil, fmt.Errorf("cluster id: %w", err)
	}
	if err := validID(nodeID); err != nil {
		return nil, fmt.Errorf("node id: %w", err)
	}
	tmpl := &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: nodeID},
		URIs:    []*url.URL{NodeURI(clusterID, nodeID)},
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		return nil, fmt.Errorf("create CSR: %w", err)
	}
	return der, nil
}

// SignCSR issues a node certificate for nodeID valid from notBefore to
// notAfter. The CSR's own subject and SANs are ignored: the identity comes
// from the arguments, which the caller has authorized. The CSR must carry a
// valid self-signature and an ECDSA P-256 key.
func (c *CA) SignCSR(csrDER []byte, nodeID string, notBefore, notAfter time.Time) ([]byte, error) {
	clusterID, err := c.ClusterID()
	if err != nil {
		return nil, err
	}
	if err := validID(nodeID); err != nil {
		return nil, fmt.Errorf("node id: %w", err)
	}
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return nil, fmt.Errorf("parse CSR: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("CSR signature: %w", err)
	}
	pub, ok := csr.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		return nil, errors.New("CSR key must be ECDSA P-256")
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: nodeID},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		URIs:         []*url.URL{NodeURI(clusterID, nodeID)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.Cert, pub, c.Key)
	if err != nil {
		return nil, fmt.Errorf("sign node certificate: %w", err)
	}
	return der, nil
}

// IssueNode is SignCSR with the default lifetime starting at now.
func (c *CA) IssueNode(csrDER []byte, nodeID string, now time.Time) ([]byte, error) {
	return c.SignCSR(csrDER, nodeID, now.Add(-clockSkew), now.Add(NodeValidity))
}

// CSRKeyHash verifies a CSR's self-signature and returns the hex sha256 of
// its public key (PKIX DER). It identifies a joiner across retries.
func CSRKeyHash(csrDER []byte) (string, error) {
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return "", fmt.Errorf("parse CSR: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return "", fmt.Errorf("CSR signature: %w", err)
	}
	der, err := x509.MarshalPKIXPublicKey(csr.PublicKey)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(der)
	return hex.EncodeToString(h[:]), nil
}

// NodeURI is the identity SAN for a node.
func NodeURI(clusterID, nodeID string) *url.URL {
	return &url.URL{Scheme: URIScheme, Host: clusterID, Path: "/node/" + nodeID}
}

func clusterURI(clusterID string) *url.URL {
	return &url.URL{Scheme: URIScheme, Host: clusterID}
}

// Identity extracts (clusterID, nodeID) from a node certificate's URI SAN.
func Identity(cert *x509.Certificate) (clusterID, nodeID string, err error) {
	for _, u := range cert.URIs {
		if u.Scheme != URIScheme {
			continue
		}
		id, ok := strings.CutPrefix(u.Path, "/node/")
		if !ok || validID(id) != nil || validID(u.Host) != nil {
			continue
		}
		return u.Host, id, nil
	}
	return "", "", ErrNoIdentity
}

// verifyNode verifies leaf (with optional intermediates ignored: the cluster
// CA signs leaves directly) against roots for usage, at time now, and checks
// the SAN belongs to clusterID. It returns the node id.
func verifyNode(leaf *x509.Certificate, roots *x509.CertPool, clusterID string, usage x509.ExtKeyUsage, now time.Time) (string, error) {
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:       roots,
		CurrentTime: now,
		KeyUsages:   []x509.ExtKeyUsage{usage},
	}); err != nil {
		return "", fmt.Errorf("verify peer certificate: %w", err)
	}
	cid, nid, err := Identity(leaf)
	if err != nil {
		return "", err
	}
	if cid != clusterID {
		return "", fmt.Errorf("%w: got %q, want %q", ErrWrongCluster, cid, clusterID)
	}
	return nid, nil
}

// VerifyNodeCert verifies a node certificate against ca (both usages are
// checked) and returns its node id.
func VerifyNodeCert(certDER []byte, caCert *x509.Certificate, clusterID string, now time.Time) (string, error) {
	leaf, err := x509.ParseCertificate(certDER)
	if err != nil {
		return "", err
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	if _, err := verifyNode(leaf, pool, clusterID, x509.ExtKeyUsageClientAuth, now); err != nil {
		return "", err
	}
	return verifyNode(leaf, pool, clusterID, x509.ExtKeyUsageServerAuth, now)
}

// NodeTLS is the identity material of one node.
type NodeTLS struct {
	ClusterID string
	NodeID    string
	CA        *x509.Certificate
	Cert      tls.Certificate // leaf (+ CA appended so joiners can pin it)
	// Now overrides the clock for verification (tests). Nil means time.Now.
	Now func() time.Time
}

// NewNodeTLS builds NodeTLS from DER/key material and checks the leaf
// matches clusterID/nodeID and chains to the CA.
func NewNodeTLS(caDER, certDER []byte, key *ecdsa.PrivateKey) (*NodeTLS, error) {
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, fmt.Errorf("parse CA: %w", err)
	}
	leaf, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, fmt.Errorf("parse node certificate: %w", err)
	}
	if !key.PublicKey.Equal(leaf.PublicKey) {
		return nil, errors.New("node key does not match node certificate")
	}
	if err := leaf.CheckSignatureFrom(ca); err != nil {
		return nil, fmt.Errorf("node certificate not signed by cluster CA: %w", err)
	}
	cid, nid, err := Identity(leaf)
	if err != nil {
		return nil, err
	}
	return &NodeTLS{
		ClusterID: cid,
		NodeID:    nid,
		CA:        ca,
		Cert: tls.Certificate{
			Certificate: [][]byte{certDER, caDER},
			PrivateKey:  key,
			Leaf:        leaf,
		},
	}, nil
}

func (n *NodeTLS) now() time.Time {
	if n.Now != nil {
		return n.Now()
	}
	return time.Now()
}

func (n *NodeTLS) pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(n.CA)
	return p
}

// ServerConfig returns the listener config. Client certificates are
// requested but optional: a client without one is anonymous and may only
// use the join stream (the transport enforces that). A client that presents a
// certificate must pass full verification or the handshake fails.
func (n *NodeTLS) ServerConfig() *tls.Config {
	pool := n.pool()
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{n.Cert},
		ClientAuth:   tls.RequestClientCert,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return nil
			}
			_, err := verifyNode(cs.PeerCertificates[0], pool, n.ClusterID, x509.ExtKeyUsageClientAuth, n.now())
			return err
		},
	}
}

// ClientConfig returns a config for dialing the node expectNodeID. The server
// certificate is verified against the cluster CA and its SAN must name
// expectNodeID (addresses are not trusted to identify a node).
func (n *NodeTLS) ClientConfig(expectNodeID string) *tls.Config {
	pool := n.pool()
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{n.Cert},
		// Hostname checks are replaced by VerifyConnection below.
		InsecureSkipVerify: true,
		ServerName:         brand.Command,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("server sent no certificate")
			}
			id, err := verifyNode(cs.PeerCertificates[0], pool, n.ClusterID, x509.ExtKeyUsageServerAuth, n.now())
			if err != nil {
				return err
			}
			if expectNodeID != "" && id != expectNodeID {
				return fmt.Errorf("reached node %q, expected %q", id, expectNodeID)
			}
			return nil
		},
	}
}

// JoinClientConfig is used by a node that has no certificate yet. It trusts
// the server only if the server presents the CA whose sha256 fingerprint is
// caFingerprint, the leaf chains to it, and the leaf is for clusterID.
func JoinClientConfig(clusterID string, caFingerprint [32]byte, now func() time.Time) *tls.Config {
	if now == nil {
		now = time.Now
	}
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		MaxVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true,
		ServerName:         brand.Command,
		VerifyConnection: func(cs tls.ConnectionState) error {
			_, err := VerifyPinned(cs.PeerCertificates, clusterID, caFingerprint, now())
			return err
		},
	}
}

// VerifyPinned checks a server chain against a pinned CA fingerprint and
// returns the CA certificate.
func VerifyPinned(chain []*x509.Certificate, clusterID string, caFingerprint [32]byte, now time.Time) (*x509.Certificate, error) {
	if len(chain) == 0 {
		return nil, errors.New("server sent no certificate")
	}
	var ca *x509.Certificate
	for _, c := range chain[1:] {
		if fp := Fingerprint(c.Raw); bytes.Equal(fp[:], caFingerprint[:]) {
			ca = c
			break
		}
	}
	if ca == nil {
		return nil, ErrFingerprint
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	if _, err := verifyNode(chain[0], pool, clusterID, x509.ExtKeyUsageServerAuth, now); err != nil {
		return nil, err
	}
	return ca, nil
}

// PeerNodeID returns the verified node id of the peer of a completed
// handshake, or "" if the peer presented no certificate. Call only on a
// connection whose config came from this package (verification already ran).
func PeerNodeID(cs tls.ConnectionState) string {
	if len(cs.PeerCertificates) == 0 {
		return ""
	}
	_, id, err := Identity(cs.PeerCertificates[0])
	if err != nil {
		return ""
	}
	return id
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 127)
	s, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("serial: %w", err)
	}
	return s.Add(s, big.NewInt(1)), nil
}

// validID accepts the ids this package puts in URIs: 1..64 chars of
// [a-z0-9-].
func validID(s string) error {
	if len(s) == 0 || len(s) > 64 {
		return fmt.Errorf("invalid id %q: length must be 1..64", s)
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return fmt.Errorf("invalid id %q: only a-z, 0-9 and '-' allowed", s)
		}
	}
	return nil
}

// ValidID reports whether s is a valid cluster or node id.
func ValidID(s string) error { return validID(s) }

// ---- files ----

// WriteKey writes key as PEM PKCS#8 with mode 0600.
func WriteKey(path string, key *ecdsa.PrivateKey) error {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600)
}

// ReadKey reads a PEM PKCS#8 ECDSA key.
func ReadKey(path string) (*ecdsa.PrivateKey, error) {
	der, err := readPEM(path, "PRIVATE KEY")
	if err != nil {
		return nil, err
	}
	k, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("parse key %s: %w", path, err)
	}
	ek, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("key %s is not ECDSA", path)
	}
	return ek, nil
}

// WriteCert writes a certificate DER as PEM (mode 0600; certs are not
// secret, but the whole state dir is private).
func WriteCert(path string, der []byte) error {
	return WriteFileAtomic(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
}

// ReadCert reads a PEM certificate and returns its DER.
func ReadCert(path string) ([]byte, error) { return readPEM(path, "CERTIFICATE") }

func readPEM(path, typ string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != typ {
		return nil, fmt.Errorf("%s: no %s PEM block", path, typ)
	}
	return blk.Bytes, nil
}

// WriteFileAtomic replaces path with data durably (temp file, fsync,
// rename, fsync of the directory). The file mode is perm (0600 for
// secrets).
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	return durable.WriteFile(path, data, perm)
}

// PostgresValidity is how long a Postgres server certificate is valid. The
// agent re-issues it on every start and whenever the members' addresses
// change, long before that.
const PostgresValidity = 90 * 24 * time.Hour

// IssuePostgresServer issues the certificate Postgres presents to clients
// (ssl=on), so a client holding the cluster CA certificate can verify the
// server (sslmode=verify-full). hosts are the names and addresses clients
// may dial: every member's address, because any node's proxy passes the
// connection through to whichever node is the main node.
//
// It is deliberately not a node certificate: no node URI and server use
// only, so it can never stand in for a member on the agent port.
func (c *CA) IssuePostgresServer(pub *ecdsa.PublicKey, hosts []string, now time.Time) ([]byte, error) {
	if pub == nil || pub.Curve != elliptic.P256() {
		return nil, errors.New("key must be ECDSA P-256")
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "coyote postgres"},
		NotBefore:    now.Add(-clockSkew),
		NotAfter:     now.Add(PostgresValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else if h != "" {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.Cert, pub, c.Key)
	if err != nil {
		return nil, fmt.Errorf("sign postgres certificate: %w", err)
	}
	return der, nil
}
