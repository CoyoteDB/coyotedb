//go:build !windows

package main

import (
	"os"
	"syscall"
)

// chownLike gives path the owner of ref (needs root when they differ).
func chownLike(path, ref string) error {
	fi, err := os.Stat(ref)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || (int(st.Uid) == os.Geteuid()) {
		return nil
	}
	return os.Chown(path, int(st.Uid), int(st.Gid))
}
