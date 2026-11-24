package svc

import "log/slog"

func newPlatform(log *slog.Logger) platform { return &linuxPlatform{h: osHost{}, log: log} }
