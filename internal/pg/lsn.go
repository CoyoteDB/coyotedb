package pg

import (
	"fmt"
	"strconv"
	"strings"
)

// LSN is a PostgreSQL write-ahead log position (pg_lsn), printed as "X/Y"
// where X and Y are the high and low 32 bits in hex.
type LSN uint64

// InvalidLSN is Postgres' InvalidXLogRecPtr (0/0).
const InvalidLSN LSN = 0

// ParseLSN parses "X/Y" (hex, case-insensitive).
func ParseLSN(s string) (LSN, error) {
	hi, lo, ok := strings.Cut(strings.TrimSpace(s), "/")
	if !ok || hi == "" || lo == "" || len(hi) > 8 || len(lo) > 8 {
		return 0, fmt.Errorf("invalid LSN %q", s)
	}
	h, err := strconv.ParseUint(hi, 16, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid LSN %q", s)
	}
	l, err := strconv.ParseUint(lo, 16, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid LSN %q", s)
	}
	return LSN(h<<32 | l), nil
}

// String formats the LSN the way Postgres does ("16/B374D848").
func (l LSN) String() string { return fmt.Sprintf("%X/%X", uint64(l)>>32, uint64(l)&0xFFFFFFFF) }

// Compare returns -1, 0 or +1.
func (l LSN) Compare(o LSN) int {
	switch {
	case l < o:
		return -1
	case l > o:
		return 1
	}
	return 0
}

// Sub returns l - o in bytes (negative if o is ahead).
func (l LSN) Sub(o LSN) int64 { return int64(uint64(l) - uint64(o)) }

// Lag returns how many bytes behind is compared with ahead (0 if not behind).
func Lag(ahead, behind LSN) uint64 {
	if behind >= ahead {
		return 0
	}
	return uint64(ahead - behind)
}

// MarshalText makes LSNs appear as "X/Y" in JSON.
func (l LSN) MarshalText() ([]byte, error) { return []byte(l.String()), nil }

// UnmarshalText parses "X/Y".
func (l *LSN) UnmarshalText(b []byte) error {
	v, err := ParseLSN(string(b))
	if err != nil {
		return err
	}
	*l = v
	return nil
}

// Scan implements sql.Scanner for pg_lsn values selected as text.
func (l *LSN) Scan(src any) error {
	switch v := src.(type) {
	case string:
		return l.UnmarshalText([]byte(v))
	case []byte:
		return l.UnmarshalText(v)
	}
	return fmt.Errorf("cannot scan %T into LSN", src)
}

// WALSegmentSize is the default (and our only) WAL segment size.
const WALSegmentSize = 16 << 20

// WALFileName returns the WAL segment file name holding lsn on timeline tli
// (the same as Postgres' pg_walfile_name for 16 MB segments).
func WALFileName(tli uint32, lsn LSN) string {
	segno := uint64(lsn) / WALSegmentSize
	perID := uint64(0x100000000) / WALSegmentSize
	return fmt.Sprintf("%08X%08X%08X", tli, segno/perID, segno%perID)
}
