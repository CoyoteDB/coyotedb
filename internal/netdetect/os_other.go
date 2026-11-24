//go:build !windows && !linux && !darwin

package netdetect

import (
	"context"
	"net"
)

func interfaces(context.Context) ([]Interface, error) {
	nifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	return fromNet(nifs), nil
}
