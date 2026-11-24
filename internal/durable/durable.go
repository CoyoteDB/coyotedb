// Package durable writes files so that they survive a power cut: either
// the old content or the complete new content is on disk afterwards,
// never a truncated or empty file.
//
// The discipline is the same everywhere: write to a temporary file in the
// same directory, fsync it, rename it over the target, fsync the
// directory (so the rename itself is on disk). On Windows, File.Sync is
// FlushFileBuffers; a directory is flushed where the file system allows
// it and otherwise left to NTFS's metadata journal (see SyncDir).
package durable

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// WriteFile replaces path with data, durably.
func WriteFile(path string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// CreateTemp uses 0600; on Windows Chmod only toggles the read-only
	// bit (the data dir's ACL protects secrets there).
	if err := os.Chmod(tmp, perm); err != nil {
		return err
	}
	if err := replace(tmp, path); err != nil {
		return err
	}
	ok = true
	return SyncDir(dir)
}

// Rename renames oldpath to newpath and makes the rename durable (both
// parent directories are synced).
func Rename(oldpath, newpath string) error {
	if err := os.Rename(oldpath, newpath); err != nil {
		return err
	}
	if err := SyncDir(filepath.Dir(newpath)); err != nil {
		return err
	}
	if d := filepath.Dir(oldpath); d != filepath.Dir(newpath) {
		return SyncDir(d)
	}
	return nil
}

// SyncDirs fsyncs every directory under root, deepest first, so the
// entries of a tree whose files were each synced as they were written
// are on disk before the tree is published with a rename.
func SyncDirs(root string) error {
	var dirs []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := SyncDir(dirs[i]); err != nil {
			return fmt.Errorf("sync %s: %w", dirs[i], err)
		}
	}
	return nil
}

// replace renames tmp over path. Windows refuses to rename over an
// existing file in some cases; then the old file is removed first (the
// window between is the one place a crash leaves no file, never a
// partial one).
func replace(tmp, path string) error {
	err := os.Rename(tmp, path)
	if err != nil && errors.Is(err, fs.ErrExist) {
		_ = os.Remove(path)
		err = os.Rename(tmp, path)
	}
	return err
}
