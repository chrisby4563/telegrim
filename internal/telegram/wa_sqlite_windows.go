//go:build windows

package telegram

import _ "modernc.org/sqlite"

// waSQLiteDriver = pure-Go SQLite-Driver (modernc.org/sqlite). Auf Windows
// kein CGo → kein MinGW-Build-Setup nötig.
const waSQLiteDriver = "sqlite"

func waSQLiteDSN(dbPath string) string {
	// modernc.org/sqlite DSN: einfacher Path mit ?_pragma=...
	return dbPath + "?_pragma=foreign_keys(1)"
}
