package pg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"coyotedb.com/coyote/internal/brand"
	"coyotedb.com/coyote/internal/durable"
)

// Extensions (D53): PostgreSQL add-ons that are not part of the pinned
// PostgreSQL build, built by us from a pinned upstream release against that
// exact build (scripts/extensions/), one archive per platform, each pinned
// here by sha256. An archive holds only new files for the Postgres tree:
//
//	<project>-<version>-pg<pg version>-<target>/
//	  lib/<name>.so|.dylib|.dll
//	  share/extension/<name>.control, <name>--*.sql
//	  dependency-notices/<project>/LICENSE
//	  build-info.json                (how it was built; not installed)
//
// The archives are not published anywhere yet (where to host them is still
// open: a release blocker). Until then they are taken from
// ExtensionsFromEnv, a folder or an http(s) base URL; the pinned sha256 is
// what makes either safe.

// ExtensionsFromEnv names the environment variable that says where the
// extension archives are: a folder, or an http(s) URL the archive's file
// name is appended to.
const ExtensionsFromEnv = brand.EnvPrefix + "EXTENSIONS_FROM"

// extensionFetchTimeout bounds fetching one extension archive (a few
// hundred kB), so an unreachable location cannot hold up a start for long.
const extensionFetchTimeout = 2 * time.Minute

// Extension is one pinned extension build for one PostgreSQL build.
type Extension struct {
	Project string // upstream project, e.g. "pgvector"
	Name    string // name in CREATE EXTENSION, e.g. "vector"
	Version string // upstream release, e.g. "0.8.7"
	Build   Build  // the PostgreSQL build it was compiled against
	SHA256  string // of the archive
}

// ArchiveName is the archive's base name without extension, and the single
// top-level folder inside it.
func (e Extension) ArchiveName() string {
	return e.Project + "-" + e.Version + "-pg" + e.Build.Version + "-" + e.Build.Target
}

// FileName is the archive's file name.
func (e Extension) FileName() string { return e.ArchiveName() + ".tar.gz" }

// Library is the extension's shared library, relative to the install root
// (slash-separated).
func (e Extension) Library() string { return "lib/" + e.Name + libSuffix(e.Build.Target) }

// Control is the extension's control file, relative to the install root.
func (e Extension) Control() string { return "share/extension/" + e.Name + ".control" }

func (e Extension) marker() string { return ".coyote-ext-" + e.Project }

// libSuffix is the file ending of a loadable module in the PostgreSQL
// build for target (PostgreSQL 16+ uses .dylib on macOS).
func libSuffix(target string) string {
	switch {
	case strings.Contains(target, "windows"):
		return ".dll"
	case strings.Contains(target, "apple-darwin"):
		return ".dylib"
	}
	return ".so"
}

type pinnedExtension struct {
	project, name, version string
	sha256                 map[string]string // by target, for Version
}

// pinnedExtensions: every archive's sha256, pinned in source like the
// PostgreSQL archives (a checksum fetched at run time proves nothing).
// Built with scripts/extensions/ (pgvector v0.8.7, commit
// f37c13f68b57d2c3472b2214fbcff699d6d34876) against PostgreSQL 18.7.0;
// how each was built is in the archive's build-info.json.
var pinnedExtensions = []pinnedExtension{{
	project: "pgvector", name: "vector", version: "0.8.7",
	sha256: map[string]string{
		"aarch64-apple-darwin":      "e0cff0dbf0b3529ee8fabd671eab6ec910ae7fb775b7d281b066aa596f05e7d2",
		"x86_64-apple-darwin":       "b2bf83a14d810f74aa0fecef0b18fa9c6b87c260038343d489230554342095e9",
		"x86_64-unknown-linux-gnu":  "5f6bae65d773f9c3f3cb1d67b864810c9c81956b89c0fa5c48993c22c1fa8077",
		"aarch64-unknown-linux-gnu": "d7a02780331c58fefe9aca869d5d0259a9a9414ab72f444a5e37f1e1b23cafe0",
		"x86_64-pc-windows-msvc":    "c027171f25a6ba81cd66ad1d37ae575313d22a2caf6078ed6fb6c5333cf216d3",
	},
}}

// ExtensionsFor lists the pinned extensions built for b.
func ExtensionsFor(b Build) []Extension {
	if b.Version != Version {
		return nil
	}
	var out []Extension
	for _, p := range pinnedExtensions {
		if sum, ok := p.sha256[b.Target]; ok {
			out = append(out, Extension{Project: p.project, Name: p.name, Version: p.version, Build: b, SHA256: sum})
		}
	}
	return out
}

// PinnedExtensionNames lists the CREATE EXTENSION names of every pinned
// extension (for messages: "comes with CoyoteDB but is not installed").
func PinnedExtensionNames() []string {
	var out []string
	for _, p := range pinnedExtensions {
		out = append(out, p.name)
	}
	return out
}

// ErrExtensionsNotConfigured means an extension is not installed and no
// location to fetch it from is set (ExtensionsFromEnv).
var ErrExtensionsNotConfigured = errors.New("no location set for extension archives")

// CheckExtension reports what is wrong with an installed extension (nil:
// installed from e's archive, every file there with its size; full also
// checks every file's sha256).
func CheckExtension(inst Install, e Extension, full bool) error {
	m, err := readExtensionMarker(inst, e)
	if err != nil {
		return err
	}
	if m.ArchiveSHA256 != e.SHA256 {
		return fmt.Errorf("%s %s is not the pinned build", e.Project, e.Version)
	}
	return checkExtensionFiles(inst, e, m, full)
}

func readExtensionMarker(inst Install, e Extension) (manifest, error) {
	var m manifest
	b, err := os.ReadFile(filepath.Join(inst.Root, e.marker()))
	if err != nil {
		return m, fmt.Errorf("%s is not installed", e.Project)
	}
	if err := json.Unmarshal(b, &m); err != nil || m.ArchiveSHA256 == "" || len(m.Files) == 0 {
		return m, fmt.Errorf("the list of %s's files is unreadable", e.Project)
	}
	return m, nil
}

// checkExtensionFiles checks the files m lists (whatever version it is).
func checkExtensionFiles(inst Install, e Extension, m manifest, full bool) error {
	if !slices.ContainsFunc(m.Files, func(f manifestEntry) bool { return f.Path == e.Library() }) ||
		!slices.ContainsFunc(m.Files, func(f manifestEntry) bool { return f.Path == e.Control() }) {
		return fmt.Errorf("the list of %s's files has no %s or %s", e.Project, e.Library(), e.Control())
	}
	if err := checkFiles(inst.Root, m, full); err != nil {
		return fmt.Errorf("%s: %w", e.Project, err)
	}
	return nil
}

// EnsureExtensions installs every pinned extension for inst that is not
// installed yet, from o.ExtensionsFrom (or ExtensionsFromEnv). Ensure calls
// it and never fails because of it (a missing extension must not keep the
// database down); callers that need the extensions (the Docker image
// build) call it themselves. An extension already installed and intact is
// kept even when no location is set, even if it is an older version.
// One that is damaged and cannot be fetched again is removed (control
// file first), so that using it fails with a clear error instead of
// loading a broken library.
func EnsureExtensions(ctx context.Context, o FetchOptions, inst Install) error {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	exts := o.extensions
	if exts == nil {
		exts = ExtensionsFor(inst.Build)
	}
	var errs []error
	for _, e := range exts {
		if err := ensureExtension(ctx, o, inst, e); err != nil {
			errs = append(errs, fmt.Errorf("%s %s: %w", e.Project, e.Version, err))
		}
	}
	return errors.Join(errs...)
}

func ensureExtension(ctx context.Context, o FetchOptions, inst Install, e Extension) error {
	if CheckExtension(inst, e, o.Full) == nil {
		return nil
	}
	cache := filepath.Dir(inst.Root)
	lock, err := AcquireLock(ctx, filepath.Join(cache, ".lock"), func() {
		o.Logger.Info("waiting for another process to finish installing an extension", "cache", cache)
	})
	if err != nil {
		return err
	}
	defer lock.Unlock()
	bad := CheckExtension(inst, e, o.Full)
	if bad == nil {
		return nil
	}
	cleanStaleExtracts(cache, o.Logger)
	old, oldErr := readExtensionMarker(inst, e)
	from := o.ExtensionsFrom
	if from == "" {
		from = os.Getenv(ExtensionsFromEnv)
	}
	if from == "" {
		switch {
		case oldErr != nil:
			return ErrExtensionsNotConfigured
		case checkExtensionFiles(inst, e, old, o.Full) == nil:
			o.Logger.Info("keeping the installed extension; the pinned build is not available here", "extension", e.Project, "why", bad)
			return nil
		}
		o.Logger.Warn("an installed extension is damaged and cannot be fetched again; removing it", "extension", e.Project, "why", bad)
		if o.Status != nil {
			o.Status("The " + e.Project + " extension's files are damaged (" + bad.Error() + ") and no copy is available here; it was removed until it can be installed again.")
		}
		if err := removeExtension(inst, e, old); err != nil {
			return fmt.Errorf("remove damaged files: %w", err)
		}
		return fmt.Errorf("%w (the damaged install was removed)", ErrExtensionsNotConfigured)
	}
	if oldErr == nil {
		o.Logger.Warn("installing an extension again", "extension", e.Project, "why", bad)
	}

	ctx, cancel := context.WithTimeout(ctx, extensionFetchTimeout)
	defer cancel()
	archive, cleanup, err := fetchExtension(ctx, o, cache, e, from)
	if err != nil {
		return err
	}
	defer cleanup()
	if err := installExtension(archive, cache, inst, e, o.fault); err != nil {
		return err
	}
	o.Logger.Info("extension installed", "extension", e.Project, "version", e.Version, "sha256", e.SHA256)
	return nil
}

// fetchExtension returns a local copy of e's archive whose sha256 matches
// the pin. from is a folder or an http(s) base URL.
func fetchExtension(ctx context.Context, o FetchOptions, cache string, e Extension, from string) (string, func(), error) {
	nothing := func() {}
	if u, err := url.Parse(from); err == nil && (u.Scheme == "https" || u.Scheme == "http") {
		u.Path = path.Join(u.Path, e.FileName())
		dst := filepath.Join(cache, e.FileName()+".partial")
		o.Logger.Info("downloading extension", "url", u.Redacted(), "to", dst)
		if err := download(ctx, o.client(), u.String(), dst, nil, o.Logger, e.Project); err != nil {
			return "", nothing, err
		}
		if err := VerifyFile(dst, e.SHA256); err != nil {
			_ = os.Remove(dst)
			return "", nothing, err
		}
		return dst, func() { _ = os.Remove(dst) }, nil
	}
	if strings.Contains(from, "://") {
		return "", nothing, fmt.Errorf("%s: only a folder or an http(s) address is supported, not %q", ExtensionsFromEnv, from)
	}
	p := filepath.Join(from, e.FileName())
	if err := VerifyFile(p, e.SHA256); err != nil {
		return "", nothing, err
	}
	return p, nothing, nil
}

// installExtension unpacks a verified archive and moves its files into
// the Postgres tree so that a power cut at any point is safe (D34):
// everything is unpacked and synced next to the tree first; the old
// marker is removed; each file is moved into place with a synced rename
// (library first, control file last, so Postgres never lists an extension
// whose library or scripts are not there yet); the marker listing every
// file with its size and sha256 is written last. No marker means "not
// installed": the next start installs it again over whatever got moved.
// Files of the PostgreSQL build itself are never replaced.
func installExtension(archive, cache string, inst Install, e Extension, fault func(string)) error {
	if fault == nil {
		fault = func(string) {}
	}
	pgFiles, err := installFiles(inst)
	if err != nil {
		return err
	}
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	tmp, err := os.MkdirTemp(cache, ".extract-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := extractTarGz(f, tmp); err != nil {
		return fmt.Errorf("unpack: %w", err)
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		return err
	}
	if len(entries) != 1 || entries[0].Name() != e.ArchiveName() || !entries[0].IsDir() {
		return fmt.Errorf("unpack: unexpected archive layout (want a single %s folder)", e.ArchiveName())
	}
	staged := filepath.Join(tmp, e.ArchiveName())
	if err := os.Remove(filepath.Join(staged, "build-info.json")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if _, err := os.Lstat(filepath.Join(staged, verifiedMarker)); err == nil {
		return fmt.Errorf("unpack: %w: an extension archive may not contain %q", errUnsafeArchive, verifiedMarker)
	}
	if err := durable.SyncDirs(staged); err != nil {
		return fmt.Errorf("unpack: write to disk: %w", err)
	}
	m, err := buildManifest(staged, e.SHA256)
	if err != nil {
		return fmt.Errorf("unpack: list files: %w", err)
	}
	for _, x := range m.Files {
		if err := extensionPathAllowed(e, x); err != nil {
			return err
		}
		if pgFiles[x.Path] {
			return fmt.Errorf("unpack: %s would replace a file of the PostgreSQL build", x.Path)
		}
	}
	if err := checkExtensionFiles(Install{Root: staged}, e, m, true); err != nil {
		return fmt.Errorf("unpack: %w", err)
	}
	fault("ext-staged")

	if err := os.Remove(filepath.Join(inst.Root, e.marker())); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	} else if err == nil {
		if err := durable.SyncDir(inst.Root); err != nil {
			return err
		}
	}
	slices.SortStableFunc(m.Files, func(a, b manifestEntry) int { return installOrder(e, a.Path) - installOrder(e, b.Path) })
	for i, x := range m.Files {
		dst := filepath.Join(inst.Root, filepath.FromSlash(x.Path))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := durable.Rename(filepath.Join(staged, filepath.FromSlash(x.Path)), dst); err != nil {
			return fmt.Errorf("move %s into place: %w", x.Path, err)
		}
		if i == 0 {
			fault("ext-first-moved")
		}
	}
	fault("ext-moved")
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err := durable.WriteFile(filepath.Join(inst.Root, e.marker()), append(data, '\n'), 0o644); err != nil {
		return err
	}
	fault("ext-marked")
	return nil
}

// installOrder: the library first, the control file last.
func installOrder(e Extension, p string) int {
	switch {
	case p == e.Library():
		return 0
	case p == e.Control():
		return 2
	}
	return 1
}

// extensionPathAllowed: an extension archive may only add its own library,
// its control and script files, and its licence notices.
func extensionPathAllowed(e Extension, x manifestEntry) error {
	dir, file := path.Split(x.Path)
	ok := x.Link == "" && file != "" && !strings.HasPrefix(file, ".")
	switch dir {
	case "lib/":
		ok = ok && x.Path == e.Library()
	case "share/extension/":
		ok = ok && (file == e.Name+".control" || strings.HasPrefix(file, e.Name+"--") && strings.HasSuffix(file, ".sql"))
	case "dependency-notices/" + e.Project + "/":
	default:
		ok = false
	}
	if !ok {
		return fmt.Errorf("unpack: %w: an extension archive may not contain %q", errUnsafeArchive, x.Path)
	}
	return nil
}

// installFiles is the set of files of the PostgreSQL build (its manifest).
func installFiles(inst Install) (map[string]bool, error) {
	b, err := os.ReadFile(filepath.Join(inst.Root, verifiedMarker))
	if err != nil {
		return nil, errors.New("PostgreSQL is not installed (no verified marker)")
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, errors.New("the list of PostgreSQL's files is unreadable")
	}
	out := make(map[string]bool, len(m.Files))
	for _, x := range m.Files {
		out[x.Path] = true
	}
	return out, nil
}

// removeExtension removes the files m lists, control file first, then the
// marker.
func removeExtension(inst Install, e Extension, m manifest) error {
	files := slices.Clone(m.Files)
	slices.SortStableFunc(files, func(a, b manifestEntry) int { return installOrder(e, b.Path) - installOrder(e, a.Path) })
	for _, x := range files {
		if !filepath.IsLocal(filepath.FromSlash(x.Path)) || extensionPathAllowed(e, x) != nil {
			continue // never delete outside what an extension may own
		}
		p := filepath.Join(inst.Root, filepath.FromSlash(x.Path))
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := durable.SyncDir(filepath.Dir(p)); err != nil {
			return err
		}
	}
	if err := os.Remove(filepath.Join(inst.Root, e.marker())); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return durable.SyncDir(inst.Root)
}
