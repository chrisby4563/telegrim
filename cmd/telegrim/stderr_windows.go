//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// redirectStderr leitet FD 2 (stderr) auf eine Datei um. Windows hat kein
// Dup2, stattdessen SetStdHandle. Damit landen Go-Runtime-Panics + Stack-
// Traces zuverlässig im Logfile.
func redirectStderr(f *os.File) {
	_ = windows.SetStdHandle(windows.STD_ERROR_HANDLE, windows.Handle(f.Fd()))
	os.Stderr = f
}
