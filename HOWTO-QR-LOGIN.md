# HOWTO: QR-Login in telegrim

Stand: gotd-Backend produktiv testbar.

## Ziel
Mit QR-Code im Settings-Tab einloggen.

## Voraussetzungen
1. Telegrim mit gotd-Backend starten (default):
   - `go run ./cmd/telegrim`
   - optional explizit: `TELEGRIM_BACKEND=gotd`
2. Eigene Telegram API-Daten bereithalten:
   - API ID
   - API Hash
   (von https://my.telegram.org)

## Schritte im TUI
1. In den Settings-Tab wechseln:
   - `F2` oder oben auf `[Settings]` klicken.
2. Felder ausfüllen:
   - `API ID`
   - `API Hash`
   - `Phone` / `Code` sind für QR nicht nötig, aber für Terminal-Login nützlich.
3. QR-Login starten:
   - bevorzugt `F3`
   - alternativ `Ctrl+g`
   - oder auf `[QR Login]` klicken
4. QR in Telegram scannen:
   - Telegram App öffnen
   - Einstellungen → Geräte → Gerät verknüpfen
   - QR im Terminal scannen

## Wenn Shortcut nicht geht
Einige Terminal-Setups reichen bestimmte Ctrl-Kombinationen nicht durch.
Nutze dann:
- `F3` (empfohlen)
- oder Maus-Klick auf `[QR Login]`

## Hinweise
- QR-Token ist kurz gültig: bei Fehler einfach neu erzeugen (`F3`).
- Für echtes Scannen muss `TELEGRIM_BACKEND=gotd` aktiv sein.
- Im Mock-Backend sind QR-Codes nur Demo und nicht scanbar.
