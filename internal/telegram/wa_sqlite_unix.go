//go:build !windows

package telegram

import _ "github.com/mattn/go-sqlite3"

// waSQLiteDriver = SQLite-Driver-Name (CGo via mattn/go-sqlite3).
const waSQLiteDriver = "sqlite3"

func waSQLiteDSN(dbPath string) string {
	return "file:" + dbPath + "?_foreign_keys=on"
}
