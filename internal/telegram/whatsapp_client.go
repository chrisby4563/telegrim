package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"telegrim/internal/notify"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	waHistorySync "go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// WhatsAppClient ist ein Backend auf Basis von whatsmeow. Implementiert das
// Client-Interface, sodass es transparent neben TDLib im MultiClient hängt.
//
// Eigenheiten:
//   - chatID ist int64, intern aus FNV-1a Hash der JID-Strings generiert (stabil
//     über Prozess-Neustarts, da JIDs deterministisch sind). Reverse-Lookup via
//     jidByChatID-Map.
//   - Login: nur QR-Pairing. Login/LoginWithCode liefern Fehler.
//   - Folders: nur Pseudo-"Alle" (ID 0). WhatsApp hat keine User-Folder.
//   - Backwards-Paging: nicht implementiert (Phase 1 MVP).
//   - Media: nicht implementiert (Phase 2).
type WhatsAppClient struct {
	dataDir string

	mu        sync.RWMutex
	container *sqlstore.Container
	device    interface{} // *store.Device, untyped to avoid import cycle weirdness
	client    *whatsmeow.Client

	// chatsMu schützt nur chatsByID + jidByChatID (Chat-Meta).
	chatsMu     sync.RWMutex
	chatsByID   map[int64]*waChat
	jidByChatID map[int64]types.JID

	// messagesMu schützt messagesByChat separat. Lese-/Schreib-Pfade
	// auf Messages blockieren so nicht das ListChats-Rendering.
	// Lock-Order bei Operationen die beides brauchen: chatsMu → messagesMu.
	messagesMu     sync.RWMutex
	messagesByChat map[int64][]Message

	nextMsgSeq atomic.Int64
	chatsDirty atomic.Bool // gepuffert für persistChats-Flush

	// Presence-Tracking: JID-User → online. Wird durch SubscribePresence
	// aktiviert (einmal pro JID) und durch *events.Presence aktualisiert.
	presenceMu      sync.RWMutex
	onlineByJID     map[string]bool
	subscribedJIDs  map[string]struct{}

	// Media-Buffer: FileID → DownloadableMessage. DownloadFile schaut hier
	// nach, ruft client.Download, schreibt auf Disk und liefert den Pfad.
	mediaMu     sync.RWMutex
	mediaByFile map[int32]waMediaRef
	nextFileID  atomic.Int32
	mediaPaths  map[int32]string // cache: bereits heruntergeladen

	// contactNames cached Adressbuch-Namen JID→Display-Name. Befüllt durch
	// ensureContactsLoaded (synchron beim ListChats-Call). Lookups aus Event-
	// Handlern blockieren so nicht den whatsmeow-Event-Loop.
	contactNamesMu sync.RWMutex
	contactNames   map[string]string

	// contactsRefresh kontrolliert wie oft ensureContactsLoaded den Network-
	// Call GetJoinedGroups + GetAllContacts macht. Cache 30s; sonst hängt
	// ListChats bei schlechter Verbindung.
	contactsLastRefresh atomic.Int64 // unix-ts der letzten erfolgreichen Refresh
	contactsRefreshing  atomic.Bool  // läuft gerade ein Refresh? (verhindert pile-up)

	qrMu      sync.Mutex
	qrLatest  string
	qrLastErr string
	qrReady   chan struct{}
	qrCancel  context.CancelFunc
	loggedIn  atomic.Bool

	flushStop chan struct{} // schließt flushLoop bei Close
}

type waMediaRef struct {
	msg  whatsmeow.DownloadableMessage
	kind MediaKind
	name string
	mime string
}

type waChat struct {
	jid      types.JID
	title    string
	last     string
	lastDate int32
	unread   int
	isGroup  bool
	// readUpTo = Unix-Timestamp bis zu dem der User in telegrim gelesen hat.
	// applyConversation überschreibt unread nur dann, wenn neue Messages
	// danach kamen — sonst würde ein verzögerter HistorySync ungelesene
	// wieder herstellen, die der User bereits weggeklickt hat.
	readUpTo int32
}

var _ Client = (*WhatsAppClient)(nil)

// NewWhatsAppClient erzeugt einen ungestarteten WhatsApp-Client. Connect muss
// vor Login/ListChats etc. aufgerufen werden.
func NewWhatsAppClient() *WhatsAppClient {
	return &WhatsAppClient{
		chatsByID:      make(map[int64]*waChat),
		jidByChatID:    make(map[int64]types.JID),
		messagesByChat: make(map[int64][]Message),
		contactNames:   make(map[string]string),
		mediaByFile:    make(map[int32]waMediaRef),
		mediaPaths:     make(map[int32]string),
		onlineByJID:    make(map[string]bool),
		subscribedJIDs: make(map[string]struct{}),
	}
}

// lookupContactName liefert den gecachten Adressbuch-Namen für eine JID oder "".
// Lock-frei lesbar aus Event-Handlern.
func (c *WhatsAppClient) lookupContactName(jid types.JID) string {
	c.contactNamesMu.RLock()
	defer c.contactNamesMu.RUnlock()
	return c.contactNames[jid.ToNonAD().String()]
}

// persistedChat ist die On-Disk-Repräsentation eines WA-Chats. Bewusst minimal
// (keine Message-Bodies — Privacy + Größe). Wird verwendet damit Chat-Liste
// Restart überlebt; whatsmeow sendet HistorySync nicht erneut nach Reconnect.
type persistedChat struct {
	JID      string `json:"jid"`
	Title    string `json:"title"`
	Last     string `json:"last,omitempty"`
	LastDate int32  `json:"last_date,omitempty"`
	Unread   int    `json:"unread,omitempty"`
	IsGroup  bool   `json:"group,omitempty"`
	ReadUpTo int32  `json:"read_up_to,omitempty"`
}

func (c *WhatsAppClient) chatsFile() string {
	return filepath.Join(waDataDir(), "chats.json")
}

func (c *WhatsAppClient) messagesFile() string {
	return filepath.Join(waDataDir(), "messages.json")
}

// waMessagesCap = max persistierte Messages pro Chat. Ältere fallen beim
// Flush hinten raus, damit messages.json nicht ins Megabyte-Land wandert.
const waMessagesCap = 200

// messageDup prüft ob eine Message bereits in der Slice ist. Priorität:
// ExternalID-Match (whatsmeow MessageKey.ID — eindeutig). Fallback für legacy
// persistierte Messages (vor ExternalID-Feld): Date+Outgoing+Text-Fingerprint.
func messageDup(slice []Message, extID string, date int32, outgoing bool, text string) bool {
	if extID != "" {
		for _, m := range slice {
			if m.ExternalID == extID {
				return true
			}
		}
	}
	// Fallback-Fingerprint nur sinnvoll wenn beide Seiten keine ExternalID
	// haben — sonst würden wir gegen die ExternalID-Branch oben doppelt
	// matchen. Hilft bei pre-restart persistierten Messages.
	for _, m := range slice {
		if m.Date == date && m.Outgoing == outgoing && m.Text == text {
			return true
		}
	}
	return false
}

// dedupeMessages entfernt Duplikate aus einer Message-Slice in-place. Behält
// jeweils den ersten Treffer pro Fingerprint, sortiert nicht um. Einmalig beim
// loadPersistedMessages aufgerufen, um Bestands-Dupes aufzuräumen.
func dedupeMessages(msgs []Message) []Message {
	if len(msgs) < 2 {
		return msgs
	}
	seenExt := make(map[string]struct{}, len(msgs))
	seenFP := make(map[string]struct{}, len(msgs))
	out := msgs[:0]
	for _, m := range msgs {
		if m.ExternalID != "" {
			if _, dup := seenExt[m.ExternalID]; dup {
				continue
			}
			seenExt[m.ExternalID] = struct{}{}
		} else {
			key := fmt.Sprintf("%d|%t|%s", m.Date, m.Outgoing, m.Text)
			if _, dup := seenFP[key]; dup {
				continue
			}
			seenFP[key] = struct{}{}
		}
		out = append(out, m)
	}
	return out
}

// loadPersistedMessages hydratisiert messagesByChat aus messages.json. Stille
// Failures bei fehlender Datei.
func (c *WhatsAppClient) loadPersistedMessages() {
	data, err := os.ReadFile(c.messagesFile())
	if err != nil {
		return
	}
	var stored map[int64][]Message
	if err := json.Unmarshal(data, &stored); err != nil {
		return
	}
	c.messagesMu.Lock()
	defer c.messagesMu.Unlock()
	maxID := int64(0)
	for chatID, msgs := range stored {
		if _, has := c.messagesByChat[chatID]; has {
			continue
		}
		for i := range msgs {
			if msgs[i].Platform == "" {
				msgs[i].Platform = PlatformWA
			}
			if msgs[i].ID > maxID {
				maxID = msgs[i].ID
			}
		}
		msgs = dedupeMessages(msgs)
		c.messagesByChat[chatID] = msgs
	}
	if maxID > 0 {
		c.nextMsgSeq.Store(maxID)
	}
}

// flushMessages schreibt messagesByChat atomar nach messages.json. Begrenzt
// pro Chat auf waMessagesCap (neueste behalten).
func (c *WhatsAppClient) flushMessages() {
	c.messagesMu.RLock()
	stored := make(map[int64][]Message, len(c.messagesByChat))
	for chatID, msgs := range c.messagesByChat {
		if len(msgs) == 0 {
			continue
		}
		if len(msgs) > waMessagesCap {
			msgs = msgs[len(msgs)-waMessagesCap:]
		}
		cp := make([]Message, len(msgs))
		copy(cp, msgs)
		stored[chatID] = cp
	}
	c.messagesMu.RUnlock()

	path := c.messagesFile()
	tmp := path + ".tmp"
	data, err := json.Marshal(stored)
	if err != nil {
		return
	}
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// loadPersistedChats hydratisiert chatsByID aus chats.json. Schweigt bei
// fehlender Datei (Erststart) oder Parse-Fehlern.
func (c *WhatsAppClient) loadPersistedChats() {
	data, err := os.ReadFile(c.chatsFile())
	if err != nil {
		return
	}
	var list []persistedChat
	if err := json.Unmarshal(data, &list); err != nil {
		return
	}
	c.chatsMu.Lock()
	defer c.chatsMu.Unlock()
	for _, p := range list {
		jid, err := types.ParseJID(p.JID)
		if err != nil {
			continue
		}
		jid = jid.ToNonAD()
		id := hashJID(jid)
		if _, ok := c.chatsByID[id]; ok {
			continue
		}
		c.chatsByID[id] = &waChat{
			jid:      jid,
			title:    p.Title,
			last:     p.Last,
			lastDate: p.LastDate,
			unread:   p.Unread,
			isGroup:  p.IsGroup,
			readUpTo: p.ReadUpTo,
		}
		c.jidByChatID[id] = jid
	}
}

// flushChats schreibt chatsByID atomar nach chats.json. Schreibt erst in .tmp
// und ersetzt dann (rename ist atomar auf POSIX).
func (c *WhatsAppClient) flushChats() {
	if !c.chatsDirty.CompareAndSwap(true, false) {
		return
	}
	c.chatsMu.RLock()
	list := make([]persistedChat, 0, len(c.chatsByID))
	for _, ch := range c.chatsByID {
		list = append(list, persistedChat{
			JID:      ch.jid.String(),
			Title:    ch.title,
			Last:     ch.last,
			LastDate: ch.lastDate,
			Unread:   ch.unread,
			IsGroup:  ch.isGroup,
			ReadUpTo: ch.readUpTo,
		})
	}
	c.chatsMu.RUnlock()

	path := c.chatsFile()
	tmp := path + ".tmp"
	data, err := json.Marshal(list)
	if err != nil {
		c.chatsDirty.Store(true)
		return
	}
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		c.chatsDirty.Store(true)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		c.chatsDirty.Store(true)
	}
}

// flushLoop läuft als Goroutine während Connect bis Close. Pollt alle 10s ob
// die dirty-Flag gesetzt ist und flushed dann. Kein Tick = kein Disk-IO.
// Messages werden zusammen mit chats geflushed — beide hängen am gleichen
// chatsDirty-Flag.
func (c *WhatsAppClient) flushLoop(stop <-chan struct{}) {
	defer func() { _ = recover() }()
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			c.flushChats()
			c.flushMessages()
			return
		case <-t.C:
			if c.chatsDirty.Load() {
				c.flushChats()
				c.flushMessages()
			}
		}
	}
}

// fileLogger schreibt waLog-Logger-Ausgabe in eine io.Writer-Senke (Logfile).
// Kein Lock — *os.File ist thread-safe via OS bei kleinen Writes.
type fileLogger struct {
	w   interface{ Write([]byte) (int, error) }
	mod string
	min int
}

var waLogLevels = map[string]int{
	"":      0,
	"DEBUG": 0,
	"INFO":  1,
	"WARN":  2,
	"ERROR": 3,
}

func newFileLogger(w interface{ Write([]byte) (int, error) }, module, minLevel string) waLog.Logger {
	return &fileLogger{w: w, mod: module, min: waLogLevels[strings.ToUpper(minLevel)]}
}

func (l *fileLogger) write(level string, levelN int, msg string, args ...interface{}) {
	if levelN < l.min {
		return
	}
	ts := time.Now().Format("15:04:05.000")
	line := fmt.Sprintf("%s [%s %s] %s\n", ts, l.mod, level, fmt.Sprintf(msg, args...))
	_, _ = l.w.Write([]byte(line))
}

func (l *fileLogger) Debugf(msg string, args ...interface{}) { l.write("DEBUG", 0, msg, args...) }
func (l *fileLogger) Infof(msg string, args ...interface{})  { l.write("INFO", 1, msg, args...) }
func (l *fileLogger) Warnf(msg string, args ...interface{})  { l.write("WARN", 2, msg, args...) }
func (l *fileLogger) Errorf(msg string, args ...interface{}) { l.write("ERROR", 3, msg, args...) }
func (l *fileLogger) Sub(module string) waLog.Logger {
	return &fileLogger{w: l.w, mod: l.mod + "/" + module, min: l.min}
}

func waDataDir() string {
	if p := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); p != "" {
		return filepath.Join(p, "telegrim", "whatsapp")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "telegrim", "whatsapp")
}

// hashJID stabilisiert die JID-String-Repräsentation auf int64. Vorzeichen wird
// negiert, damit es nicht mit TDLib-IDs (oft positiv) kollidiert.
func hashJID(jid types.JID) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(jid.ToNonAD().String()))
	v := int64(h.Sum64())
	if v > 0 {
		v = -v
	}
	if v == 0 {
		v = -1
	}
	return v
}

func (c *WhatsAppClient) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.client != nil {
		return nil
	}

	dir := waDataDir()
	if dir == "" {
		return fmt.Errorf("whatsapp: keine data-dir bestimmbar")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("whatsapp: data-dir anlegen: %w", err)
	}
	dbPath := filepath.Join(dir, "store.db")

	// waLog.Stdout schreibt via fmt.Printf nach os.Stdout — das würde die TUI-
	// Rendering im altscreen zerschießen. Stattdessen: eigener Logger der
	// in eine Datei schreibt. Default: Noop (keine Logs).
	level := "WARN"
	debug := false
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("TELEGRIM_WA_DEBUG"))); v == "1" || v == "true" {
		level = "DEBUG"
		debug = true
	}
	logPath := strings.TrimSpace(os.Getenv("TELEGRIM_WA_LOG"))
	if logPath == "" {
		logPath = filepath.Join(dir, "whatsapp.log")
	}
	var dbLog, cliLog waLog.Logger = waLog.Noop, waLog.Noop
	if debug || os.Getenv("TELEGRIM_WA_LOG") != "" {
		if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			dbLog = newFileLogger(f, "wa-db", level)
			cliLog = newFileLogger(f, "wa", level)
		}
	}
	dsn := waSQLiteDSN(dbPath)
	container, err := sqlstore.New(ctx, waSQLiteDriver, dsn, dbLog)
	if err != nil {
		return fmt.Errorf("whatsapp: sqlstore: %w", err)
	}
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		return fmt.Errorf("whatsapp: device: %w", err)
	}

	client := whatsmeow.NewClient(device, cliLog)
	client.AddEventHandler(c.handleEvent)

	c.container = container
	c.device = device
	c.client = client

	// Persistente Chat-Metadata + Messages laden, damit Liste + Verlauf auch
	// ohne erneuten HistorySync (whatsmeow sendet das nur einmal pro Pairing)
	// Restarts überleben.
	c.loadPersistedChats()
	c.loadPersistedMessages()

	c.flushStop = make(chan struct{})
	go c.flushLoop(c.flushStop)

	if client.Store.ID != nil {
		c.loggedIn.Store(true)
		if err := client.Connect(); err != nil {
			return fmt.Errorf("whatsapp: connect: %w", err)
		}
	}
	return nil
}

// Login wird für WhatsApp nicht verwendet — Anmeldung läuft ausschließlich über
// StartQRLogin. Wir geben hier nil zurück, damit der bestehende Login-Flow im
// TUI (Phone/Code) den Aufruf ignorieren kann, wenn Backend == WhatsApp.
func (c *WhatsAppClient) Login(ctx context.Context, auth AuthSettings) error {
	return nil
}

func (c *WhatsAppClient) LoginWithCode(ctx context.Context, auth AuthSettings) error {
	return fmt.Errorf("whatsapp: Phone/Code-Login nicht unterstützt — QR-Login nutzen")
}

// StartQRLogin initiiert das QR-Pairing. Blockiert bis der erste QR-Code
// vorliegt (oder Fehler). Liefert den rohen QR-Inhalt zurück, den der Caller
// per renderASCIIQR in ASCII-Art wandelt. Bei mehrfachem Aufruf wird ein
// laufender Flow weiterverwendet.
func (c *WhatsAppClient) StartQRLogin(ctx context.Context) (string, error) {
	c.mu.RLock()
	client := c.client
	c.mu.RUnlock()
	if client == nil {
		return "", fmt.Errorf("whatsapp: Connect zuerst aufrufen")
	}
	if client.Store.ID != nil {
		c.loggedIn.Store(true)
		return "", fmt.Errorf("whatsapp: bereits angemeldet")
	}

	c.qrMu.Lock()
	if c.qrLatest != "" {
		code := c.qrLatest
		c.qrMu.Unlock()
		return code, nil
	}
	if c.qrReady == nil {
		c.qrReady = make(chan struct{})
		flowCtx, cancel := context.WithCancel(context.Background())
		c.qrCancel = cancel
		qrChan, err := client.GetQRChannel(flowCtx)
		if err != nil {
			c.qrMu.Unlock()
			cancel()
			return "", fmt.Errorf("whatsapp: QR-Kanal: %w", err)
		}
		if err := client.Connect(); err != nil {
			c.qrMu.Unlock()
			cancel()
			return "", fmt.Errorf("whatsapp: connect: %w", err)
		}
		go c.consumeQR(qrChan)
	}
	ready := c.qrReady
	c.qrMu.Unlock()

	select {
	case <-ready:
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(15 * time.Second):
		return "", fmt.Errorf("whatsapp: QR-Code nicht rechtzeitig erhalten")
	}

	c.qrMu.Lock()
	code := c.qrLatest
	lastErr := c.qrLastErr
	c.qrMu.Unlock()
	if code == "" {
		if lastErr != "" {
			return "", fmt.Errorf("whatsapp: %s", lastErr)
		}
		return "", fmt.Errorf("whatsapp: kein QR-Code")
	}
	return code, nil
}

func (c *WhatsAppClient) consumeQR(qrChan <-chan whatsmeow.QRChannelItem) {
	defer func() { _ = recover() }()
	for evt := range qrChan {
		switch evt.Event {
		case "code":
			c.qrMu.Lock()
			c.qrLatest = evt.Code
			c.qrLastErr = ""
			if c.qrReady != nil {
				select {
				case <-c.qrReady:
				default:
					close(c.qrReady)
				}
			}
			c.qrMu.Unlock()
		case "success":
			c.loggedIn.Store(true)
			c.qrMu.Lock()
			c.qrLatest = ""
			c.qrLastErr = ""
			c.qrMu.Unlock()
			return
		default:
			// timeout, err-client-outdated, err-scanned-without-multidevice,
			// err-unexpected-state etc. → in qrLastErr stashen + signalisieren.
			c.qrMu.Lock()
			c.qrLastErr = evt.Event
			if evt.Error != nil {
				c.qrLastErr = evt.Event + ": " + evt.Error.Error()
			}
			if c.qrReady != nil {
				select {
				case <-c.qrReady:
				default:
					close(c.qrReady)
				}
			}
			c.qrMu.Unlock()
		}
	}
}

func (c *WhatsAppClient) handleEvent(evt interface{}) {
	switch v := evt.(type) {
	case *events.Message:
		c.onMessage(v)
	case *events.HistorySync:
		c.onHistorySync(v)
	case *events.Receipt:
		c.onReceipt(v)
	case *events.Presence:
		c.onPresence(v)
	case *events.Connected:
		c.loggedIn.Store(true)
		// Eigene Verfügbarkeit melden — WhatsApp sendet sonst keine
		// Presence-Updates anderer User an uns.
		c.sendOwnPresence()
	case *events.LoggedOut:
		c.loggedIn.Store(false)
	}
}

func (c *WhatsAppClient) onPresence(e *events.Presence) {
	if e == nil {
		return
	}
	key := e.From.ToNonAD().String()
	c.presenceMu.Lock()
	c.onlineByJID[key] = !e.Unavailable
	c.presenceMu.Unlock()
}

func (c *WhatsAppClient) sendOwnPresence() {
	c.mu.RLock()
	client := c.client
	c.mu.RUnlock()
	if client == nil {
		return
	}
	go func() {
		defer func() { _ = recover() }()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = client.SendPresence(ctx, types.PresenceAvailable)
	}()
}

// onReceipt updated den Lese-/Liefer-Status unserer ausgehenden Messages
// anhand der vom Empfänger zurückgesendeten Read/Delivered-Receipts.
func (c *WhatsAppClient) onReceipt(e *events.Receipt) {
	if e == nil || len(e.MessageIDs) == 0 {
		return
	}
	var newStatus MessageStatus
	switch e.Type {
	case types.ReceiptTypeRead, types.ReceiptTypeReadSelf:
		newStatus = MessageStatusRead
	case types.ReceiptTypeDelivered:
		newStatus = MessageStatusDelivered
	default:
		return // sender/retry/etc ignorieren
	}

	chatJID := e.Chat.ToNonAD()
	chatID := hashJID(chatJID)

	idSet := make(map[string]struct{}, len(e.MessageIDs))
	for _, id := range e.MessageIDs {
		idSet[string(id)] = struct{}{}
	}

	c.messagesMu.Lock()
	defer c.messagesMu.Unlock()
	msgs := c.messagesByChat[chatID]
	changed := false
	for i := range msgs {
		if !msgs[i].Outgoing {
			continue
		}
		if _, hit := idSet[msgs[i].ExternalID]; !hit {
			continue
		}
		if msgs[i].Status < newStatus {
			msgs[i].Status = newStatus
			changed = true
		}
	}
	if changed {
		c.chatsDirty.Store(true)
	}
}

// onHistorySync verarbeitet History-Blobs vom Handy nach Pairing oder
// Reconnect. Pro Conversation: Chat-Metadata füllen + die letzten paar
// Nachrichten in messagesByChat hängen.
func (c *WhatsAppClient) onHistorySync(e *events.HistorySync) {
	if e == nil || e.Data == nil {
		return
	}
	// Pushnames füllen den Name-Cache vorab — falls weder Adressbuch noch
	// Conv.Name greifen, kann onMessage so wenigstens den WA-Profilnamen
	// zeigen statt blanker Telefonnummer.
	if pns := e.Data.GetPushnames(); len(pns) > 0 {
		c.contactNamesMu.Lock()
		for _, pn := range pns {
			id := strings.TrimSpace(pn.GetID())
			name := strings.TrimSpace(pn.GetPushname())
			if id == "" || name == "" {
				continue
			}
			jid, err := types.ParseJID(id)
			if err != nil {
				continue
			}
			key := jid.ToNonAD().String()
			if _, exists := c.contactNames[key]; !exists {
				// Pushname nur setzen wenn noch kein FullName aus Adressbuch da
				c.contactNames[key] = name
			}
		}
		c.contactNamesMu.Unlock()
	}
	for _, conv := range e.Data.GetConversations() {
		c.applyConversation(conv)
	}
}

func (c *WhatsAppClient) applyConversation(conv *waHistorySync.Conversation) {
	if conv == nil {
		return
	}
	jidStr := conv.GetID()
	jid, err := types.ParseJID(jidStr)
	if err != nil || jid.IsEmpty() {
		return
	}
	jid = jid.ToNonAD()
	if jid.Server != types.DefaultUserServer && jid.Server != types.GroupServer {
		return // Newsletter/Broadcast etc. ignorieren
	}
	chatID := hashJID(jid)

	// Contact-Lookup VOR chatsMu — Lock-Order siehe onMessage.
	convName := strings.TrimSpace(conv.GetName())
	var contactName string
	if convName == "" && jid.Server == types.DefaultUserServer {
		contactName = c.lookupContactName(jid)
	}

	// Lock-Order: chatsMu → messagesMu (überall konsistent).
	c.chatsMu.Lock()
	defer c.chatsMu.Unlock()
	c.messagesMu.Lock()
	defer c.messagesMu.Unlock()

	ch, ok := c.chatsByID[chatID]
	if !ok {
		ch = &waChat{jid: jid, isGroup: jid.Server == types.GroupServer}
		c.chatsByID[chatID] = ch
		c.jidByChatID[chatID] = jid
	}
	if convName != "" {
		ch.title = convName
	} else if contactName != "" {
		if ch.title == "" || isPhoneFallback(ch.title, jid) {
			ch.title = contactName
		}
	} else if ch.title == "" {
		ch.title = defaultTitleFor(jid, "")
	}
	if ts := conv.GetLastMsgTimestamp(); ts > 0 {
		ch.lastDate = int32(ts)
	} else if ts := conv.GetConversationTimestamp(); ts > 0 {
		ch.lastDate = int32(ts)
	}
	// UnreadCount: 0 immer übernehmen (Sync von gelesenem Stand).
	// >0 nur wenn Conversation-Timestamp > readUpTo (User hat seit telegrim-
	// Read noch nicht alle gelesenen Nachrichten wegmarkiert).
	newUnread := int(conv.GetUnreadCount())
	convTS := int32(conv.GetLastMsgTimestamp())
	if convTS == 0 {
		convTS = int32(conv.GetConversationTimestamp())
	}
	if newUnread == 0 || ch.readUpTo == 0 || convTS > ch.readUpTo {
		ch.unread = newUnread
	}

	msgs := conv.GetMessages()
	if len(msgs) == 0 {
		// Selbst ohne übertragene Nachrichten: Konversation hatte Aktivität
		// (LastMsgTimestamp gesetzt). Platzhalter-Last damit Filter "≥1 Msg"
		// nicht greift.
		if ch.last == "" && ch.lastDate > 0 {
			ch.last = "…"
		}
		return
	}

	// Messages kommen i.d.R. neueste-zuerst — wir sortieren nicht, sondern
	// hängen in der gegebenen Reihenfolge ein und setzen ch.last auf den
	// neuesten Text.
	existing := c.messagesByChat[chatID]
	added := 0
	for i := len(msgs) - 1; i >= 0; i-- { // reverse: ältester zuerst
		hsm := msgs[i]
		wmi := hsm.GetMessage()
		if wmi == nil {
			continue
		}
		protoMsg := wmi.GetMessage()
		text := extractWAText(protoMsg)
		media := c.registerWAMedia(protoMsg)
		if text == "" && media == nil {
			continue
		}
		if text == "" && media != nil {
			text = "[" + media.Kind.Label() + "]"
		}
		key := wmi.GetKey()
		ts := int32(wmi.GetMessageTimestamp())
		extID := key.GetID()
		// Dedupe: HistorySync schickt nach Restart oder On-Demand-Request
		// dieselben Messages erneut. Wir prüfen WA-MessageID + Fallback-
		// Fingerprint gegen schon Bestehendes.
		if messageDup(existing, extID, ts, key.GetFromMe(), text) {
			continue
		}
		sender := wmi.GetPushName()
		if key.GetFromMe() {
			sender = "Du"
		} else if sender == "" {
			sender = jid.User
		}
		msgID := c.nextMsgSeq.Add(1)
		existing = append(existing, Message{
			ID:         msgID,
			ChatID:     chatID,
			Platform:   PlatformWA,
			Sender:     sender,
			Text:       text,
			Outgoing:   key.GetFromMe(),
			Date:       ts,
			Media:      media,
			ExternalID: extID,
		})
		added++
		ch.last = text
		if ts > ch.lastDate {
			ch.lastDate = ts
		}
	}
	if added > 0 {
		// Chronologisch sortieren (Date ASC). ON_DEMAND-Syncs liefern ältere
		// Messages — die kämen sonst hinten an, obwohl sie vor allen anderen
		// stehen sollen. Stable damit Reihenfolge bei gleichem ts erhalten
		// bleibt.
		sort.SliceStable(existing, func(i, j int) bool {
			return existing[i].Date < existing[j].Date
		})
		// Cap auf waMessagesCap pro Chat (älteste fallen raus). Verhindert
		// dass repeated requests unbounded wachsen.
		if len(existing) > waMessagesCap {
			existing = existing[len(existing)-waMessagesCap:]
		}
		c.messagesByChat[chatID] = existing
	} else if ch.last == "" && ch.lastDate > 0 {
		ch.last = "…"
	}
	c.chatsDirty.Store(true)
}

func (c *WhatsAppClient) onMessage(e *events.Message) {
	text := extractWAText(e.Message)
	media := c.registerWAMedia(e.Message)
	if text == "" && media == nil {
		return // weder Text noch unterstütztes Media
	}
	if text == "" && media != nil {
		text = "[" + media.Kind.Label() + "]"
	}
	chatJID := e.Info.Chat.ToNonAD()
	chatID := hashJID(chatJID)
	sender := e.Info.PushName
	if sender == "" {
		sender = e.Info.Sender.User
	}
	if e.Info.IsFromMe {
		sender = "Du"
	}

	// Contact-Lookup VOR chatsMu — Lock-Order konsistent mit
	// ensureContactsLoaded (contactNamesMu zuerst, chatsMu danach), sonst
	// Deadlock.
	var contactName string
	if !e.Info.IsGroup {
		contactName = c.lookupContactName(chatJID)
	}

	// Lock-Order: chatsMu → messagesMu.
	c.chatsMu.Lock()
	defer c.chatsMu.Unlock()
	c.messagesMu.Lock()
	defer c.messagesMu.Unlock()

	if _, ok := c.jidByChatID[chatID]; !ok {
		c.jidByChatID[chatID] = chatJID
	}
	ch, ok := c.chatsByID[chatID]
	if !ok {
		title := contactName
		// Niemals den Sender-PushName als Chat-Title verwenden wenn der
		// Sender wir selbst sind — sonst bekommt Michelles Chat unsere
		// eigene PushName ("Chrisby") als Label.
		if title == "" && !e.Info.IsFromMe {
			title = defaultTitleFor(chatJID, e.Info.PushName)
		}
		if title == "" {
			title = defaultTitleFor(chatJID, "")
		}
		ch = &waChat{
			jid:     chatJID,
			title:   title,
			isGroup: e.Info.IsGroup,
		}
		c.chatsByID[chatID] = ch
	} else if isPhoneFallback(ch.title, chatJID) {
		if contactName != "" {
			ch.title = contactName
		} else if !e.Info.IsFromMe && e.Info.PushName != "" {
			ch.title = e.Info.PushName
		}
	}
	ch.last = text
	ch.lastDate = int32(e.Info.Timestamp.Unix())
	if !e.Info.IsFromMe {
		ch.unread++
	}

	extID := e.Info.ID
	if !messageDup(c.messagesByChat[chatID], extID, int32(e.Info.Timestamp.Unix()), e.Info.IsFromMe, text) {
		msgID := c.nextMsgSeq.Add(1)
		status := MessageStatusNone
		if e.Info.IsFromMe {
			status = MessageStatusSent
		}
		msg := Message{
			ID:         msgID,
			ChatID:     chatID,
			Platform:   PlatformWA,
			Sender:     sender,
			Text:       text,
			Outgoing:   e.Info.IsFromMe,
			Date:       int32(e.Info.Timestamp.Unix()),
			Media:      media,
			ExternalID: extID,
			Status:     status,
		}
		c.messagesByChat[chatID] = append(c.messagesByChat[chatID], msg)
		c.chatsDirty.Store(true)
	}

	// Desktop-Notification für eingehende Messages. Outgoing + IsFromMe
	// werden oben schon ausgeschlossen. Senderlabel = Chat-Title, Body =
	// PushName/Sender + Textauszug.
	if !e.Info.IsFromMe {
		notifyTitle := ch.title
		notifyBody := text
		if e.Info.IsGroup && sender != "" && sender != "Du" {
			notifyBody = sender + ": " + text
		}
		notify.Send(notifyTitle, notifyBody)
	}
}

// unwrapWAMessage löst WhatsApp-Wrapper-Messages (ViewOnce, Ephemeral,
// DocumentWithCaption, Edited) bis zur eigentlichen Inhalts-Message auf.
// Begrenzt Rekursion (~4 Levels) gegen versehentliche Loops.
func unwrapWAMessage(m *waProto.Message) *waProto.Message {
	for i := 0; i < 4 && m != nil; i++ {
		switch {
		case m.GetViewOnceMessage() != nil && m.GetViewOnceMessage().GetMessage() != nil:
			m = m.GetViewOnceMessage().GetMessage()
		case m.GetViewOnceMessageV2() != nil && m.GetViewOnceMessageV2().GetMessage() != nil:
			m = m.GetViewOnceMessageV2().GetMessage()
		case m.GetViewOnceMessageV2Extension() != nil && m.GetViewOnceMessageV2Extension().GetMessage() != nil:
			m = m.GetViewOnceMessageV2Extension().GetMessage()
		case m.GetEphemeralMessage() != nil && m.GetEphemeralMessage().GetMessage() != nil:
			m = m.GetEphemeralMessage().GetMessage()
		case m.GetDocumentWithCaptionMessage() != nil && m.GetDocumentWithCaptionMessage().GetMessage() != nil:
			m = m.GetDocumentWithCaptionMessage().GetMessage()
		case m.GetEditedMessage() != nil && m.GetEditedMessage().GetMessage() != nil:
			m = m.GetEditedMessage().GetMessage()
		default:
			return m
		}
	}
	return m
}

func extractWAText(m *waProto.Message) string {
	m = unwrapWAMessage(m)
	if m == nil {
		return ""
	}
	if s := m.GetConversation(); s != "" {
		return s
	}
	if ext := m.GetExtendedTextMessage(); ext != nil {
		return ext.GetText()
	}
	// Captions an Bildern/Videos werden hier mit zurückgegeben, damit die Bubble
	// trotz Media-Anhang einen Text-Body bekommt.
	if img := m.GetImageMessage(); img != nil {
		return img.GetCaption()
	}
	if vid := m.GetVideoMessage(); vid != nil {
		return vid.GetCaption()
	}
	if doc := m.GetDocumentMessage(); doc != nil {
		if cap := doc.GetCaption(); cap != "" {
			return cap
		}
		return doc.GetFileName()
	}
	return ""
}

// registerWAMedia legt eine DownloadableMessage in der Media-Buffer ab und
// liefert einen frischen FileID + Media-Struct zurück. Wenn msg keine
// unterstützte Media trägt: returnt (nil).
func (c *WhatsAppClient) registerWAMedia(m *waProto.Message) *Media {
	m = unwrapWAMessage(m)
	if m == nil {
		return nil
	}
	var (
		dl   whatsmeow.DownloadableMessage
		kind MediaKind
		name string
		mime string
		size int64
		w, h int32
		dur  int32
	)
	switch {
	case m.GetImageMessage() != nil:
		img := m.GetImageMessage()
		dl, kind = img, MediaPhoto
		mime = img.GetMimetype()
		size = int64(img.GetFileLength())
		w = int32(img.GetWidth())
		h = int32(img.GetHeight())
	case m.GetVideoMessage() != nil:
		vid := m.GetVideoMessage()
		dl, kind = vid, MediaVideo
		mime = vid.GetMimetype()
		size = int64(vid.GetFileLength())
		w = int32(vid.GetWidth())
		h = int32(vid.GetHeight())
		dur = int32(vid.GetSeconds())
	case m.GetDocumentMessage() != nil:
		doc := m.GetDocumentMessage()
		dl, kind = doc, MediaDocument
		mime = doc.GetMimetype()
		size = int64(doc.GetFileLength())
		name = doc.GetFileName()
	default:
		return nil
	}

	fileID := c.nextFileID.Add(1)
	c.mediaMu.Lock()
	c.mediaByFile[fileID] = waMediaRef{msg: dl, kind: kind, name: name, mime: mime}
	c.mediaMu.Unlock()

	return &Media{
		Kind:     kind,
		FileID:   fileID,
		FileName: name,
		MimeType: mime,
		Size:     size,
		Width:    w,
		Height:   h,
		Duration: dur,
	}
}

// titleFromContact wählt den besten Display-Namen aus einem ContactInfo. Priorität:
// FullName (Adressbuch-Name aus dem Handy) → BusinessName → PushName (WA-Profil-Name)
// → FirstName → "". Adressbuch zuerst weil Nutzer ihre eigenen Kontaktnamen
// erwarten, nicht den fremd-gesetzten WA-Profilnamen.
func titleFromContact(info types.ContactInfo) string {
	if s := strings.TrimSpace(info.FullName); s != "" {
		return s
	}
	if s := strings.TrimSpace(info.BusinessName); s != "" {
		return s
	}
	if s := strings.TrimSpace(info.PushName); s != "" {
		return s
	}
	if s := strings.TrimSpace(info.FirstName); s != "" {
		return s
	}
	return ""
}

func ownUserOf(client *whatsmeow.Client) string {
	if client == nil || client.Store == nil || client.Store.ID == nil {
		return ""
	}
	return client.Store.ID.User
}

func defaultTitleFor(jid types.JID, push string) string {
	if push != "" {
		return push
	}
	if jid.Server == types.GroupServer {
		return "Gruppe " + jid.User
	}
	return jid.User
}

// isPhoneFallback erkennt, ob ein Titel nur ein JID-User-Fallback ist (reine
// Ziffern oder "Gruppe <ziffern>"). Damit wir wissen ob besserer Name den
// Phonenumber-Titel überschreiben darf.
func isPhoneFallback(title string, jid types.JID) bool {
	if title == jid.User {
		return true
	}
	if title == "Gruppe "+jid.User {
		return true
	}
	if title == "" {
		return true
	}
	return false
}

// ensurePresenceSubscriptions abonniert Presence-Updates für alle bekannten
// 1:1-Chats. Idempotent (subscribedJIDs-Set). Non-blocking fire-and-forget.
func (c *WhatsAppClient) ensurePresenceSubscriptions() {
	c.mu.RLock()
	client := c.client
	c.mu.RUnlock()
	if client == nil || !c.loggedIn.Load() {
		return
	}
	c.chatsMu.RLock()
	var toSub []types.JID
	c.presenceMu.RLock()
	for _, ch := range c.chatsByID {
		if ch.isGroup {
			continue
		}
		if ch.jid.Server != types.DefaultUserServer {
			continue
		}
		key := ch.jid.String()
		if _, done := c.subscribedJIDs[key]; done {
			continue
		}
		toSub = append(toSub, ch.jid)
	}
	c.presenceMu.RUnlock()
	c.chatsMu.RUnlock()
	if len(toSub) == 0 {
		return
	}
	c.presenceMu.Lock()
	for _, j := range toSub {
		c.subscribedJIDs[j.String()] = struct{}{}
	}
	c.presenceMu.Unlock()
	go func() {
		defer func() { _ = recover() }()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, j := range toSub {
			_ = client.SubscribePresence(ctx, j)
		}
	}()
}

// ensureContactsLoaded befüllt chatsByID aus Store-Kontakten + GetJoinedGroups.
// Contacts (lokale SQLite-Iteration über 600 Einträge) sind throttled auf 10s,
// sonst blockiert chatsMu.Lock auf jedem Tick die Reader-Pfade. GetJoinedGroups
// (Network) noch zusätzlich auf 30s throttled gegen schlechte Verbindung.
func (c *WhatsAppClient) ensureContactsLoaded(ctx context.Context) {
	if !c.contactsRefreshing.CompareAndSwap(false, true) {
		return // bereits am Laufen
	}
	defer c.contactsRefreshing.Store(false)

	c.mu.RLock()
	client := c.client
	c.mu.RUnlock()
	if client == nil || !c.loggedIn.Load() {
		return
	}

	now := time.Now().Unix()
	last := c.contactsLastRefresh.Load()
	if last > 0 && now-last < 10 {
		return // contacts-Iteration zu jung
	}
	runGroups := last == 0 || now-last >= 30

	// Eigener Timeout, damit ein hängender Server nicht die UI freezed.
	ctxGroups, cancelGroups := context.WithTimeout(ctx, 5*time.Second)
	defer cancelGroups()

	if client.Store.Contacts != nil {
		contacts, err := client.Store.Contacts.GetAllContacts(ctx)
		if err == nil {
			// Eigene JID NICHT in den Name-Cache packen — sonst können
			// fehlgeleitete Lookups die OWN-Pushname als Chat-Titel anderer
			// User setzen.
			var ownUser string
			if client.Store.ID != nil {
				ownUser = client.Store.ID.User
			}
			// Erst Cache füllen (für Event-Handler).
			c.contactNamesMu.Lock()
			for jid, info := range contacts {
				jid = jid.ToNonAD()
				if jid.Server != types.DefaultUserServer {
					continue
				}
				if ownUser != "" && jid.User == ownUser {
					continue
				}
				if name := titleFromContact(info); name != "" {
					c.contactNames[jid.String()] = name
				}
			}
			c.contactNamesMu.Unlock()
			// Eigene PushName ermitteln um fälschlich gesetzte Titel
			// (z.B. von outgoing-only Chats vor dem Bug-Fix) zu erkennen.
			// Whatsmeow speichert die OWN PushName direkt auf dem Device,
			// nicht im Contacts-Store.
			ownPush := strings.TrimSpace(client.Store.PushName)
			// Eigene LID — damit wir Self-LID-Chats nicht mit fremden Namen
			// belegen.
			ownLIDUser := ""
			if !client.Store.LID.IsEmpty() {
				ownLIDUser = client.Store.LID.User
			}
			// Lookup-Map JID→Name via Contacts. Inkl. LID-Mapping: für jeden
			// LID-Contact die zugehörige PN auflösen damit wir Chats mit
			// LID-JID (kommen aus Gruppen-Mitgliedern) korrekt benennen
			// können.
			nameByJID := make(map[string]string, len(contacts))
			for jid, info := range contacts {
				jid = jid.ToNonAD()
				name := titleFromContact(info)
				if name == "" {
					continue
				}
				nameByJID[jid.String()] = name
			}
			// LID→PN auflösen für alle Chats mit LID-JID → Name aus PN holen.
			c.chatsMu.RLock()
			var lidChats []types.JID
			for _, ch := range c.chatsByID {
				if ch.jid.Server == types.HiddenUserServer {
					lidChats = append(lidChats, ch.jid)
				}
			}
			c.chatsMu.RUnlock()
			for _, lid := range lidChats {
				if pn, err := client.Store.LIDs.GetPNForLID(ctx, lid); err == nil && !pn.IsEmpty() {
					if name, ok := nameByJID[pn.ToNonAD().String()]; ok {
						nameByJID[lid.String()] = name
					}
				}
			}

			c.chatsMu.Lock()
			changed := false
			for id, ch := range c.chatsByID {
				if ch.jid.Server != types.DefaultUserServer && ch.jid.Server != types.HiddenUserServer {
					continue
				}
				isSelf := (ch.jid.User == ownUserOf(client)) || (ownLIDUser != "" && ch.jid.User == ownLIDUser)
				name := nameByJID[ch.jid.String()]
				if name == "" {
					continue
				}
				suspicious := ownPush != "" && ch.title == ownPush && !isSelf
				if isPhoneFallback(ch.title, ch.jid) || suspicious {
					ch.title = name
					changed = true
				}
				c.jidByChatID[id] = ch.jid
			}
			c.chatsMu.Unlock()
			if changed {
				c.chatsDirty.Store(true)
			}
		}
	}

	// Refresh-Stempel setzen (deckt sowohl contacts-Iteration als auch
	// optional Groups ab).
	c.contactsLastRefresh.Store(time.Now().Unix())
	if !runGroups {
		return
	}

	groups, err := client.GetJoinedGroups(ctxGroups)
	if err == nil {
		c.chatsMu.Lock()
		groupChanged := false
		for _, g := range groups {
			jid := g.JID.ToNonAD()
			id := hashJID(jid)
			title := strings.TrimSpace(g.GroupName.Name)
			ch, ok := c.chatsByID[id]
			if !ok {
				if title == "" {
					title = "Gruppe " + jid.User
				}
				c.chatsByID[id] = &waChat{jid: jid, title: title, isGroup: true}
				c.jidByChatID[id] = jid
				groupChanged = true
				continue
			}
			if title != "" && isPhoneFallback(ch.title, jid) {
				ch.title = title
				groupChanged = true
			}
			if !ch.isGroup {
				ch.isGroup = true
				groupChanged = true
			}
		}
		c.chatsMu.Unlock()
		if groupChanged {
			c.chatsDirty.Store(true)
		}
	}
}

func (c *WhatsAppClient) ListChats(ctx context.Context) ([]Chat, error) {
	if !c.loggedIn.Load() {
		return nil, nil
	}
	c.ensureContactsLoaded(ctx)
	c.ensurePresenceSubscriptions()

	// Eigene JID für Self-Chat-Detection (WA hat Note-to-Self mit eigener
	// User-ID — den labeln wir explizit, nicht via PushName).
	var ownUser string
	c.mu.RLock()
	if c.client != nil && c.client.Store != nil && c.client.Store.ID != nil {
		ownUser = c.client.Store.ID.User
	}
	c.mu.RUnlock()

	// Lock-Order: chatsMu → messagesMu (RLock).
	c.chatsMu.RLock()
	defer c.chatsMu.RUnlock()
	c.messagesMu.RLock()
	defer c.messagesMu.RUnlock()

	out := make([]Chat, 0, len(c.chatsByID))
	for id, ch := range c.chatsByID {
		// Filter: nur Chats mit Verlauf zeigen (mind. eine empfangene oder
		// gesendete Nachricht). Stille Kontakte aus dem Adressbuch ohne
		// Konversation überfüllen sonst die Liste.
		if len(c.messagesByChat[id]) == 0 && strings.TrimSpace(ch.last) == "" {
			continue
		}
		isChannel := ch.jid.Server == types.BroadcastServer || ch.jid.Server == types.NewsletterServer
		title := ch.title
		if ownUser != "" && ch.jid.User == ownUser {
			title = "Saved Messages"
		}
		online := false
		if !ch.isGroup && !isChannel {
			c.presenceMu.RLock()
			online = c.onlineByJID[ch.jid.String()]
			c.presenceMu.RUnlock()
		}
		out = append(out, Chat{
			ID:        id,
			Platform:  PlatformWA,
			Title:     title,
			Last:      ch.last,
			Unread:    ch.unread,
			IsPrivate: !ch.isGroup,
			Online:    online,
			LastDate:  ch.lastDate,
			IsChannel: isChannel,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].LastDate != out[j].LastDate {
			return out[i].LastDate > out[j].LastDate
		}
		return out[i].Title < out[j].Title
	})
	return out, nil
}

func (c *WhatsAppClient) ListChatsInFolder(ctx context.Context, folderID int32) ([]Chat, error) {
	return c.ListChats(ctx)
}

func (c *WhatsAppClient) ListFolders(ctx context.Context) ([]Folder, error) {
	return []Folder{{ID: 0, Title: "Alle"}}, nil
}

func (c *WhatsAppClient) ListMessages(ctx context.Context, chatID int64) ([]Message, error) {
	c.chatsMu.RLock()
	jid, hasJID := c.jidByChatID[chatID]
	c.chatsMu.RUnlock()
	c.messagesMu.RLock()
	msgs := c.messagesByChat[chatID]
	c.messagesMu.RUnlock()

	// Bei leerem Cache: On-Demand-HistorySync async triggern (kein Blocken).
	// Der Live-Refresh-Tick der TUI holt die Daten nach, sobald sie da sind.
	if len(msgs) == 0 && hasJID {
		c.requestMoreHistory(jid, 0)
		return nil, nil
	}
	if len(msgs) == 0 {
		return nil, nil
	}
	out := make([]Message, len(msgs))
	copy(out, msgs)
	return out, nil
}

func (c *WhatsAppClient) ListMessagesBefore(ctx context.Context, chatID, beforeMsgID int64, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 50
	}
	c.chatsMu.RLock()
	jid, hasJID := c.jidByChatID[chatID]
	c.chatsMu.RUnlock()
	c.messagesMu.RLock()
	msgs := c.messagesByChat[chatID]
	c.messagesMu.RUnlock()

	// Lokal alles ältere finden.
	idx := -1
	for i, m := range msgs {
		if m.ID == beforeMsgID {
			idx = i
			break
		}
	}
	if idx > 0 {
		start := idx - limit
		if start < 0 {
			start = 0
		}
		out := make([]Message, idx-start)
		copy(out, msgs[start:idx])
		return out, nil
	}

	// Cache leer: Phone um mehr History bitten (kommt async via HistorySync).
	// Nichts blockierendes hier — wir liefern leer zurück und der nächste
	// Versuch findet hoffentlich befüllten Cache.
	if hasJID {
		c.requestMoreHistory(jid, beforeMsgID)
	}
	return nil, nil
}

// RequestFreshHistory triggert einen On-Demand HistorySync für einen
// einzelnen Chat — zum Aufruf aus der TUI wenn der Chat geöffnet wird, damit
// auch ältere/verpasste Messages nachgezogen werden. Idempotent + non-blocking.
func (c *WhatsAppClient) RequestFreshHistory(chatID int64) {
	c.chatsMu.RLock()
	jid, ok := c.jidByChatID[chatID]
	c.chatsMu.RUnlock()
	if !ok {
		return
	}
	c.requestMoreHistory(jid, 0)
}

// requestMoreHistory sendet via SendPeerMessage einen
// HistorySync-Request für ältere Messages eines Chats. whatsmeow liefert die
// Antwort als events.HistorySync (Type ON_DEMAND), die unser onHistorySync
// dann ins messagesByChat einträgt.
func (c *WhatsAppClient) requestMoreHistory(chatJID types.JID, beforeMsgID int64) {
	c.mu.RLock()
	client := c.client
	c.mu.RUnlock()
	if client == nil || !c.loggedIn.Load() {
		return
	}

	// Wir brauchen ein types.MessageInfo als Referenzpunkt. Aus unserem
	// Message-Cache reicht das nicht; wir bauen ein minimales Info-Object aus
	// dem JID. Der count=50 ist whatsmeow-Recommendation.
	info := &types.MessageInfo{
		MessageSource: types.MessageSource{
			Chat: chatJID,
		},
	}
	req := client.BuildHistorySyncRequest(info, 50)
	if req == nil {
		return
	}
	go func() {
		defer func() { _ = recover() }()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = client.SendPeerMessage(ctx, req)
	}()
}

func (c *WhatsAppClient) SendMessage(ctx context.Context, chatID int64, text string) (Message, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Message{}, fmt.Errorf("empty message")
	}
	c.mu.RLock()
	client := c.client
	c.mu.RUnlock()
	if client == nil || !c.loggedIn.Load() {
		return Message{}, fmt.Errorf("whatsapp: nicht angemeldet")
	}

	c.chatsMu.RLock()
	jid, ok := c.jidByChatID[chatID]
	c.chatsMu.RUnlock()
	if !ok {
		return Message{}, fmt.Errorf("whatsapp: unbekannte chat-id %d", chatID)
	}

	resp, err := client.SendMessage(ctx, jid, &waProto.Message{
		Conversation: proto.String(text),
	})
	if err != nil {
		return Message{}, fmt.Errorf("whatsapp: send: %w", err)
	}

	msgID := c.nextMsgSeq.Add(1)
	ts := int32(resp.Timestamp.Unix())
	msg := Message{
		ID:         msgID,
		ChatID:     chatID,
		Platform:   PlatformWA,
		Sender:     "Du",
		Text:       text,
		Outgoing:   true,
		Date:       ts,
		Status:     MessageStatusSent,
		ExternalID: string(resp.ID),
	}
	c.chatsMu.Lock()
	if ch, ok := c.chatsByID[chatID]; ok {
		ch.last = text
		ch.lastDate = ts
	}
	c.chatsMu.Unlock()
	c.messagesMu.Lock()
	c.messagesByChat[chatID] = append(c.messagesByChat[chatID], msg)
	c.messagesMu.Unlock()
	c.chatsDirty.Store(true)
	return msg, nil
}

// MarkChatRead nullt unsere lokale Unread-Zählung und sendet eine
// Read-Receipt an WhatsApp, damit das Handy auch nicht mehr ungelesen
// anzeigt. Sammelt die letzten un-gelesenen MessageIDs aus dem Cache als
// Read-Bestätigung.
func (c *WhatsAppClient) MarkChatRead(ctx context.Context, chatID int64) error {
	c.chatsMu.Lock()
	jid, hasJID := c.jidByChatID[chatID]
	ch, hasCh := c.chatsByID[chatID]
	if hasCh {
		ch.unread = 0
		ch.readUpTo = int32(time.Now().Unix())
	}
	c.chatsMu.Unlock()
	// Letzte ~50 incoming Messages als gelesen markieren.
	var ids []types.MessageID
	c.messagesMu.RLock()
	msgs := c.messagesByChat[chatID]
	for i := len(msgs) - 1; i >= 0 && len(ids) < 50; i-- {
		if msgs[i].Outgoing || msgs[i].ExternalID == "" {
			continue
		}
		ids = append(ids, types.MessageID(msgs[i].ExternalID))
	}
	c.messagesMu.RUnlock()
	c.chatsDirty.Store(true)

	if !hasJID || len(ids) == 0 {
		return nil
	}

	c.mu.RLock()
	client := c.client
	c.mu.RUnlock()
	if client == nil || !c.loggedIn.Load() {
		return nil
	}
	// MarkRead fire-and-forget mit kurzem Timeout; UI muss nicht warten.
	go func() {
		defer func() { _ = recover() }()
		rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = client.MarkRead(rctx, ids, time.Now(), jid, types.JID{})
	}()
	return nil
}

func (c *WhatsAppClient) DownloadFile(ctx context.Context, fileID int32) (string, error) {
	// Cache hit: schon mal heruntergeladen.
	c.mediaMu.RLock()
	if p, ok := c.mediaPaths[fileID]; ok {
		c.mediaMu.RUnlock()
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	ref, found := c.mediaByFile[fileID]
	c.mediaMu.RUnlock()
	if !found {
		return "", fmt.Errorf("whatsapp: unbekannte file-id %d", fileID)
	}

	c.mu.RLock()
	client := c.client
	c.mu.RUnlock()
	if client == nil {
		return "", fmt.Errorf("whatsapp: nicht verbunden")
	}

	data, err := client.Download(ctx, ref.msg)
	if err != nil {
		return "", fmt.Errorf("whatsapp: download: %w", err)
	}

	dir, subdir, ext := waMediaPath(ref)
	target := filepath.Join(dir, subdir)
	if err := os.MkdirAll(target, 0o755); err != nil {
		return "", fmt.Errorf("whatsapp: mkdir: %w", err)
	}
	name := ref.name
	if name == "" {
		name = fmt.Sprintf("wa-%d%s", fileID, ext)
	} else if !strings.Contains(name, ".") && ext != "" {
		name = name + ext
	}
	path := filepath.Join(target, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", fmt.Errorf("whatsapp: write: %w", err)
	}

	c.mediaMu.Lock()
	c.mediaPaths[fileID] = path
	c.mediaMu.Unlock()
	return path, nil
}

// waMediaPath wählt Speicherort + Datei-Extension je nach Media-Kind. Layout
// analog zur TG-Cache-Struktur: ~/.local/share/telegrim/whatsapp/files/{photos,videos,documents}.
// Symlinks unter ~/WhatsApp werden bei MkdirAll nicht angelegt — kann der User
// manuell setzen wenn gewünscht.
func waMediaPath(ref waMediaRef) (base, sub, ext string) {
	base = filepath.Join(waDataDir(), "files")
	switch ref.kind {
	case MediaPhoto:
		sub = "photos"
		ext = mimeExt(ref.mime, ".jpg")
	case MediaVideo:
		sub = "videos"
		ext = mimeExt(ref.mime, ".mp4")
	case MediaDocument:
		sub = "documents"
		ext = mimeExt(ref.mime, "")
	default:
		sub = "misc"
	}
	return
}

func mimeExt(mime, fallback string) string {
	mime = strings.ToLower(strings.TrimSpace(mime))
	switch mime {
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	case "video/mp4":
		return ".mp4"
	case "video/webm":
		return ".webm"
	case "video/quicktime":
		return ".mov"
	case "application/pdf":
		return ".pdf"
	}
	if i := strings.LastIndex(mime, "/"); i > 0 && i < len(mime)-1 {
		return "." + mime[i+1:]
	}
	return fallback
}

func (c *WhatsAppClient) Close() error {
	c.qrMu.Lock()
	if c.qrCancel != nil {
		c.qrCancel()
		c.qrCancel = nil
	}
	c.qrMu.Unlock()

	c.mu.Lock()
	if c.flushStop != nil {
		close(c.flushStop) // löst final flush in der Goroutine aus
		c.flushStop = nil
	}
	if c.client != nil {
		c.client.Disconnect()
		c.client = nil
	}
	if c.container != nil {
		_ = c.container.Close()
		c.container = nil
	}
	c.mu.Unlock()
	// Fallback: falls flushLoop nicht lief (z.B. Connect-Fehler), trotzdem
	// synchron schreiben.
	c.flushChats()
	c.flushMessages()
	return nil
}
