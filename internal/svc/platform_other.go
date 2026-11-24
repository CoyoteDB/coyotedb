//go:build !linux && !darwin && !windows

package svc

import (
	"context"
	"log/slog"
	"path/filepath"

	"coyotedb.com/coyote/internal/brand"
)

type unsupported struct{}

func newPlatform(*slog.Logger) platform { return unsupported{} }

func (unsupported) install(context.Context, Options) (Report, error) { return Report{}, ErrUnsupported }
func (unsupported) uninstall(context.Context, UninstallOptions) (Report, error) {
	return Report{}, ErrUnsupported
}
func (unsupported) start(context.Context) error            { return ErrUnsupported }
func (unsupported) stop(context.Context) error             { return ErrUnsupported }
func (unsupported) status(context.Context) (Status, error) { return Status{}, ErrUnsupported }
func (unsupported) defaultDataDir() string {
	return filepath.Join("/var/lib", brand.DirNameLower)
}
