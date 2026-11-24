package backup

import (
	"os"
	"path/filepath"
)

// NearestExisting is p itself when it exists, else its closest parent
// that does (the folder a new backup or restore folder is made in). ""
// when nothing on the path exists (cannot happen for an absolute path).
func NearestExisting(p string) string {
	p = filepath.Clean(p)
	for {
		if _, err := os.Stat(p); err == nil {
			return p
		}
		up := filepath.Dir(p)
		if up == p {
			return ""
		}
		p = up
	}
}

// SameDisk reports whether a and b (or, for a path that does not exist
// yet, its nearest existing parent) are on the same disk (file system).
func SameDisk(a, b string) (bool, error) {
	ea, eb := NearestExisting(a), NearestExisting(b)
	if ea == "" || eb == "" {
		return false, os.ErrNotExist
	}
	return sameDevice(ea, eb)
}
