//go:build unix

package durable

import "os"

// SyncDir fsyncs a directory so that entries created, renamed or removed
// in it are on disk.
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
