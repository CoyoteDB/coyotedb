//go:build !windows

package svc

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"

	"github.com/kardianos/service"
)

// Run is the service entry point (also fine in a terminal). On Linux and
// macOS it runs fn until SIGTERM (systemd stop, launchd bootout) or
// SIGINT, then cancels fn's context and waits for it to return. It
// returns fn's error. kardianos/service drives the start/stop lifecycle.
func Run(fn RunFunc) error {
	p := &program{fn: fn, done: make(chan struct{})}
	cfg := &service.Config{Name: Name, DisplayName: DisplayName, Description: Description,
		Option: service.KeyValue{"RunWait": p.wait}}
	s, err := service.New(p, cfg)
	if err != nil {
		// No service system detected (for example a container without an
		// init system): same behaviour, without kardianos.
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
		defer stop()
		return safeRun(ctx, fn)
	}
	return s.Run()
}

type program struct {
	fn     RunFunc
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

func (p *program) Start(service.Service) error {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	go func() {
		defer close(p.done)
		p.err = safeRun(ctx, p.fn)
	}()
	return nil
}

// wait blocks until a stop signal arrives or fn ends by itself (kardianos
// would otherwise wait for a signal forever after fn failed).
func (p *program) wait() {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, os.Interrupt)
	defer signal.Stop(sig)
	select {
	case <-sig:
	case <-p.done:
	}
}

func (p *program) Stop(service.Service) error {
	p.cancel()
	<-p.done
	if errors.Is(p.err, context.Canceled) {
		return nil
	}
	return p.err
}
