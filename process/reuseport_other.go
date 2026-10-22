//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package process

import (
	"errors"
	"syscall"
)

const reusePortSupported = false

func reusePort(network, address string, c syscall.RawConn) error {
	return errors.New("process: SO_REUSEPORT is not supported on this platform")
}
