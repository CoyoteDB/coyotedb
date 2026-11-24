//go:build !windows

package pg

// encodeConfFile returns the bytes of a Postgres config file. Postgres
// reads paths in its config files as raw bytes; on Unix those are the
// UTF-8 bytes Go uses.
func encodeConfFile(s string) ([]byte, error) { return []byte(s), nil }

// checkPostgresPath reports a data folder Postgres cannot use on this OS.
// Unix Postgres takes any path bytes.
func checkPostgresPath(string) error { return nil }
