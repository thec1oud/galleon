//go:build unix

package process

import "syscall"

const supervisorSupported = true

func newProcessGroup() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }
