//go:build !unix

package process

import "syscall"

const supervisorSupported = false

func newProcessGroup() *syscall.SysProcAttr { return nil }
