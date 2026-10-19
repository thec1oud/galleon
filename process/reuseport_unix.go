//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package process

import (
	"syscall"

	"golang.org/x/sys/unix"
)

const reusePortSupported = true

func reusePort(network, address string, c syscall.RawConn) error {
	var serr error
	err := c.Control(func(fd uintptr) {
		serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, soReusePort, 1)
	})
	if err != nil {
		return err
	}
	return serr
}
