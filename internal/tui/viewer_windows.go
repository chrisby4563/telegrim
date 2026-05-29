//go:build windows

package tui

import "telegrim/internal/telegram"

// pickViewer auf Windows: `cmd /c start "" "<path>"` öffnet den Default-
// Handler für den Dateityp (Foto-App, Movies&TV, Adobe Reader etc).
// "" als zweiter Arg = leerer Window-Title, sonst würde start den Pfad als
// Title interpretieren wenn er Anführungszeichen enthält.
//
// Kill-Verhalten: start beendet sich selbst nach Launch — der eigentliche
// Viewer läuft eigenständig. Esc-Kill greift nicht, das ist auf Windows
// nicht zu umgehen ohne tiefere Process-Tree-API.
func pickViewer(med telegram.Media, path string) (string, []string) {
	return "cmd", []string{"/c", "start", "", path}
}
