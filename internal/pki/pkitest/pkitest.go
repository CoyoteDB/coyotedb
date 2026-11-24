// Package pkitest builds throwaway cluster identities for tests.
package pkitest

import (
	"testing"
	"time"

	"coyotedb.com/coyote/internal/pki"
)

// ClusterID is a fixed cluster id for tests.
const ClusterID = "7e57c1a57e57c1a57e57c1a57e57c1a5"

// CA creates a cluster CA for ClusterID.
func CA(t testing.TB) *pki.CA {
	t.Helper()
	ca, err := pki.NewCA(ClusterID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

// Node issues a node identity signed by ca.
func Node(t testing.TB, ca *pki.CA, nodeID string) *pki.NodeTLS {
	t.Helper()
	key, err := pki.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	cid, err := ca.ClusterID()
	if err != nil {
		t.Fatal(err)
	}
	csr, err := pki.NewCSR(key, cid, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	der, err := ca.IssueNode(csr, nodeID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	n, err := pki.NewNodeTLS(ca.Cert.Raw, der, key)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
