package svc

import (
	"log/slog"
	"time"
)

func newPlatform(log *slog.Logger) platform {
	return &darwinPlatform{h: osHost{}, log: log, sleep: time.Sleep}
}
