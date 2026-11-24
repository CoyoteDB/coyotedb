package agent

import (
	"errors"
	"syscall"
)

// wsaeaddrinuse is WSAEADDRINUSE ("Only one usage of each socket address").
const wsaeaddrinuse = syscall.Errno(10048)

func addrInUse(err error) bool {
	return errors.Is(err, wsaeaddrinuse) || errors.Is(err, syscall.EADDRINUSE)
}
