//go:build !windows

package tui

import (
	"os/exec"
	"syscall"
)

// setMediaProcessGroup setzt eine eigene Process-Group für externe Viewer
// (imv/mpv). Damit erwischt killMediaProcess via Kill(-pgid) auch Kindprozesse.
func setMediaProcessGroup(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killMediaProcess sendet SIGTERM an die gesamte Process-Group. Auf Unix mit
// Setpgid=true bedeutet Kill(-pid) die ganze Gruppe.
func killMediaProcess(c *exec.Cmd) {
	if c == nil || c.Process == nil {
		return
	}
	_ = syscall.Kill(-c.Process.Pid, syscall.SIGTERM)
}
