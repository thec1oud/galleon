//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package process

import (
	"syscall"
)

const reusePortSupported = true

func reusePort(network, address string, c syscall.RawConn) error {
	var serr error
	err := c.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, soReusePort, 1)
	})
	if err != nil {
		return err
	}
	return serr
}
