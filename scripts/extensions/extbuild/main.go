// Command extbuild is the Go half of scripts/extensions (D53): it fetches
// the pinned PostgreSQL tree a build compiles against, packs a built
// extension into a reproducible archive, and checks one against the pins.
//
//	extbuild pgtree -goos linux -goarch arm64 -cache DIR   # prints the tree's folder
//	extbuild pack -src DIR -out FILE.tar.gz                 # prints the sha256
//	extbuild verify -dir DIR                                # every pinned archive in DIR
//
// pgtree uses coyote's own download, checksum and unpack code (pg.Ensure),
// so an extension is always built against exactly the tree coyote runs.
// pack writes entries sorted by name, with fixed times, owners and modes
// and no gzip header time: the same files always give the same bytes, on
// any machine.
package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"coyotedb.com/coyote/internal/pg"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "pgtree":
		err = pgtree(os.Args[2:])
	case "pack":
		err = pack(os.Args[2:])
	case "verify":
		err = verify(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "extbuild %s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: extbuild pgtree -goos OS -goarch ARCH -cache DIR | pack -src DIR -out FILE | verify -dir DIR")
	os.Exit(2)
}

func pgtree(args []string) error {
	flags := flag.NewFlagSet("pgtree", flag.ExitOnError)
	goos := flags.String("goos", "", "operating system of the tree: linux, darwin, windows")
	goarch := flags.String("goarch", "", "processor of the tree: amd64, arm64")
	cache := flags.String("cache", "", "folder to unpack PostgreSQL into")
	archive := flags.String("archive", "", "use this .tar.gz instead of downloading (still checksum-verified)")
	_ = flags.Parse(args)
	if *goos == "" || *goarch == "" || *cache == "" {
		return errors.New("-goos, -goarch and -cache are required")
	}
	b, err := pg.BuildFor(*goos, *goarch)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	inst, err := pg.Ensure(ctx, pg.FetchOptions{
		CacheDir: *cache, Build: b, Archive: *archive, SkipExtensions: true,
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})),
	})
	if err != nil {
		return err
	}
	if err := pg.CheckInstall(inst, true); err != nil {
		return fmt.Errorf("verify %s: %w", inst.Root, err)
	}
	fmt.Println(inst.Root)
	return nil
}

func pack(args []string) error {
	flags := flag.NewFlagSet("pack", flag.ExitOnError)
	src := flags.String("src", "", "the staged folder; it becomes the archive's single top-level folder")
	out := flags.String("out", "", "archive to write (.tar.gz)")
	_ = flags.Parse(args)
	if *src == "" || *out == "" {
		return errors.New("-src and -out are required")
	}
	root, err := filepath.Abs(*src)
	if err != nil {
		return err
	}
	top := filepath.Base(root)
	type entry struct {
		rel string
		dir bool
		exe bool
	}
	var entries []entry
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		name := top
		if rel != "." {
			name = top + "/" + filepath.ToSlash(rel)
		}
		switch {
		case d.IsDir():
			entries = append(entries, entry{rel: name + "/", dir: true})
		case d.Type().IsRegular():
			fi, err := d.Info()
			if err != nil {
				return err
			}
			entries = append(entries, entry{rel: name, exe: fi.Mode()&0o111 != 0})
		default:
			return fmt.Errorf("%s: only files and folders may be packed", rel)
		}
		return nil
	})
	if err != nil {
		return err
	}
	slices.SortFunc(entries, func(a, b entry) int { return strings.Compare(a.rel, b.rel) })

	tmp := *out + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	h := sha256.New()
	gz, err := gzip.NewWriterLevel(io.MultiWriter(f, h), gzip.BestCompression)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(gz)
	epoch := time.Unix(0, 0).UTC()
	for _, e := range entries {
		hdr := &tar.Header{Name: e.rel, ModTime: epoch, Format: tar.FormatUSTAR, Mode: 0o644}
		if e.dir {
			hdr.Typeflag, hdr.Mode = tar.TypeDir, 0o755
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			continue
		}
		if e.exe {
			hdr.Mode = 0o755
		}
		p := filepath.Join(filepath.Dir(root), filepath.FromSlash(e.rel))
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		hdr.Typeflag, hdr.Size = tar.TypeReg, int64(len(data))
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := tw.Write(data); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, *out); err != nil {
		return err
	}
	fmt.Println(hex.EncodeToString(h.Sum(nil)))
	return nil
}

func verify(args []string) error {
	flags := flag.NewFlagSet("verify", flag.ExitOnError)
	dir := flags.String("dir", "", "folder with the extension archives")
	_ = flags.Parse(args)
	if *dir == "" {
		return errors.New("-dir is required")
	}
	var errs []error
	for _, t := range [][2]string{{"darwin", "arm64"}, {"darwin", "amd64"}, {"linux", "amd64"}, {"linux", "arm64"}, {"windows", "amd64"}} {
		b, err := pg.BuildFor(t[0], t[1])
		if err != nil {
			return err
		}
		exts := pg.ExtensionsFor(b)
		if len(exts) == 0 {
			errs = append(errs, fmt.Errorf("%s/%s: nothing pinned", t[0], t[1]))
		}
		for _, e := range exts {
			p := filepath.Join(*dir, e.FileName())
			if err := pg.VerifyFile(p, e.SHA256); err != nil {
				errs = append(errs, err)
				continue
			}
			fmt.Printf("ok  %s  %s\n", e.SHA256, e.FileName())
		}
	}
	return errors.Join(errs...)
}
