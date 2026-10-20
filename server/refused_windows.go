package server

import (
	"errors"
	"syscall"
)

// wsaeconnrefused is WSAECONNREFUSED, which Windows reports in place of
// ECONNREFUSED.
const wsaeconnrefused syscall.Errno = 10061

// refused says whether a dial failed because nothing listens on the socket.
func refused(err error) bool {
	return errors.Is(err, wsaeconnrefused) || errors.Is(err, syscall.ECONNREFUSED)
}
