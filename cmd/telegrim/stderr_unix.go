//go:build !windows

package main

import (
	"os"
	"syscall"
)

func redirectStderr(f *os.File) {
	_ = syscall.Dup2(int(f.Fd()), int(os.Stderr.Fd()))
}
