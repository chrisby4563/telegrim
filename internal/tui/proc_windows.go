//go:build windows

package tui

import "os/exec"

// setMediaProcessGroup: Windows kennt keine Unix-Process-Groups. Wir lassen
// das Kind direkt vom Parent erben — Kill bricht später best-effort den
// Hauptprozess ab, Sub-Prozesse müssen sich selbst beenden.
func setMediaProcessGroup(c *exec.Cmd) {}

// killMediaProcess: kein Process-Group-Signal auf Windows. Direkt Kill.
func killMediaProcess(c *exec.Cmd) {
	if c == nil || c.Process == nil {
		return
	}
	_ = c.Process.Kill()
}
