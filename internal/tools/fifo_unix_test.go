//go:build unix

package tools

import "syscall"

func makeTestFIFO(path string) error { return syscall.Mkfifo(path, 0600) }
