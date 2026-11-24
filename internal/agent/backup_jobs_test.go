package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"coyotedb.com/coyote/internal/cluster"
	"coyotedb.com/coyote/internal/pg"
)

func TestWithin(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "srv")
	cases := []struct {
		p, dir string
		want   bool
	}{
		{filepath.Join(root, "data"), filepath.Join(root, "data"), true},
		{filepath.Join(root, "data", "x"), filepath.Join(root, "data"), true},
		{filepath.Join(root, "data", "..x"), filepath.Join(root, "data"), true},
		{filepath.Join(root, "data-backups"), filepath.Join(root, "data"), false},
		{root, filepath.Join(root, "data"), false},
	}
	for _, c := range cases {
		if got := within(c.p, c.dir); got != c.want {
			t.Errorf("within(%q, %q) = %v, want %v", c.p, c.dir, got, c.want)
		}
	}
}

// The setup page's folder check: before a cluster exists, with real
// folders on this computer's disk.
func TestCheckDestFolder(t *testing.T) {
	root := t.TempDir()
	n, err := pg.NewNode(filepath.Join(root, "node-a"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(n.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	a := &Agent{node: n}
	ctx := context.Background()

	// The default: next to the data folder, same disk, created on save.
	c := a.checkDest(ctx, BackupDestinationRequest{Kind: cluster.BackupKindFolder})
	if !c.OK || c.Folder != DefaultBackupFolder(n.StateDir) || c.Exists || !c.SameDisk || c.FreeBytes == 0 {
		t.Fatalf("default folder: %+v", c)
	}
	if ents, _ := os.ReadDir(root); len(ents) != 1 {
		t.Fatalf("the check left files behind: %v", ents)
	}

	other := filepath.Join(root, "usb", "backups")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if c := a.checkDest(ctx, BackupDestinationRequest{Kind: cluster.BackupKindFolder, Folder: other, Encrypt: true}); !c.OK || !c.Exists || !c.Encrypted {
		t.Fatalf("existing folder: %+v", c)
	}
	if ents, _ := os.ReadDir(other); len(ents) != 0 {
		t.Fatalf("the check left files behind: %v", ents)
	}

	bad := map[string]string{
		"relative/path":                     "full path",
		filepath.Join(n.StateDir, "backup"): "outside the database's own folder",
		root:                                "outside the database's own folder",
	}
	file := filepath.Join(root, "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	bad[filepath.Join(file, "sub")] = "is a file"
	for folder, want := range bad {
		c := a.checkDest(ctx, BackupDestinationRequest{Kind: cluster.BackupKindFolder, Folder: folder})
		if c.OK || !strings.Contains(c.Problem, want) {
			t.Errorf("%s: want a problem with %q, got %+v", folder, want, c)
		}
	}
}

func TestCheckDestS3Fields(t *testing.T) {
	a := &Agent{}
	ctx := context.Background()
	for _, s3 := range []*cluster.BackupS3{
		nil,
		{Bucket: "b", AccessKey: "k", SecretKey: "s"},
		{Endpoint: "s3.example.com", Bucket: "b", AccessKey: "k", SecretKey: "s"},
		{Endpoint: "https://s3.example.com", AccessKey: "k", SecretKey: "s"},
		{Endpoint: "https://s3.example.com", Bucket: "b", AccessKey: "k"},
	} {
		c := a.checkDest(ctx, BackupDestinationRequest{Kind: cluster.BackupKindS3, S3: s3})
		if c.OK || c.Problem == "" || !c.Encrypted {
			t.Errorf("%+v: want a problem, got %+v", s3, c)
		}
	}
}

func TestRestoredStartCommand(t *testing.T) {
	cmd := RestoredStartCommand(filepath.Join(t.TempDir(), "my restore"))
	for _, want := range []string{" up --data-dir ", "my restore", "--pg-port ", "--client-port ", "--listen 0.0.0.0:", "--control-port "} {
		if !strings.Contains(cmd, want) {
			t.Errorf("%q lacks %q", cmd, want)
		}
	}
}

// The `up` banner: two short lines when on, one when off.
func TestBackupBanner(t *testing.T) {
	n, err := pg.NewNode(filepath.Join(t.TempDir(), "node-a"))
	if err != nil {
		t.Fatal(err)
	}
	a := &Agent{node: n, name: "node-a"}
	on := a.backupBanner(cluster.State{})
	if len(on) != 2 || on[0] != "on: every change saved + a full copy every night at 03:00, kept 7 days" ||
		!strings.HasPrefix(on[1], "in "+DefaultBackupFolder(n.StateDir)) {
		t.Fatalf("on: %q", on)
	}
	off := cluster.DefaultBackupSettings()
	off.Enabled = false
	if got := a.backupBanner(cluster.State{Backup: &off}); len(got) != 1 || !strings.HasPrefix(got[0], "off, so nothing is saved") {
		t.Fatalf("off: %q", got)
	}
}
