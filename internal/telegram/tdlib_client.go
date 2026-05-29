//go:build !windows

package telegram

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	td "github.com/zelenin/go-tdlib/client"

	"telegrim/internal/notify"
)

type tdAuthState int32

const (
	tdAuthIdle tdAuthState = iota
	tdAuthWaitParams
	tdAuthWaitPhone
	tdAuthWaitCode
	tdAuthWaitPassword
	tdAuthReady
	tdAuthClosed
	tdAuthError
)

// TDLibClient ist ein Telegram-Backend auf Basis der offiziellen TDLib.
type TDLibClient struct {
	mu sync.Mutex

	apiID    int
	apiHash  string
	dataDir  string
	logLevel int32

	tdClient *td.Client
	auth     *tdAuthorizer

	foldersMu sync.RWMutex
	folders   []Folder

	statusMu     sync.RWMutex
	onlineByUser map[int64]bool

	userMu     sync.RWMutex
	nameByUser map[int64]string

	chatTitleMu sync.RWMutex
	chatTitles  map[int64]string

	// LastReadOutbox pro Chat — wird via UpdateChatReadOutbox + UpdateNewChat
	// gepflegt. Ersetzt GetChat-Roundtrip in ListMessages.
	chatReadMu         sync.RWMutex
	lastReadOutboxByID map[int64]int64

	// Voller Chat-Cache: gefüttert aus UpdateNewChat + diversen Update*-
	// Events. Spart pro ListChatsInFolder bis zu 200 GetChat-Roundtrips.
	chatCacheMu sync.RWMutex
	chatCache   map[int64]*td.Chat
	state      atomic.Int32
	lastErr    atomic.Pointer[error]

	authDone chan struct{}
}

// Compiler-Helfer: stelle sicher dass das Interface gehalten wird.
var _ Client = (*TDLibClient)(nil)

type tdAuthorizer struct {
	params   *td.SetTdlibParametersRequest
	phoneCh  chan string
	codeCh   chan string
	pwCh     chan string
	stateCh  chan td.AuthorizationState
	closed   atomic.Bool
}

func newTdAuthorizer(params *td.SetTdlibParametersRequest) *tdAuthorizer {
	return &tdAuthorizer{
		params:  params,
		phoneCh: make(chan string, 1),
		codeCh:  make(chan string, 1),
		pwCh:    make(chan string, 1),
		stateCh: make(chan td.AuthorizationState, 16),
	}
}

func (a *tdAuthorizer) Handle(c *td.Client, state td.AuthorizationState) error {
	if !a.closed.Load() {
		select {
		case a.stateCh <- state:
		default:
		}
	}
	switch state.AuthorizationStateType() {
	case td.TypeAuthorizationStateWaitTdlibParameters:
		_, err := c.SetTdlibParameters(a.params)
		return err
	case td.TypeAuthorizationStateWaitPhoneNumber:
		phone, ok := <-a.phoneCh
		if !ok {
			return fmt.Errorf("authorizer geschlossen")
		}
		_, err := c.SetAuthenticationPhoneNumber(&td.SetAuthenticationPhoneNumberRequest{
			PhoneNumber: phone,
			Settings: &td.PhoneNumberAuthenticationSettings{
				AllowFlashCall:       false,
				IsCurrentPhoneNumber: false,
				AllowSmsRetrieverApi: false,
			},
		})
		return err
	case td.TypeAuthorizationStateWaitCode:
		code, ok := <-a.codeCh
		if !ok {
			return fmt.Errorf("authorizer geschlossen")
		}
		_, err := c.CheckAuthenticationCode(&td.CheckAuthenticationCodeRequest{Code: code})
		return err
	case td.TypeAuthorizationStateWaitPassword:
		pw, ok := <-a.pwCh
		if !ok {
			return fmt.Errorf("authorizer geschlossen")
		}
		_, err := c.CheckAuthenticationPassword(&td.CheckAuthenticationPasswordRequest{Password: pw})
		return err
	case td.TypeAuthorizationStateReady,
		td.TypeAuthorizationStateClosing,
		td.TypeAuthorizationStateClosed:
		return nil
	}
	return td.NotSupportedAuthorizationState(state)
}

func (a *tdAuthorizer) Close() {
	if a.closed.Swap(true) {
		return
	}
	close(a.phoneCh)
	close(a.codeCh)
	close(a.pwCh)
	close(a.stateCh)
}

func NewTDLibClient(apiID int, apiHash, dataDir string) *TDLibClient {
	if strings.TrimSpace(dataDir) == "" {
		home, _ := os.UserHomeDir()
		dataDir = filepath.Join(home, ".local", "share", "telegrim", "tdlib")
	}
	c := &TDLibClient{
		apiID:    apiID,
		apiHash:  strings.TrimSpace(apiHash),
		dataDir:  dataDir,
		logLevel: 1, // 0=fatal, 1=error, 2=warning, 3=info — höher = laut
	}
	c.state.Store(int32(tdAuthIdle))
	return c
}

func (c *TDLibClient) currentState() tdAuthState {
	return tdAuthState(c.state.Load())
}

func (c *TDLibClient) setState(s tdAuthState) {
	c.state.Store(int32(s))
}

func (c *TDLibClient) setErr(err error) {
	if err != nil {
		c.lastErr.Store(&err)
	}
}

// Connect ist no-op – die echte Verbindung beginnt erst mit dem ersten Login-Schritt,
// damit api_id/api_hash aus den Settings übernommen werden können.
func (c *TDLibClient) Connect(ctx context.Context) error {
	return nil
}

func (c *TDLibClient) ensureStarted(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Wenn der Client schon läuft (Ready) ODER gerade gestartet wird (auth != nil
	// während NewClient noch in Authorize blockt), keinen zweiten erzeugen.
	if c.tdClient != nil || c.auth != nil {
		return nil
	}
	if c.apiID <= 0 {
		return fmt.Errorf("api id fehlt")
	}
	if strings.TrimSpace(c.apiHash) == "" {
		return fmt.Errorf("api hash fehlt")
	}
	if err := os.MkdirAll(c.dataDir, 0o700); err != nil {
		return fmt.Errorf("tdlib dir anlegen: %w", err)
	}

	params := &td.SetTdlibParametersRequest{
		UseTestDc:           false,
		DatabaseDirectory:   filepath.Join(c.dataDir, "db"),
		FilesDirectory:      filepath.Join(c.dataDir, "files"),
		UseFileDatabase:     true,
		UseChatInfoDatabase: true,
		UseMessageDatabase:  true,
		UseSecretChats:      false,
		ApiId:               int32(c.apiID),
		ApiHash:             c.apiHash,
		SystemLanguageCode:  "de",
		DeviceModel:         "telegrim",
		SystemVersion:       "linux",
		ApplicationVersion:  "0.1",
	}

	authz := newTdAuthorizer(params)
	c.auth = authz
	c.authDone = make(chan struct{})

	// Logs in Datei umleiten, damit sie nicht über die TUI laufen.
	logPath := filepath.Join(c.dataDir, "tdlib.log")
	_, _ = td.SetLogStream(&td.SetLogStreamRequest{
		LogStream: &td.LogStreamFile{
			Path:           logPath,
			MaxFileSize:    10 * 1024 * 1024, // 10 MB, rotiert automatisch
			RedirectStderr: true,
		},
	})
	_, _ = td.SetLogVerbosityLevel(&td.SetLogVerbosityLevelRequest{NewVerbosityLevel: c.logLevel})

	c.setState(tdAuthWaitParams)
	go c.watchAuthState()

	// td.NewClient blockiert intern in Authorize() bis Ready/Error.
	// Daher in eigener Goroutine starten; tdClient wird erst NACH erfolgreichem
	// Login gesetzt. Bis dahin laufen alle Auth-Schritte über die Channels im
	// authorizer, getriggert vom State-Watcher unten.
	go func(authz *tdAuthorizer) {
		tdClient, err := td.NewClient(authz, td.WithLogVerbosity(&td.SetLogVerbosityLevelRequest{NewVerbosityLevel: c.logLevel}))
		c.mu.Lock()
		defer c.mu.Unlock()
		if err != nil {
			c.setErr(err)
			c.setState(tdAuthError)
			// Reset, damit ein erneuter Login-Klick einen frischen Client starten kann.
			c.auth = nil
			c.tdClient = nil
			return
		}
		c.tdClient = tdClient
		c.setState(tdAuthReady)
		go c.watchUpdates(tdClient)
	}(authz)

	return nil
}

func (c *TDLibClient) watchAuthState() {
	defer close(c.authDone)
	for state := range c.auth.stateCh {
		switch state.AuthorizationStateType() {
		case td.TypeAuthorizationStateWaitTdlibParameters:
			c.setState(tdAuthWaitParams)
		case td.TypeAuthorizationStateWaitPhoneNumber:
			c.setState(tdAuthWaitPhone)
		case td.TypeAuthorizationStateWaitCode:
			c.setState(tdAuthWaitCode)
		case td.TypeAuthorizationStateWaitPassword:
			c.setState(tdAuthWaitPassword)
		case td.TypeAuthorizationStateReady:
			c.setState(tdAuthReady)
		case td.TypeAuthorizationStateLoggingOut,
			td.TypeAuthorizationStateClosing,
			td.TypeAuthorizationStateClosed:
			c.setState(tdAuthClosed)
		}
	}
}

// waitForState blockt bis einer der gewünschten States erreicht ist oder Timeout.
func (c *TDLibClient) waitForState(ctx context.Context, want ...tdAuthState) (tdAuthState, error) {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		s := c.currentState()
		for _, w := range want {
			if s == w {
				return s, nil
			}
		}
		if s == tdAuthError || s == tdAuthClosed {
			if errp := c.lastErr.Load(); errp != nil && *errp != nil {
				return s, *errp
			}
			return s, fmt.Errorf("auth abgebrochen")
		}
		select {
		case <-ctx.Done():
			return s, ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
	return c.currentState(), fmt.Errorf("timeout im state %v", c.currentState())
}

// Login: setzt API-Credentials, startet falls nötig den TDLib-Client.
// Wenn auth.Phone gesetzt ist und der Client auf die Telefonnummer wartet, wird sie gesendet.
func (c *TDLibClient) Login(ctx context.Context, auth AuthSettings) error {
	if auth.APIID <= 0 {
		return fmt.Errorf("api id fehlt oder ungültig")
	}
	if strings.TrimSpace(auth.APIHash) == "" {
		return fmt.Errorf("api hash fehlt")
	}
	c.mu.Lock()
	c.apiID = auth.APIID
	c.apiHash = strings.TrimSpace(auth.APIHash)
	c.mu.Unlock()

	if err := c.ensureStarted(ctx); err != nil {
		return err
	}
	// Nur warten bis State irgendwas Sinnvolles ist. Phone wird ausschließlich
	// in LoginWithCode geschickt, damit kein Doppel-Send auf den Channel passiert.
	_, err := c.waitForState(ctx, tdAuthWaitPhone, tdAuthWaitCode, tdAuthWaitPassword, tdAuthReady)
	return err
}

// LoginWithCode treibt den Login einen Schritt weiter, basierend auf dem aktuellen TDLib-State.
// Idempotent: kann mehrfach aufgerufen werden, jeder Aufruf liefert den nächsten Schritt
// oder einen fehlenden Eingabewert als Error zurück.
func (c *TDLibClient) LoginWithCode(ctx context.Context, auth AuthSettings) error {
	if err := c.Login(ctx, auth); err != nil {
		return err
	}
	for step := 0; step < 6; step++ {
		switch c.currentState() {
		case tdAuthWaitPhone:
			phone := strings.TrimSpace(auth.Phone)
			if phone == "" {
				return fmt.Errorf("telefonnummer fehlt")
			}
			if !trySend(c.auth.phoneCh, phone) {
				// Schon gesendet → warten bis State weiter ist.
			}
			if _, err := c.waitForState(ctx, tdAuthWaitCode, tdAuthWaitPassword, tdAuthReady); err != nil {
				return err
			}
		case tdAuthWaitCode:
			code := strings.TrimSpace(auth.Code)
			if code == "" {
				return fmt.Errorf("login-code fehlt – Telegram hat den Code per SMS/App geschickt; jetzt ins Feld 'Code' eintragen und nochmal [Terminal Login]")
			}
			if !trySend(c.auth.codeCh, code) {
			}
			if _, err := c.waitForState(ctx, tdAuthWaitPassword, tdAuthReady); err != nil {
				return err
			}
		case tdAuthWaitPassword:
			pw := strings.TrimSpace(auth.Password)
			if pw == "" {
				return fmt.Errorf("2FA-Passwort erforderlich – ins Feld 'Passwort (2FA)' eintragen und nochmal [Terminal Login]")
			}
			if !trySend(c.auth.pwCh, pw) {
			}
			if _, err := c.waitForState(ctx, tdAuthReady); err != nil {
				return err
			}
		case tdAuthReady:
			return nil
		case tdAuthClosed, tdAuthError:
			if errp := c.lastErr.Load(); errp != nil && *errp != nil {
				return *errp
			}
			return fmt.Errorf("auth abgebrochen")
		default:
			if _, err := c.waitForState(ctx, tdAuthWaitPhone, tdAuthWaitCode, tdAuthWaitPassword, tdAuthReady); err != nil {
				return err
			}
		}
	}
	return fmt.Errorf("zu viele auth-schritte ohne fortschritt")
}

// StartQRLogin ist in der TDLib-Variante (noch) nicht implementiert. TDLib bietet
// einen eigenen QR-Authorizer; um die Mehrfach-Authentifizierung sauber zu halten
// wird er separat eingebaut, sobald der Code-Pfad steht.
func (c *TDLibClient) StartQRLogin(ctx context.Context) (string, error) {
	return "", fmt.Errorf("QR-Login mit TDLib noch nicht implementiert – bitte Terminal-Login (Phone+Code) nutzen")
}

func (c *TDLibClient) requireReady() error {
	// Race: State-Watcher setzt Ready evtl. ~1s vor Setzen von c.tdClient
	// (NewClient kehrt erst danach zurück). Kurz pollen.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		switch c.currentState() {
		case tdAuthReady:
			c.mu.Lock()
			ok := c.tdClient != nil
			c.mu.Unlock()
			if ok {
				return nil
			}
		case tdAuthError, tdAuthClosed:
			if errp := c.lastErr.Load(); errp != nil && *errp != nil {
				return *errp
			}
			return fmt.Errorf("auth abgebrochen")
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("nicht eingeloggt: erst Terminal-Login abschließen")
}

// watchUpdates abonniert den TDLib-Update-Stream und füllt internen Cache
// für u.a. Chat-Folder.
func (c *TDLibClient) watchUpdates(tdc *td.Client) {
	listener := tdc.GetListener()
	defer listener.Close()
	for update := range listener.Updates {
		switch u := update.(type) {
		case *td.UpdateUserStatus:
			online := u.Status.UserStatusType() == td.TypeUserStatusOnline
			c.statusMu.Lock()
			if c.onlineByUser == nil {
				c.onlineByUser = make(map[int64]bool)
			}
			c.onlineByUser[u.UserId] = online
			c.statusMu.Unlock()
		case *td.UpdateUser:
			if u.User == nil {
				continue
			}
			if u.User.Status != nil {
				online := u.User.Status.UserStatusType() == td.TypeUserStatusOnline
				c.statusMu.Lock()
				if c.onlineByUser == nil {
					c.onlineByUser = make(map[int64]bool)
				}
				c.onlineByUser[u.User.Id] = online
				c.statusMu.Unlock()
			}
			name := userDisplayName(u.User)
			if name != "" {
				c.userMu.Lock()
				if c.nameByUser == nil {
					c.nameByUser = make(map[int64]string)
				}
				c.nameByUser[u.User.Id] = name
				c.userMu.Unlock()
			}
		case *td.UpdateNewChat:
			if u.Chat != nil {
				c.chatCacheMu.Lock()
				if c.chatCache == nil {
					c.chatCache = make(map[int64]*td.Chat)
				}
				c.chatCache[u.Chat.Id] = u.Chat
				c.chatCacheMu.Unlock()
				c.chatTitleMu.Lock()
				if c.chatTitles == nil {
					c.chatTitles = make(map[int64]string)
				}
				c.chatTitles[u.Chat.Id] = u.Chat.Title
				c.chatTitleMu.Unlock()
				c.chatReadMu.Lock()
				if c.lastReadOutboxByID == nil {
					c.lastReadOutboxByID = make(map[int64]int64)
				}
				c.lastReadOutboxByID[u.Chat.Id] = u.Chat.LastReadOutboxMessageId
				c.chatReadMu.Unlock()
			}
		case *td.UpdateChatReadOutbox:
			c.chatReadMu.Lock()
			if c.lastReadOutboxByID == nil {
				c.lastReadOutboxByID = make(map[int64]int64)
			}
			c.lastReadOutboxByID[u.ChatId] = u.LastReadOutboxMessageId
			c.chatReadMu.Unlock()
			c.chatCacheMu.Lock()
			if ch, ok := c.chatCache[u.ChatId]; ok {
				ch.LastReadOutboxMessageId = u.LastReadOutboxMessageId
			}
			c.chatCacheMu.Unlock()
		case *td.UpdateChatTitle:
			c.chatTitleMu.Lock()
			if c.chatTitles == nil {
				c.chatTitles = make(map[int64]string)
			}
			c.chatTitles[u.ChatId] = u.Title
			c.chatTitleMu.Unlock()
			c.chatCacheMu.Lock()
			if ch, ok := c.chatCache[u.ChatId]; ok {
				ch.Title = u.Title
			}
			c.chatCacheMu.Unlock()
		case *td.UpdateChatLastMessage:
			c.chatCacheMu.Lock()
			if ch, ok := c.chatCache[u.ChatId]; ok {
				ch.LastMessage = u.LastMessage
			}
			c.chatCacheMu.Unlock()
		case *td.UpdateChatReadInbox:
			c.chatCacheMu.Lock()
			if ch, ok := c.chatCache[u.ChatId]; ok {
				ch.UnreadCount = u.UnreadCount
				ch.LastReadInboxMessageId = u.LastReadInboxMessageId
			}
			c.chatCacheMu.Unlock()
		case *td.UpdateNewMessage:
			if u.Message != nil && !u.Message.IsOutgoing {
				c.notifyTGMessage(u.Message)
			}
		case *td.UpdateChatFolders:
			fs := make([]Folder, 0, len(u.ChatFolders)+2)
			mainPos := int(u.MainChatListPosition)
			added := false
			for i, info := range u.ChatFolders {
				if i == mainPos {
					fs = append(fs, Folder{ID: 0, Title: "Alle"})
					added = true
				}
				fs = append(fs, Folder{ID: info.Id, Title: strings.TrimSpace(info.Title)})
			}
			if !added {
				// Main einfügen wenn Position außerhalb des Folder-Index liegt.
				fs = append([]Folder{{ID: 0, Title: "Alle"}}, fs...)
			}
			fs = append(fs, Folder{ID: -1, Title: "📁 Archiv"})
			c.foldersMu.Lock()
			c.folders = fs
			c.foldersMu.Unlock()
		}
	}
}

func (c *TDLibClient) ListFolders(ctx context.Context) ([]Folder, error) {
	if err := c.requireReady(); err != nil {
		return nil, err
	}
	c.foldersMu.RLock()
	defer c.foldersMu.RUnlock()
	if len(c.folders) == 0 {
		// Fallback: minimal "Alle" + "Archiv" anbieten.
		return []Folder{{ID: 0, Title: "Alle"}, {ID: -1, Title: "📁 Archiv"}}, nil
	}
	out := make([]Folder, len(c.folders))
	copy(out, c.folders)
	return out, nil
}

func (c *TDLibClient) chatListForFolder(folderID int32) td.ChatList {
	switch folderID {
	case 0:
		return &td.ChatListMain{}
	case -1:
		return &td.ChatListArchive{}
	default:
		return &td.ChatListFolder{ChatFolderId: folderID}
	}
}

func (c *TDLibClient) ListChatsInFolder(ctx context.Context, folderID int32) ([]Chat, error) {
	if err := c.requireReady(); err != nil {
		return nil, err
	}
	list := c.chatListForFolder(folderID)
	_, _ = c.tdClient.LoadChats(&td.LoadChatsRequest{ChatList: list, Limit: 200})
	res, err := c.tdClient.GetChats(&td.GetChatsRequest{ChatList: list, Limit: 200})
	if err != nil {
		return nil, fmt.Errorf("getChats folder %d: %w", folderID, err)
	}
	out := make([]Chat, 0, len(res.ChatIds))
	for _, id := range res.ChatIds {
		// Cache-Lookup zuerst — UpdateNewChat/UpdateChatLastMessage/
		// UpdateChatReadInbox pflegen die Map kontinuierlich. Fallback nur
		// bei Miss (initial). Spart 200 TDLib-Roundtrips pro ListChats.
		c.chatCacheMu.RLock()
		ch, ok := c.chatCache[id]
		c.chatCacheMu.RUnlock()
		if !ok {
			fresh, err := c.tdClient.GetChat(&td.GetChatRequest{ChatId: id})
			if err != nil || fresh == nil {
				continue
			}
			ch = fresh
			c.chatCacheMu.Lock()
			if c.chatCache == nil {
				c.chatCache = make(map[int64]*td.Chat)
			}
			c.chatCache[id] = ch
			c.chatCacheMu.Unlock()
		}
		last := ""
		var lastDate int32
		if ch.LastMessage != nil {
			last = previewMessage(ch.LastMessage)
			lastDate = ch.LastMessage.Date
		}
		title := strings.TrimSpace(ch.Title)
		if title == "" {
			title = fmt.Sprintf("Chat %d", ch.Id)
		}
		isPrivate, online := c.presenceFor(ch.Type)
		isChannel := false
		if sg, ok := ch.Type.(*td.ChatTypeSupergroup); ok && sg != nil && sg.IsChannel {
			isChannel = true
		}
		out = append(out, Chat{
			ID:        ch.Id,
			Title:     title,
			Last:      last,
			Unread:    int(ch.UnreadCount),
			IsPrivate: isPrivate,
			Online:    online,
			LastDate:  lastDate,
			IsChannel: isChannel,
		})
	}
	sortChatsByLastDate(out)
	return out, nil
}

// sortChatsByLastDate sortiert die Chat-Liste absteigend nach Aktivität (LastDate),
// damit der zuletzt aktive Chat oben steht. Chats ohne LastMessage rutschen nach unten.
func sortChatsByLastDate(chats []Chat) {
	sort.SliceStable(chats, func(i, j int) bool {
		if chats[i].LastDate == chats[j].LastDate {
			return strings.ToLower(chats[i].Title) < strings.ToLower(chats[j].Title)
		}
		return chats[i].LastDate > chats[j].LastDate
	})
}

// presenceFor liefert (isPrivate, isOnline) für einen Chat-Typ. Online-Status
// kommt aus dem User-Status-Cache, der via UpdateUser/UpdateUserStatus gefüllt wird.
// Bei Erstaufruf vor Update: Status default false.
func (c *TDLibClient) presenceFor(t td.ChatType) (bool, bool) {
	priv, ok := t.(*td.ChatTypePrivate)
	if !ok || priv == nil {
		return false, false
	}
	c.statusMu.RLock()
	online := c.onlineByUser[priv.UserId]
	c.statusMu.RUnlock()
	if !online {
		// Cache-Miss: einmaliger sync-Lookup, danach pflegt der Update-Listener.
		u, err := c.tdClient.GetUser(&td.GetUserRequest{UserId: priv.UserId})
		if err == nil && u != nil && u.Status != nil {
			online = u.Status.UserStatusType() == td.TypeUserStatusOnline
			c.statusMu.Lock()
			if c.onlineByUser == nil {
				c.onlineByUser = make(map[int64]bool)
			}
			c.onlineByUser[priv.UserId] = online
			c.statusMu.Unlock()
		}
	}
	return true, online
}

func (c *TDLibClient) ListChats(ctx context.Context) ([]Chat, error) {
	if err := c.requireReady(); err != nil {
		return nil, err
	}
	// LoadChats triggert TDLib, Positionen für beide Listen reinzuziehen.
	_, _ = c.tdClient.LoadChats(&td.LoadChatsRequest{ChatList: &td.ChatListMain{}, Limit: 200})
	_, _ = c.tdClient.LoadChats(&td.LoadChatsRequest{ChatList: &td.ChatListArchive{}, Limit: 200})

	seen := make(map[int64]bool, 256)
	var out []Chat
	collect := func(list td.ChatList, archive bool) error {
		res, err := c.tdClient.GetChats(&td.GetChatsRequest{ChatList: list, Limit: 200})
		if err != nil {
			return err
		}
		for _, id := range res.ChatIds {
			if seen[id] {
				continue
			}
			seen[id] = true
			ch, err := c.tdClient.GetChat(&td.GetChatRequest{ChatId: id})
			if err != nil {
				continue
			}
			last := ""
			var lastDate int32
			if ch.LastMessage != nil {
				last = previewMessage(ch.LastMessage)
				lastDate = ch.LastMessage.Date
			}
			title := strings.TrimSpace(ch.Title)
			if title == "" {
				title = fmt.Sprintf("Chat %d", ch.Id)
			}
			if archive {
				title = "📁 " + title
			}
			isPrivate, online := c.presenceFor(ch.Type)
			out = append(out, Chat{
				ID:        ch.Id,
				Title:     title,
				Last:      last,
				Unread:    int(ch.UnreadCount),
				IsPrivate: isPrivate,
				Online:    online,
				LastDate:  lastDate,
			})
		}
		return nil
	}
	if err := collect(&td.ChatListMain{}, false); err != nil {
		return nil, fmt.Errorf("getChats main: %w", err)
	}
	// Archive optional – wenn nicht vorhanden ignorieren.
	_ = collect(&td.ChatListArchive{}, true)
	sortChatsByLastDate(out)
	return out, nil
}

func (c *TDLibClient) ListMessages(ctx context.Context, chatID int64) ([]Message, error) {
	if err := c.requireReady(); err != nil {
		return nil, err
	}
	// Erst lokalen Cache versuchen (sub-millisecond), nur bei Miss auf Server
	// gehen. Reduziert Network-Roundtrips dramatisch wenn Messages schon
	// via UpdateNewMessage angekommen sind.
	req := &td.GetChatHistoryRequest{
		ChatId:        chatID,
		FromMessageId: 0,
		Offset:        0,
		Limit:         50,
		OnlyLocal:     true,
	}
	hist, err := c.tdClient.GetChatHistory(req)
	if err != nil || hist == nil || len(hist.Messages) == 0 {
		req.OnlyLocal = false
		hist, err = c.tdClient.GetChatHistory(req)
		if err != nil {
			return nil, fmt.Errorf("getChatHistory: %w", err)
		}
	}
	// LastReadOutbox aus Cache (gefüllt via UpdateNewChat/UpdateChatReadOutbox).
	// Fallback auf GetChat nur wenn Cache leer (initial bevor Updates flossen).
	c.chatReadMu.RLock()
	lastReadOutbox, hasRead := c.lastReadOutboxByID[chatID]
	c.chatReadMu.RUnlock()
	if !hasRead {
		if chat, err := c.tdClient.GetChat(&td.GetChatRequest{ChatId: chatID}); err == nil && chat != nil {
			lastReadOutbox = chat.LastReadOutboxMessageId
			c.chatReadMu.Lock()
			if c.lastReadOutboxByID == nil {
				c.lastReadOutboxByID = make(map[int64]int64)
			}
			c.lastReadOutboxByID[chatID] = lastReadOutbox
			c.chatReadMu.Unlock()
		}
	}
	// TDLib liefert neueste zuerst → umdrehen für chronologische Anzeige.
	out := make([]Message, 0, len(hist.Messages))
	for i := len(hist.Messages) - 1; i >= 0; i-- {
		m := hist.Messages[i]
		if m == nil {
			continue
		}
		text := previewMessage(m)
		sender := c.senderLabelFor(m)
		out = append(out, Message{
			ID:       m.Id,
			ChatID:   chatID,
			Sender:   sender,
			Text:     text,
			Outgoing: m.IsOutgoing,
			Date:     m.Date,
			Media:    extractMedia(m),
			Status:   tgMessageStatus(m, lastReadOutbox),
		})
	}
	return out, nil
}

// tgMessageStatus leitet den Versand-/Lese-Status aus TDLib-Feldern ab.
// Nur für ausgehende Messages relevant — eingehende geben None zurück.
func tgMessageStatus(m *td.Message, lastReadOutbox int64) MessageStatus {
	if m == nil || !m.IsOutgoing {
		return MessageStatusNone
	}
	if m.SendingState != nil {
		// SendingStatePending / SendingStateFailed → noch nicht raus.
		return MessageStatusPending
	}
	if lastReadOutbox >= m.Id {
		return MessageStatusRead
	}
	// TDLib unterscheidet Delivered nicht eigenständig — wir mappen auf Sent.
	return MessageStatusSent
}

// ListMessagesBefore: Backwards-Paging. FromMessageId=beforeMsgID, Offset=0
// liefert nur ältere Messages. TDLib's erster Call kann leer kommen wenn der
// lokale Cache leer ist – dann ein Retry um den Server-Fetch zu triggern.
func (c *TDLibClient) ListMessagesBefore(ctx context.Context, chatID, beforeMsgID int64, limit int) ([]Message, error) {
	if err := c.requireReady(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 50
	}
	req := &td.GetChatHistoryRequest{
		ChatId:        chatID,
		FromMessageId: beforeMsgID,
		Offset:        0,
		Limit:         int32(limit),
		OnlyLocal:     true,
	}
	hist, err := c.tdClient.GetChatHistory(req)
	if err != nil || hist == nil || len(hist.Messages) == 0 {
		// Lokaler Cache leer → vom Server holen.
		req.OnlyLocal = false
		hist, err = c.tdClient.GetChatHistory(req)
		if err != nil {
			return nil, fmt.Errorf("getChatHistory retry: %w", err)
		}
	}
	var lastReadOutbox int64
	if chat, err := c.tdClient.GetChat(&td.GetChatRequest{ChatId: chatID}); err == nil && chat != nil {
		lastReadOutbox = chat.LastReadOutboxMessageId
	}
	out := make([]Message, 0, len(hist.Messages))
	for i := len(hist.Messages) - 1; i >= 0; i-- {
		m := hist.Messages[i]
		if m == nil {
			continue
		}
		out = append(out, Message{
			ID:       m.Id,
			ChatID:   chatID,
			Sender:   c.senderLabelFor(m),
			Text:     previewMessage(m),
			Outgoing: m.IsOutgoing,
			Date:     m.Date,
			Media:    extractMedia(m),
			Status:   tgMessageStatus(m, lastReadOutbox),
		})
	}
	return out, nil
}

func (c *TDLibClient) SendMessage(ctx context.Context, chatID int64, text string) (Message, error) {
	if err := c.requireReady(); err != nil {
		return Message{}, err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return Message{}, fmt.Errorf("empty message")
	}
	res, err := c.tdClient.SendMessage(&td.SendMessageRequest{
		ChatId: chatID,
		InputMessageContent: &td.InputMessageText{
			Text: &td.FormattedText{Text: text},
		},
	})
	if err != nil {
		return Message{}, fmt.Errorf("sendMessage: %w", err)
	}
	return Message{
		ID:       res.Id,
		ChatID:   chatID,
		Sender:   "Du",
		Text:     text,
		Outgoing: true,
		Date:     int32(time.Now().Unix()),
	}, nil
}

// DownloadFile lädt eine Datei synchron via TDLib. Wenn bereits gecached,
// kehrt sofort mit dem Pfad zurück. Synchronous=true blockt bis Download
// fertig/abgebrochen/fehlgeschlagen — Caller sollte das in einem Goroutine
// laufen lassen (z.B. tea.Cmd).
func (c *TDLibClient) DownloadFile(ctx context.Context, fileID int32) (string, error) {
	if err := c.requireReady(); err != nil {
		return "", err
	}
	f, err := c.tdClient.DownloadFile(&td.DownloadFileRequest{
		FileId:      fileID,
		Priority:    16,
		Offset:      0,
		Limit:       0,
		Synchronous: true,
	})
	if err != nil {
		return "", fmt.Errorf("downloadFile %d: %w", fileID, err)
	}
	if f == nil || f.Local == nil || f.Local.Path == "" {
		return "", fmt.Errorf("downloadFile %d: no local path", fileID)
	}
	return f.Local.Path, nil
}

// MarkChatRead signalisiert TDLib dass der Chat geöffnet ist und markiert die
// jüngsten Messages als gelesen. OpenChat alleine reicht oft nicht — wir
// schicken zusätzlich ViewMessages mit den neuesten Message-IDs und
// ForceRead=true. Damit ist UnreadCount beim nächsten ListChats reliable 0.
func (c *TDLibClient) MarkChatRead(ctx context.Context, chatID int64) error {
	if err := c.requireReady(); err != nil {
		return nil
	}
	c.mu.Lock()
	tdc := c.tdClient
	c.mu.Unlock()
	if tdc == nil {
		return nil
	}
	_, _ = tdc.OpenChat(&td.OpenChatRequest{ChatId: chatID})

	// Letzte ~20 Message-IDs einsammeln. GetChatHistory braucht einen
	// from_message_id=0 (= neueste) Anker.
	hist, err := tdc.GetChatHistory(&td.GetChatHistoryRequest{
		ChatId:        chatID,
		FromMessageId: 0,
		Offset:        0,
		Limit:         20,
		OnlyLocal:     false,
	})
	if err != nil || hist == nil || len(hist.Messages) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(hist.Messages))
	for _, m := range hist.Messages {
		if m != nil {
			ids = append(ids, m.Id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	_, _ = tdc.ViewMessages(&td.ViewMessagesRequest{
		ChatId:     chatID,
		MessageIds: ids,
		ForceRead:  true,
	})
	return nil
}

func (c *TDLibClient) Close() error {
	c.mu.Lock()
	tdc := c.tdClient
	authDone := c.authDone
	c.mu.Unlock()
	if tdc == nil {
		return nil
	}
	// Anfragen an TDLib, sauber zu schließen. TDLib schreibt dabei den
	// Binlog/DB-Status durch und fährt erst dann runter.
	_, err := tdc.Close()
	// Warten bis State-Watcher die Authorize-Schleife komplett durchlaufen hat
	// (Closed-Event empfangen). Timeout schützt vor Hängern.
	if authDone != nil {
		select {
		case <-authDone:
		case <-time.After(5 * time.Second):
		}
	}
	c.mu.Lock()
	c.tdClient = nil
	if c.auth != nil {
		c.auth = nil
	}
	c.mu.Unlock()
	return err
}

// trySend schreibt in einen gepufferten Channel, ohne zu blockieren wenn er voll ist.
// Liefert true wenn gesendet wurde, false wenn Channel bereits gefüllt war.
func trySend(ch chan string, v string) bool {
	select {
	case ch <- v:
		return true
	default:
		return false
	}
}

// notifyTGMessage feuert eine Desktop-Notification für eine eingehende
// TDLib-Nachricht. Title = Chat-Title (oder "Telegram"), Body = Sender + Text.
// Schweigt bei leerem Body.
func (c *TDLibClient) notifyTGMessage(m *td.Message) {
	text := strings.TrimSpace(previewMessage(m))
	if text == "" {
		return
	}
	title := "Telegram"
	c.chatTitleMu.RLock()
	if t, ok := c.chatTitles[m.ChatId]; ok && strings.TrimSpace(t) != "" {
		title = t
	}
	c.chatTitleMu.RUnlock()

	sender := ""
	if mu, ok := m.SenderId.(*td.MessageSenderUser); ok && mu != nil {
		c.userMu.RLock()
		sender = c.nameByUser[mu.UserId]
		c.userMu.RUnlock()
	}
	body := text
	if sender != "" && sender != title {
		body = sender + ": " + text
	}
	notify.Send(title, body)
}

func previewMessage(m *td.Message) string {
	if m == nil || m.Content == nil {
		return ""
	}
	raw := ""
	switch v := m.Content.(type) {
	case *td.MessageText:
		if v.Text != nil {
			raw = v.Text.Text
		}
	case *td.MessagePhoto:
		if v.Caption != nil {
			raw = v.Caption.Text
		}
	case *td.MessageVideo:
		if v.Caption != nil {
			raw = v.Caption.Text
		}
	case *td.MessageAnimation:
		if v.Caption != nil {
			raw = v.Caption.Text
		}
	case *td.MessageDocument:
		if v.Caption != nil {
			raw = v.Caption.Text
		}
	case *td.MessageVoiceNote:
		return "[Sprachnachricht]"
	case *td.MessageSticker:
		return "[Sticker]"
	default:
		return "[" + m.Content.MessageContentType() + "]"
	}
	return strings.TrimSpace(raw)
}

// extractMedia zieht aus TDLib-MessageContent unsere Media-Repräsentation.
// nil bei reinem Text oder nicht-unterstützten Typen (Voice, Sticker, ...).
func extractMedia(m *td.Message) *Media {
	if m == nil || m.Content == nil {
		return nil
	}
	switch v := m.Content.(type) {
	case *td.MessagePhoto:
		if v.Photo == nil || len(v.Photo.Sizes) == 0 {
			return nil
		}
		// Größtes Photo nehmen (letztes in Sizes ist meist das größte, aber
		// sicherheitshalber max-by-area).
		var best *td.PhotoSize
		var bestArea int32
		for _, s := range v.Photo.Sizes {
			a := s.Width * s.Height
			if a > bestArea {
				best = s
				bestArea = a
			}
		}
		if best == nil || best.Photo == nil {
			return nil
		}
		return &Media{
			Kind:      MediaPhoto,
			FileID:    best.Photo.Id,
			Size:      fileSize(best.Photo),
			Width:     best.Width,
			Height:    best.Height,
			LocalPath: fileLocalPath(best.Photo),
		}
	case *td.MessageVideo:
		if v.Video == nil || v.Video.Video == nil {
			return nil
		}
		return &Media{
			Kind:      MediaVideo,
			FileID:    v.Video.Video.Id,
			FileName:  v.Video.FileName,
			MimeType:  v.Video.MimeType,
			Size:      fileSize(v.Video.Video),
			Width:     v.Video.Width,
			Height:    v.Video.Height,
			Duration:  v.Video.Duration,
			LocalPath: fileLocalPath(v.Video.Video),
		}
	case *td.MessageAnimation:
		if v.Animation == nil || v.Animation.Animation == nil {
			return nil
		}
		return &Media{
			Kind:      MediaAnimation,
			FileID:    v.Animation.Animation.Id,
			FileName:  v.Animation.FileName,
			MimeType:  v.Animation.MimeType,
			Size:      fileSize(v.Animation.Animation),
			Width:     v.Animation.Width,
			Height:    v.Animation.Height,
			Duration:  v.Animation.Duration,
			LocalPath: fileLocalPath(v.Animation.Animation),
		}
	case *td.MessageDocument:
		if v.Document == nil || v.Document.Document == nil {
			return nil
		}
		return &Media{
			Kind:      MediaDocument,
			FileID:    v.Document.Document.Id,
			FileName:  v.Document.FileName,
			MimeType:  v.Document.MimeType,
			Size:      fileSize(v.Document.Document),
			LocalPath: fileLocalPath(v.Document.Document),
		}
	}
	return nil
}

func fileSize(f *td.File) int64 {
	if f == nil {
		return 0
	}
	if f.Size > 0 {
		return int64(f.Size)
	}
	return int64(f.ExpectedSize)
}

func fileLocalPath(f *td.File) string {
	if f == nil || f.Local == nil || !f.Local.IsDownloadingCompleted {
		return ""
	}
	return f.Local.Path
}

func userDisplayName(u *td.User) string {
	if u == nil {
		return ""
	}
	name := strings.TrimSpace(strings.TrimSpace(u.FirstName) + " " + strings.TrimSpace(u.LastName))
	if name != "" {
		return name
	}
	if u.Usernames != nil && u.Usernames.EditableUsername != "" {
		return "@" + u.Usernames.EditableUsername
	}
	if u.PhoneNumber != "" {
		return "+" + u.PhoneNumber
	}
	return fmt.Sprintf("User %d", u.Id)
}

// senderLabelFor übersetzt die Sender-ID einer Message in einen menschlichen Namen.
// Zuerst Cache, dann ggf. synchroner GetUser/GetChat-Lookup mit Update des Caches.
func (c *TDLibClient) senderLabelFor(m *td.Message) string {
	if m == nil || m.SenderId == nil {
		return "Unbekannt"
	}
	if m.IsOutgoing {
		return "Du"
	}
	switch s := m.SenderId.(type) {
	case *td.MessageSenderUser:
		c.userMu.RLock()
		name, ok := c.nameByUser[s.UserId]
		c.userMu.RUnlock()
		if ok && name != "" {
			return name
		}
		u, err := c.tdClient.GetUser(&td.GetUserRequest{UserId: s.UserId})
		if err == nil && u != nil {
			n := userDisplayName(u)
			c.userMu.Lock()
			if c.nameByUser == nil {
				c.nameByUser = make(map[int64]string)
			}
			c.nameByUser[s.UserId] = n
			c.userMu.Unlock()
			return n
		}
		return fmt.Sprintf("User %d", s.UserId)
	case *td.MessageSenderChat:
		ch, err := c.tdClient.GetChat(&td.GetChatRequest{ChatId: s.ChatId})
		if err == nil && ch != nil && strings.TrimSpace(ch.Title) != "" {
			return strings.TrimSpace(ch.Title)
		}
		return fmt.Sprintf("Chat %d", s.ChatId)
	}
	return "Unbekannt"
}
