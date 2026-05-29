package tui

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// Profiling-Instrumentierung gated über TELEGRIM_PROF=1.
// Output landet in TELEGRIM_PROF_FILE (default /tmp/telegrim-prof.log).
// Format: "<unix_ns> <label> <duration_us>"

var (
	profEnabled bool
	profFile    *os.File
	profMu      sync.Mutex
)

func init() {
	if os.Getenv("TELEGRIM_PROF") != "1" {
		return
	}
	path := os.Getenv("TELEGRIM_PROF_FILE")
	if path == "" {
		path = "/tmp/telegrim-prof.log"
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	profFile = f
	profEnabled = true
	fmt.Fprintf(profFile, "# telegrim profiling start %s\n", time.Now().Format(time.RFC3339))
}

// profTime: `defer profTime("label")()` misst Block-Dauer und schreibt sie ins Profil-File.
// Bei deaktiviertem Profiling ist der Overhead ein Bool-Check + leerer Closure-Call.
func profTime(label string) func() {
	if !profEnabled {
		return func() {}
	}
	start := time.Now()
	return func() {
		d := time.Since(start)
		profMu.Lock()
		fmt.Fprintf(profFile, "%d %s %d\n", start.UnixNano(), label, d.Microseconds())
		profMu.Unlock()
	}
}

// profNote: zusätzliche Info-Zeile (z.B. Cache hit/miss, Größen).
func profNote(label, value string) {
	if !profEnabled {
		return
	}
	profMu.Lock()
	fmt.Fprintf(profFile, "%d %s %s\n", time.Now().UnixNano(), label, value)
	profMu.Unlock()
}
