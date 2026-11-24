package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"coyotedb.com/coyote/internal/durable"
)

// Destination kinds.
const (
	KindFolder = "folder"
	KindS3     = "s3"
)

// S3Settings say how to reach an S3-compatible bucket.
type S3Settings struct {
	Endpoint  string `json:"endpoint"`
	Region    string `json:"region,omitempty"`
	Bucket    string `json:"bucket"`
	Prefix    string `json:"prefix,omitempty"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
	// PathStyle: "" = automatic (virtual-host for amazonaws.com, path
	// style elsewhere), "on", "off".
	PathStyle string `json:"path_style,omitempty"`
}

// Config is one node's resolved backup configuration, rendered by the
// agent into <data-dir>/backup/config.json (0600) for wal-push, or built
// from flags for a restore when the cluster is gone.
type Config struct {
	Enabled   bool       `json:"enabled"`
	ClusterID string     `json:"cluster_id"`
	Node      string     `json:"node"`
	Kind      string     `json:"kind"`
	Folder    string     `json:"folder,omitempty"` // absolute, this machine
	S3        S3Settings `json:"s3,omitzero"`
	Key       string     `json:"key,omitempty"` // EncodeKey form; "" = not encrypted
}

// ConfigFile is where the agent renders Config inside a node's folder.
func ConfigFile(stateDir string) string { return filepath.Join(StateDir(stateDir), "config.json") }

// StateDir is the node's own backup bookkeeping folder.
func StateDir(stateDir string) string { return filepath.Join(stateDir, "backup") }

// ReadConfig reads a rendered config. A missing file is a disabled config.
func ReadConfig(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// WriteConfig writes a rendered config durably (0600: it may hold the
// storage secret and the key).
func WriteConfig(path string, c Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return durable.WriteFile(path, append(b, '\n'), 0o600)
}

// Codec returns the codec for this config.
func (c Config) Codec() (Codec, error) {
	if c.Key == "" {
		return Codec{}, nil
	}
	k, err := DecodeKey(c.Key)
	if err != nil {
		return Codec{}, err
	}
	return Codec{Key: k}, nil
}

// Shared reports whether every node writes one shared set (S3) instead of
// its own (folder).
func (c Config) Shared() bool { return c.Kind == KindS3 }

// BaseStore opens the destination without the cluster folder.
func (c Config) BaseStore() (Store, error) {
	switch c.Kind {
	case KindFolder:
		if c.Folder == "" || !filepath.IsAbs(c.Folder) {
			return nil, fmt.Errorf("the backup folder must be a full path (got %q)", c.Folder)
		}
		return FolderStore{Root: c.Folder}, nil
	case KindS3:
		s := c.S3
		if s.Endpoint == "" || s.Bucket == "" || s.AccessKey == "" || s.SecretKey == "" {
			return nil, ErrS3Config
		}
		prefix := strings.Trim(s.Prefix, "/")
		if prefix != "" {
			prefix += "/"
		}
		ps := s.PathStyle == "on"
		if s.PathStyle == "" {
			u, err := url.Parse(s.Endpoint)
			if err != nil {
				return nil, fmt.Errorf("bad storage endpoint: %w", err)
			}
			ps = !strings.HasSuffix(u.Hostname(), "amazonaws.com")
		}
		return &S3Store{Endpoint: s.Endpoint, Region: s.Region, Bucket: s.Bucket, Prefix: prefix,
			AccessKey: s.AccessKey, SecretKey: s.SecretKey, PathStyle: ps}, nil
	}
	return nil, fmt.Errorf("unknown backup destination %q", c.Kind)
}

// Store opens this cluster's folder inside the destination.
func (c Config) Store() (Store, error) {
	if c.ClusterID == "" {
		return nil, errors.New("backup config has no cluster id")
	}
	b, err := c.BaseStore()
	if err != nil {
		return nil, err
	}
	return Sub{S: b, Prefix: c.ClusterID + "/"}, nil
}

// Label says where backups go, for people.
func (c Config) Label() string {
	switch c.Kind {
	case KindFolder:
		return c.Folder
	case KindS3:
		if b, err := c.BaseStore(); err == nil {
			return b.Label()
		}
		return "s3://" + c.S3.Bucket
	}
	return ""
}

// ---- archive identity ----

// IdentityKey is the archive's identity object.
const IdentityKey = "coyote-backup.json"

// Identity ties an archive to one cluster and one Postgres database.
type Identity struct {
	Format    int       `json:"format"`
	ClusterID string    `json:"cluster_id"`
	SystemID  string    `json:"system_identifier"`
	KeyID     string    `json:"key_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// ErrOtherDatabase: the archive belongs to another database.
var ErrOtherDatabase = errors.New("this backup location already holds backups of a different database; choose another folder or prefix")

// EnsureIdentity checks the archive identity, creating it if missing.
// sysid "" skips the database check (history files carry no id).
func EnsureIdentity(ctx context.Context, st Store, c Codec, clusterID, sysid string) (Identity, error) {
	for range 3 {
		b, err := st.Get(ctx, IdentityKey)
		if errors.Is(err, ErrNotFound) {
			if sysid == "" {
				return Identity{}, nil // created by the first segment or full backup
			}
			id := Identity{Format: 1, ClusterID: clusterID, SystemID: sysid, KeyID: KeyID(c.Key), CreatedAt: time.Now().UTC()}
			nb, _ := json.MarshalIndent(id, "", "  ")
			if err := st.Put(ctx, IdentityKey, nb, true); err != nil && !errors.Is(err, ErrExists) {
				return Identity{}, err
			}
			continue
		}
		if err != nil {
			return Identity{}, err
		}
		var id Identity
		if err := json.Unmarshal(b, &id); err != nil {
			return Identity{}, fmt.Errorf("%s: %w", IdentityKey, err)
		}
		if id.ClusterID != clusterID || (sysid != "" && id.SystemID != sysid) {
			return id, ErrOtherDatabase
		}
		if id.KeyID != "" && id.KeyID != KeyID(c.Key) {
			return id, fmt.Errorf("this backup location was written with a different backup key: %w", ErrWrongKey)
		}
		return id, nil
	}
	return Identity{}, errors.New("could not create the backup identity file")
}

// ReadIdentity reads the identity of an archive.
func ReadIdentity(ctx context.Context, st Store) (Identity, error) {
	b, err := st.Get(ctx, IdentityKey)
	if err != nil {
		return Identity{}, err
	}
	var id Identity
	err = json.Unmarshal(b, &id)
	return id, err
}
