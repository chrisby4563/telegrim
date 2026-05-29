//go:build windows

package notify

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

func probeNotifyBackend() bool {
	// PowerShell ist auf jedem modernen Windows vorhanden. Wir prüfen
	// trotzdem damit Send() bei abgespeckten Setups (Server Core etc) silent
	// skipt statt zu crashen.
	_, err := exec.LookPath("powershell.exe")
	return err == nil
}

// psEscape encoded einen String für eingebettete PowerShell-Argumente. Single-
// Quote-Strings in PS escapen interne Single-Quotes durch Verdopplung.
func psEscape(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// sendNotification feuert eine Windows-Toast-Notification via PowerShell.
// Nutzt die Windows.UI.Notifications API — kein extra Go-Package nötig.
// Skript läuft als One-Liner; bei Fehler einfach silent skip (siehe _ = Run()).
func sendNotification(title, body string) {
	script := "" +
		"[Windows.UI.Notifications.ToastNotificationManager,Windows.UI.Notifications,ContentType=WindowsRuntime] > $null;" +
		"$xml = [Windows.Data.Xml.Dom.XmlDocument]::new();" +
		"$xml.LoadXml('<toast><visual><binding template=\"ToastGeneric\"><text>" + psEscape(title) +
		"</text><text>" + psEscape(body) + "</text></binding></visual></toast>');" +
		"$toast = [Windows.UI.Notifications.ToastNotification]::new($xml);" +
		"[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier('telegrim').Show($toast);"

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx,
		"powershell.exe",
		"-NoProfile", "-NonInteractive",
		"-WindowStyle", "Hidden",
		"-Command", script,
	).Run()
}
