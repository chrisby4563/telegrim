package telegram

import (
	"context"
	"errors"
	"sort"
	"sync"
)

// MultiClient bündelt mehrere Backend-Clients (z. B. TDLib + WhatsApp) hinter
// einer einzigen Client-Schnittstelle. Reads werden auf alle Backends fanned-out
// und nach LastDate sortiert; Writes (Send, Download, ListMessages) werden
// anhand der Chat-ID an das passende Backend geroutet.
//
// Wichtig: Backends müssen kollisionsfreie int64-Chat-IDs liefern. TDLib nutzt
// originale Telegram-IDs, WhatsApp hasht JIDs auf negative int64 — Kollisionen
// sind astronomisch unwahrscheinlich, werden aber pessimistisch beim Routing
// per Backend-Tag aufgelöst.
type MultiClient struct {
	primary Client
	extras  []Client

	mu        sync.RWMutex
	ownerByID map[int64]Client // chatID → backend
	platforms map[Client]string
}

var _ Client = (*MultiClient)(nil)

// NewMultiClient erzeugt einen Aggregator. Der primary-Client ist derjenige,
// gegen den Login/StartQRLogin laufen (üblicherweise TDLib). Extras werden für
// Reads gemerged und nach Chat-ID geroutet.
func NewMultiClient(primary Client, extras ...Client) *MultiClient {
	m := &MultiClient{
		primary:   primary,
		extras:    extras,
		ownerByID: make(map[int64]Client),
		platforms: make(map[Client]string),
	}
	m.platforms[primary] = PlatformTG
	for _, e := range extras {
		if _, isWA := e.(*WhatsAppClient); isWA {
			m.platforms[e] = PlatformWA
		} else {
			m.platforms[e] = PlatformTG
		}
	}
	return m
}

func (m *MultiClient) allClients() []Client {
	out := make([]Client, 0, 1+len(m.extras))
	out = append(out, m.primary)
	out = append(out, m.extras...)
	return out
}

func (m *MultiClient) registerOwner(chatID int64, owner Client) {
	m.mu.Lock()
	m.ownerByID[chatID] = owner
	m.mu.Unlock()
}

func (m *MultiClient) ownerOf(chatID int64) Client {
	m.mu.RLock()
	owner := m.ownerByID[chatID]
	m.mu.RUnlock()
	if owner == nil {
		return m.primary
	}
	return owner
}

func (m *MultiClient) Connect(ctx context.Context) error {
	var errs []error
	for _, c := range m.allClients() {
		if err := c.Connect(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Login geht nur an den primary-Client (TG). WhatsApp läuft über StartQRLogin.
func (m *MultiClient) Login(ctx context.Context, auth AuthSettings) error {
	return m.primary.Login(ctx, auth)
}

func (m *MultiClient) LoginWithCode(ctx context.Context, auth AuthSettings) error {
	return m.primary.LoginWithCode(ctx, auth)
}

// StartQRLogin geht ebenfalls an den primary-Client. Für WA-QR existiert ein
// separater Pfad in der TUI (StartWAQRLogin via Backend-Type-Assertion).
func (m *MultiClient) StartQRLogin(ctx context.Context) (string, error) {
	return m.primary.StartQRLogin(ctx)
}

// PrimaryBackend gibt das Hauptbackend zurück (für Auth-Operationen, die nicht
// im Client-Interface stehen — z. B. WA-spezifische QR-Logik).
func (m *MultiClient) PrimaryBackend() Client { return m.primary }

// Extras liefert die weiteren Backends (z. B. um den WA-Client für QR-Login zu
// erreichen).
func (m *MultiClient) Extras() []Client { return m.extras }

func (m *MultiClient) ListChats(ctx context.Context) ([]Chat, error) {
	return m.ListChatsInFolder(ctx, 0)
}

func (m *MultiClient) ListChatsInFolder(ctx context.Context, folderID int32) ([]Chat, error) {
	var (
		mu     sync.Mutex
		merged []Chat
		errs   []error
		wg     sync.WaitGroup
	)
	for _, c := range m.allClients() {
		c := c
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Nur primary respektiert Folder; extras zeigen immer alle.
			var (
				chats []Chat
				err   error
			)
			if c == m.primary {
				chats, err = c.ListChatsInFolder(ctx, folderID)
			} else {
				chats, err = c.ListChats(ctx)
			}
			mu.Lock()
			if err != nil {
				errs = append(errs, err)
				mu.Unlock()
				return
			}
			platform := m.platforms[c]
			// ownerByID-Schreibe MUSS m.mu.Lock halten — ownerOf liest mit
			// RLock konkurrent (z.B. aus ListMessages via Tick). Race war
			// Crash-Quelle "concurrent map read and map write".
			m.mu.Lock()
			for _, ch := range chats {
				if ch.Platform == "" {
					ch.Platform = platform
				}
				m.ownerByID[ch.ID] = c
				merged = append(merged, ch)
			}
			m.mu.Unlock()
			mu.Unlock()
		}()
	}
	wg.Wait()

	sort.Slice(merged, func(i, j int) bool {
		if merged[i].LastDate != merged[j].LastDate {
			return merged[i].LastDate > merged[j].LastDate
		}
		return merged[i].Title < merged[j].Title
	})
	return merged, errors.Join(errs...)
}

// ListFolders gibt nur die Folder des primary-Backends zurück. Extras (WA) haben
// keine vergleichbare Struktur — ihre Chats erscheinen in jedem Folder-View.
func (m *MultiClient) ListFolders(ctx context.Context) ([]Folder, error) {
	return m.primary.ListFolders(ctx)
}

func (m *MultiClient) ListMessages(ctx context.Context, chatID int64) ([]Message, error) {
	owner := m.ownerOf(chatID)
	msgs, err := owner.ListMessages(ctx, chatID)
	if err != nil {
		return nil, err
	}
	platform := m.platforms[owner]
	for i := range msgs {
		if msgs[i].Platform == "" {
			msgs[i].Platform = platform
		}
	}
	return msgs, nil
}

func (m *MultiClient) ListMessagesBefore(ctx context.Context, chatID, beforeMsgID int64, limit int) ([]Message, error) {
	owner := m.ownerOf(chatID)
	return owner.ListMessagesBefore(ctx, chatID, beforeMsgID, limit)
}

func (m *MultiClient) MarkChatRead(ctx context.Context, chatID int64) error {
	return m.ownerOf(chatID).MarkChatRead(ctx, chatID)
}

func (m *MultiClient) SendMessage(ctx context.Context, chatID int64, text string) (Message, error) {
	owner := m.ownerOf(chatID)
	msg, err := owner.SendMessage(ctx, chatID, text)
	if err == nil && msg.Platform == "" {
		msg.Platform = m.platforms[owner]
	}
	return msg, err
}

// DownloadFile geht an primary — Media ist Phase 2 für WA. TDLib-FileIDs sind
// global eindeutig im TG-Backend, also kein Routing-Konflikt.
func (m *MultiClient) DownloadFile(ctx context.Context, fileID int32) (string, error) {
	return m.primary.DownloadFile(ctx, fileID)
}

func (m *MultiClient) Close() error {
	var errs []error
	for _, c := range m.allClients() {
		if err := c.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
