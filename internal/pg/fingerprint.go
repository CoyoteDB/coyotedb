package pg

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
)

// The fingerprint is everything that must be identical on two machines for
// one of them to read the other's data files correctly (safety rule 1's
// purpose; docs/MIXED_OS.md condition 1). It replaces "same OS, same CPU"
// as the join check: two machines may differ in operating system and
// processor as long as their fingerprints are equal (and the behaviour
// probe, probe.go, gives the same answers).
//
// The fingerprint has three parts, gathered from three places:
//
//   - ControlFingerprint, from pg_controldata: the on-disk layout the
//     server was compiled for (sizes, alignment, catalog version, ...).
//   - InstallFingerprint, from the Postgres program files: version, build
//     options that change what can be read (lz4, zstd, ICU), and hashes of
//     the shipped time-zone and text-search files, which behave like code.
//   - SQLFingerprint, from the running server: Unicode tables of the
//     built-in locale provider, ICU versions, available extensions.
//
// OS and CPU are recorded for the report but are not compared.

// FingerprintFormat versions the canonical form. Bump it when a field is
// added, removed or changes meaning: fingerprints of different formats
// never compare equal.
//
// Format 2 (D48) no longer compares the ICU versions: a cluster that mixes
// machines is builtin-only (builtinonly.go), so nothing can use ICU and
// its version cannot change what is stored. They are still measured.
const FingerprintFormat = 2

// ControlFingerprint holds the layout fields of pg_controldata.
type ControlFingerprint struct {
	ControlVersion  string `json:"control_version"`  // pg_control version number
	CatalogVersion  string `json:"catalog_version"`  // Catalog version number
	MaxAlign        string `json:"max_align"`        // Maximum data alignment
	BlockSize       string `json:"block_size"`       // Database block size
	SegmentBlocks   string `json:"segment_blocks"`   // Blocks per segment of large relation
	WALBlockSize    string `json:"wal_block_size"`   // WAL block size
	WALSegmentSize  string `json:"wal_segment_size"` // Bytes per WAL segment
	MaxIdentifier   string `json:"max_identifier"`   // Maximum length of identifiers
	MaxIndexColumns string `json:"max_index_columns"`
	ToastChunk      string `json:"toast_chunk"`     // Maximum size of a TOAST chunk
	LOChunk         string `json:"lo_chunk"`        // Size of a large-object chunk
	DateTime        string `json:"datetime"`        // Date/time type storage
	Float8ByVal     string `json:"float8_by_value"` // Float8 argument passing
	ChecksumVersion string `json:"checksum_version"`
	CharSignedness  string `json:"char_signedness"` // Default char data signedness
	// ByteOrder of the control file itself ("little" or "big"), read from
	// global/pg_control when the data folder is at hand ("" otherwise).
	ByteOrder string `json:"byte_order,omitempty"`
}

// controlFields maps pg_controldata labels (LC_ALL=C) to fields. Every one
// is required: a missing line means output we do not understand.
var controlFields = []struct {
	label string
	field func(*ControlFingerprint) *string
}{
	{"pg_control version number", func(c *ControlFingerprint) *string { return &c.ControlVersion }},
	{"Catalog version number", func(c *ControlFingerprint) *string { return &c.CatalogVersion }},
	{"Maximum data alignment", func(c *ControlFingerprint) *string { return &c.MaxAlign }},
	{"Database block size", func(c *ControlFingerprint) *string { return &c.BlockSize }},
	{"Blocks per segment of large relation", func(c *ControlFingerprint) *string { return &c.SegmentBlocks }},
	{"WAL block size", func(c *ControlFingerprint) *string { return &c.WALBlockSize }},
	{"Bytes per WAL segment", func(c *ControlFingerprint) *string { return &c.WALSegmentSize }},
	{"Maximum length of identifiers", func(c *ControlFingerprint) *string { return &c.MaxIdentifier }},
	{"Maximum columns in an index", func(c *ControlFingerprint) *string { return &c.MaxIndexColumns }},
	{"Maximum size of a TOAST chunk", func(c *ControlFingerprint) *string { return &c.ToastChunk }},
	{"Size of a large-object chunk", func(c *ControlFingerprint) *string { return &c.LOChunk }},
	{"Date/time type storage", func(c *ControlFingerprint) *string { return &c.DateTime }},
	{"Float8 argument passing", func(c *ControlFingerprint) *string { return &c.Float8ByVal }},
	{"Data page checksum version", func(c *ControlFingerprint) *string { return &c.ChecksumVersion }},
	{"Default char data signedness", func(c *ControlFingerprint) *string { return &c.CharSignedness }},
}

// FromControlData reads the layout fields from pg_controldata output (run
// with LC_ALL=C; Windows line endings are fine). A byte-order warning from
// pg_controldata is an error: the files were written by a machine with the
// other byte order.
func FromControlData(out string) (ControlFingerprint, error) {
	var c ControlFingerprint
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n") {
		if strings.Contains(line, "byte ordering mismatch") {
			return c, errors.New("pg_controldata: the data files were written with a different byte order")
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.Join(strings.Fields(v), " ")
		for _, f := range controlFields {
			if f.label == k {
				*f.field(&c) = v
				seen[k] = true
			}
		}
	}
	var missing []string
	for _, f := range controlFields {
		if !seen[f.label] || *f.field(&c) == "" {
			missing = append(missing, f.label)
		}
	}
	if len(missing) > 0 {
		return c, fmt.Errorf("pg_controldata output not understood: missing %s", strings.Join(missing, ", "))
	}
	return c, nil
}

// ControlFileByteOrder reports the byte order of pgdata/global/pg_control:
// the control version (a uint32 after the 8-byte system identifier) is a
// small number in the writer's byte order.
func ControlFileByteOrder(pgdata string) (string, error) {
	f, err := os.Open(filepath.Join(pgdata, "global", "pg_control"))
	if err != nil {
		return "", err
	}
	defer f.Close()
	var b [12]byte
	if _, err := io.ReadFull(f, b[:]); err != nil {
		return "", fmt.Errorf("read pg_control: %w", err)
	}
	le, be := binary.LittleEndian.Uint32(b[8:]), binary.BigEndian.Uint32(b[8:])
	switch {
	case le > 0 && le < 1<<16:
		return "little", nil
	case be > 0 && be < 1<<16:
		return "big", nil
	}
	return "", fmt.Errorf("pg_control: unrecognised control version bytes % x", b[8:])
}

// ControlFingerprintOf runs pg_controldata on a data folder (read-only; the
// server may be running) and adds the control file's byte order.
func ControlFingerprintOf(ctx context.Context, inst Install, pgdata string) (ControlFingerprint, error) {
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, inst.Bin("pg_controldata"), "-D", pgdata)
	cmd.Env = toolEnv()
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return ControlFingerprint{}, fmt.Errorf("pg_controldata: %w: %s", err, tail(out.String(), 5))
	}
	c, err := FromControlData(out.String())
	if err != nil {
		return c, err
	}
	if c.ByteOrder, err = ControlFileByteOrder(pgdata); err != nil {
		return c, err
	}
	return c, nil
}

// InstallFingerprint describes the Postgres program files.
type InstallFingerprint struct {
	// Version is what `postgres --version` prints after "(PostgreSQL) ",
	// e.g. "18.7".
	Version string `json:"version"`
	// Options are the build options that change what a server can read or
	// how it compares text: "icu", "lz4", "zstd" when built with them.
	Options []string `json:"options"`
	// TimezoneSHA256 hashes share/timezone (the zone rules) and
	// TimezoneSetsSHA256 share/timezonesets (abbreviations); "none" when
	// the build uses the operating system's zone files instead
	// (--with-system-tzdata), which is a difference in itself.
	TimezoneSHA256     string `json:"timezone_sha256"`
	TimezoneSetsSHA256 string `json:"timezonesets_sha256"`
	// TSearchSHA256 hashes share/tsearch_data (dictionaries, stop words,
	// affix and rules files): they decide what full-text search stores.
	TSearchSHA256 string `json:"tsearch_sha256"`
	// SnowballSHA256 hashes share/snowball_create.sql (the stemmer configs
	// a new database gets).
	SnowballSHA256 string `json:"snowball_sha256"`
}

// fingerprintOptions are the configure switches we compare. A server built
// without lz4 cannot read lz4-compressed values; without zstd it cannot
// read zstd-compressed WAL; ICU decides ICU collations (which the audit
// forbids, but a mismatch still means a different build).
var fingerprintOptions = []string{"icu", "lz4", "zstd"}

// ParseConfigure picks the compared options out of `pg_config --configure`.
func ParseConfigure(out string) []string {
	var opts []string
	for _, f := range strings.Fields(out) {
		f = strings.Trim(f, `'"`)
		for _, o := range fingerprintOptions {
			if f == "--with-"+o {
				opts = append(opts, o)
			}
		}
	}
	slices.Sort(opts)
	return slices.Compact(opts)
}

// ParsePostgresVersion reads `postgres --version` output.
func ParsePostgresVersion(out string) (string, error) {
	const marker = "(PostgreSQL) "
	i := strings.Index(out, marker)
	if i < 0 {
		return "", fmt.Errorf("postgres --version: unexpected output %q", strings.TrimSpace(out))
	}
	f := strings.Fields(out[i+len(marker):])
	if len(f) == 0 {
		return "", fmt.Errorf("postgres --version: no version in %q", strings.TrimSpace(out))
	}
	return f[0], nil
}

// FromInstall gathers the install fingerprint. It runs `postgres
// --version` and `pg_config --configure` (no shell) and hashes share/.
func FromInstall(ctx context.Context, inst Install) (InstallFingerprint, error) {
	var f InstallFingerprint
	out, err := runQuiet(ctx, inst.Bin("postgres"), "--version")
	if err != nil {
		return f, err
	}
	if f.Version, err = ParsePostgresVersion(out); err != nil {
		return f, err
	}
	if out, err = runQuiet(ctx, inst.Bin("pg_config"), "--configure"); err != nil {
		return f, err
	}
	f.Options = ParseConfigure(out)
	share := filepath.Join(inst.Root, "share")
	for _, h := range []struct {
		dst      *string
		rel      string
		optional bool
	}{
		{&f.TimezoneSHA256, "timezone", true},
		{&f.TimezoneSetsSHA256, "timezonesets", false},
		{&f.TSearchSHA256, "tsearch_data", false},
		{&f.SnowballSHA256, "snowball_create.sql", false},
	} {
		sum, err := HashTree(filepath.Join(share, h.rel))
		if errors.Is(err, fs.ErrNotExist) && h.optional {
			sum = "none"
		} else if err != nil {
			return f, fmt.Errorf("hash share/%s: %w", h.rel, err)
		}
		*h.dst = sum
	}
	return f, nil
}

func runQuiet(ctx context.Context, bin string, args ...string) (string, error) {
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = toolEnv()
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", filepath.Base(bin), strings.Join(args, " "), err, tail(out.String(), 5))
	}
	return out.String(), nil
}

// HashTree hashes a file or a folder tree: sha256 over every regular file,
// sorted by its path relative to root (with '/' separators on every OS),
// each entry as "<path>\x00<size>\x00<sha256 of content>\n". Contents are
// not normalised (line endings included): the files must be byte-identical.
// A symbolic link is followed and hashed as the file it points to.
func HashTree(root string) (string, error) {
	st, err := os.Stat(root)
	if err != nil {
		return "", err
	}
	type entry struct{ rel, abs string }
	var files []entry
	if st.Mode().IsRegular() {
		files = append(files, entry{filepath.Base(root), root})
	} else {
		err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			files = append(files, entry{filepath.ToSlash(rel), p})
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	slices.SortFunc(files, func(a, b entry) int { return strings.Compare(a.rel, b.rel) })
	h := sha256.New()
	for _, e := range files {
		sum, size, err := hashFile(e.abs)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\x00%d\x00%s\n", e.rel, size, sum)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func hashFile(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// SQLFingerprint is read from the running server.
type SQLFingerprint struct {
	VersionNum string `json:"version_num"` // server_version_num
	Encoding   string `json:"encoding"`    // server_encoding
	// UnicodeVersion is the Unicode version of the built-in locale
	// provider's tables (unicode_version()): it decides upper/lower,
	// character classes and normalisation for C.UTF-8.
	UnicodeVersion string `json:"unicode_version"`
	// ICUUnicodeVersion (icu_unicode_version()) and ICUCollatorVersion
	// (the version of the "unicode" ICU collation, which tracks the ICU
	// library) are "" when the build has no ICU. Recorded, not compared
	// (format 2, D48): mixed clusters are builtin-only.
	ICUUnicodeVersion  string `json:"icu_unicode_version"`
	ICUCollatorVersion string `json:"icu_collator_version"`
	// BuiltinCollations lists name=version for the built-in provider's
	// collations (pg_c_utf8, pg_unicode_fast, ucs_basic).
	BuiltinCollations []string `json:"builtin_collations"`
	// Extensions lists name=default_version of every extension the
	// install ships (pg_available_extensions). Two machines must offer the
	// same set, or an extension in the shared catalog has no code on one.
	Extensions []string `json:"extensions"`
}

// FromSQL gathers the SQL part of the fingerprint. Read-only; works on a
// standby.
func FromSQL(ctx context.Context, q Querier) (SQLFingerprint, error) {
	var f SQLFingerprint
	err := q.QueryRow(ctx, `SELECT current_setting('server_version_num'), current_setting('server_encoding'),
		unicode_version(), coalesce(icu_unicode_version(), ''),
		coalesce((SELECT pg_collation_actual_version(oid) FROM pg_collation WHERE collname = 'unicode' AND collprovider = 'i' AND collnamespace = 'pg_catalog'::regnamespace), '')`).
		Scan(&f.VersionNum, &f.Encoding, &f.UnicodeVersion, &f.ICUUnicodeVersion, &f.ICUCollatorVersion)
	if err != nil {
		return f, fmt.Errorf("fingerprint: %w", err)
	}
	if f.BuiltinCollations, err = stringList(ctx, q, `SELECT collname || '=' || coalesce(pg_collation_actual_version(oid), '')
		FROM pg_collation WHERE collprovider = 'b' AND collnamespace = 'pg_catalog'::regnamespace ORDER BY 1`); err != nil {
		return f, fmt.Errorf("fingerprint: %w", err)
	}
	if f.Extensions, err = stringList(ctx, q, `SELECT name || '=' || coalesce(default_version, '') FROM pg_available_extensions ORDER BY 1`); err != nil {
		return f, fmt.Errorf("fingerprint: %w", err)
	}
	return f, nil
}

func stringList(ctx context.Context, q Querier, sql string, args ...any) ([]string, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Fingerprint is the full fingerprint of one data node.
type Fingerprint struct {
	Format int `json:"format"`
	// OS and Arch are recorded, not compared.
	OS      string             `json:"os"`
	Arch    string             `json:"arch"`
	Control ControlFingerprint `json:"control"`
	Install InstallFingerprint `json:"install"`
	SQL     SQLFingerprint     `json:"sql"`
}

// CollectFingerprint gathers all three parts: pg_controldata on pgdata, the
// install's files, and q (a connection to the server running on pgdata).
func CollectFingerprint(ctx context.Context, inst Install, pgdata string, q Querier) (Fingerprint, error) {
	f := Fingerprint{Format: FingerprintFormat, OS: runtime.GOOS, Arch: runtime.GOARCH}
	var err error
	if f.Control, err = ControlFingerprintOf(ctx, inst, pgdata); err != nil {
		return f, err
	}
	if f.Install, err = FromInstall(ctx, inst); err != nil {
		return f, err
	}
	if f.SQL, err = FromSQL(ctx, q); err != nil {
		return f, err
	}
	return f, nil
}

// fpField is one compared field: a stable key for the canonical form, the
// words a person reads, and why a difference matters.
type fpField struct {
	key, what, why string
	get            func(Fingerprint) string
	// lst, if set, is the list behind get: differences then name the
	// items found on only one side.
	lst func(Fingerprint) []string
}

func list(s []string) string { return strings.Join(s, ",") }

var fpFields = []fpField{
	{"format", "fingerprint format", "the two machines run different versions of this program; update both", func(f Fingerprint) string { return strconv.Itoa(f.Format) }, nil},
	{"control_version", "data-file control version", "the database programs are different releases", func(f Fingerprint) string { return f.Control.ControlVersion }, nil},
	{"catalog_version", "system catalog version", "the database programs are different releases", func(f Fingerprint) string { return f.Control.CatalogVersion }, nil},
	{"max_align", "memory alignment of stored rows", "rows are laid out differently on disk", func(f Fingerprint) string { return f.Control.MaxAlign }, nil},
	{"block_size", "data page size", "pages on disk have different sizes", func(f Fingerprint) string { return f.Control.BlockSize }, nil},
	{"segment_blocks", "table file segment size", "table files are split differently", func(f Fingerprint) string { return f.Control.SegmentBlocks }, nil},
	{"wal_block_size", "change-log page size", "the change log is written differently", func(f Fingerprint) string { return f.Control.WALBlockSize }, nil},
	{"wal_segment_size", "change-log file size", "the change log is split differently", func(f Fingerprint) string { return f.Control.WALSegmentSize }, nil},
	{"max_identifier", "longest allowed name", "names are stored with a different fixed length", func(f Fingerprint) string { return f.Control.MaxIdentifier }, nil},
	{"max_index_columns", "most columns in an index", "indexes are stored differently", func(f Fingerprint) string { return f.Control.MaxIndexColumns }, nil},
	{"toast_chunk", "large-value chunk size", "large values are split differently", func(f Fingerprint) string { return f.Control.ToastChunk }, nil},
	{"lo_chunk", "large-object chunk size", "large objects are split differently", func(f Fingerprint) string { return f.Control.LOChunk }, nil},
	{"datetime", "date and time storage", "dates and times are stored differently", func(f Fingerprint) string { return f.Control.DateTime }, nil},
	{"float8_by_value", "how decimals are passed in memory", "the programs were compiled differently", func(f Fingerprint) string { return f.Control.Float8ByVal }, nil},
	{"checksum_version", "page checksums", "one copy checks pages for damage and the other does not", func(f Fingerprint) string { return f.Control.ChecksumVersion }, nil},
	{"char_signedness", "default sign of single bytes", "some text indexes would be read in a different order", func(f Fingerprint) string { return f.Control.CharSignedness }, nil},
	{"byte_order", "byte order", "numbers are stored back to front on one machine", func(f Fingerprint) string { return f.Control.ByteOrder }, nil},
	{"pg_version", "Postgres version", "both machines must run exactly the same Postgres release", func(f Fingerprint) string { return f.Install.Version }, nil},
	{"build_options", "Postgres build options (compression, ICU)", "one machine could not read values the other one compressed", func(f Fingerprint) string { return list(f.Install.Options) }, func(f Fingerprint) []string { return f.Install.Options }},
	{"timezone", "time-zone rules", "the same moment would show as different local times", func(f Fingerprint) string { return f.Install.TimezoneSHA256 }, nil},
	{"timezonesets", "time-zone abbreviations", "the same text would be read as different times", func(f Fingerprint) string { return f.Install.TimezoneSetsSHA256 }, nil},
	{"tsearch", "text-search dictionaries and stop words", "full-text search would store and find different words", func(f Fingerprint) string { return f.Install.TSearchSHA256 }, nil},
	{"snowball", "text-search language setup", "full-text search would store and find different words", func(f Fingerprint) string { return f.Install.SnowballSHA256 }, nil},
	{"version_num", "running Postgres version", "both machines must run exactly the same Postgres release", func(f Fingerprint) string { return f.SQL.VersionNum }, nil},
	{"encoding", "text encoding", "text would be stored differently", func(f Fingerprint) string { return f.SQL.Encoding }, nil},
	{"unicode_version", "Unicode tables for upper/lower case", "upper/lower case and character classes could differ, which breaks indexes", func(f Fingerprint) string { return f.SQL.UnicodeVersion }, nil},
	{"builtin_collations", "built-in sort rules", "text would sort differently, which breaks indexes", func(f Fingerprint) string { return list(f.SQL.BuiltinCollations) }, func(f Fingerprint) []string { return f.SQL.BuiltinCollations }},
	{"extensions", "available extensions", "an extension one machine uses could be missing on the other", func(f Fingerprint) string { return list(f.SQL.Extensions) }, func(f Fingerprint) []string { return f.SQL.Extensions }},
}

// Canonical is the compared content as "key=value" lines, in a fixed
// order. OS and Arch are not in it.
func (f Fingerprint) Canonical() string {
	var b strings.Builder
	for _, fl := range fpFields {
		fmt.Fprintf(&b, "%s=%s\n", fl.key, fl.get(f))
	}
	return b.String()
}

// Hash is the sha256 of Canonical, in hex.
func (f Fingerprint) Hash() string {
	s := sha256.Sum256([]byte(f.Canonical()))
	return hex.EncodeToString(s[:])
}

// Platform is "os/arch", for messages.
func (f Fingerprint) Platform() string { return f.OS + "/" + f.Arch }

// FingerprintDiff is one differing field.
type FingerprintDiff struct {
	Key  string `json:"key"`
	A    string `json:"a"`
	B    string `json:"b"`
	Text string `json:"text"` // plain words
}

// CompareFingerprints lists every compared field that differs between a
// (this machine) and b (the other), in plain words. Empty: compatible.
func CompareFingerprints(a, b Fingerprint) []FingerprintDiff {
	var out []FingerprintDiff
	for _, fl := range fpFields {
		va, vb := fl.get(a), fl.get(b)
		if va == vb {
			continue
		}
		var text string
		if fl.lst != nil {
			onlyA, onlyB := listDiff(fl.lst(a), fl.lst(b))
			text = fmt.Sprintf("The %s are different (only on this machine: %s; only on the other: %s): %s.", fl.what, noneIfEmpty(onlyA), noneIfEmpty(onlyB), fl.why)
		} else {
			text = fmt.Sprintf("The %s is different (%s on this machine, %s on the other): %s.", fl.what, shortVal(va), shortVal(vb), fl.why)
		}
		out = append(out, FingerprintDiff{Key: fl.key, A: va, B: vb, Text: text})
	}
	return out
}

// listDiff returns the items only in a and only in b (sorted inputs not
// required).
func listDiff(a, b []string) (onlyA, onlyB []string) {
	in := func(s []string, v string) bool { return slices.Contains(s, v) }
	for _, v := range a {
		if !in(b, v) {
			onlyA = append(onlyA, v)
		}
	}
	for _, v := range b {
		if !in(a, v) {
			onlyB = append(onlyB, v)
		}
	}
	return onlyA, onlyB
}

func noneIfEmpty(s []string) string {
	if len(s) == 0 {
		return "none"
	}
	if len(s) > 8 {
		return strings.Join(s[:8], ", ") + fmt.Sprintf(" and %d more", len(s)-8)
	}
	return strings.Join(s, ", ")
}

// shortVal keeps long values (hashes, lists) readable in a sentence.
func shortVal(v string) string {
	switch {
	case v == "":
		return "missing"
	case len(v) == 64 && isHex(v):
		return v[:12]
	case len(v) > 60:
		return v[:57] + "..."
	}
	return v
}

func isHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil
}

// ExplainCanonical compares two canonical fingerprints (Canonical output,
// as kept in the cluster state) and says in plain words what differs,
// here (this machine) first. Unknown keys (a newer format) are named as
// they are. Empty: no compared field differs.
func ExplainCanonical(here, there string) []string {
	parse := func(s string) map[string]string {
		m := map[string]string{}
		for _, l := range strings.Split(s, "\n") {
			if k, v, ok := strings.Cut(l, "="); ok {
				m[k] = v
			}
		}
		return m
	}
	h, t := parse(here), parse(there)
	var out []string
	known := map[string]bool{}
	for _, fl := range fpFields {
		known[fl.key] = true
		va, vb := h[fl.key], t[fl.key]
		if va == vb {
			continue
		}
		if fl.lst != nil {
			split := func(v string) []string {
				if v == "" {
					return nil
				}
				return strings.Split(v, ",")
			}
			onlyA, onlyB := listDiff(split(va), split(vb))
			out = append(out, fmt.Sprintf("The %s are different (only on this machine: %s; only on the other: %s): %s.", fl.what, noneIfEmpty(onlyA), noneIfEmpty(onlyB), fl.why))
			continue
		}
		out = append(out, fmt.Sprintf("The %s is different (%s on this machine, %s on the other): %s.", fl.what, shortVal(va), shortVal(vb), fl.why))
	}
	var extra []string
	for k := range h {
		if !known[k] && h[k] != t[k] {
			extra = append(extra, k)
		}
	}
	for k := range t {
		if !known[k] && h[k] != t[k] && !slices.Contains(extra, k) {
			extra = append(extra, k)
		}
	}
	slices.Sort(extra)
	for _, k := range extra {
		out = append(out, fmt.Sprintf("The field %q is different (%s on this machine, %s on the other).", k, shortVal(h[k]), shortVal(t[k])))
	}
	return out
}

// ScratchOptions controls ScratchCheck.
type ScratchOptions struct {
	Install Install
	// Guard is the agent's process guard (safety rule 5): the scratch
	// server runs under it like any other Postgres of this node.
	Guard *Guard
	// Dir is a folder the check owns: emptied first, removed after.
	Dir    string
	Logger *slog.Logger
	// Probe options (timeouts).
	Probe ProbeOptions
}

// ScratchCheck measures this machine before it holds any data
// (docs/MIXED_OS.md phase 2 item 4): it makes a throwaway database with
// the product's exact initdb settings (Node.InitDB), starts it under the
// guard on a free 127.0.0.1 port, collects the fingerprint and runs the
// behaviour probe, then stops it and removes the folder. A joining data
// node is checked this way before anything is copied to it, so a machine
// that cannot share the cluster's files is refused before cloning.
func ScratchCheck(ctx context.Context, o ScratchOptions) (Fingerprint, ProbeResult, error) {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Dir == "" {
		return Fingerprint{}, ProbeResult{}, errors.New("scratch check needs a folder")
	}
	if err := os.RemoveAll(o.Dir); err != nil {
		return Fingerprint{}, ProbeResult{}, fmt.Errorf("clear %s: %w", o.Dir, err)
	}
	defer func() {
		if err := os.RemoveAll(o.Dir); err != nil {
			o.Logger.Warn("remove scratch database", "dir", o.Dir, "err", err)
		}
	}()
	n, err := NewNode(o.Dir)
	if err != nil {
		return Fingerprint{}, ProbeResult{}, err
	}
	if err := n.Prepare(); err != nil {
		return Fingerprint{}, ProbeResult{}, err
	}
	if _, err := n.InitDB(ctx, o.Install, o.Logger); err != nil {
		return Fingerprint{}, ProbeResult{}, fmt.Errorf("scratch initdb: %w", err)
	}
	var srv *Server
	for attempt := 0; ; attempt++ {
		port, err := freeLocalPort()
		if err != nil {
			return Fingerprint{}, ProbeResult{}, err
		}
		if err := n.WriteInfo(NodeInfo{Name: "scratch", Port: port, PGVersion: Version}); err != nil {
			return Fingerprint{}, ProbeResult{}, err
		}
		srv, err = n.Start(ctx, StartOptions{Install: o.Install, Guard: o.Guard, Port: port, Name: "scratch", Logger: o.Logger})
		if err == nil {
			break
		}
		if attempt >= 2 || ctx.Err() != nil {
			return Fingerprint{}, ProbeResult{}, fmt.Errorf("start scratch database: %w", err)
		}
	}
	defer func() {
		if err := srv.Stop(guardStopTimeouts); err != nil {
			o.Logger.Warn("stop scratch database", "err", err)
		}
	}()
	c, err := n.Connect(ctx)
	if err != nil {
		return Fingerprint{}, ProbeResult{}, err
	}
	defer c.Close(context.WithoutCancel(ctx))
	fp, err := CollectFingerprint(ctx, o.Install, n.PGData(), c)
	if err != nil {
		return fp, ProbeResult{}, err
	}
	pr, err := RunProbe(ctx, c, o.Probe)
	if err != nil {
		return fp, pr, err
	}
	return fp, pr, nil
}

// freeLocalPort asks the kernel for a free TCP port on 127.0.0.1.
func freeLocalPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
