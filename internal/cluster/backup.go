package cluster

import (
	"log/slog"
	"time"
)

// Backup destination kinds (D63).
const (
	BackupKindFolder = "folder"
	BackupKindS3     = "s3"
)

// BackupS3 says how to reach S3-compatible storage. SecretKey is secret.
type BackupS3 struct {
	Endpoint  string `json:"endpoint"`
	Region    string `json:"region,omitempty"`
	Bucket    string `json:"bucket"`
	Prefix    string `json:"prefix,omitempty"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
	PathStyle string `json:"path_style,omitempty"` // "", "on", "off"
}

// BackupSettings are the cluster's backup settings (D63). A State without
// them uses DefaultBackupSettings. Key and S3.SecretKey are secrets: they
// travel inside Raft over mTLS like PGSecrets and are never logged.
type BackupSettings struct {
	Enabled bool   `json:"enabled"`
	Kind    string `json:"kind"`
	// Folder: "" = next to each node's data folder ("<data-dir>-backups").
	Folder  string    `json:"folder,omitempty"`
	S3      *BackupS3 `json:"s3,omitempty"`
	Encrypt bool      `json:"encrypt,omitempty"` // always on for S3
	Key     string    `json:"key,omitempty"`     // backup.EncodeKey form
	// Every and At: full backups at At (HH:MM, each node's local time)
	// and every Every after it.
	Every time.Duration `json:"every"`
	At    string        `json:"at"`
	// Keep: how far back a restore must reach.
	Keep time.Duration `json:"keep"`
	// ArchiveTimeout: archive_timeout (most a restore can miss).
	ArchiveTimeout time.Duration `json:"archive_timeout"`
	// ChangedAt is when the settings were last set (proposer's clock).
	ChangedAt time.Time `json:"changed_at,omitzero"`
}

// DefaultBackupSettings: on, a folder next to the data folder, a full
// backup every night at 03:00, WAL continuously, 7 days kept.
func DefaultBackupSettings() BackupSettings {
	return BackupSettings{
		Enabled: true, Kind: BackupKindFolder,
		Every: 24 * time.Hour, At: "03:00", Keep: 7 * 24 * time.Hour,
		ArchiveTimeout: time.Minute,
	}
}

// EffectiveBackup is the state's backup settings or the defaults.
func (s State) EffectiveBackup() BackupSettings {
	if s.Backup != nil {
		return *s.Backup
	}
	return DefaultBackupSettings()
}

// LogValue redacts the secrets in slog output.
func (b BackupSettings) LogValue() slog.Value {
	return slog.GroupValue(slog.Bool("enabled", b.Enabled), slog.String("kind", b.Kind))
}

// String redacts the secrets.
func (b BackupSettings) String() string { return "BackupSettings{" + b.Kind + ", redacted}" }

// GoString redacts the secrets (%#v).
func (b BackupSettings) GoString() string { return b.String() }

func (b *BackupSettings) clone() *BackupSettings {
	if b == nil {
		return nil
	}
	c := *b
	if b.S3 != nil {
		s := *b.S3
		c.S3 = &s
	}
	return &c
}

// SetBackup replaces the cluster's backup settings. Written only when every
// member's agent knows it (the "caps" RPC says "backup"), so an older state
// machine never halts on it.
type SetBackup struct {
	Backup BackupSettings `json:"backup"`
}

func (*SetBackup) commandType() string { return "set_backup" }

func applySetBackup(st *State, c *SetBackup) error {
	b := c.Backup
	switch b.Kind {
	case BackupKindFolder:
		b.S3 = nil
	case BackupKindS3:
		if b.S3 == nil || b.S3.Endpoint == "" || b.S3.Bucket == "" || b.S3.AccessKey == "" || b.S3.SecretKey == "" {
			return errorf(ErrInvalid, "cloud storage needs an endpoint, a bucket, an access key and a secret key")
		}
		if b.Key == "" {
			return errorf(ErrInvalid, "cloud backups are always encrypted and need a key")
		}
		b.Encrypt = true
	default:
		return errorf(ErrInvalid, "unknown backup destination %q", b.Kind)
	}
	if b.Encrypt && b.Key == "" {
		return errorf(ErrInvalid, "encrypted backups need a key")
	}
	if b.Every < time.Hour || b.Every > 31*24*time.Hour {
		return errorf(ErrInvalid, "full backups must be between every hour and every 31 days")
	}
	if b.Keep < b.Every {
		return errorf(ErrInvalid, "backups must be kept at least as long as the time between them")
	}
	if b.ArchiveTimeout < 10*time.Second || b.ArchiveTimeout > time.Hour {
		return errorf(ErrInvalid, "archive_timeout must be between 10s and 1h")
	}
	if _, err := time.Parse("15:04", b.At); err != nil {
		return errorf(ErrInvalid, "the backup time must look like 03:00")
	}
	st.Backup = &b
	return nil
}
