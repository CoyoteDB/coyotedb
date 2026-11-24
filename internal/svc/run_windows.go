//go:build windows

package svc

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"time"

	"golang.org/x/sys/windows/svc"
)

// Run is the service entry point. Under the service manager it accepts
// stop, shutdown and preshutdown; on any of them it cancels fn's context
// and keeps reporting "stop pending" with progress until fn returns, so
// Windows gives Postgres the time it needs (up to the preshutdown timeout
// set at install). A failing fn exits with a service-specific error so
// the recovery actions restart it. In a terminal it runs until Ctrl+C.
func Run(fn RunFunc) error {
	isSvc, err := svc.IsWindowsService()
	if err != nil {
		return err
	}
	if !isSvc {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		return safeRun(ctx, fn)
	}
	h := &handler{fn: fn}
	if err := svc.Run(Name, h); err != nil {
		return err
	}
	return h.err
}

type handler struct {
	fn  RunFunc
	err error
}

func (h *handler) Execute(_ []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	const accepts = svc.AcceptStop | svc.AcceptShutdown | svc.AcceptPreShutdown
	status <- svc.Status{State: svc.StartPending, WaitHint: 10000}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- safeRun(ctx, h.fn) }()
	status <- svc.Status{State: svc.Running, Accepts: accepts}

	stopping := false
	var checkpoint uint32
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				h.err = err
				status <- svc.Status{State: svc.StopPending}
				return true, 1 // service-specific failure: recovery actions apply
			}
			status <- svc.Status{State: svc.StopPending}
			return false, 0
		case <-tick.C:
			if stopping {
				checkpoint++
				status <- svc.Status{State: svc.StopPending, CheckPoint: checkpoint, WaitHint: 15000}
			}
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown, svc.PreShutdown:
				if !stopping {
					stopping = true
					checkpoint++
					status <- svc.Status{State: svc.StopPending, CheckPoint: checkpoint, WaitHint: 15000}
					cancel()
				}
			}
		}
	}
}
