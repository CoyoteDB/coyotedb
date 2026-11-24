package backup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"coyotedb.com/coyote/internal/durable"
)

// ErrNotFound: no object under that key.
var ErrNotFound = errors.New("not found")

// ErrExists: Put with ifAbsent found an object already there.
var ErrExists = errors.New("already exists")

// ObjectInfo is one listed object.
type ObjectInfo struct {
	Key      string
	Size     int64
	Modified time.Time
}

// Store is where backup objects live. Keys use "/" and never start with it.
type Store interface {
	// Put stores data under key. With ifAbsent it fails with ErrExists
	// instead of replacing an existing object.
	Put(ctx context.Context, key string, data []byte, ifAbsent bool) error
	// Get returns the whole object, or ErrNotFound.
	Get(ctx context.Context, key string) ([]byte, error)
	// GetPrefix returns at most the first n bytes, or ErrNotFound.
	GetPrefix(ctx context.Context, key string, n int) ([]byte, error)
	// List returns every object whose key starts with prefix, sorted.
	List(ctx context.Context, prefix string) ([]ObjectInfo, error)
	// Delete removes an object; a missing one is not an error.
	Delete(ctx context.Context, key string) error
	// Label says where this is, for people (no secrets).
	Label() string
}

// FolderStore keeps objects as files under Root.
type FolderStore struct {
	Root string
}

func (f FolderStore) Label() string { return f.Root }

func (f FolderStore) path(key string) (string, error) {
	if key == "" || strings.HasPrefix(key, "/") || slices.Contains(strings.Split(key, "/"), "..") {
		return "", fmt.Errorf("bad object key %q", key)
	}
	return filepath.Join(f.Root, filepath.FromSlash(key)), nil
}

func (f FolderStore) Put(_ context.Context, key string, data []byte, ifAbsent bool) error {
	p, err := f.path(key)
	if err != nil {
		return err
	}
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if !ifAbsent {
		return durable.WriteFile(p, data, 0o600)
	}
	if _, err := os.Lstat(p); err == nil {
		return ErrExists
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(p)+".tmp-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// A hard link never replaces an existing file, so two writers cannot
	// overwrite each other. File systems without links (FAT, exFAT) fall
	// back to check-then-rename; a folder there has one writer (its node).
	if err := os.Link(name, p); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return ErrExists
		}
		if _, serr := os.Lstat(p); serr == nil {
			return ErrExists
		}
		if err := os.Rename(name, p); err != nil {
			return err
		}
	}
	return durable.SyncDir(dir)
}

func (f FolderStore) Get(_ context.Context, key string) ([]byte, error) {
	p, err := f.path(key)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	return b, err
}

func (f FolderStore) GetPrefix(_ context.Context, key string, n int) ([]byte, error) {
	p, err := f.path(key)
	if err != nil {
		return nil, err
	}
	fh, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	buf := make([]byte, n)
	k, err := fh.Read(buf)
	if err != nil && k == 0 {
		return nil, err
	}
	return buf[:k], nil
}

func (f FolderStore) List(_ context.Context, prefix string) ([]ObjectInfo, error) {
	var out []ObjectInfo
	err := filepath.WalkDir(f.Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() || strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		rel, err := filepath.Rel(f.Root, p)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if !strings.HasPrefix(key, prefix) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		out = append(out, ObjectInfo{Key: key, Size: info.Size(), Modified: info.ModTime()})
		return nil
	})
	slices.SortFunc(out, func(a, b ObjectInfo) int { return strings.Compare(a.Key, b.Key) })
	return out, err
}

func (f FolderStore) Delete(_ context.Context, key string) error {
	p, err := f.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	// Remove an emptied base backup folder (ignore "not empty").
	if d := filepath.Dir(p); d != f.Root && path.Base(path.Dir(key)) != "wal" {
		_ = os.Remove(d)
	}
	return nil
}

// Sub is a Store whose keys live under a prefix of another one.
type Sub struct {
	S      Store
	Prefix string // ends with "/"
}

func (s Sub) Label() string {
	return strings.TrimSuffix(s.S.Label(), "/") + "/" + strings.TrimSuffix(s.Prefix, "/")
}
func (s Sub) Put(ctx context.Context, key string, data []byte, ifAbsent bool) error {
	return s.S.Put(ctx, s.Prefix+key, data, ifAbsent)
}
func (s Sub) Get(ctx context.Context, key string) ([]byte, error) { return s.S.Get(ctx, s.Prefix+key) }
func (s Sub) GetPrefix(ctx context.Context, key string, n int) ([]byte, error) {
	return s.S.GetPrefix(ctx, s.Prefix+key, n)
}
func (s Sub) Delete(ctx context.Context, key string) error { return s.S.Delete(ctx, s.Prefix+key) }
func (s Sub) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	l, err := s.S.List(ctx, s.Prefix+prefix)
	for i := range l {
		l[i].Key = strings.TrimPrefix(l[i].Key, s.Prefix)
	}
	return l, err
}
