//go:build !windows

package server

import (
	"errors"
	"syscall"
)

// refused says whether a dial failed because nothing listens on the socket.
func refused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED)
}
