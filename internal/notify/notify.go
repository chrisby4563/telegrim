// Package notify schickt Desktop-Benachrichtigungen.
// Plattform-spezifisch: Linux/BSD = notify-send (libnotify), Windows =
// PowerShell-Toast. Schaltet sich ab via TELEGRIM_NOTIFY=0/false oder wenn
// das Backend-Binary nicht gefunden wird (silent skip).
package notify

import (
	"os"
	"strings"
	"sync"
	"sync/atomic"
)

var (
	once     sync.Once
	enabled  atomic.Bool
	binaryOK atomic.Bool
)

func initOnce() {
	once.Do(func() {
		v := strings.ToLower(strings.TrimSpace(os.Getenv("TELEGRIM_NOTIFY")))
		on := !(v == "0" || v == "false" || v == "no" || v == "off")
		enabled.Store(on)
		binaryOK.Store(probeNotifyBackend())
	})
}

// Send schickt eine Notification. Title ≤ 60 Chars, Body kürzt sich auf 200.
// Non-blocking — feuert Goroutine ab, wartet maximal 3s auf Backend-Process.
func Send(title, body string) {
	initOnce()
	if !enabled.Load() || !binaryOK.Load() {
		return
	}
	title = clip(title, 60)
	body = clip(body, 200)
	go sendNotification(title, body)
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len([]rune(s)) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}
