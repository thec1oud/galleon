//go:build !unix

package process

func closeOnExec(int) {}
