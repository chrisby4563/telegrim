package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	AppName     string
	SessionFile string
	Backend     string

	// Telegram API credentials.
	APIID   int
	APIHash string

	// EnableWhatsApp aktiviert den whatsmeow-Client parallel zum primary-Backend
	// (über MultiClient). Steuert nur das Laden — die Anmeldung erfolgt beim
	// ersten QR-Pairing aus dem TUI heraus.
	EnableWhatsApp bool

	// Version wird vom main-Package per ldflags gesetzt und im Header
	// angezeigt. Default "dev" bei `go run`.
	Version string
}

// credentialsPath liefert den Pfad zur persistenten Credentials-Datei.
// Liegt in ~/.config/telegrim/credentials.json (XDG-konform).
func credentialsPath() string {
	if p := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); p != "" {
		return filepath.Join(p, "telegrim", "credentials.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "telegrim", "credentials.json")
}

type storedCreds struct {
	APIID   int    `json:"api_id"`
	APIHash string `json:"api_hash"`
}

func loadStoredCreds() (int, string) {
	path := credentialsPath()
	if path == "" {
		return 0, ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, ""
	}
	var s storedCreds
	if err := json.Unmarshal(data, &s); err != nil {
		return 0, ""
	}
	return s.APIID, strings.TrimSpace(s.APIHash)
}

// SaveCredentials persistiert API ID/Hash, damit sie nach Neustart verfügbar sind.
func SaveCredentials(apiID int, apiHash string) error {
	path := credentialsPath()
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(storedCreds{APIID: apiID, APIHash: strings.TrimSpace(apiHash)}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func LoadDefault() Config {
	apiID, _ := strconv.Atoi(strings.TrimSpace(os.Getenv("TELEGRAM_API_ID")))
	apiHash := strings.TrimSpace(os.Getenv("TELEGRAM_API_HASH"))
	if apiID <= 0 || apiHash == "" {
		fileID, fileHash := loadStoredCreds()
		if apiID <= 0 {
			apiID = fileID
		}
		if apiHash == "" {
			apiHash = fileHash
		}
	}
	sessionFile := strings.TrimSpace(os.Getenv("TELEGRAM_SESSION_FILE"))
	if sessionFile == "" {
		sessionFile = "telegrim.session.json"
	}
	backend := strings.TrimSpace(strings.ToLower(os.Getenv("TELEGRIM_BACKEND")))
	if backend == "" {
		backend = "tdlib"
	}
	if backend != "gotd" && backend != "mock" && backend != "tdlib" {
		backend = "tdlib"
	}

	enableWA := envFlag("TELEGRIM_WHATSAPP")

	return Config{
		AppName:        "telegrim",
		SessionFile:    sessionFile,
		Backend:        backend,
		APIID:          apiID,
		APIHash:        apiHash,
		EnableWhatsApp: enableWA,
	}
}

// envFlag liest eine bool-artige Env-Variable. Akzeptiert 1/true/yes/on.
func envFlag(key string) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch v {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
