package pg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"coyotedb.com/coyote/internal/durable"
)

// ErrChecksumMismatch means an archive did not match its pinned sha256.
var ErrChecksumMismatch = errors.New("checksum mismatch")

// maxDownloadBytes caps how much we accept from the server.
const maxDownloadBytes int64 = 1 << 30

// verifiedMarker is written inside an extracted install once it has been
// verified, fully unpacked and synced to disk. It holds the manifest (see
// manifest): the archive sha256 and every file's path, size and sha256.
const verifiedMarker = ".coyote-verified"

// Install is an unpacked, verified Postgres build on disk.
type Install struct {
	Root  string
	Build Build
}

// Bin returns the path of a Postgres program (adds .exe on Windows, and
// for a Windows build unpacked elsewhere, as scripts/extensions does).
func (i Install) Bin(name string) string {
	if runtime.GOOS == "windows" || strings.Contains(i.Build.Target, "-windows-") {
		name += ".exe"
	}
	return filepath.Join(i.Root, "bin", name)
}

// FetchOptions controls Ensure.
type FetchOptions struct {
	// CacheDir holds unpacked builds, one versioned folder per build.
	CacheDir string
	// Build to fetch. Zero value means CurrentBuild().
	Build Build
	// Archive, if set, is a local .tar.gz used instead of downloading. It is
	// still checked against the pinned sha256.
	Archive string
	// Progress, if set, is called during download with bytes done/total
	// (total is -1 when unknown).
	Progress func(done, total int64)
	// Client is the HTTP client; nil means a client with sane timeouts.
	Client *http.Client
	Logger *slog.Logger
	// Full: check every file's sha256 against the manifest, not just its
	// size (used when a Postgres program crashed or failed to run).
	Full bool
	// Status, if set, is told in plain words when a damaged install is
	// being replaced.
	Status func(msg string)

	// ExtensionsFrom is where the pinned extension archives are (D53): a
	// folder or an http(s) base URL. Empty: ExtensionsFromEnv.
	ExtensionsFrom string
	// SkipExtensions installs PostgreSQL only.
	SkipExtensions bool

	fault      func(point string) // tests: called at each step of unpack
	extensions []Extension        // tests: instead of ExtensionsFor
}

// Ensure returns a verified Postgres install, downloading and unpacking it
// into the cache on first use and reusing it afterwards. It then installs
// the pinned extensions (EnsureExtensions); a problem there is logged and
// told through Status, never returned: the database runs without them.
func Ensure(ctx context.Context, o FetchOptions) (Install, error) {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	inst, err := ensurePostgres(ctx, o)
	if err != nil || o.SkipExtensions {
		return inst, err
	}
	if err := EnsureExtensions(ctx, o, inst); err != nil {
		if errors.Is(err, ErrExtensionsNotConfigured) {
			o.Logger.Info("extensions not installed", "why", err, "set", ExtensionsFromEnv)
		} else {
			o.Logger.Warn("could not install an extension; the database runs without it", "err", err)
			if o.Status != nil {
				o.Status("Could not install an extension (" + err.Error() + "); the database runs without it.")
			}
		}
	}
	return inst, nil
}

func ensurePostgres(ctx context.Context, o FetchOptions) (Install, error) {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Build == (Build{}) {
		b, err := CurrentBuild()
		if err != nil {
			return Install{}, err
		}
		o.Build = b
	}
	if o.CacheDir == "" {
		return Install{}, errors.New("no cache directory")
	}
	cache, err := filepath.Abs(o.CacheDir)
	if err != nil {
		return Install{}, err
	}
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return Install{}, fmt.Errorf("create cache dir: %w", err)
	}
	inst := Install{Root: filepath.Join(cache, o.Build.Name()), Build: o.Build}
	if CheckInstall(inst, o.Full) == nil {
		return inst, nil
	}

	lock, err := AcquireLock(ctx, filepath.Join(cache, ".lock"), func() {
		o.Logger.Info("waiting for another process to finish fetching Postgres", "cache", cache)
	})
	if err != nil {
		return Install{}, err
	}
	defer lock.Unlock()
	err = CheckInstall(inst, o.Full)
	if err == nil { // someone else finished while we waited
		return inst, nil
	}
	if _, serr := os.Lstat(inst.Root); serr == nil {
		// Damaged (a power cut during an old install that was not synced,
		// a disk error): replace it.
		o.Logger.Warn("the PostgreSQL install is damaged; installing it again", "dir", inst.Root, "why", err)
		if o.Status != nil {
			o.Status("The PostgreSQL program files on this machine are damaged (" + err.Error() + "); downloading and installing them again…")
		}
	}
	cleanStaleExtracts(cache, o.Logger)

	archive := o.Archive
	downloaded := false
	if archive == "" {
		archive = filepath.Join(cache, o.Build.Name()+".tar.gz.partial")
		o.Logger.Info("downloading postgres", "url", o.Build.URL(), "to", archive)
		if err := download(ctx, o.client(), o.Build.URL(), archive, o.Progress, o.Logger, "postgres"); err != nil {
			return Install{}, err
		}
		downloaded = true
	}
	if err := VerifyFile(archive, o.Build.SHA256); err != nil {
		if downloaded && errors.Is(err, ErrChecksumMismatch) {
			// Do not resume from a bad file next time.
			_ = os.Remove(archive)
		}
		return Install{}, err
	}
	o.Logger.Info("archive verified", "sha256", o.Build.SHA256)

	if err := unpack(archive, cache, inst, o.fault); err != nil {
		return Install{}, err
	}
	if downloaded {
		_ = os.Remove(archive)
	}
	return inst, nil
}

func (o FetchOptions) client() *http.Client {
	if o.Client != nil {
		return o.Client
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 60 * time.Second
	return &http.Client{Transport: tr}
}

// manifest is the content of the verified marker.
type manifest struct {
	ArchiveSHA256 string          `json:"archive_sha256"`
	Files         []manifestEntry `json:"files"`
}

type manifestEntry struct {
	Path   string `json:"path"` // slash-separated, relative to the install root
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256,omitempty"`
	Link   string `json:"link,omitempty"` // symlink target (then no size/hash)
}

// CheckInstall reports what is wrong with an install (nil: it is complete
// and matches its manifest). It always checks that every file is there
// with its size; full also checks every file's sha256.
func CheckInstall(inst Install, full bool) error {
	b, err := os.ReadFile(filepath.Join(inst.Root, verifiedMarker))
	if err != nil {
		return errors.New("not installed (no verified marker)")
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil || m.ArchiveSHA256 == "" || len(m.Files) == 0 {
		return errors.New("its list of files is unreadable")
	}
	if m.ArchiveSHA256 != inst.Build.SHA256 {
		return errors.New("it is from another PostgreSQL build")
	}
	sawPostgres := false
	bin := filepath.ToSlash(mustRel(inst.Root, inst.Bin("postgres")))
	for _, e := range m.Files {
		sawPostgres = sawPostgres || e.Path == bin
	}
	if err := checkFiles(inst.Root, m, full); err != nil {
		return err
	}
	if !sawPostgres {
		return errors.New("its list of files has no postgres program")
	}
	return nil
}

// checkFiles checks that every file m lists is under root with its size
// (full: and its sha256).
func checkFiles(root string, m manifest, full bool) error {
	for _, e := range m.Files {
		p := filepath.Join(root, filepath.FromSlash(e.Path))
		if !filepath.IsLocal(filepath.FromSlash(e.Path)) {
			return fmt.Errorf("its list of files names %q, outside the install", e.Path)
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return fmt.Errorf("%s is missing", e.Path)
		}
		if e.Link != "" {
			if t, err := os.Readlink(p); err != nil || t != e.Link {
				return fmt.Errorf("%s does not point where it should", e.Path)
			}
			continue
		}
		if !fi.Mode().IsRegular() || fi.Size() != e.Size {
			return fmt.Errorf("%s is %d bytes, not %d (cut short)", e.Path, fi.Size(), e.Size)
		}
		if full {
			if h, err := fileSHA256(p); err != nil || h != e.SHA256 {
				return fmt.Errorf("%s is corrupted (its contents do not match)", e.Path)
			}
		}
	}
	return nil
}

func mustRel(base, p string) string {
	r, err := filepath.Rel(base, p)
	if err != nil {
		return p
	}
	return r
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// buildManifest lists every file under root (the marker excluded).
func buildManifest(root, archiveSHA string) (manifest, error) {
	m := manifest{ArchiveSHA256: archiveSHA}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if rel == verifiedMarker {
			return nil
		}
		e := manifestEntry{Path: filepath.ToSlash(rel)}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			if e.Link, err = os.Readlink(p); err != nil {
				return err
			}
		case d.Type().IsRegular():
			fi, err := d.Info()
			if err != nil {
				return err
			}
			e.Size = fi.Size()
			if e.SHA256, err = fileSHA256(p); err != nil {
				return err
			}
		default:
			return nil
		}
		m.Files = append(m.Files, e)
		return nil
	})
	return m, err
}

func cleanStaleExtracts(cache string, log *slog.Logger) {
	entries, _ := os.ReadDir(cache)
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), ".extract-") {
			log.Info("removing leftover extract dir", "dir", e.Name())
			_ = os.RemoveAll(filepath.Join(cache, e.Name()))
		}
	}
}

// unpack extracts into a temp dir next to the final location and makes it
// survive a power cut before it is published: every file and folder is
// synced, then the manifest (the marker) is written durably, then the
// folder is renamed into place and the cache folder synced. A crash at
// any point leaves either no install at that path (or the old damaged
// one, which fails CheckInstall) or a complete, synced one.
func unpack(archive, cache string, inst Install, fault func(string)) error {
	if fault == nil {
		fault = func(string) {}
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
		return fmt.Errorf("unpack postgres: %w", err)
	}
	fault("extracted")
	entries, err := os.ReadDir(tmp)
	if err != nil {
		return err
	}
	if len(entries) != 1 || entries[0].Name() != inst.Build.Name() || !entries[0].IsDir() {
		return fmt.Errorf("unpack postgres: unexpected archive layout (want a single %s folder)", inst.Build.Name())
	}
	staged := Install{Root: filepath.Join(tmp, inst.Build.Name()), Build: inst.Build}
	if _, err := os.Stat(staged.Bin("postgres")); err != nil {
		return fmt.Errorf("unpack postgres: postgres program missing: %w", err)
	}
	// Every file was synced as it was written (extract); now the folders.
	if err := durable.SyncDirs(staged.Root); err != nil {
		return fmt.Errorf("unpack postgres: write to disk: %w", err)
	}
	fault("synced")
	m, err := buildManifest(staged.Root, inst.Build.SHA256)
	if err != nil {
		return fmt.Errorf("unpack postgres: list files: %w", err)
	}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err := durable.WriteFile(filepath.Join(staged.Root, verifiedMarker), append(data, '\n'), 0o644); err != nil {
		return err
	}
	fault("marked")
	if _, err := os.Lstat(inst.Root); err == nil {
		// A broken earlier install. We hold the cache lock.
		if err := os.RemoveAll(inst.Root); err != nil {
			return fmt.Errorf("remove broken install: %w", err)
		}
		if err := durable.SyncDir(cache); err != nil {
			return err
		}
		fault("removed-old")
	}
	if err := durable.Rename(staged.Root, inst.Root); err != nil {
		return fmt.Errorf("move install into place: %w", err)
	}
	fault("renamed")
	return nil
}

// VerifyFile checks a file's sha256 against want (hex).
func VerifyFile(path, want string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open archive: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("hash archive: %w", err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("%w for %s: got %s, want %s", ErrChecksumMismatch, filepath.Base(path), got, want)
	}
	return nil
}

var errPermanent = errors.New("permanent download error")

// download fetches url into dst, resuming a partial file when the server
// supports ranges and restarting otherwise. Retries transient failures.
func download(ctx context.Context, c *http.Client, url, dst string, progress func(int64, int64), log *slog.Logger, what string) error {
	const attempts = 5
	var err error
	for i := 1; i <= attempts; i++ {
		if fi, serr := os.Stat(dst); serr == nil && fi.Size() > 0 {
			log.Info("resuming download", "have_bytes", fi.Size())
		}
		err = downloadOnce(ctx, c, url, dst, progress)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errPermanent) {
			break
		}
		log.Warn("download failed, will retry", "attempt", i, "err", err)
		t := time.NewTimer(time.Duration(i) * time.Second) // retry backoff
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
	return fmt.Errorf("download %s: %w", what, err)
}

func downloadOnce(ctx context.Context, c *http.Client, url, dst string, progress func(int64, int64)) error {
	var have int64
	if fi, err := os.Stat(dst); err == nil {
		have = fi.Size()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("%w: %v", errPermanent, err)
	}
	if have > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(have, 10)+"-")
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	flags := os.O_CREATE | os.O_WRONLY
	var total int64 = -1
	switch resp.StatusCode {
	case http.StatusOK:
		have = 0
		flags |= os.O_TRUNC
		if resp.ContentLength >= 0 {
			total = resp.ContentLength
		}
	case http.StatusPartialContent:
		start, size, ok := parseContentRange(resp.Header.Get("Content-Range"))
		if !ok || start != have {
			// Server answered a different range than asked; restart cleanly.
			_ = os.Remove(dst)
			return errors.New("server sent an unexpected range; restarting download")
		}
		flags |= os.O_APPEND
		total = size
	case http.StatusRequestedRangeNotSatisfiable:
		// Most likely the partial file is already complete. The checksum
		// decides; a corrupt file is deleted and re-fetched by the caller.
		return nil
	default:
		err := fmt.Errorf("server returned %s", resp.Status)
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden {
			return fmt.Errorf("%w: %v", errPermanent, err)
		}
		return err
	}
	if total > maxDownloadBytes {
		return fmt.Errorf("%w: archive is unexpectedly large (%d bytes)", errPermanent, total)
	}

	f, err := os.OpenFile(dst, flags, 0o644)
	if err != nil {
		return fmt.Errorf("%w: %v", errPermanent, err)
	}
	pw := &progressWriter{done: have, total: total, fn: progress}
	limit := maxDownloadBytes - have
	_, err = io.Copy(io.MultiWriter(f, pw), io.LimitReader(resp.Body, limit))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if total >= 0 && pw.done != total {
		return fmt.Errorf("download ended early (%d of %d bytes)", pw.done, total)
	}
	return nil
}

// parseContentRange parses "bytes start-end/size".
func parseContentRange(v string) (start, size int64, ok bool) {
	v, found := strings.CutPrefix(v, "bytes ")
	if !found {
		return 0, 0, false
	}
	rng, sz, found := strings.Cut(v, "/")
	if !found {
		return 0, 0, false
	}
	s, _, found := strings.Cut(rng, "-")
	if !found {
		return 0, 0, false
	}
	start, err1 := strconv.ParseInt(s, 10, 64)
	size, err2 := strconv.ParseInt(sz, 10, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return start, size, true
}

type progressWriter struct {
	done, total int64
	fn          func(int64, int64)
}

func (p *progressWriter) Write(b []byte) (int, error) {
	p.done += int64(len(b))
	if p.fn != nil {
		p.fn(p.done, p.total)
	}
	return len(b), nil
}
