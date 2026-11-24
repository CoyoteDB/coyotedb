package svc

import (
	"context"
	"fmt"
)

// RunFunc is the agent's body. It must return soon after ctx is cancelled
// (having stopped Postgres cleanly), and return a non-nil error for a
// failure that should make the service manager restart it.
type RunFunc func(ctx context.Context) error

// safeRun turns a panic in fn into an error, so a service always reports
// a failure (and is restarted) instead of dying without a status.
func safeRun(ctx context.Context, fn RunFunc) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return fn(ctx)
}
