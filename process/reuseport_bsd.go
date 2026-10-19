//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package process

const soReusePort = 0x200 // SO_REUSEPORT
