package pg

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

// maxExtractBytes caps the total uncompressed size we are willing to write
// (decompression-bomb guard). The real archives are ~400 MB unpacked.
const maxExtractBytes int64 = 4 << 30

// errUnsafeArchive marks an archive entry we refuse to extract.
var errUnsafeArchive = errors.New("unsafe archive entry")

// extractTarGz unpacks a .tar.gz into dest, which must already exist and be
// empty. Every entry must stay inside dest:
//   - no absolute paths, volume names, backslashes or ".." components
//   - no writing through a symlink created earlier in the same archive
//   - symlink targets must be relative and resolve inside dest
//   - hard links must point at a regular file already extracted
//   - only regular files, directories, symlinks and hard links are allowed
//
// Hard links are materialised as copies (portable, and avoids aliasing).
// On Windows, symlinks are materialised as copies too.
func extractTarGz(src io.Reader, dest string) error {
	gz, err := gzip.NewReader(src)
	if err != nil {
		return fmt.Errorf("open gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	root, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	var total int64
	type deferredLink struct{ name, target string }
	var winSymlinks []deferredLink

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read archive: %w", err)
		}
		rel, err := cleanEntryName(hdr.Name)
		if err != nil {
			return err
		}
		if rel == "" { // the archive root itself ("./")
			continue
		}
		target := filepath.Join(root, filepath.FromSlash(rel))
		if err := checkNoSymlinkParents(root, rel); err != nil {
			return err
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA: //nolint:staticcheck // TypeRegA appears in old archives
			if hdr.Size < 0 || total+hdr.Size > maxExtractBytes {
				return fmt.Errorf("%w: archive too large", errUnsafeArchive)
			}
			total += hdr.Size
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := writeFile(target, tr, hdr.Size, fileMode(hdr.Mode)); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := checkSymlinkTarget(rel, hdr.Linkname); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if runtime.GOOS == "windows" {
				winSymlinks = append(winSymlinks, deferredLink{rel, hdr.Linkname})
				continue
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return fmt.Errorf("create symlink %s: %w", rel, err)
			}
		case tar.TypeLink:
			linkRel, err := cleanEntryName(hdr.Linkname)
			if err != nil || linkRel == "" {
				return fmt.Errorf("%w: hard link %q -> %q", errUnsafeArchive, hdr.Name, hdr.Linkname)
			}
			if err := checkNoSymlinkParents(root, linkRel); err != nil {
				return err
			}
			srcPath := filepath.Join(root, filepath.FromSlash(linkRel))
			fi, err := os.Lstat(srcPath)
			if err != nil || !fi.Mode().IsRegular() {
				return fmt.Errorf("%w: hard link %q must point at a file extracted earlier", errUnsafeArchive, hdr.Name)
			}
			if total+fi.Size() > maxExtractBytes {
				return fmt.Errorf("%w: archive too large", errUnsafeArchive)
			}
			total += fi.Size()
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := copyFile(srcPath, target, fi.Mode().Perm()); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: %q has unsupported type %q", errUnsafeArchive, hdr.Name, string(hdr.Typeflag))
		}
	}

	// Windows: copy symlink targets (they are all inside root, checked above).
	for _, l := range winSymlinks {
		linkPath := filepath.Join(root, filepath.FromSlash(l.name))
		srcPath := filepath.Join(filepath.Dir(linkPath), filepath.FromSlash(l.target))
		fi, err := os.Stat(srcPath)
		if err != nil || !fi.Mode().IsRegular() {
			return fmt.Errorf("%w: symlink %q target missing", errUnsafeArchive, l.name)
		}
		if err := copyFile(srcPath, linkPath, fi.Mode().Perm()); err != nil {
			return err
		}
	}
	return nil
}

// cleanEntryName validates an archive path and returns it in clean,
// slash-separated, relative form ("" for the root).
func cleanEntryName(name string) (string, error) {
	bad := func() (string, error) {
		return "", fmt.Errorf("%w: %q", errUnsafeArchive, name)
	}
	if name == "" || strings.ContainsRune(name, '\\') || strings.ContainsRune(name, 0) {
		return bad()
	}
	if strings.HasPrefix(name, "/") || filepath.IsAbs(name) || filepath.VolumeName(name) != "" {
		return bad()
	}
	// A drive letter like "C:" anywhere in the first component.
	if len(name) >= 2 && name[1] == ':' {
		return bad()
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return bad()
		}
	}
	c := path.Clean(name)
	if c == "." {
		return "", nil
	}
	if c == ".." || strings.HasPrefix(c, "../") || strings.HasPrefix(c, "/") {
		return bad()
	}
	return c, nil
}

// checkSymlinkTarget ensures a symlink at rel pointing at linkname resolves
// inside the extraction root, lexically.
func checkSymlinkTarget(rel, linkname string) error {
	if linkname == "" || strings.ContainsRune(linkname, '\\') || strings.HasPrefix(linkname, "/") ||
		filepath.IsAbs(linkname) || filepath.VolumeName(linkname) != "" || (len(linkname) >= 2 && linkname[1] == ':') {
		return fmt.Errorf("%w: symlink %q -> %q", errUnsafeArchive, rel, linkname)
	}
	resolved := path.Clean(path.Join(path.Dir(rel), linkname))
	if resolved == "." || resolved == ".." || strings.HasPrefix(resolved, "../") {
		return fmt.Errorf("%w: symlink %q -> %q escapes the archive", errUnsafeArchive, rel, linkname)
	}
	return nil
}

// checkNoSymlinkParents refuses to write rel if any existing parent component
// (or the path itself) is a symlink: that would let a later entry write
// through an earlier link.
func checkNoSymlinkParents(root, rel string) error {
	parts := strings.Split(rel, "/")
	cur := root
	for _, p := range parts {
		cur = filepath.Join(cur, p)
		fi, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%w: %q goes through a symlink", errUnsafeArchive, rel)
		}
	}
	return nil
}

func fileMode(m int64) fs.FileMode {
	// Keep only rwx bits; drop setuid/setgid/sticky. Always owner-readable.
	perm := fs.FileMode(m) & 0o777
	return perm | 0o600
}

func writeFile(target string, r io.Reader, size int64, mode fs.FileMode) error {
	f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return fmt.Errorf("create %s: %w", target, err)
	}
	n, err := io.Copy(f, io.LimitReader(r, size))
	if err == nil {
		err = f.Sync() // on disk before the install is published (unpack)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("write %s: %w", target, err)
	}
	if n != size {
		return fmt.Errorf("write %s: short file", target)
	}
	return nil
}

func copyFile(src, dst string, mode fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	return writeFile(dst, in, fi.Size(), mode|0o600)
}
