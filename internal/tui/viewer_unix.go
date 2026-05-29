//go:build !windows

package tui

import (
	"strings"

	"telegrim/internal/telegram"
)

// pickViewer wählt einen direkt killbaren Viewer pro Mediakind/MIME.
// imv/mpv blockieren bis Fenster zu → wir können sie sauber tracken und
// killen. Fallback xdg-open: launcht oft Daemon-Viewer und exitet selbst →
// Kill greift nur best-effort über Process-Group.
func pickViewer(med telegram.Media, path string) (string, []string) {
	mime := med.MimeType
	switch med.Kind {
	case telegram.MediaPhoto, telegram.MediaAnimation:
		return "imv", []string{path}
	case telegram.MediaVideo:
		return "mpv", []string{path}
	case telegram.MediaDocument:
		if strings.HasPrefix(mime, "image/") {
			return "imv", []string{path}
		}
		if strings.HasPrefix(mime, "video/") || strings.HasPrefix(mime, "audio/") {
			return "mpv", []string{path}
		}
	}
	return "xdg-open", []string{path}
}
