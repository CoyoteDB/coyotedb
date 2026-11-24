// Command pgfetch puts the pinned PostgreSQL build into a folder while the
// Docker image is built (docker/Dockerfile, D46).
//
// It uses the exact code `coyote` uses on first start (internal/pg.Ensure):
// the same download URL, the same sha256 pinned in internal/pg/version.go,
// the same safe unpack and the same verified-files manifest. So a node in a
// container runs byte-for-byte the build a node on a Linux machine
// downloads, and there is one list of checksums, not two.
//
//	pgfetch -arch arm64 -cache /out/pg [-extensions-from DIR]
//
// -arch is the image's processor (Docker's TARGETARCH), not the one this
// program runs on: the image for arm64 is assembled on an amd64 machine.
// The install is then checked again file by file (sha256 of every file).
// With -extensions-from, the pinned extension archives found there (D53)
// are installed into the tree and checked the same way; a folder without
// any is allowed (the image then has no extensions) and said loudly.
// The last line printed is the install folder.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"coyotedb.com/coyote/internal/pg"
)

func main() {
	arch := flag.String("arch", "", "processor of the image: amd64 or arm64")
	cache := flag.String("cache", "", "folder to unpack PostgreSQL into")
	archive := flag.String("archive", "", "use this .tar.gz instead of downloading (still checksum-verified)")
	exts := flag.String("extensions-from", "", "folder with the extension archives to install (D53)")
	flag.Parse()
	if *arch == "" || *cache == "" || flag.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "usage: pgfetch -arch amd64|arm64 -cache DIR [-archive FILE]")
		os.Exit(2)
	}
	if err := run(*arch, *cache, *archive, *exts); err != nil {
		fmt.Fprintf(os.Stderr, "pgfetch: %v\n", err)
		os.Exit(1)
	}
}

func run(arch, cache, archive, exts string) error {
	b, err := pg.BuildFor("linux", arch)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	last := -1
	inst, err := pg.Ensure(ctx, pg.FetchOptions{
		CacheDir:       cache,
		Build:          b,
		Archive:        archive,
		Logger:         log,
		SkipExtensions: true, // below, so that a problem fails the build
		Progress: func(done, total int64) {
			if total <= 0 {
				return
			}
			if p := int(done * 100 / total / 25 * 25); p != last {
				last = p
				fmt.Fprintf(os.Stderr, "downloading %s: %d%%\n", b.Name(), p)
			}
		},
	})
	if err != nil {
		return fmt.Errorf("fetch %s: %w", b.Name(), err)
	}
	if err := pg.CheckInstall(inst, true); err != nil {
		return fmt.Errorf("verify %s: %w", inst.Root, err)
	}
	fmt.Fprintf(os.Stderr, "verified %s (archive sha256 %s)\n", b.Name(), b.SHA256)
	if exts != "" {
		if err := installExtensions(ctx, inst, exts, log); err != nil {
			return err
		}
	}
	fmt.Println(inst.Root)
	return nil
}

// installExtensions installs every pinned extension whose archive is in
// dir, and fails if any of them does not then check out file by file.
func installExtensions(ctx context.Context, inst pg.Install, dir string, log *slog.Logger) error {
	var want []pg.Extension
	for _, e := range pg.ExtensionsFor(inst.Build) {
		if _, err := os.Stat(filepath.Join(dir, e.FileName())); err == nil {
			want = append(want, e)
		} else {
			fmt.Fprintf(os.Stderr, "WARNING: %s is not in %s: the image will not have %s\n", e.FileName(), dir, e.Project)
		}
	}
	if len(want) == 0 {
		return nil
	}
	if err := pg.EnsureExtensions(ctx, pg.FetchOptions{ExtensionsFrom: dir, Logger: log}, inst); err != nil {
		return fmt.Errorf("install extensions: %w", err)
	}
	for _, e := range want {
		if err := pg.CheckExtension(inst, e, true); err != nil {
			return fmt.Errorf("verify %s: %w", e.Project, err)
		}
		fmt.Fprintf(os.Stderr, "verified %s %s (archive sha256 %s)\n", e.Project, e.Version, e.SHA256)
	}
	return nil
}
