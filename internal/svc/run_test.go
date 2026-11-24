//go:build !windows

package svc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRunReturnsWhenFnEnds(t *testing.T) {
	want := errors.New("fatal: removed from cluster")
	done := make(chan error, 1)
	go func() { done <- Run(func(ctx context.Context) error { return want }) }()
	select {
	case err := <-done:
		if !errors.Is(err, want) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after fn ended by itself")
	}
}

func TestRunPanicBecomesError(t *testing.T) {
	err := Run(func(ctx context.Context) error { panic("boom") })
	if err == nil || !strings.Contains(err.Error(), "panic: boom") {
		t.Fatalf("err = %v", err)
	}
}

func TestProgramStopCancelsAndWaits(t *testing.T) {
	stopped := make(chan struct{})
	p := &program{fn: func(ctx context.Context) error {
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond) // "stopping Postgres"
		close(stopped)
		return ctx.Err()
	}, done: make(chan struct{})}
	if err := p.Start(nil); err != nil {
		t.Fatal(err)
	}
	if err := p.Stop(nil); err != nil {
		t.Fatalf("clean stop returned %v", err)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("Stop returned before fn finished")
	}
}
