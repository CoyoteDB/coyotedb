//go:build !windows

package pg

// protectDataDir: on Unix the data folder is created 0700 (Node.Prepare).
func protectDataDir(string) error { return nil }
