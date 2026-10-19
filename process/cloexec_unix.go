//go:build unix

package process

import "syscall"

func closeOnExec(fd int) { syscall.CloseOnExec(fd) }
