# HOWTO: Terminal-Login in telegrim (ohne Browser-Wechsel)

Stand: gotd-Backend produktiv testbar.

## Ziel
Login komplett im Terminal durchführen (Phone + Code), ohne während des Login-Flows in den Browser zu wechseln.

## Wichtiger Hinweis
- Für die erste Erstellung von `API ID` und `API Hash` ist Telegram-seitig weiterhin `https://my.telegram.org` nötig.
- Danach läuft der eigentliche Login-Flow in telegrim im Terminal.

## Schritte im TUI
1. telegrim starten:
   - `go run ./cmd/telegrim`
   - optional: `TELEGRIM_BACKEND=gotd`
2. In den Settings-Tab wechseln:
   - `F2` oder auf `[Settings]` klicken
3. Felder ausfüllen:
   - `API ID`
   - `API Hash`
   - `Phone` (z.B. `+491701234567`)
   - `Code` (Telegram Login-Code)
4. Terminal-Login ausführen:
   - `Ctrl+L`
   - oder `Enter` im Settings-Tab
   - oder auf `[Terminal Login]` klicken

## Erwartetes Verhalten
- Bei gültigen Daten zeigt telegrim einen Login-Erfolg im Settings-Status.
- Kein Browser-Wechsel für den Login-Schritt.
- Session wird lokal in `telegrim.session.json` gespeichert (oder via `TELEGRAM_SESSION_FILE`).

## Typische Fehler
- `API ID ist ungültig`: API ID leer/falsch
- `API Hash fehlt`: API Hash leer
- `Telefonnummer fehlt`: Phone leer
- `Login-Code fehlt`: Code leer

## Zusatz
- QR-Login bleibt optional verfügbar (`F3` oder `[QR Login]`).
