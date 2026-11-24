package pg

import "path/filepath"

// CheckProgramPath reports a folder that PostgreSQL's program files cannot
// be kept in on this OS: on Windows, a path too long for the files inside
// it or with characters outside the system's code page (the same rules as
// the data folder, D44). Always nil on Unix.
func CheckProgramPath(p string) error {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	return checkPostgresPath(p)
}
