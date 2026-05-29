# telegrim

Leichtgewichtiges Telegram-TUI-Grundgerüst in Go.

Stack:

- Go
- Bubble Tea für TUI-State und Eventloop
- Lip Gloss für Layout/Styling
- TDLib (offizielle Telegram-C-Lib) als Primärbackend, gotd/td als pure-Go-Fallback

Backends (`TELEGRIM_BACKEND`):
- `tdlib` (default): offizielle Telegram-Lib via [zelenin/go-tdlib](https://github.com/zelenin/go-tdlib). Auth-State-Machine handled SMS-Code + 2FA. Braucht `telegram-tdlib` (AUR) o.ä.
- `gotd`: pure-Go MTProto via [gotd/td](https://github.com/gotd/td). Aktuell nur teilweise funktional.
- `mock`: Demo ohne echte Telegram-Verbindung.

How-to für QR-Login im TUI:
- siehe `HOWTO-QR-LOGIN.md` (nur gotd, bei TDLib aktuell deaktiviert)

How-to für Terminal-Login ohne Browser-Wechsel:
- siehe `HOWTO-TERMINAL-LOGIN.md`

## Voraussetzungen (TDLib-Backend)

Arch:

```bash
yay -S telegram-tdlib
# Verifizieren:
pkg-config --modversion tdjson   # erwartet ≥ 1.8
```

Alternativ TDLib selbst kompilieren (siehe https://tdlib.github.io/td/build.html).

## Start

```bash
go mod tidy
# Optional: explizit Backend wählen (default ist tdlib)
export TELEGRIM_BACKEND=tdlib   # oder gotd / mock
# API-Daten können per UI (Settings) ODER per Env gesetzt werden:
# export TELEGRAM_API_ID=123456
# export TELEGRAM_API_HASH=abcd...
go run -tags libtdjson ./cmd/telegrim
```

Build-Tag `libtdjson` linkt gegen die dynamische libtdjson.so (kleineres Binary).
Ohne Tag würde gegen die statische Variante gelinkt – das geht auch, braucht aber
zusätzliche Libs aus dem TDLib-Source-Build.

Session-Daten: `~/.local/share/telegrim/tdlib/` (DB + Files).

## Bedienung

2-Spalten-Layout:
- links: Kontakte
- rechts: Chatverlauf + Eingabe

Mouse:
- Kontakt anklicken → Chat laden
- Scrollrad im Kontaktbereich → Auswahl hoch/runter
- `[Send]` anklicken → Nachricht senden
- `[X]` oben rechts anklicken → beenden

Tastatur:

| Taste | Aktion |
|------|--------|
| ↑ / k | Kontakt hoch (oder im Settings-Tab Feld hoch) |
| ↓ / j | Kontakt runter (oder im Settings-Tab Feld runter) |
| Tab / Shift+Tab | Chats-Tab: Fokus Kontakte↔Chat, Settings-Tab: nächstes/voriges Feld |
| Enter | Chats: öffnen/senden, Settings: Terminal Login |
| Ctrl+s | Nachricht senden |
| r / Ctrl+r | Refresh (Chats + aktueller Chat) |
| F1 | Chats-Tab |
| F2 | Settings-Tab |
| F3 | QR-Login starten |
| Ctrl+g | QR-Login starten (falls Terminal es durchreicht) |
| Ctrl+l | Terminal-Login (Phone+Code, ohne Browser) |
| Esc | Fokus zurück auf Kontakte bzw. zurück zu Chats |
| q / Ctrl+c | Beenden |

## Login-Flow (TDLib)

1. Settings öffnen (F2)
2. API ID + API Hash + Telefonnummer eintragen → `[Terminal Login]`
3. Telegram schickt Code per SMS/App → Code-Feld füllen → `[Terminal Login]` erneut
4. Falls 2FA aktiv: Passwort-Feld füllen → `[Terminal Login]` erneut
5. Status meldet „Eingeloggt (TDLib)“, Chats werden geladen

Session bleibt persistent in `~/.local/share/telegrim/tdlib/` – beim nächsten Start
ist der Login wieder aktiv.

## Nächste Baustellen

1. TDLib-Update-Listener anbinden (live neue Nachrichten in der UI).
2. QR-Login für TDLib (eigener `qrAuthorizer`).
3. WhatsApp-Backend via [whatsmeow](https://github.com/tulir/whatsmeow) als zweite
   Account-Quelle, gemeinsame Chat-Liste mit `{platform}:{id}`-IDs.
4. Sender-Namen aus User-Cache statt „User <id>“ auflösen.
5. Paging der Chat-Historie (mehr als 50 Nachrichten).
