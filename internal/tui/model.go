package tui

import (
	"context"
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unsafe"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	qrcode "github.com/skip2/go-qrcode"

	"telegrim/internal/config"
	"telegrim/internal/telegram"
)

type focusArea int

type appTab int

const (
	focusContacts focusArea = iota
	focusChat
	focusSettings
)

const (
	tabChats appTab = iota
	tabFeed
	tabSettings
)

// applyAutoSort: ungelesene Chats nach oben, darunter nach LastDate.
// sort.SliceStable — O(N log N), wichtig bei WA mit 500+ Kontakten.
func applyAutoSort(chats []telegram.Chat) {
	sort.SliceStable(chats, func(i, j int) bool {
		return autoSortLess(chats[i], chats[j])
	})
}

func autoSortLess(a, b telegram.Chat) bool {
	if (a.Unread > 0) != (b.Unread > 0) {
		return a.Unread > 0
	}
	if a.LastDate != b.LastDate {
		return a.LastDate > b.LastDate
	}
	return a.Title < b.Title
}

// unreadBoundary liefert den Index des ersten gelesenen Chats. Bei
// vollständig ungelesen/gelesen = len(chats) bzw. 0.
func unreadBoundary(chats []telegram.Chat) int {
	for i, c := range chats {
		if c.Unread == 0 {
			return i
		}
	}
	return len(chats)
}

type Model struct {
	cfg    config.Config
	client telegram.Client

	width  int
	height int

	activeTab appTab
	focus     focusArea

	chats         []telegram.Chat
	selected      int
	contactScroll int

	messages    []telegram.Message
	currentChat *telegram.Chat
	msgScroll   int
	// selectedMsg ist der Index in m.messages der gerade gewählten Nachricht
	// (für Open-Media). -1 = keine Auswahl (z.B. leerer Chat).
	selectedMsg int
	// mediaStatus zeigt unter dem Eingabefeld kurzlebige Infos (z.B.
	// "Downloading photo.jpg…"). Leer = nichts anzeigen.
	mediaStatus string
	// mediaCmd hält den laufenden Viewer-Prozess. Esc → kill via Process-Group.
	// nil = kein Viewer offen.
	mediaCmd *exec.Cmd

	// Backwards-Paging: hasMoreMessages = false sobald ListMessagesBefore leer
	// kam, dann nicht weiter triggern. loadingMore verhindert Trigger-Storm
	// während ein Fetch läuft.
	loadingMore     bool
	hasMoreMessages bool

	// Render-Cache: vermeidet erneutes Wrappen+lipgloss-Rendern auf jedem Keystroke.
	// Pointer-Feld → Mutation überlebt value-receiver Methoden (View).
	cache *renderCache

	input textinput.Model

	settingsInputs []textinput.Model
	settingsField  int
	settingsStatus string
	qrLoginURL     string
	qrASCII        string

	splitX   int  // chars für Kontaktspalte; 0 = auto (width/3)
	dragging bool // True solange Splitter mit gedrückter Maus gezogen wird

	folders       []telegram.Folder
	currentFolder int32 // 0 = Alle, -1 = Archiv, >0 = User-Folder

	// platformFilter schränkt die Kontaktliste auf eine Plattform ein. "" =
	// alle, "tg" = nur Telegram, "wa" = nur WhatsApp. Toggle via Ctrl+P oder
	// Klick auf Tab-Leiste. WA-Tab nur sichtbar wenn cfg.EnableWhatsApp.
	platformFilter string

	searchMode  bool
	searchInput textinput.Model

	err error
}

type chatsLoadedMsg struct {
	chats []telegram.Chat
	err   error
}

type messagesLoadedMsg struct {
	messages []telegram.Message
	err      error
}

type messageSentMsg struct {
	msg telegram.Message
	err error
}

type loginResultMsg struct {
	status string
	err    error
}

type foldersLoadedMsg struct {
	folders []telegram.Folder
	err     error
}

type qrLoginReadyMsg struct {
	url      string
	ascii    string
	platform string // "tg" oder "wa"
	err      error
}

type mediaOpenedMsg struct {
	path string
	cmd  *exec.Cmd
	err  error
}

type mediaClosedMsg struct {
	cmd *exec.Cmd
}

type moreLoadedMsg struct {
	chatID   int64
	beforeID int64
	messages []telegram.Message
	err      error
}

type uiLayout struct {
	bodyTop         int
	bodyH           int
	leftW           int
	rightW          int
	contactsStartY  int
	contactsVisible int
	inputY          int
}

func NewModel(cfg config.Config, client telegram.Client) Model {
	input := textinput.New()
	input.Placeholder = "Nachricht schreiben..."
	input.CharLimit = 4096
	input.Prompt = "> "

	search := textinput.New()
	search.Placeholder = "suchen…"
	search.CharLimit = 64
	search.Prompt = "🔎 "

	settingsInputs := newSettingsInputs(cfg)

	return Model{
		cfg:            cfg,
		client:         client,
		input:          input,
		searchInput:    search,
		settingsInputs: settingsInputs,
		focus:          focusContacts,
		activeTab:      tabChats,
		selectedMsg:    -1,
		cache:          &renderCache{},
	}
}

func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.connectCmd()}
	// Auto-Login wenn Credentials persistent vorhanden: TDLib öffnet seine DB,
	// findet (idealerweise) eine gespeicherte Session, geht direkt auf Ready.
	// Phone leer → Backend wartet bei Bedarf auf Re-Auth, blockt aber nicht.
	if m.cfg.APIID > 0 && strings.TrimSpace(m.cfg.APIHash) != "" {
		cmds = append(cmds, m.autoLoginCmd())
	}
	cmds = append(cmds, m.loadChatsCmd(), m.loadFoldersCmd(), m.refreshTickCmd())
	return tea.Batch(cmds...)
}

// refreshInterval steuert wie oft Chat-Liste + aktueller Chat automatisch neu
// gezogen werden, damit eingehende Messages live erscheinen. 2s ist ein guter
// Kompromiss zwischen Reaktivität und Backend-Last.
const refreshInterval = 2 * time.Second

type refreshTickMsg time.Time

func (m Model) refreshTickCmd() tea.Cmd {
	return tea.Tick(refreshInterval, func(t time.Time) tea.Msg {
		return refreshTickMsg(t)
	})
}

func (m Model) autoLoginCmd() tea.Cmd {
	auth := telegram.AuthSettings{APIID: m.cfg.APIID, APIHash: m.cfg.APIHash}
	return func() tea.Msg {
		if err := m.client.Login(context.Background(), auth); err != nil {
			return loginResultMsg{err: err}
		}
		return loginResultMsg{status: "Auto-Login versucht – falls Session vorhanden, sind die Chats gleich da."}
	}
}

func (m *Model) setActiveTab(t appTab) {
	m.activeTab = t
	if t == tabSettings {
		m.focus = focusSettings
		m.input.Blur()
		m.focusSettingsField(m.settingsField)
		return
	}
	if m.focus == focusSettings {
		m.focus = focusContacts
	}
	for i := range m.settingsInputs {
		m.settingsInputs[i].Blur()
	}
	if m.focus == focusChat {
		m.input.Focus()
	} else {
		m.input.Blur()
	}
}

func (m Model) connectCmd() tea.Cmd {
	return func() tea.Msg {
		err := m.client.Connect(context.Background())
		if err != nil {
			return chatsLoadedMsg{err: err}
		}
		return nil
	}
}

func (m Model) refreshCmd() tea.Cmd {
	cmds := []tea.Cmd{m.loadChatsCmd()}
	if m.currentChat != nil {
		cmds = append(cmds, m.loadMessagesCmd(m.currentChat.ID))
	}
	return tea.Batch(cmds...)
}

func (m Model) loadChatsCmd() tea.Cmd {
	folder := m.currentFolder
	return func() tea.Msg {
		chats, err := m.client.ListChatsInFolder(context.Background(), folder)
		return chatsLoadedMsg{chats: chats, err: err}
	}
}

func (m Model) loadFoldersCmd() tea.Cmd {
	return func() tea.Msg {
		fs, err := m.client.ListFolders(context.Background())
		return foldersLoadedMsg{folders: fs, err: err}
	}
}

func (m Model) openMediaCmd(med telegram.Media) tea.Cmd {
	client := m.client
	return func() tea.Msg {
		path := med.LocalPath
		// LocalPath kann veraltet sein (Datei gelöscht/verschoben). Stat
		// prüft Existenz; bei Miss → frisch via DownloadFile holen, TDLib
		// lädt dann nach.
		if path != "" {
			if _, err := os.Stat(path); err != nil {
				path = ""
			}
		}
		if path == "" {
			p, err := client.DownloadFile(context.Background(), med.FileID)
			if err != nil {
				return mediaOpenedMsg{err: err}
			}
			path = p
		}
		name, args := pickViewer(med, path)
		c := exec.Command(name, args...)
		// Eigene Process-Group, damit Esc per kill(-pgid) auch Kind-Prozesse
		// (z.B. mpv-Worker) erwischt. Bei xdg-open ist der Kill best-effort,
		// weil der echte Viewer nach Exec reparented werden kann.
		setMediaProcessGroup(c)
		if err := c.Start(); err != nil {
			return mediaOpenedMsg{err: fmt.Errorf("%s: %w", name, err)}
		}
		return mediaOpenedMsg{path: path, cmd: c}
	}
}

// pickViewer wählt einen Viewer pro Mediakind/MIME. Plattform-spezifisch:
// Unix nutzt imv/mpv/xdg-open, Windows ruft Default-Handler via `cmd /c start`.

func waitMediaCmd(c *exec.Cmd) tea.Cmd {
	return func() tea.Msg {
		_ = c.Wait()
		return mediaClosedMsg{cmd: c}
	}
}

// maybeLoadMoreCmd triggert Backwards-Paging wenn der User nahe am Anfang
// der geladenen Historie ist. Eager-Prefetch: wir warten nicht bis selectedMsg
// auf 0 sitzt, sondern laden schon ab 5er-Abstand.
func (m *Model) maybeLoadMoreCmd() tea.Cmd {
	if m.loadingMore || !m.hasMoreMessages || m.currentChat == nil || len(m.messages) == 0 {
		return nil
	}
	// Eager: laden wenn Auswahl nahe Anfang ODER Viewport ganz oben.
	if m.selectedMsg > 5 && m.msgScroll > 0 {
		return nil
	}
	m.loadingMore = true
	chatID := m.currentChat.ID
	beforeID := m.messages[0].ID
	client := m.client
	return func() tea.Msg {
		older, err := client.ListMessagesBefore(context.Background(), chatID, beforeID, 50)
		return moreLoadedMsg{chatID: chatID, beforeID: beforeID, messages: older, err: err}
	}
}

func (m Model) loadMessagesCmd(chatID int64) tea.Cmd {
	return func() tea.Msg {
		messages, err := m.client.ListMessages(context.Background(), chatID)
		return messagesLoadedMsg{messages: messages, err: err}
	}
}

func (m Model) sendMessageCmd(chatID int64, text string) tea.Cmd {
	return func() tea.Msg {
		msg, err := m.client.SendMessage(context.Background(), chatID, text)
		return messageSentMsg{msg: msg, err: err}
	}
}

func newSettingsInputs(cfg config.Config) []textinput.Model {
	apiID := textinput.New()
	apiID.Prompt = "API ID: "
	apiID.Placeholder = "z.B. 123456"
	if cfg.APIID > 0 {
		apiID.SetValue(strconv.Itoa(cfg.APIID))
	}
	apiID.CharLimit = 16

	apiHash := textinput.New()
	apiHash.Prompt = "API Hash: "
	apiHash.Placeholder = "dein Telegram API Hash"
	apiHash.SetValue(cfg.APIHash)
	apiHash.CharLimit = 128

	phone := textinput.New()
	phone.Prompt = "Phone: "
	phone.Placeholder = "+49..."
	phone.CharLimit = 32

	code := textinput.New()
	code.Prompt = "Code: "
	code.Placeholder = "Telegram Login-Code"
	code.CharLimit = 16

	password := textinput.New()
	password.Prompt = "Passwort (2FA): "
	password.Placeholder = "optional – nur falls Telegram danach fragt"
	password.CharLimit = 128
	password.EchoMode = textinput.EchoPassword
	password.EchoCharacter = '•'

	inputs := []textinput.Model{apiID, apiHash, phone, code, password}
	inputs[0].Focus()
	return inputs
}

func (m *Model) focusSettingsField(index int) {
	if len(m.settingsInputs) == 0 {
		return
	}
	if index < 0 {
		index = 0
	}
	if index >= len(m.settingsInputs) {
		index = len(m.settingsInputs) - 1
	}
	m.settingsField = index
	for i := range m.settingsInputs {
		if i == m.settingsField {
			m.settingsInputs[i].Focus()
		} else {
			m.settingsInputs[i].Blur()
		}
	}
}

func (m *Model) settingsAPIAuth() (telegram.AuthSettings, error) {
	apiID, err := strconv.Atoi(strings.TrimSpace(m.settingsInputs[0].Value()))
	if err != nil || apiID <= 0 {
		return telegram.AuthSettings{}, fmt.Errorf("API ID ist ungültig")
	}
	apiHash := strings.TrimSpace(m.settingsInputs[1].Value())
	if apiHash == "" {
		return telegram.AuthSettings{}, fmt.Errorf("API Hash fehlt")
	}
	return telegram.AuthSettings{
		APIID:    apiID,
		APIHash:  apiHash,
		Phone:    strings.TrimSpace(m.settingsInputs[2].Value()),
		Code:     strings.TrimSpace(m.settingsInputs[3].Value()),
		Password: strings.TrimSpace(m.settingsInputs[4].Value()),
	}, nil
}

func (m *Model) settingsAuth() (telegram.AuthSettings, error) {
	auth, err := m.settingsAPIAuth()
	if err != nil {
		return telegram.AuthSettings{}, err
	}
	if strings.TrimSpace(auth.Phone) == "" {
		return telegram.AuthSettings{}, fmt.Errorf("Telefonnummer fehlt")
	}
	// Code + Password absichtlich nicht hier prüfen: bei TDLib-Flow gibt es
	// mehrere Schritte (Phone → Telegram schickt Code → Code-Feld füllen →
	// erneut klicken; ggf. 2FA-Passwort danach). Das Backend meldet was fehlt.
	return auth, nil
}

func (m *Model) loginCmd() tea.Cmd {
	auth, err := m.settingsAuth()
	if err != nil {
		return func() tea.Msg { return loginResultMsg{err: err} }
	}
	m.cfg.APIID = auth.APIID
	m.cfg.APIHash = auth.APIHash
	// Credentials persistent ablegen – nächste Session lädt sie automatisch.
	_ = config.SaveCredentials(auth.APIID, auth.APIHash)
	return func() tea.Msg {
		err := m.client.LoginWithCode(context.Background(), auth)
		if err != nil {
			return loginResultMsg{err: err}
		}
		return loginResultMsg{status: backendLoginSuccessLabel(m.cfg.Backend)}
	}
}

func (m *Model) startQRLoginCmd() tea.Cmd {
	auth, err := m.settingsAPIAuth()
	if err != nil {
		return func() tea.Msg { return qrLoginReadyMsg{err: err} }
	}
	m.cfg.APIID = auth.APIID
	m.cfg.APIHash = auth.APIHash
	return func() tea.Msg {
		if err := m.client.Login(context.Background(), auth); err != nil {
			return qrLoginReadyMsg{err: err}
		}
		url, err := m.client.StartQRLogin(context.Background())
		if err != nil {
			return qrLoginReadyMsg{err: err}
		}
		ascii, err := renderASCIIQR(url)
		if err != nil {
			return qrLoginReadyMsg{err: err}
		}
		return qrLoginReadyMsg{url: url, ascii: ascii, platform: telegram.PlatformTG}
	}
}

// whatsAppBackend extrahiert (falls vorhanden) den WA-Client aus einem
// MultiClient. Liefert nil, wenn WA nicht aktiviert ist.
func (m *Model) whatsAppBackend() *telegram.WhatsAppClient {
	mc, ok := m.client.(*telegram.MultiClient)
	if !ok {
		return nil
	}
	for _, e := range mc.Extras() {
		if wa, ok := e.(*telegram.WhatsAppClient); ok {
			return wa
		}
	}
	return nil
}

// startWAQRLoginCmd triggert den whatsmeow-QR-Pairing-Flow. Anders als TG
// braucht WA keine API-Credentials – Connect+QR reichen.
func (m *Model) startWAQRLoginCmd() tea.Cmd {
	wa := m.whatsAppBackend()
	if wa == nil {
		return func() tea.Msg {
			return qrLoginReadyMsg{err: fmt.Errorf("WhatsApp-Backend nicht aktiv (TELEGRIM_WHATSAPP=1 setzen)")}
		}
	}
	return func() tea.Msg {
		if err := wa.Connect(context.Background()); err != nil {
			return qrLoginReadyMsg{err: err}
		}
		code, err := wa.StartQRLogin(context.Background())
		if err != nil {
			return qrLoginReadyMsg{err: err, platform: telegram.PlatformWA}
		}
		ascii, err := renderASCIIQR(code)
		if err != nil {
			return qrLoginReadyMsg{err: err, platform: telegram.PlatformWA}
		}
		return qrLoginReadyMsg{url: code, ascii: ascii, platform: telegram.PlatformWA}
	}
}

// renderASCIIQR rendert ein QR-Bitmap als Unicode-Braille (2×4-Modul-Patterns
// in U+2800..U+28FF). Vorteil gegenüber Block-Glyphen: vertikal komprimiert
// 1/4 (statt 1/2 wie Half-Block/Quadrant) — entscheidend für lange Payloads
// wie WhatsApp-Pairing-URLs (~280 Zeichen → QR-Version 14+ = 74 Module tall).
// Quiet-Zone auf 2 reduziert; EC-Level Low.
//
// Braille-Bit-Reihenfolge (Standard Unicode):
//
//	0x01 0x08
//	0x02 0x10
//	0x04 0x20
//	0x40 0x80
//
// Scanner-Anmerkung: Braille hat sichtbare Punktabstände, aber moderne Phone-
// Kameras (incl. WhatsApp) erkennen das zuverlässig, solange Quiet-Zone und
// Kontrast stimmen.
func renderASCIIQR(content string) (string, error) {
	code, err := qrcode.New(content, qrcode.Low)
	if err != nil {
		return "", err
	}
	bitmap := code.Bitmap()
	if len(bitmap) == 0 {
		return "", fmt.Errorf("leeres qr bitmap")
	}

	quiet := 2
	width := len(bitmap[0]) + 2*quiet
	height := len(bitmap) + 2*quiet
	isDark := func(x, y int) bool {
		bx := x - quiet
		by := y - quiet
		if by < 0 || by >= len(bitmap) {
			return false
		}
		if bx < 0 || bx >= len(bitmap[by]) {
			return false
		}
		return bitmap[by][bx]
	}

	bits := [4][2]byte{
		{0x01, 0x08},
		{0x02, 0x10},
		{0x04, 0x20},
		{0x40, 0x80},
	}

	var b strings.Builder
	for y := 0; y < height; y += 4 {
		for x := 0; x < width; x += 2 {
			var cell byte
			for dy := 0; dy < 4; dy++ {
				for dx := 0; dx < 2; dx++ {
					if isDark(x+dx, y+dy) {
						cell |= bits[dy][dx]
					}
				}
			}
			b.WriteRune(rune(0x2800) + rune(cell))
		}
		if y+4 < height {
			b.WriteByte('\n')
		}
	}
	return b.String(), nil
}

// contactRowHeight = Zeilen pro Kontakt-Eintrag (Titel + Preview + Trennlinie).
const contactRowHeight = 3

func (m Model) layout() uiLayout {
	// Fixed lines: header (1) + topFrame (1) + bottomFrame (1) + footer (1) = 4
	// Frame ersetzt den alten sep unter Header und fügt eine Zeile unter Body.
	bodyTop := 2
	bodyH := max(8, m.height-4)
	leftW := m.splitX
	if leftW <= 0 {
		leftW = max(28, m.width/3)
	}
	// Klemmen: nicht enger als 20, nicht breiter als width-22 (Chat braucht Platz).
	if leftW < 20 {
		leftW = 20
	}
	if leftW > m.width-22 {
		leftW = max(20, m.width-22)
	}
	// -3: 1 für Splitter, 1 für linke Fokus-Bar, 1 für rechte Fokus-Bar.
	rightW := max(20, m.width-leftW-3)
	contactsStartY := bodyTop + 2
	contactsVisible := max(1, (bodyH-2)/contactRowHeight)
	inputY := bodyTop + bodyH - 2

	return uiLayout{
		bodyTop:         bodyTop,
		bodyH:           bodyH,
		leftW:           leftW,
		rightW:          rightW,
		contactsStartY:  contactsStartY,
		contactsVisible: contactsVisible,
		inputY:          inputY,
	}
}

// visibleChats wendet ggf. den Suchfilter an.
func (m Model) visibleChats() []telegram.Chat {
	q := strings.TrimSpace(strings.ToLower(m.searchInput.Value()))
	filter := m.platformFilter
	wantChannels := m.activeTab == tabFeed

	// Memoize: bei unverändertem Input direkt das vorherige Result zurück.
	// Sehr häufig aus View/clamp/Mouse aufgerufen — O(N)-Filter sonst 2-3x
	// pro Frame.
	key := visibleCacheKey{
		chatsLen:       len(m.chats),
		searchMode:     m.searchMode,
		searchQ:        q,
		platformFilter: filter,
		activeTab:      m.activeTab,
	}
	if len(m.chats) > 0 {
		key.chatsPtr = uintptr(unsafe.Pointer(&m.chats[0]))
	}
	if m.cache != nil && m.cache.visibleValid && m.cache.visibleKey == key {
		return m.cache.visibleResult
	}

	out := make([]telegram.Chat, 0, len(m.chats))
	for _, c := range m.chats {
		if c.IsChannel != wantChannels {
			continue
		}
		if filter != "" {
			p := c.Platform
			if p == "" {
				p = telegram.PlatformTG
			}
			if p != filter {
				continue
			}
		}
		if m.searchMode && q != "" {
			if !strings.Contains(strings.ToLower(c.Title), q) {
				continue
			}
		}
		out = append(out, c)
	}
	if m.cache != nil {
		m.cache.visibleResult = out
		m.cache.visibleKey = key
		m.cache.visibleValid = true
	}
	return out
}

func (m *Model) clampSelection() {
	chats := m.visibleChats()
	if len(chats) == 0 {
		m.selected = 0
		m.contactScroll = 0
		return
	}
	if m.selected < 0 {
		m.selected = 0
	}
	if m.selected >= len(chats) {
		m.selected = len(chats) - 1
	}
	l := m.layout()
	if m.selected < m.contactScroll {
		m.contactScroll = m.selected
	}
	if m.selected >= m.contactScroll+l.contactsVisible {
		m.contactScroll = m.selected - l.contactsVisible + 1
	}
	if m.contactScroll < 0 {
		m.contactScroll = 0
	}
}

func (m *Model) openSelectedChat() tea.Cmd {
	chats := m.visibleChats()
	if len(chats) == 0 {
		return nil
	}
	if m.selected >= len(chats) {
		m.selected = len(chats) - 1
	}
	chat := chats[m.selected]
	m.currentChat = &chat
	m.msgScroll = 0
	m.selectedMsg = -1
	m.focus = focusChat
	m.input.Focus()
	m.loadingMore = false
	m.hasMoreMessages = true
	// Unread auf 0 setzen – sucht im Original-Slice nach passender ID.
	for i := range m.chats {
		if m.chats[i].ID == chat.ID && m.chats[i].Unread > 0 {
			m.chats[i].Unread = 0
			break
		}
	}
	chatID := chat.ID
	client := m.client
	markCmd := func() tea.Msg {
		_ = client.MarkChatRead(context.Background(), chatID)
		return nil
	}
	// WA aktiv vom Server pullen falls verfügbar — ListMessages allein
	// returnt nur Cache, der könnte alt sein.
	if wa := m.whatsAppBackend(); wa != nil {
		go wa.RequestFreshHistory(chatID)
	}
	// Delayed Reload nach 1.5s — gibt WA-History-Sync / TDLib-Server-Fetch
	// Zeit nachzuziehen, damit neueste Messages sichtbar werden auch wenn der
	// Cache veraltet war.
	delayedReload := tea.Tick(1500*time.Millisecond, func(time.Time) tea.Msg {
		return chatOpenReloadMsg{chatID: chatID}
	})
	return tea.Batch(m.loadMessagesCmd(chatID), markCmd, delayedReload)
}

// chatOpenReloadMsg triggert einen zweiten ListMessages-Call kurz nach dem
// Chat-Öffnen. Nur wirksam wenn der User noch im selben Chat ist (Race-Schutz).
type chatOpenReloadMsg struct {
	chatID int64
}

func (m *Model) sendCurrentInput() tea.Cmd {
	if m.currentChat == nil {
		return nil
	}
	text := strings.TrimSpace(m.input.Value())
	if text == "" {
		return nil
	}
	return m.sendMessageCmd(m.currentChat.ID, text)
}

func messageViewportHeight(l uiLayout) int {
	return max(1, l.bodyH-5)
}

func clampMsgScroll(total, viewport, current int) int {
	if viewport < 1 {
		viewport = 1
	}
	maxStart := max(0, total-viewport)
	if current < 0 {
		return 0
	}
	if current > maxStart {
		return maxStart
	}
	return current
}

func (m *Model) clampMessageScroll() {
	m.msgScroll = clampMsgScroll(len(m.messages), messageViewportHeight(m.layout()), m.msgScroll)
}

func (m *Model) stickMessagesToBottom() {
	l := m.layout()
	mr := m.cachedMessageRender(l)
	m.msgScroll = max(0, len(mr.lines)-messageViewportHeight(l))
}

type messageRender struct {
	lines  []string
	starts []int // Zeilen-Index, an dem Nachricht i beginnt (inkl. evtl. Datum-Block davor).
}

type msgCacheKey struct {
	chatID      int64
	msgCount    int
	lastMsgID   int64
	rightW      int
	selectedMsg int
}

func (m Model) currentMsgCacheKey(l uiLayout) msgCacheKey {
	k := msgCacheKey{rightW: l.rightW, msgCount: len(m.messages), selectedMsg: m.selectedMsg}
	if m.currentChat != nil {
		k.chatID = m.currentChat.ID
	}
	if n := len(m.messages); n > 0 {
		k.lastMsgID = m.messages[n-1].ID
	}
	return k
}

type renderCache struct {
	msg         messageRender
	msgKey      msgCacheKey
	contacts    []string
	contactsKey contactsCacheKey

	// msgLRU cached gerenderte Message-Listen pro Chat. Chat-Switch ist sonst
	// teuer (Lipgloss pro Bubble × 50–200 Messages). Kleines fixes Limit:
	// Wenn voll, wird der älteste verworfen — kein echtes LRU-Ranking nötig
	// für 4 Slots.
	msgLRU [4]msgLRUEntry

	// visibleChats-Memo: O(N)-Filter wird sonst 2-3x pro View() gerechnet.
	visibleResult []telegram.Chat
	visibleKey    visibleCacheKey
	visibleValid  bool
}

type visibleCacheKey struct {
	chatsLen       int
	chatsPtr       uintptr // m.chats[0] Adresse - erkennt slice-Re-allocation
	searchMode     bool
	searchQ        string
	platformFilter string
	activeTab      appTab
}

type msgLRUEntry struct {
	key   msgCacheKey
	val   messageRender
	stamp uint64 // touch counter — höher = jünger
}

var msgLRUStamp uint64

func msgLRUStampNext() uint64 {
	msgLRUStamp++
	return msgLRUStamp
}

type contactsCacheKey struct {
	leftW          int
	bodyH          int
	selected       int
	contactScroll  int
	chatsLen       int
	chatsFinger    uint64 // hash über Title+Unread+Online der sichtbaren Slice
	searchMode     bool
	searchQ        string
	focus          focusArea
	activeTab      appTab
	folder         int32
	platformFilter string
	err            string
}

func (m Model) currentContactsCacheKey(l uiLayout) contactsCacheKey {
	visible := m.visibleChats()
	k := contactsCacheKey{
		leftW:          l.leftW,
		bodyH:          l.bodyH,
		selected:       m.selected,
		contactScroll:  m.contactScroll,
		chatsLen:       len(visible),
		searchMode:     m.searchMode,
		searchQ:        m.searchInput.Value(),
		focus:          m.focus,
		activeTab:      m.activeTab,
		folder:         m.currentFolder,
		platformFilter: m.platformFilter,
	}
	if m.err != nil {
		k.err = m.err.Error()
	}
	// Lightweight fingerprint: nur sichtbare Zeilen + ein paar drumherum.
	// FNV-1a über Title/Unread/Online/Platform pro Eintrag — über die
	// gefilterte Slice, damit Platform-Switch sicher invalidiert.
	h := uint64(1469598103934665603)
	start := m.contactScroll
	end := start + l.contactsVisible
	if end > len(visible) {
		end = len(visible)
	}
	for i := start; i < end; i++ {
		c := visible[i]
		for _, b := range []byte(c.Title) {
			h ^= uint64(b)
			h *= 1099511628211
		}
		h ^= uint64(c.Unread)
		h *= 1099511628211
		if c.Online {
			h ^= 1
			h *= 1099511628211
		}
		for _, b := range []byte(c.Platform) {
			h ^= uint64(b)
			h *= 1099511628211
		}
	}
	k.chatsFinger = h
	return k
}

func (m Model) cachedContactsLines(l uiLayout) []string {
	key := m.currentContactsCacheKey(l)
	if key == m.cache.contactsKey && m.cache.contacts != nil {
		profNote("contactsCache", "hit")
		return m.cache.contacts
	}
	profNote("contactsCache", "miss")
	defer profTime("renderContactsLines")()
	m.cache.contacts = m.renderContactsLines(l)
	m.cache.contactsKey = key
	return m.cache.contacts
}

// cachedMessageRender liefert die gerenderten Zeilen, neu berechnet nur wenn
// sich Chat, Nachrichtenanzahl, letzte Nachricht oder Spaltenbreite ändern.
//
// Zusätzlich: 4-Slot-LRU pro Chat, damit Chat-Switch (zurück + vor) ohne
// neues Lipgloss-Rendering der ~50–200 Bubbles auskommt.
func (m Model) cachedMessageRender(l uiLayout) messageRender {
	key := m.currentMsgCacheKey(l)
	if key == m.cache.msgKey && m.cache.msg.lines != nil {
		profNote("msgCache", "hit-primary")
		return m.cache.msg
	}
	// LRU-Lookup: vielleicht haben wir den Chat in einem anderen Slot.
	for i := range m.cache.msgLRU {
		if m.cache.msgLRU[i].val.lines != nil && m.cache.msgLRU[i].key == key {
			profNote("msgCache", "hit-lru")
			m.cache.msg = m.cache.msgLRU[i].val
			m.cache.msgKey = key
			m.cache.msgLRU[i].stamp = msgLRUStampNext()
			return m.cache.msg
		}
	}
	profNote("msgCache", "miss")
	defer profTime("renderMessageLines")()
	// Verdränge ältesten Slot (kleinster stamp). Speichere ALTEN primary-render
	// dort bevor wir überschreiben — damit ist der Vorgänger-Chat-Render im
	// LRU verfügbar für Back-Switch.
	if m.cache.msg.lines != nil {
		oldest := 0
		for i := 1; i < len(m.cache.msgLRU); i++ {
			if m.cache.msgLRU[i].stamp < m.cache.msgLRU[oldest].stamp {
				oldest = i
			}
		}
		m.cache.msgLRU[oldest] = msgLRUEntry{
			key:   m.cache.msgKey,
			val:   m.cache.msg,
			stamp: msgLRUStampNext(),
		}
	}
	m.cache.msg = m.renderMessageLines(l)
	m.cache.msgKey = key
	return m.cache.msg
}

// renderMessageLines baut die gerenderten Zeilen (Bubbles, Datum-Separatoren,
// Spacer) für den aktuellen Chat und merkt sich pro Nachricht den Startindex,
// damit Scroll-Sprünge nachrichtenweise sein können.
func (m Model) renderMessageLines(l uiLayout) messageRender {
	if m.currentChat == nil {
		return messageRender{}
	}
	showSender := !m.currentChat.IsPrivate
	bubbleMaxW := max(24, l.rightW*7/10)
	textWidth := max(1, bubbleMaxW-4)

	var rendered []string
	starts := make([]int, 0, len(m.messages))
	appendAligned := func(line string, right bool) {
		lineW := lipgloss.Width(line)
		if right {
			pad := l.rightW - lineW - 1
			if pad < 0 {
				pad = 0
			}
			rendered = append(rendered, strings.Repeat(" ", pad)+line)
		} else {
			rendered = append(rendered, " "+line)
		}
	}

	var lastDay string
	for i, msg := range m.messages {
		starts = append(starts, len(rendered))
		day := formatMessageDay(msg.Date)
		if day != "" && day != lastDay {
			rendered = append(rendered, "")
			rendered = append(rendered, fitToWidth(centerInWidth(dateSeparatorStyle.Render(day), l.rightW), l.rightW))
			rendered = append(rendered, "")
			lastDay = day
		}

		selected := i == m.selectedMsg
		style := bubbleIncoming
		metaStyle := bubbleMetaIncoming
		right := false
		isWA := msg.Platform == telegram.PlatformWA
		if msg.Outgoing {
			if isWA {
				style = bubbleOutgoingWA
				metaStyle = bubbleMetaOutgoingWA
			} else {
				style = bubbleOutgoing
				metaStyle = bubbleMetaOutgoing
			}
			right = true
		}
		if selected {
			switch {
			case msg.Outgoing && isWA:
				style = bubbleOutgoingWASelected
			case msg.Outgoing:
				style = bubbleOutgoingSelected
			default:
				style = bubbleIncomingSelected
			}
		}

		if showSender && !msg.Outgoing && strings.TrimSpace(msg.Sender) != "" {
			appendAligned(bubbleSenderStyle.Render(msg.Sender), false)
		}

		var bubbleLines []string
		if msg.Media != nil {
			for _, ml := range formatMediaLines(msg.Media, textWidth) {
				bubbleLines = append(bubbleLines, mediaLineStyle.Render(ml))
			}
		}
		for _, para := range strings.Split(msg.Text, "\n") {
			wrapped := wrapWords(para, textWidth)
			if len(wrapped) == 0 {
				wrapped = []string{""}
			}
			bubbleLines = append(bubbleLines, wrapped...)
		}
		if len(bubbleLines) == 0 {
			bubbleLines = []string{""}
		}

		meta := formatMessageTime(msg.Date)
		if msg.Outgoing {
			meta = meta + " " + statusGlyph(msg.Status)
		}
		metaW := lipgloss.Width(meta)
		lastIdx := len(bubbleLines) - 1
		lastLineW := lipgloss.Width(bubbleLines[lastIdx])
		if lastLineW+2+metaW <= textWidth {
			gap := textWidth - lastLineW - metaW
			bubbleLines[lastIdx] = bubbleLines[lastIdx] + strings.Repeat(" ", gap) + metaStyle.Render(meta)
		} else {
			bubbleLines = append(bubbleLines, strings.Repeat(" ", max(0, textWidth-metaW))+metaStyle.Render(meta))
		}

		bubble := style.Render(strings.Join(bubbleLines, "\n"))
		for _, bl := range strings.Split(bubble, "\n") {
			appendAligned(bl, right)
		}
		rendered = append(rendered, "")
	}
	return messageRender{lines: rendered, starts: starts}
}

// moveSelectedMsg verschiebt die Auswahl um delta und scrollt so dass die
// neue Auswahl im Sichtbereich bleibt.
func (m *Model) moveSelectedMsg(delta int, starts []int, vp int) {
	if len(m.messages) == 0 {
		m.selectedMsg = -1
		return
	}
	if m.selectedMsg < 0 {
		m.selectedMsg = len(m.messages) - 1
	}
	next := m.selectedMsg + delta
	if next < 0 {
		next = 0
	}
	if next >= len(m.messages) {
		next = len(m.messages) - 1
	}
	m.selectedMsg = next
	if next >= len(starts) {
		return
	}
	startLine := starts[next]
	// Auto-Scroll: wenn ausgewählte Nachricht oberhalb oder unterhalb des
	// Sichtbereichs liegt, scrollen wir so dass ihr Anfang sichtbar wird.
	if startLine < m.msgScroll {
		m.msgScroll = startLine
	} else if startLine >= m.msgScroll+vp {
		m.msgScroll = startLine
	}
}

// prevMessageStart liefert den größten Start-Index < cur, sonst 0.
func prevMessageStart(starts []int, cur int) int {
	best := 0
	for _, s := range starts {
		if s >= cur {
			break
		}
		best = s
	}
	return best
}

// nextMessageStart liefert den kleinsten Start-Index > cur, sonst -1.
func nextMessageStart(starts []int, cur int) int {
	for _, s := range starts {
		if s > cur {
			return s
		}
	}
	return -1
}

// appLabel liefert "telegrim" oder "telegrim <version>" (Version != dev).
// Wird sowohl im Header-Render als auch in tabRanges/folderRanges genutzt
// damit Click-Hit-Areas stimmen.
func (m Model) appLabel() string {
	if v := strings.TrimSpace(m.cfg.Version); v != "" && v != "dev" {
		return m.cfg.AppName + " " + v
	}
	return m.cfg.AppName
}

// tabRanges liefert die Spalten-Ranges der drei Header-Tabs:
// [Chats]  [Aktuelles]  [Settings]
func (m Model) tabRanges() (chatStart, chatEnd, feedStart, feedEnd, settingsStart, settingsEnd int) {
	prefix := fmt.Sprintf("%s · ", m.appLabel())
	chatLabel := "[Chats]"
	feedLabel := "[Aktuelles]"
	settingsLabel := "[Settings]"
	chatStart = len(prefix)
	chatEnd = chatStart + len(chatLabel) - 1
	feedStart = chatEnd + 2
	feedEnd = feedStart + len(feedLabel) - 1
	settingsStart = feedEnd + 2
	settingsEnd = settingsStart + len(settingsLabel) - 1
	return
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		// textinput-Viewport-Breite anpassen, damit Cursor-Scroll bei langen
		// Messages funktioniert. Muss VOR der nächsten Update() gesetzt sein —
		// Render-Zeit ist zu spät, weil textinput's Offset in Update() berechnet
		// wird, nicht in View().
		l := m.layout()
		m.input.Width = max(8, l.rightW-len("[Send]")-3)
		m.searchInput.Width = max(8, l.leftW-3)
		m.clampSelection()
		m.clampMessageScroll()
		m.cachedMessageRender(l)
		return m, nil

	case refreshTickMsg:
		// Live-Refresh: Chat-Liste + Messages des offenen Chats.
		// Defensiv: nur wenn kein Backwards-Paging-Fetch gerade läuft
		// (loadingMore), und Tick-Cmd reicht eine einzelne loadMessagesCmd
		// raus statt parallel mehrere zu schicken.
		// Außerdem alte "Geöffnet..."-Media-Statuszeile clearen wenn kein
		// Viewer mehr läuft — sonst hängt sie als Geister-Zeile rum.
		if m.mediaCmd == nil && strings.HasPrefix(m.mediaStatus, "Geöffnet") {
			m.mediaStatus = ""
		}
		cmds := []tea.Cmd{m.loadChatsCmd(), m.refreshTickCmd()}
		if m.currentChat != nil && !m.loadingMore {
			cmds = append(cmds, m.loadMessagesCmd(m.currentChat.ID))
		}
		return m, tea.Batch(cmds...)

	case chatOpenReloadMsg:
		// Delayed-Reload nach Chat-Öffnen: zieht spät eingegangene Messages
		// nach. Nur wenn der User noch im selben Chat ist (sonst wäre ein
		// wechselnder Chat-Inhalt verwirrend).
		if m.currentChat != nil && m.currentChat.ID == msg.chatID {
			return m, m.loadMessagesCmd(msg.chatID)
		}
		return m, nil

	case chatsLoadedMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		m.chats = msg.chats
		applyAutoSort(m.chats)
		if m.cache != nil {
			m.cache.visibleValid = false
		}
		m.clampSelection()
		// Kein Auto-Open: User landet im Kontakte-Fokus und navigiert/sucht selbst.
		return m, nil

	case messagesLoadedMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		// Defensiv: leere Antwort vom Backend (z.B. WA cache leer wegen
		// pending History-Sync) NICHT existierende Messages wegwerfen lassen.
		// Sonst flackert die Liste auf jedem 2s-Tick wenn Backend kurz hängt.
		if len(msg.messages) == 0 && len(m.messages) > 0 {
			return m, nil
		}
		isInitial := len(m.messages) == 0
		wasAtBottom := false
		if !isInitial {
			l := m.layout()
			vp := messageViewportHeight(l)
			mr := m.cachedMessageRender(l)
			wasAtBottom = m.msgScroll >= max(0, len(mr.lines)-vp)
		}
		oldCount := len(m.messages)
		m.messages = msg.messages
		// selectedMsg auf gültigen Index klemmen (alte Selection könnte jetzt
		// jenseits len(messages) liegen).
		if m.selectedMsg >= len(m.messages) {
			m.selectedMsg = len(m.messages) - 1
		}
		if isInitial {
			m.selectedMsg = len(m.messages) - 1
			m.stickMessagesToBottom()
		} else if len(m.messages) > oldCount && wasAtBottom {
			m.selectedMsg = len(m.messages) - 1
			m.stickMessagesToBottom()
		}
		return m, nil

	case messageSentMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		m.messages = append(m.messages, msg.msg)
		m.selectedMsg = len(m.messages) - 1
		m.stickMessagesToBottom()
		m.input.Reset()
		return m, nil

	case loginResultMsg:
		if msg.err != nil {
			m.err = msg.err
			m.settingsStatus = ""
			return m, nil
		}
		m.err = nil
		m.settingsStatus = msg.status
		return m, tea.Batch(m.loadFoldersCmd(), m.refreshCmd())

	case mediaOpenedMsg:
		if msg.err != nil {
			m.mediaStatus = "Fehler: " + msg.err.Error()
			return m, nil
		}
		// Vorherigen Viewer abräumen, sonst stapeln sich Prozesse.
		if m.mediaCmd != nil && m.mediaCmd.Process != nil {
			killMediaProcess(m.mediaCmd)
		}
		m.mediaCmd = msg.cmd
		// Nur Dateinamen anzeigen, nicht Full-Path. User-Erwartung: "ist offen",
		// nicht "hier liegt's auf der Platte".
		shortName := msg.path
		if idx := strings.LastIndex(shortName, "/"); idx >= 0 {
			shortName = shortName[idx+1:]
		}
		m.mediaStatus = "Geöffnet (Esc schließt): " + shortName
		return m, waitMediaCmd(msg.cmd)

	case mediaClosedMsg:
		// Race-safe: nur clearen wenn das auch der aktuell registrierte Viewer war.
		if msg.cmd == m.mediaCmd {
			m.mediaCmd = nil
			if strings.HasPrefix(m.mediaStatus, "Geöffnet") {
				m.mediaStatus = ""
			}
		}
		return m, nil

	case moreLoadedMsg:
		m.loadingMore = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		// Chat-Wechsel während Fetch → Ergebnis verwerfen.
		if m.currentChat == nil || m.currentChat.ID != msg.chatID {
			return m, nil
		}
		// Stale: zwischenzeitlich anderes Prepend → ignorieren.
		if len(m.messages) == 0 || m.messages[0].ID != msg.beforeID {
			return m, nil
		}
		if len(msg.messages) == 0 {
			m.hasMoreMessages = false
			return m, nil
		}
		addedN := len(msg.messages)
		merged := make([]telegram.Message, 0, addedN+len(m.messages))
		merged = append(merged, msg.messages...)
		merged = append(merged, m.messages...)
		m.messages = merged
		if m.selectedMsg >= 0 {
			m.selectedMsg += addedN
		}
		// Cache invalidieren, neu rendern, msgScroll auf alten obersten Eintrag
		// (= jetzt starts[addedN]) setzen damit Viewport optisch stabil bleibt.
		m.cache.msg = messageRender{}
		l := m.layout()
		mr := m.cachedMessageRender(l)
		if addedN < len(mr.starts) {
			m.msgScroll = clampMsgScroll(len(mr.lines), messageViewportHeight(l), mr.starts[addedN])
		}
		return m, nil

	case foldersLoadedMsg:
		if msg.err != nil {
			// Folders sind optional, kein UI-Error setzen.
			return m, nil
		}
		m.folders = msg.folders
		return m, nil

	case qrLoginReadyMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		m.qrLoginURL = msg.url
		m.qrASCII = msg.ascii
		switch {
		case msg.platform == telegram.PlatformWA:
			m.settingsStatus = "QR erzeugt (WhatsApp): Handy → WhatsApp → Einstellungen → Verknüpfte Geräte → Gerät verknüpfen. Code rotiert ~20s."
		case m.cfg.Backend == "gotd":
			m.settingsStatus = "QR erzeugt (gotd): in Telegram > Einstellungen > Geräte > Gerät verknüpfen scannen. Token ist nur kurz gültig."
		case m.cfg.Backend == "tdlib":
			m.settingsStatus = "QR-Login für TDLib noch nicht implementiert – bitte Terminal-Login (Phone+Code) nutzen."
		default:
			m.settingsStatus = "QR erzeugt (Mock-Demo): Telegram erkennt ihn nicht. Für echten Scan TDLib/gotd-Backend nutzen."
		}
		return m, nil

	case tea.MouseMsg:
		l := m.layout()

		// Splitter-Drag: Press auf Splitter-Spalte (Toleranz ±1) startet Drag,
		// Motion verschiebt, Release beendet.
		if msg.Button == tea.MouseButtonLeft && msg.Y >= l.bodyTop {
			switch msg.Action {
			case tea.MouseActionPress:
				if msg.X >= l.leftW-1 && msg.X <= l.leftW+1 {
					m.dragging = true
					return m, nil
				}
			case tea.MouseActionMotion:
				if m.dragging {
					m.splitX = msg.X
					l := m.layout()
					m.input.Width = max(8, l.rightW-len("[Send]")-3)
					m.searchInput.Width = max(8, l.leftW-3)
					m.clampSelection()
					m.clampMessageScroll()
					return m, nil
				}
			case tea.MouseActionRelease:
				if m.dragging {
					m.dragging = false
					return m, nil
				}
			}
		}

		if msg.Action == tea.MouseActionRelease && msg.Button == tea.MouseButtonLeft {
			if m.width > 0 && msg.Y == 0 && msg.X >= m.width-4 {
				return m, tea.Quit
			}
			if msg.Y == 0 {
				chatStart, chatEnd, feedStart, feedEnd, settingsStart, settingsEnd := m.tabRanges()
				if msg.X >= chatStart && msg.X <= chatEnd {
					m.setActiveTab(tabChats)
					return m, nil
				}
				if msg.X >= feedStart && msg.X <= feedEnd {
					m.setActiveTab(tabFeed)
					return m, nil
				}
				if msg.X >= settingsStart && msg.X <= settingsEnd {
					m.setActiveTab(tabSettings)
					return m, nil
				}
				// Folder-Tabs anklicken
				for i, r := range m.folderRanges() {
					if msg.X >= r[0] && msg.X <= r[1] && i < len(m.folders) {
						if m.currentFolder == m.folders[i].ID {
							return m, nil
						}
						m.currentFolder = m.folders[i].ID
						m.selected = 0
						m.contactScroll = 0
						return m, m.loadChatsCmd()
					}
				}
				// Platform-Tabs anklicken
				tabs := platformTabs()
				for i, r := range m.platformRanges() {
					if msg.X >= r[0] && msg.X <= r[1] && i < len(tabs) {
						if m.platformFilter == tabs[i].id {
							return m, nil
						}
						m.platformFilter = tabs[i].id
						m.selected = 0
						m.contactScroll = 0
						return m, nil
					}
				}
			}
		}

		if m.activeTab == tabSettings {
			if msg.Action == tea.MouseActionRelease && msg.Button == tea.MouseButtonLeft {
				if msg.X > l.leftW && msg.Y >= l.bodyTop {
					localY := msg.Y - l.bodyTop
					fieldsStart := 2
					fieldsEnd := fieldsStart + len(m.settingsInputs) - 1
					buttonsY := len(m.settingsInputs) + 3
					switch {
					case localY >= fieldsStart && localY <= fieldsEnd:
						m.focusSettingsField(localY - fieldsStart)
						m.focus = focusSettings
						return m, nil
					case strings.TrimSpace(m.qrASCII) == "" && localY == buttonsY:
						localX := msg.X - l.leftW - 1
						if localX >= 1 && localX <= 16 {
							return m, m.loginCmd()
						}
						if localX >= 19 && localX <= 28 {
							return m, m.startQRLoginCmd()
						}
						if localX >= 31 && localX <= 39 {
							m.settingsStatus = "Aktualisiert"
							return m, m.refreshCmd()
						}
					}
					// Klick irgendwo sonst im Settings-Bereich: nur Focus setzen.
					m.focus = focusSettings
					return m, nil
				}
			}
			return m, nil
		}

		// Mausrad bewusst nicht behandelt – nur Tastatur scrollt (j/k, ↑/↓, PgUp/PgDn).
		if msg.Action == tea.MouseActionRelease && msg.Button == tea.MouseButtonLeft {
			// Kontaktliste links: Focus auf Kontakte + Selection auf Chat.
			// Öffnet NICHT automatisch — Enter oder Doppel-Klick öffnet.
			if msg.X < l.leftW && msg.Y >= l.contactsStartY && msg.Y < l.contactsStartY+l.contactsVisible*contactRowHeight {
				idx := m.contactScroll + (msg.Y-l.contactsStartY)/contactRowHeight
				if idx >= 0 && idx < len(m.visibleChats()) {
					m.selected = idx
					m.focus = focusContacts
					m.input.Blur()
					m.clampSelection()
					return m, nil
				}
			}
			// Chat-Input-Zeile: Focus auf Chat + Input fokussieren.
			if msg.X > l.leftW && msg.Y >= l.inputY {
				m.focus = focusChat
				m.input.Focus()
				if msg.X >= m.width-8 {
					return m, m.sendCurrentInput()
				}
				return m, nil
			}
			// Chat-Bubble-Bereich (rechts oben, über dem Input): auch Chat-Focus.
			if msg.X > l.leftW && msg.Y >= l.bodyTop && msg.Y < l.inputY {
				m.focus = focusChat
				m.input.Focus()
				return m, nil
			}
		}
		return m, nil

	case tea.KeyMsg:
		// Im Suchmodus übernimmt das Search-Input fast alle Tasten.
		if m.searchMode {
			switch msg.String() {
			case "esc":
				m.searchMode = false
				m.searchInput.Blur()
				m.searchInput.Reset()
				m.clampSelection()
				return m, nil
			case "enter":
				chats := m.visibleChats()
				if m.selected >= len(chats) {
					m.searchMode = false
					m.searchInput.Blur()
					m.searchInput.Reset()
					return m, nil
				}
				targetID := chats[m.selected].ID
				m.searchMode = false
				m.searchInput.Blur()
				m.searchInput.Reset()
				// Bug-Fix: m.selected ist Index in visibleChats(), NICHT in
				// m.chats. Nach Verlassen des Suchmodus die Position des
				// Treffers in der jetzt aktuellen visibleChats()-Slice (ohne
				// Suchfilter, aber mit Platform/Tab-Filter) finden.
				newVisible := m.visibleChats()
				for i, c := range newVisible {
					if c.ID == targetID {
						m.selected = i
						break
					}
				}
				m.clampSelection()
				return m, m.openSelectedChat()
			case "up", "ctrl+k":
				if m.selected > 0 {
					m.selected--
					m.clampSelection()
				}
				return m, nil
			case "down", "ctrl+j":
				if m.selected < len(m.visibleChats())-1 {
					m.selected++
					m.clampSelection()
				}
				return m, nil
			}
			m.searchInput, cmd = m.searchInput.Update(msg)
			m.selected = 0
			m.contactScroll = 0
			return m, cmd
		}

		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "q":
			if m.activeTab == tabChats && m.focus == focusChat {
				// q im Chat-Input darf nicht beenden, sondern als Buchstabe ankommen.
				break
			}
			return m, tea.Quit
		case "ctrl+p":
			// Platform-Filter cyclen: Alle → TG → WA → Alle. Nur wenn WA aktiv.
			if !m.cfg.EnableWhatsApp {
				return m, nil
			}
			tabs := platformTabs()
			cur := 0
			for i, t := range tabs {
				if t.id == m.platformFilter {
					cur = i
					break
				}
			}
			m.platformFilter = tabs[(cur+1)%len(tabs)].id
			m.selected = 0
			m.contactScroll = 0
			return m, nil
		case "ctrl+f":
			// Ctrl+F: globaler Suchstart — egal ob Focus auf Kontakten oder
			// Chat-Input liegt. Wechselt Tab → Chats, Fokus → Kontakte.
			if m.activeTab != tabChats {
				m.setActiveTab(tabChats)
			}
			m.focus = focusContacts
			m.input.Blur()
			m.searchMode = true
			m.searchInput.Focus()
			m.searchInput.Reset()
			m.selected = 0
			m.contactScroll = 0
			return m, nil
		case "s":
			if m.activeTab == tabChats && m.focus == focusContacts {
				m.searchMode = true
				m.searchInput.Focus()
				m.searchInput.Reset()
				m.selected = 0
				m.contactScroll = 0
				return m, nil
			}
		case "ctrl+r":
			m.settingsStatus = "Aktualisiert"
			return m, m.refreshCmd()
		case "r":
			// Im Chat-Input ist r ein normaler Buchstabe — nicht als
			// Refresh-Hotkey verwenden. Nur auf Kontaktliste/Settings
			// triggern.
			if m.activeTab == tabChats && m.focus == focusChat {
				break
			}
			m.settingsStatus = "Aktualisiert"
			return m, m.refreshCmd()
		case "tab":
			if m.activeTab == tabSettings {
				m.focusSettingsField((m.settingsField + 1) % len(m.settingsInputs))
				return m, nil
			}
			if m.focus == focusContacts {
				m.focus = focusChat
				m.input.Focus()
			} else {
				m.focus = focusContacts
				m.input.Blur()
			}
			return m, nil
		case "shift+tab":
			if m.activeTab == tabSettings {
				next := m.settingsField - 1
				if next < 0 {
					next = len(m.settingsInputs) - 1
				}
				m.focusSettingsField(next)
				return m, nil
			}
			if m.focus == focusChat {
				m.focus = focusContacts
				m.input.Blur()
			} else {
				m.focus = focusChat
				m.input.Focus()
			}
			return m, nil
		case "esc":
			if m.mediaCmd != nil && m.mediaCmd.Process != nil {
				killMediaProcess(m.mediaCmd)
				m.mediaCmd = nil
				m.mediaStatus = "Viewer geschlossen"
				return m, nil
			}
			if m.activeTab == tabSettings {
				m.setActiveTab(tabChats)
				m.focus = focusContacts
				return m, nil
			}
			m.focus = focusContacts
			m.input.Blur()
			return m, nil
		case "f1":
			m.setActiveTab(tabChats)
			return m, nil
		case "f2":
			m.setActiveTab(tabFeed)
			return m, nil
		case "f3":
			m.setActiveTab(tabSettings)
			return m, nil
		case "f7":
			m.setActiveTab(tabSettings)
			return m, m.startQRLoginCmd()
		case "f4", "ctrl+w":
			m.setActiveTab(tabSettings)
			return m, m.startWAQRLoginCmd()
		}

		if m.activeTab == tabSettings {
			switch msg.String() {
			case "up", "k":
				m.focusSettingsField(m.settingsField - 1)
				return m, nil
			case "down", "j":
				m.focusSettingsField(m.settingsField + 1)
				return m, nil
			case "enter", "ctrl+l":
				return m, m.loginCmd()
			case "ctrl+g":
				return m, m.startQRLoginCmd()
			}
		}

		if m.activeTab == tabChats {
			l := m.layout()
			vp := messageViewportHeight(l)
			mr := m.cachedMessageRender(l)
			m.msgScroll = clampMsgScroll(len(mr.lines), vp, m.msgScroll)
			switch msg.String() {
			case "up":
				if m.focus == focusContacts {
					if m.selected > 0 {
						m.selected--
						m.clampSelection()
					}
					return m, nil
				}
				m.moveSelectedMsg(-1, mr.starts, vp)
				return m, m.maybeLoadMoreCmd()
			case "down":
				if m.focus == focusContacts {
					if m.selected < len(m.visibleChats())-1 {
						m.selected++
						m.clampSelection()
					}
				} else {
					m.moveSelectedMsg(+1, mr.starts, vp)
				}
				return m, nil
			case "ctrl+o":
				if m.focus == focusChat && m.selectedMsg >= 0 && m.selectedMsg < len(m.messages) {
					sel := m.messages[m.selectedMsg]
					if sel.Media != nil {
						m.mediaStatus = "Lade " + sel.Media.Kind.Label() + "…"
						return m, m.openMediaCmd(*sel.Media)
					}
					m.mediaStatus = "Keine Mediendatei"
				}
				return m, nil
			case "k":
				if m.focus == focusContacts {
					if m.selected > 0 {
						m.selected--
						m.clampSelection()
					}
					return m, nil
				}
				// In focusChat fällt 'k' als Buchstabe durch zu input.Update.
			case "j":
				if m.focus == focusContacts {
					if m.selected < len(m.visibleChats())-1 {
						m.selected++
						m.clampSelection()
					}
					return m, nil
				}
			case "pgup", "ctrl+u":
				m.msgScroll -= vp
				if m.msgScroll < 0 {
					m.msgScroll = 0
				}
				// Wenn Viewport oben anschlägt, nach Backwards-Page fragen.
				if m.msgScroll == 0 && m.focus == focusChat {
					return m, m.maybeLoadMoreCmd()
				}
				return m, nil
			case "pgdown", "ctrl+d":
				m.msgScroll += vp
				return m, nil
			case "home":
				m.msgScroll = 0
				return m, nil
			case "end":
				// Sehr hoher Wert → in View geclampt auf len(rendered)-viewport.
				m.msgScroll = 1 << 30
				return m, nil
			case "enter":
				if m.focus == focusContacts {
					return m, m.openSelectedChat()
				}
				// focusChat + leerer Input + selektierte Media → Viewer.
				// Mit Text im Input → ganz normal senden.
				if strings.TrimSpace(m.input.Value()) == "" &&
					m.selectedMsg >= 0 && m.selectedMsg < len(m.messages) {
					sel := m.messages[m.selectedMsg]
					if sel.Media != nil {
						m.mediaStatus = "Lade " + sel.Media.Kind.Label() + "…"
						return m, m.openMediaCmd(*sel.Media)
					}
				}
				return m, m.sendCurrentInput()
			case "ctrl+s":
				return m, m.sendCurrentInput()
			}
		}
	}

	if m.activeTab == tabSettings {
		m.settingsInputs[m.settingsField], cmd = m.settingsInputs[m.settingsField].Update(msg)
		return m, cmd
	}

	if m.activeTab == tabChats && m.focus == focusChat {
		if key, ok := msg.(tea.KeyMsg); ok && looksLikeMouseEscape(key) {
			// Rohe SGR-Mouse-Sequenzen (z.B. "[<64;26;22M") nicht ins Textinput
			// rutschen lassen – sie sind eine Fehlanzeige des Terminals.
			return m, nil
		}
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}

	return m, nil
}

// looksLikeMouseEscape erkennt Bruchstücke einer SGR-Mouse-Escape-Sequenz die
// trotz aktivem Mouse-Mode als KeyMsg durchrutschen können.
// Format: [optional "["] "<" <btn> ";" <col> ";" <row> ("M"|"m")
func looksLikeMouseEscape(k tea.KeyMsg) bool {
	s := k.String()
	if s == "" {
		return false
	}
	s = strings.TrimPrefix(s, "[")
	if !strings.HasPrefix(s, "<") {
		return false
	}
	if len(s) < 7 {
		return false
	}
	last := s[len(s)-1]
	if last != 'M' && last != 'm' {
		return false
	}
	// SGR-Mouse hat genau zwei Semikolons: <btn;col;rowM
	if strings.Count(s, ";") < 2 {
		return false
	}
	// Mittelteil sollte überwiegend aus Ziffern bestehen.
	mid := s[1 : len(s)-1]
	for _, r := range mid {
		if r != ';' && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func (m Model) renderTabs() string {
	chatTab := tabStyle.Render("[Chats]")
	feedTab := tabStyle.Render("[Aktuelles]")
	settingsTab := tabStyle.Render("[Settings]")
	switch m.activeTab {
	case tabChats:
		chatTab = tabActiveStyle.Render("[Chats]")
	case tabFeed:
		feedTab = tabActiveStyle.Render("[Aktuelles]")
	case tabSettings:
		settingsTab = tabActiveStyle.Render("[Settings]")
	}
	out := chatTab + " " + feedTab + " " + settingsTab
	if m.activeTab != tabChats {
		return out
	}
	if len(m.folders) > 0 {
		out += mutedStyle.Render(" · ")
		for _, f := range m.folders {
			label := fmt.Sprintf("[%s]", truncateLine(f.Title, 14))
			styled := tabStyle.Render(label)
			if f.ID == m.currentFolder {
				styled = tabActiveStyle.Render(label)
			}
			out += styled + " "
		}
		out = strings.TrimRight(out, " ")
	}
	if m.cfg.EnableWhatsApp {
		out += mutedStyle.Render(" · ")
		for _, p := range platformTabs() {
			styled := tabStyle.Render(p.label)
			if m.platformFilter == p.id {
				styled = tabActiveStyle.Render(p.label)
			}
			out += styled + " "
		}
		out = strings.TrimRight(out, " ")
	}
	return out
}

type platformTab struct {
	id    string // "", "tg", "wa"
	label string
}

func platformTabs() []platformTab {
	return []platformTab{
		{id: "", label: "[Alle]"},
		{id: telegram.PlatformTG, label: "[TG]"},
		{id: telegram.PlatformWA, label: "[WA]"},
	}
}

// folderRanges berechnet [start,end]-X-Spalten pro Folder-Tab im Header.
// Reine Funktion, kein State-Mutation – kann von Update() bei jedem Mouse-Klick
// neu berechnet werden.
func (m Model) folderRanges() [][2]int {
	if m.activeTab != tabChats || len(m.folders) == 0 {
		return nil
	}
	// Header-Aufbau exakt wie renderTabs():
	// "<appname> · [Chats] [Settings] · [Folder1] [Folder2] …"
	x := lipgloss.Width(m.appLabel()) + 3 // appname + " · "
	x += lipgloss.Width("[Chats] [Aktuelles] [Settings]")
	x += 3 // " · " vor den Folder-Tabs
	ranges := make([][2]int, 0, len(m.folders))
	for _, f := range m.folders {
		label := fmt.Sprintf("[%s]", truncateLine(f.Title, 14))
		w := lipgloss.Width(label)
		ranges = append(ranges, [2]int{x, x + w - 1})
		x += w + 1 // +1 für Trenn-Space zwischen Tabs
	}
	return ranges
}

// platformRanges berechnet [start,end]-X-Spalten pro Platform-Tab. Liegt im
// Header rechts neben den Folder-Tabs. Liefert nil wenn WA nicht aktiv.
func (m Model) platformRanges() [][2]int {
	if m.activeTab != tabChats || !m.cfg.EnableWhatsApp {
		return nil
	}
	x := lipgloss.Width(m.cfg.AppName) + 3
	x += lipgloss.Width("[Chats] [Aktuelles] [Settings]")
	if len(m.folders) > 0 {
		x += 3
		for _, f := range m.folders {
			label := fmt.Sprintf("[%s]", truncateLine(f.Title, 14))
			x += lipgloss.Width(label) + 1
		}
		x-- // letztes Trenn-Space wurde durch TrimRight entfernt
	}
	x += 3 // " · " vor Platform-Tabs
	tabs := platformTabs()
	ranges := make([][2]int, 0, len(tabs))
	for _, t := range tabs {
		w := lipgloss.Width(t.label)
		ranges = append(ranges, [2]int{x, x + w - 1})
		x += w + 1
	}
	return ranges
}

func (m Model) View() string {
	defer profTime("View")()
	if m.width <= 20 || m.height <= 8 {
		return "telegrim: Fenster zu klein"
	}
	l := m.layout()

	tabs := m.renderTabs()
	headerLeft := titleStyle.Render(fmt.Sprintf("%s · %s", m.appLabel(), tabs))
	header := fitToWidth(headerLeft+strings.Repeat(" ", max(1, m.width-lipgloss.Width(headerLeft)-4))+closeStyle.Render("[X]"), m.width)

	contactsDone := profTime("contactsLines")
	leftLines := m.cachedContactsLines(l)
	contactsDone()
	chatDone := profTime("renderChatLines")
	rightLines := m.renderChatLines(l)
	chatDone()
	if m.activeTab == tabSettings {
		rightLines = m.renderSettingsLines(l)
	}
	splitStyle := splitterStyle
	if m.dragging {
		splitStyle = splitterActiveStyle
	}
	splitChar := splitStyle.Render("┃")

	// Fokus-Rahmen: vollständige Box um die aktive Spalte (Aussenkanten
	// vertikal + Top/Bottom-Linien). Kontakte-Fokus → linker Block;
	// Chat-/Settings-Fokus → rechter Block.
	leftFocused := m.focus == focusContacts
	rightFocused := m.focus == focusChat || m.focus == focusSettings
	// Platform-Akzent: WA-Chat selected/offen → grün, sonst TG-blau.
	focusStyle := focusBarStyle
	if m.platformOfCurrentSelection() == telegram.PlatformWA {
		focusStyle = focusBarWAStyle
	}
	leftBarChar := idleBarStyle.Render("▎")
	rightBarChar := idleBarStyle.Render("▕")
	if leftFocused {
		leftBarChar = focusStyle.Render("▎")
	}
	if rightFocused {
		rightBarChar = focusStyle.Render("▕")
	}
	// Top/Bottom-Frame-Zeilen. l.leftW + l.rightW sind bereits die inneren
	// Inhalts-Breiten (layout subtrahiert die 3 Frame-Spalten).
	makeFrame := func(top bool) string {
		leftCornerL, leftCornerR := "▎", " "
		rightCornerL, rightCornerR := " ", "▕"
		if leftFocused {
			if top {
				leftCornerL = "▛"
				leftCornerR = "▜"
			} else {
				leftCornerL = "▙"
				leftCornerR = "▟"
			}
		}
		if rightFocused {
			if top {
				rightCornerL = "▛"
				rightCornerR = "▜"
			} else {
				rightCornerL = "▙"
				rightCornerR = "▟"
			}
		}
		leftBlock := leftCornerL + strings.Repeat("▔", max(0, l.leftW-2)) + leftCornerR
		rightBlock := rightCornerL + strings.Repeat("▔", max(0, l.rightW-2)) + rightCornerR
		if !top {
			leftBlock = leftCornerL + strings.Repeat("▁", max(0, l.leftW-2)) + leftCornerR
			rightBlock = rightCornerL + strings.Repeat("▁", max(0, l.rightW-2)) + rightCornerR
		}
		if !leftFocused {
			leftBlock = strings.Repeat(" ", l.leftW)
		} else {
			leftBlock = focusStyle.Render(leftBlock)
		}
		if !rightFocused {
			rightBlock = strings.Repeat(" ", l.rightW)
		} else {
			rightBlock = focusStyle.Render(rightBlock)
		}
		return idleBarStyle.Render(" ") + leftBlock + " " + rightBlock + idleBarStyle.Render(" ")
	}
	// Frame-Strings cachen — Repeat() ist nicht gratis und ändert sich nur
	// bei Resize/Fokus-Wechsel.
	fkey := frameCacheKey{
		leftW: l.leftW, rightW: l.rightW,
		leftFocused: leftFocused, rightFocused: rightFocused,
		wa: m.platformOfCurrentSelection() == telegram.PlatformWA,
	}
	var topFrame, bottomFrame string
	if frameCacheRef.valid && frameCacheRef.key == fkey {
		topFrame = frameCacheRef.top
		bottomFrame = frameCacheRef.bot
	} else {
		topFrame = makeFrame(true)
		bottomFrame = makeFrame(false)
		frameCacheRef = frameCacheEntry{key: fkey, top: topFrame, bot: bottomFrame, valid: true}
	}

	// strings.Builder aus dem Pool — spart Buffer-Allokation pro Frame.
	sb := getBuilder()
	defer putBuilder(sb)
	bodyLines := make([]string, 0, l.bodyH)
	for i := 0; i < l.bodyH; i++ {
		left := ""
		right := ""
		if i < len(leftLines) {
			left = leftLines[i]
		}
		if i < len(rightLines) {
			right = rightLines[i]
		}
		sb.Reset()
		sb.WriteString(leftBarChar)
		sb.WriteString(fitToWidth(left, l.leftW))
		sb.WriteString(splitChar)
		sb.WriteString(fitToWidth(right, l.rightW))
		sb.WriteString(rightBarChar)
		bodyLines = append(bodyLines, sb.String())
	}

	footer := mutedStyle.Render("Ctrl+F suche · Ctrl+P TG/WA · Enter öffnen · Ctrl+O Media · F1 Chats · F2 Aktuelles · F3 Settings · F4 WA-QR · F7 TG-QR · Ctrl+R Refresh · Q quit")
	if m.mediaStatus != "" {
		footer = mediaLineStyle.Render(m.mediaStatus)
	}
	if m.err != nil {
		footer = errorStyle.Render("error: " + m.err.Error())
	}

	return strings.Join([]string{header, topFrame, strings.Join(bodyLines, "\n"), bottomFrame, fitToWidth(footer, m.width)}, "\n")
}

func (m Model) renderContactsLines(l uiLayout) []string {
	lines := make([]string, 0, l.bodyH)
	chats := m.visibleChats()
	if m.searchMode {
		// Search-Input ersetzt den Title: zeigt Eingabe + Treffer-Anzahl.
		input := fitToWidth(" "+m.searchInput.View(), l.leftW)
		lines = append(lines, titleStyle.Render(input))
	} else {
		title := " Kontakte"
		if len(chats) > 0 {
			title = fmt.Sprintf(" Kontakte (%d/%d)", m.selected+1, len(chats))
		}
		if m.focus == focusContacts && m.activeTab == tabChats {
			title = titleStyle.Render(title + " •")
		} else {
			title = titleStyle.Render(title)
		}
		lines = append(lines, fitToWidth(title, l.leftW))
	}
	// Scroll-Indikator: zeigt an, ob oberhalb noch Einträge sind.
	headerBar := strings.Repeat("─", l.leftW)
	if m.contactScroll > 0 {
		marker := fmt.Sprintf(" ▲ %d weitere ", m.contactScroll)
		headerBar = padCenter(marker, l.leftW, "─")
	}
	lines = append(lines, mutedStyle.Render(headerBar))

	sep := contactSepStyle.Render(strings.Repeat("╌", l.leftW))
	// Separator-Zeile zwischen unread und read. Tritt einmal auf wenn beide
	// Sektionen nicht-leer sind und im sichtbaren Fenster liegen.
	boundary := unreadBoundary(chats)
	showSep := boundary > 0 && boundary < len(chats)
	separatorEmitted := false
	chatIdx := m.contactScroll
	for row := 0; row < l.contactsVisible && len(lines) < l.bodyH; row++ {
		if showSep && !separatorEmitted && chatIdx == boundary {
			markerText := " ─── gelesen "
			marker := padCenter(markerText, l.leftW, "─")
			lines = append(lines, mutedStyle.Render(marker), "", sep)
			separatorEmitted = true
			continue
		}
		if chatIdx >= len(chats) {
			lines = append(lines, "", "", "")
			continue
		}
		chat := chats[chatIdx]
		avatar := avatarIcon(chat.Title)
		badge := unreadBadge(chat.Unread)
		dot := " "
		if chat.Online {
			dot = onlineDotStyle.Render("●")
		}
		platIcon := platformPrefix(chat.Platform)
		// Avatar ist 5 Display-Spalten, plus Dot, plus 1 Space davor und 1 hinter dem Namen.
		nameRoom := max(1, l.leftW-lipgloss.Width(avatar)-lipgloss.Width(badge)-lipgloss.Width(platIcon)-4)
		name := truncateLine(chat.Title, nameRoom)
		titleLine := fitToWidth(fmt.Sprintf(" %s%s%s %s %s", platIcon, avatar, dot, name, badge), l.leftW)
		previewRoom := max(1, l.leftW-5)
		preview := fitToWidth("    "+mutedStyle.Render(truncateLine(chat.Last, previewRoom)), l.leftW)
		if chatIdx == m.selected {
			titleLine = selectedStyle.Render(fitToWidth(fmt.Sprintf("▶ %s%s%s %s %s", platIcon, avatar, dot, name, badge), l.leftW))
			preview = selectedMetaStyle.Render(fitToWidth("   "+truncateLine(chat.Last, previewRoom), l.leftW))
		}
		lines = append(lines, titleLine, preview, sep)
		chatIdx++
	}
	// Footer-Indikator: weitere Einträge unterhalb des sichtbaren Fensters.
	remaining := len(chats) - (m.contactScroll + l.contactsVisible)
	if remaining > 0 {
		marker := fmt.Sprintf(" ▼ %d weitere ", remaining)
		bar := padCenter(marker, l.leftW, "─")
		// Letzte Body-Zeile durch Footer ersetzen (statt anhängen).
		for len(lines) < l.bodyH-1 {
			lines = append(lines, "")
		}
		lines = append(lines, mutedStyle.Render(bar))
	}
	for len(lines) < l.bodyH {
		lines = append(lines, "")
	}
	return lines
}

// padCenter umrahmt label mit fill-chars links/rechts auf width w.
func padCenter(label string, w int, fill string) string {
	lw := lipgloss.Width(label)
	if lw >= w {
		return label
	}
	leftPad := (w - lw) / 2
	rightPad := w - lw - leftPad
	return strings.Repeat(fill, leftPad) + label + strings.Repeat(fill, rightPad)
}

func (m Model) renderChatLines(l uiLayout) []string {
	lines := make([]string, 0, l.bodyH)
	header := " Chat wählen…"
	if m.currentChat != nil {
		avatar := avatarIcon(m.currentChat.Title)
		dot := ""
		statusMeta := ""
		if m.currentChat.IsPrivate {
			if m.currentChat.Online {
				dot = onlineDotStyle.Render("●") + " "
				statusMeta = "online"
			} else {
				statusMeta = "offline"
			}
		}
		nameTxt := chatHeaderName.Render(m.currentChat.Title)
		if statusMeta != "" {
			nameTxt += "  " + chatHeaderMeta.Render(statusMeta)
		}
		focusMark := ""
		if m.focus == focusChat && m.activeTab == tabChats {
			focusMark = titleStyle.Render(" •")
		}
		header = " " + avatar + " " + dot + nameTxt + focusMark
	} else if m.focus == focusChat && m.activeTab == tabChats {
		header = titleStyle.Render(" Chat •")
	} else {
		header = titleStyle.Render(" Chat")
	}
	lines = append(lines, fitToWidth(header, l.rightW))
	lines = append(lines, mutedStyle.Render(strings.Repeat("─", l.rightW)))

	msgAreaH := messageViewportHeight(l)
	if m.currentChat == nil {
		lines = append(lines, mutedStyle.Render(" Chat wählen…"))
		for len(lines) < 2+msgAreaH {
			lines = append(lines, "")
		}
	} else {
		mr := m.cachedMessageRender(l)
		start := clampMsgScroll(len(mr.lines), msgAreaH, m.msgScroll)
		end := min(len(mr.lines), start+msgAreaH)
		for i := start; i < end; i++ {
			lines = append(lines, mr.lines[i])
		}
		for len(lines) < 2+msgAreaH {
			lines = append(lines, "")
		}
	}

	lines = append(lines, mutedStyle.Render(strings.Repeat("─", l.rightW)))
	send := sendStyle.Render("[Send]")
	// textinput-Viewport breite setzen, sonst wächst der String unsichtbar
	// nach rechts wenn lange Messages getippt werden. -3 für Prompt "> "
	// und Sicherheits-Padding.
	inputW := max(8, l.rightW-lipgloss.Width(send)-3)
	m.input.Width = inputW
	inputLine := m.input.View()
	if m.focus != focusChat || m.activeTab != tabChats {
		inputLine = mutedStyle.Render(m.input.Value())
		if strings.TrimSpace(m.input.Value()) == "" {
			inputLine = mutedStyle.Render(m.input.Placeholder)
		}
		inputLine = "> " + inputLine
	}
	line := fitToWidth(inputLine, max(1, l.rightW-lipgloss.Width(send)-1)) + " " + send
	lines = append(lines, fitToWidth(line, l.rightW))
	lines = append(lines, mutedStyle.Render(" Enter/Ctrl+S senden · keine Sticker/GIFs"))

	for len(lines) < l.bodyH {
		lines = append(lines, "")
	}
	if len(lines) > l.bodyH {
		lines = lines[:l.bodyH]
	}
	return lines
}

func (m Model) renderSettingsLines(l uiLayout) []string {
	lines := make([]string, 0, l.bodyH)
	title := titleStyle.Render(" Einstellungen •")
	lines = append(lines, fitToWidth(title, l.rightW))
	lines = append(lines, mutedStyle.Render(strings.Repeat("─", l.rightW)))

	status := m.settingsStatus
	if strings.TrimSpace(status) == "" {
		status = backendInitialHint(m.cfg.Backend)
	}

	if strings.TrimSpace(m.qrASCII) != "" {
		lines = append(lines, fitToWidth(" "+mutedStyle.Render(status), l.rightW))
		if strings.TrimSpace(m.qrLoginURL) != "" {
			lines = append(lines, fitToWidth(" URL: "+m.qrLoginURL, l.rightW))
		}
		lines = append(lines, mutedStyle.Render(strings.Repeat("─", l.rightW)))
		footer := fitToWidth(" F1=Chats · F2=Settings · Ctrl+L=Terminal Login · F3=QR neu", l.rightW)

		qrASCII := strings.TrimRight(m.qrASCII, "\n")
		qrLines := strings.Split(qrASCII, "\n")
		qrWidth := 0
		for _, qrLine := range qrLines {
			if w := lipgloss.Width(qrLine); w > qrWidth {
				qrWidth = w
			}
		}
		availableQRLines := l.bodyH - len(lines) - 1
		if availableQRLines < 0 {
			availableQRLines = 0
		}

		if qrWidth+1 > l.rightW {
			warn := fmt.Sprintf(" QR passt nicht: Terminal zu schmal (QR %d Zeichen, Bereich %d). Bitte Fenster breiter machen und F3 neu.", qrWidth+1, l.rightW)
			lines = append(lines, fitToWidth(warn, l.rightW))
			lines = append(lines, footer)
		} else if len(qrLines) > availableQRLines {
			warn := fmt.Sprintf(" QR passt nicht: Terminal zu niedrig (QR %d Zeilen, frei %d). Bitte Fenster höher machen und F3 neu.", len(qrLines), availableQRLines)
			lines = append(lines, fitToWidth(warn, l.rightW))
			lines = append(lines, footer)
		} else {
			for _, qrLine := range qrLines {
				lines = append(lines, fitToWidth(" "+qrLine, l.rightW))
			}
			lines = append(lines, footer)
		}
	} else {
		for i := range m.settingsInputs {
			lines = append(lines, fitToWidth(m.settingsInputs[i].View(), l.rightW))
		}
		lines = append(lines, mutedStyle.Render(strings.Repeat("─", l.rightW)))
		buttons := " " + loginStyle.Render("[Terminal Login]") + "  " + qrStyle.Render("[QR Login]") + "  " + refreshStyle.Render("[Refresh]")
		lines = append(lines, fitToWidth(buttons, l.rightW))
		lines = append(lines, fitToWidth(" "+mutedStyle.Render(status), l.rightW))
		if strings.TrimSpace(m.qrLoginURL) != "" {
			lines = append(lines, fitToWidth(" URL: "+m.qrLoginURL, l.rightW))
		}
		lines = append(lines, fitToWidth(" F1=Chats · F2=Settings · Ctrl+L=Terminal Login · F3=QR · Ctrl+G QR", l.rightW))
	}

	for len(lines) < l.bodyH {
		lines = append(lines, "")
	}
	if len(lines) > l.bodyH {
		lines = lines[:l.bodyH]
	}
	return lines
}

func backendLoginSuccessLabel(backend string) string {
	switch backend {
	case "tdlib":
		return "Eingeloggt (TDLib). Chats werden geladen."
	case "gotd":
		return "Terminal-Login erfolgreich (gotd)."
	default:
		return "Terminal-Login erfolgreich (Mock)."
	}
}

func backendInitialHint(backend string) string {
	switch backend {
	case "tdlib":
		return "TDLib-Login: API ID/Hash + Phone eintragen, [Terminal Login] → Telegram schickt Code → Code-Feld füllen, [Terminal Login] nochmal. Bei 2FA dann auch Passwort."
	case "gotd":
		return "Terminal-Login (gotd): API ID/API Hash + Phone + Code eingeben, dann Terminal Login klicken."
	default:
		return "Mock-Backend: beliebige Werte → Terminal Login."
	}
}

// avatarPalette: kräftige, gut unterscheidbare Farben (HSL gut verteilt).
var avatarPalette = []lipgloss.Color{
	"#e74c3c", "#3498db", "#2ecc71", "#9b59b6",
	"#e67e22", "#1abc9c", "#f39c12", "#16a085",
	"#27ae60", "#2980b9", "#8e44ad", "#c0392b",
	"#d35400", "#7f8c8d", "#34495e", "#f1c40f",
}

// Vor-instanziierte Styles pro Palette-Farbe → kein Style-Bau pro Render-Tick.
var avatarStyles = func() []lipgloss.Style {
	out := make([]lipgloss.Style, len(avatarPalette))
	for i, c := range avatarPalette {
		out[i] = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#ffffff")).
			Background(c).
			Bold(true).
			Padding(0, 2)
	}
	return out
}()

// avatarCache speichert gerenderte Avatare per Seed (Titel). Wachstum begrenzt durch
// reset bei zu vielen Einträgen – verhindert unbeschränktes Wachsen.
var avatarCache sync.Map // map[string]string

// platformOfCurrentSelection liefert "tg" oder "wa" basierend auf dem gerade
// offenen Chat, oder — falls keiner offen — auf dem markierten Kontakt in der
// Liste. Leer wenn nichts da. Wird für platform-spezifische UI-Akzente genutzt.
func (m Model) platformOfCurrentSelection() string {
	if m.currentChat != nil && m.currentChat.Platform != "" {
		return m.currentChat.Platform
	}
	chats := m.visibleChats()
	if m.selected >= 0 && m.selected < len(chats) {
		return chats[m.selected].Platform
	}
	return ""
}

// sbPool: strings.Builder Recyling. Hot-path Render-Allokationen schlucken
// sonst pro Frame Kilobyte → GC-Druck. Reset+Put nach Gebrauch.
var sbPool = sync.Pool{
	New: func() any { return new(strings.Builder) },
}

func getBuilder() *strings.Builder {
	sb := sbPool.Get().(*strings.Builder)
	sb.Reset()
	return sb
}

func putBuilder(sb *strings.Builder) {
	// Riesige Buffer NICHT zurück in den Pool — sonst hält ein einzelner
	// 10MB-Frame den Speicher dauerhaft hoch. 64KB-Grenze.
	if sb.Cap() > 64*1024 {
		return
	}
	sbPool.Put(sb)
}

// frameCache memoized die top/bottom-Frame-Strings einer View. Sie hängen nur
// von leftW/rightW + Fokus-Seite + Platform-Akzent ab — ändert sich kaum
// zwischen Frames, jedes Repeat() würde sonst alle 2s laufen.
type frameCacheEntry struct {
	key   frameCacheKey
	top   string
	bot   string
	valid bool
}
type frameCacheKey struct {
	leftW, rightW int
	leftFocused   bool
	rightFocused  bool
	wa            bool // Platform-Akzent (TG-blau vs WA-grün)
}

var frameCacheRef frameCacheEntry

// statusGlyph rendert den Lesestatus einer ausgehenden Nachricht als
// Check-Mark — analog WhatsApp: ✓ gesendet, ✓✓ geliefert, ✓✓ blau gelesen.
func statusGlyph(s telegram.MessageStatus) string {
	switch s {
	case telegram.MessageStatusPending:
		return "⧗"
	case telegram.MessageStatusDelivered:
		return "✓✓"
	case telegram.MessageStatusRead:
		return readTickStyle.Render("✓✓")
	default:
		return "✓"
	}
}

// platformPrefix liefert das Plattform-Icon (inkl. trailing space) zum Voran-
// stellen vor den Chat-Namen. Leerer Platform-String → "" (TG-only Setup).
func platformPrefix(platform string) string {
	switch platform {
	case telegram.PlatformTG:
		return "🟦 "
	case telegram.PlatformWA:
		return "🟢 "
	default:
		return ""
	}
}

func avatarIcon(seed string) string {
	if v, ok := avatarCache.Load(seed); ok {
		return v.(string)
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(seed))
	idx := int(h.Sum64()&0xFFFF) % len(avatarStyles)
	letter := initialLetter(seed)
	rendered := avatarStyles[idx].Render(letter)
	avatarCache.Store(seed, rendered)
	return rendered
}

// initialLetter zieht den ersten Buchstaben/Ziffer aus dem Titel als Großbuchstaben.
// Emojis oder Sonderzeichen werden übersprungen; Fallback "?".
func initialLetter(title string) string {
	for _, r := range title {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return strings.ToUpper(string(r))
		}
	}
	return "?"
}

func unreadBadge(unread int) string {
	if unread <= 0 {
		return ""
	}
	label := fmt.Sprintf("%d", unread)
	if unread > 99 {
		label = "99+"
	}
	return lipgloss.NewStyle().
		Foreground(lipgloss.Color("#1e1e2e")).
		Background(lipgloss.Color("#f38ba8")).
		Bold(true).
		Padding(0, 1).
		Render(label)
}

func fitToWidth(s string, w int) string {
	if w < 1 {
		return ""
	}
	// Hart auf eine Zeile zwingen – sonst kann Wrap im Body-Composer ganze
	// Zeilen verschieben (Avatar/Title/Preview rutschen in den nächsten Slot).
	s = oneLine(s)
	cw := lipgloss.Width(s)
	if cw == w {
		return s
	}
	if cw < w {
		return s + strings.Repeat(" ", w-cw)
	}
	// cw > w: ANSI-aware hart clippen, kein Wrap.
	return lipgloss.NewStyle().MaxWidth(w).Render(s)
}

// oneLine ersetzt alle vertikalen Whitespaces durch Spaces, damit ein String
// garantiert in eine einzelne Display-Zeile passt.
func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, s)
}

// formatMessageTime liefert "HH:MM" für die Nachricht. Leer wenn Date=0.
func formatMessageTime(date int32) string {
	if date <= 0 {
		return ""
	}
	return time.Unix(int64(date), 0).Local().Format("15:04")
}

// formatMessageDay liefert ein menschenlesbares Tageslabel ("Heute", "Gestern",
// sonst "2. Jan" / "2. Jan 2024"). Leer wenn Date=0.
func formatMessageDay(date int32) string {
	if date <= 0 {
		return ""
	}
	t := time.Unix(int64(date), 0).Local()
	now := time.Now().Local()
	y1, m1, d1 := t.Date()
	y2, m2, d2 := now.Date()
	if y1 == y2 && m1 == m2 && d1 == d2 {
		return "Heute"
	}
	yest := now.AddDate(0, 0, -1)
	y3, m3, d3 := yest.Date()
	if y1 == y3 && m1 == m3 && d1 == d3 {
		return "Gestern"
	}
	if y1 == y2 {
		return t.Format("2. Jan")
	}
	return t.Format("2. Jan 2006")
}

// centerInWidth zentriert label in einem Bereich der Breite w mit Spaces drumherum.
func centerInWidth(label string, w int) string {
	lw := lipgloss.Width(label)
	if lw >= w {
		return label
	}
	left := (w - lw) / 2
	right := w - lw - left
	return strings.Repeat(" ", left) + label + strings.Repeat(" ", right)
}

// wrapWords bricht einen Text an Wortgrenzen auf maximal w Display-Spalten um.
// Wörter, die einzeln länger als w sind, werden hart in w-breite Stücke geschnitten.
func wrapWords(s string, w int) []string {
	if w < 1 {
		return []string{s}
	}
	var lines []string
	cur := ""
	flush := func() {
		if cur != "" {
			lines = append(lines, cur)
			cur = ""
		}
	}
	for _, word := range strings.Fields(s) {
		ww := lipgloss.Width(word)
		// Wort selbst zu breit: hart in Stücke schneiden.
		if ww > w {
			flush()
			r := []rune(word)
			for len(r) > 0 {
				chunk := r
				for lipgloss.Width(string(chunk)) > w {
					chunk = chunk[:len(chunk)-1]
				}
				lines = append(lines, string(chunk))
				r = r[len(chunk):]
			}
			continue
		}
		if cur == "" {
			cur = word
			continue
		}
		if lipgloss.Width(cur)+1+ww <= w {
			cur += " " + word
		} else {
			flush()
			cur = word
		}
	}
	flush()
	return lines
}

// truncateLine schneidet einen String auf maximal w Display-Spalten, hängt "…" an
// wenn abgeschnitten wurde. Mehrzeilige Strings werden vorher zu einer Zeile zusammengezogen.
func truncateLine(s string, w int) string {
	s = oneLine(s)
	if w < 1 {
		return ""
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	if w == 1 {
		return "…"
	}
	r := []rune(s)
	// Erst grob runebasiert kürzen, dann auf Display-Width prüfen.
	for len(r) > 0 && lipgloss.Width(string(r))+1 > w {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
