package cluster

import (
	"crypto/ecdsa"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"coyotedb.com/coyote/internal/pki"
)

// On-disk layout under Config.Dir (directories 0700, files 0600):
//
//	identity.json      cluster id, node id, name, role
//	node.key           this node's private key (PEM PKCS#8)
//	node.crt, ca.crt   this node's certificate and the cluster CA cert
//	peers.json         last known members + addresses, with the Raft index
//	                   they were read at (used before the FSM catches up)
//	join.pending.json  only while a Join is unfinished (invite, node id;
//	                   node.key is written with it) so a retry resumes
//	bootstrap.pending  only between Bootstrap's Raft bootstrap and its first
//	                   command (holds the CA key until it is in Raft)
//	raft/raft.db       Raft log + stable store (bbolt)
//	raft/snapshots/    Raft snapshots (contain the CA key: keep private)
const (
	fileIdentity    = "identity.json"
	fileKey         = "node.key"
	fileCert        = "node.crt"
	fileCA          = "ca.crt"
	filePeers       = "peers.json"
	filePending     = "bootstrap.pending"
	fileJoinPending = "join.pending.json"
	joinPendingFmt  = 1
	dirRaft         = "raft"
	fileRaftDB      = "raft.db"
	identityFmt     = 1
	peersCacheFmt   = 1
)

type identity struct {
	Format    int    `json:"format"`
	ClusterID string `json:"cluster_id"`
	NodeID    string `json:"node_id"`
	Name      string `json:"name"`
	Role      Role   `json:"role"`
}

// peerCache is the address book and authorization fallback used while the
// local FSM is behind (fresh join, restart before log replay).
type peerCache struct {
	Format  int                 `json:"format"`
	Index   uint64              `json:"index"`
	Members map[string][]string `json:"members"`
	// Names are the members' names (for messages while the FSM is empty).
	Names map[string]string `json:"names,omitempty"`
	// Local are addresses given on this machine with `node set-address
	// --local` (D62), dialed before the recorded ones. Each entry is
	// dropped once the cluster's record holds all of its addresses.
	Local map[string][]string `json:"local,omitempty"`
}

func (c peerCache) clone() peerCache {
	m := make(map[string][]string, len(c.Members))
	for k, v := range c.Members {
		m[k] = slices.Clone(v)
	}
	c.Members = m
	c.Names = maps.Clone(c.Names)
	if c.Local != nil {
		l := make(map[string][]string, len(c.Local))
		for k, v := range c.Local {
			l[k] = slices.Clone(v)
		}
		c.Local = l
	}
	return c
}

func (c peerCache) equalMembers(o map[string][]string) bool {
	return maps.EqualFunc(c.Members, o, slices.Equal[[]string])
}

// HasState reports whether dir already holds a node identity.
func HasState(dir string) (bool, error) {
	_, err := os.Stat(filepath.Join(dir, fileIdentity))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// Identity is what a node's state dir says about the node.
type Identity struct {
	ClusterID string
	NodeID    string
	Name      string
	Role      Role
}

// ReadIdentity reads the identity of a node that has joined or bootstrapped
// (HasState is true), without starting it.
func ReadIdentity(dir string) (Identity, error) {
	id, err := loadIdentity(dir)
	if err != nil {
		return Identity{}, err
	}
	return Identity{ClusterID: id.ClusterID, NodeID: id.NodeID, Name: id.Name, Role: id.Role}, nil
}

// HasPendingJoin reports whether dir holds an unfinished Join (rerun Join
// to resume it).
func HasPendingJoin(dir string) (bool, error) {
	p, err := loadJoinPending(dir)
	return p != nil, err
}

func ensureDirs(dir string) error {
	for _, d := range []string{dir, filepath.Join(dir, dirRaft)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
		// Tighten a pre-existing dir too: raft writes snapshots (which hold
		// the CA key) with 0755/0644 modes, so these parents must be private.
		// On Windows this only touches the read-only bit; the data dir ACL
		// protects it there.
		if err := os.Chmod(d, 0o700); err != nil {
			return err
		}
	}
	return nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return pki.WriteFileAtomic(path, b, 0o600)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func loadIdentity(dir string) (identity, error) {
	var id identity
	if err := readJSON(filepath.Join(dir, fileIdentity), &id); err != nil {
		return id, err
	}
	if id.Format != identityFmt {
		return id, fmt.Errorf("%s: unknown format %d", fileIdentity, id.Format)
	}
	return id, nil
}

func loadPeers(dir string) (peerCache, error) {
	var c peerCache
	err := readJSON(filepath.Join(dir, filePeers), &c)
	if errors.Is(err, fs.ErrNotExist) {
		return peerCache{Format: peersCacheFmt, Members: map[string][]string{}}, nil
	}
	if err != nil {
		return c, err
	}
	if c.Format != peersCacheFmt {
		return c, fmt.Errorf("%s: unknown format %d", filePeers, c.Format)
	}
	if c.Members == nil {
		c.Members = map[string][]string{}
	}
	return c, nil
}

// writeNodeFiles stores identity material for a new node.
func writeNodeFiles(dir string, id identity, keyPEMWriter func(path string) error, certDER, caDER []byte, peers peerCache) error {
	if err := ensureDirs(dir); err != nil {
		return err
	}
	if err := keyPEMWriter(filepath.Join(dir, fileKey)); err != nil {
		return err
	}
	if err := pki.WriteCert(filepath.Join(dir, fileCert), certDER); err != nil {
		return err
	}
	if err := pki.WriteCert(filepath.Join(dir, fileCA), caDER); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(dir, filePeers), peers); err != nil {
		return err
	}
	// Identity last: its presence means the node files are complete.
	id.Format = identityFmt
	return writeJSON(filepath.Join(dir, fileIdentity), id)
}

func loadNodeTLS(dir string) (*pki.NodeTLS, error) {
	key, err := pki.ReadKey(filepath.Join(dir, fileKey))
	if err != nil {
		return nil, err
	}
	cert, err := pki.ReadCert(filepath.Join(dir, fileCert))
	if err != nil {
		return nil, err
	}
	ca, err := pki.ReadCert(filepath.Join(dir, fileCA))
	if err != nil {
		return nil, err
	}
	return pki.NewNodeTLS(ca, cert, key)
}

func randomID(nbytes int) string {
	b := make([]byte, nbytes)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return hex.EncodeToString(b)
}

// joinPending is the saved state of an unfinished Join.
type joinPending struct {
	Format int    `json:"format"`
	Invite string `json:"invite"`
	NodeID string `json:"node_id"`
	Name   string `json:"name"`
	Role   Role   `json:"role"`
}

func loadJoinPending(dir string) (*joinPending, error) {
	var p joinPending
	err := readJSON(filepath.Join(dir, fileJoinPending), &p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if p.Format != joinPendingFmt {
		return nil, fmt.Errorf("%s: unknown format %d", fileJoinPending, p.Format)
	}
	return &p, nil
}

func saveJoinPending(dir string, p *joinPending, key *ecdsa.PrivateKey) error {
	if err := ensureDirs(dir); err != nil {
		return err
	}
	if err := pki.WriteKey(filepath.Join(dir, fileKey), key); err != nil {
		return err
	}
	return writeJSON(filepath.Join(dir, fileJoinPending), p)
}

// ClearPendingJoin forgets a join that did not finish (its saved request
// and the node key made for it), so the next join starts over. Refused
// once the node is a member.
func ClearPendingJoin(dir string) error {
	if has, err := HasState(dir); err != nil || has {
		return errors.Join(err, errors.New("cluster: this node already belongs to a cluster"))
	}
	return clearJoinPending(dir)
}

func clearJoinPending(dir string) error {
	return errors.Join(removeFile(filepath.Join(dir, fileJoinPending)), removeFile(filepath.Join(dir, fileKey)))
}
