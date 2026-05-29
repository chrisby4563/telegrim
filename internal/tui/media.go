package tui

import (
	"fmt"
	"strings"

	"telegrim/internal/telegram"
)

// formatMediaLines baut 1–2 Zeilen für eine Media-Bubble. Zeile 1: Icon + Name/
// Dimension + Größe. Optional Zeile 2 für Dauer/MIME bei Video/Doc, wenn Platz.
// Bricht hart auf textWidth.
func formatMediaLines(m *telegram.Media, textWidth int) []string {
	if m == nil || m.Kind == telegram.MediaNone {
		return nil
	}
	parts := []string{m.Kind.Icon() + " " + m.Kind.Label()}

	if name := strings.TrimSpace(m.FileName); name != "" {
		parts = append(parts, name)
	} else if m.Width > 0 && m.Height > 0 {
		parts = append(parts, fmt.Sprintf("%d×%d", m.Width, m.Height))
	}
	if m.Duration > 0 {
		parts = append(parts, formatDuration(int(m.Duration)))
	}
	if m.Size > 0 {
		parts = append(parts, formatBytes(m.Size))
	}
	if m.LocalPath == "" && m.Kind != telegram.MediaNone {
		parts = append(parts, "↓")
	} else if m.LocalPath != "" {
		parts = append(parts, "✓")
	}

	line := strings.Join(parts, " · ")
	if textWidth > 0 && len(line) > textWidth {
		line = truncateLine(line, textWidth)
	}
	return []string{line}
}

func formatBytes(n int64) string {
	const k = 1024
	switch {
	case n < k:
		return fmt.Sprintf("%dB", n)
	case n < k*k:
		return fmt.Sprintf("%.1fKB", float64(n)/k)
	case n < k*k*k:
		return fmt.Sprintf("%.1fMB", float64(n)/(k*k))
	default:
		return fmt.Sprintf("%.1fGB", float64(n)/(k*k*k))
	}
}

func formatDuration(sec int) string {
	if sec < 60 {
		return fmt.Sprintf("0:%02d", sec)
	}
	m := sec / 60
	s := sec % 60
	if m < 60 {
		return fmt.Sprintf("%d:%02d", m, s)
	}
	h := m / 60
	m = m % 60
	return fmt.Sprintf("%d:%02d:%02d", h, m, s)
}
