//go:build darwin || netbsd || openbsd || dragonfly

package process

import "golang.org/x/sys/unix"

const soReusePort = unix.SO_REUSEPORT
