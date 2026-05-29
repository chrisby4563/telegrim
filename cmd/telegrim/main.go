package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"telegrim/internal/config"
	"telegrim/internal/telegram"
	"telegrim/internal/tui"
)

// version wird per -ldflags "-X main.version=..." zur Build-Zeit gesetzt.
// Defaultwert für `go run`-Sessions ohne Flag.
var version = "dev"

// crashLog schreibt Panics in eine Datei, damit man sie nach altscreen-Exit
// noch sieht (Stderr im TUI ist meist nicht direkt einsehbar).
func crashLog(payload string) {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		if h, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(h, ".local", "state")
		}
	}
	dir = filepath.Join(dir, "telegrim")
	_ = os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, "crash.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "\n=== %s ===\n%s\n", time.Now().Format(time.RFC3339), payload)
}

func main() {
	// Stderr früh in Datei umlenken: BubbleTea-altscreen verschluckt sonst
	// alle Panic-Stacktraces (sowohl Main-Goroutine als auch Cmd-Goroutines).
	// Die Datei läuft per append, sodass mehrere Runs übereinander loggen.
	if h, err := os.UserHomeDir(); err == nil {
		dir := filepath.Join(h, ".local", "state", "telegrim")
		if err := os.MkdirAll(dir, 0o755); err == nil {
			path := filepath.Join(dir, "stderr.log")
			if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
				fmt.Fprintf(f, "\n--- run start %s ---\n", time.Now().Format(time.RFC3339))
				// Echtes FD-Level-Redirect, damit auch Go-Runtime-Panics
				// (die direkt nach FD 2 schreiben) ins Logfile landen.
				redirectStderr(f)
			}
		}
	}
	defer func() {
		if r := recover(); r != nil {
			payload := fmt.Sprintf("panic: %v\n%s", r, debug.Stack())
			crashLog(payload)
			fmt.Fprintln(os.Stderr, payload)
			os.Exit(2)
		}
	}()
	cfg := config.LoadDefault()

	var primary telegram.Client
	switch cfg.Backend {
	case "mock":
		primary = telegram.NewMockClient()
	case "gotd":
		primary = telegram.NewGotdClient(cfg.APIID, cfg.APIHash, cfg.SessionFile)
	default:
		primary = telegram.NewTDLibClient(cfg.APIID, cfg.APIHash, "")
	}

	var client telegram.Client = primary
	if cfg.EnableWhatsApp {
		wa := telegram.NewWhatsAppClient()
		client = telegram.NewMultiClient(primary, wa)
	}

	// Version in Config schreiben, damit die TUI sie im Header zeigen kann.
	cfg.Version = version
	app := tui.NewModel(cfg, client)

	p := tea.NewProgram(app, tea.WithAltScreen(), tea.WithMouseCellMotion())

	_, err := p.Run()
	// TDLib sauber herunterfahren, damit Session/Binlog konsistent bleiben.
	_ = client.Close()
	if err != nil {
		fmt.Fprintf(os.Stderr, "telegrim crashed: %v\n", err)
		os.Exit(1)
	}
}
