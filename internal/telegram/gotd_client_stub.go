package telegram

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/session"
	gtg "github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/auth/qrlogin"
	"github.com/gotd/td/tg"
)

// GotdClient ist ein produktiv testbarer Telegram-Client auf Basis von gotd/td.
// Auth-Phase nutzt einen persistenten Run mit channel-basiertem
// UserAuthenticator → Login = SendCode (SMS), LoginWithCode = SignIn (+ 2FA).
// Nach erfolgreichem Auth bleibt eine persistente Verbindung offen; alle
// API-Calls teilen sich diese eine TCP-Verbindung (connMu-geschützt).
type GotdClient struct {
	mu sync.RWMutex

	apiID       int
	apiHash     string
	sessionFile string

	authMu     sync.Mutex
	auth       *gotdAuth
	authCancel context.CancelFunc
	authDoneCh chan error // 1-Slot; gepostet wenn IfNecessary returnt

	peerCache map[int64]tg.InputPeerClass

	// Persistente Verbindung — einmal gestartet, für alle API-Calls geteilt.
	connMu    sync.Mutex
	connAPI   *gtg.Client    // gesetzt innerhalb Run(), nil wenn getrennt
	connCtx   context.Context
	connStop  context.CancelFunc
	connReady chan struct{} // closed sobald connAPI gesetzt ist
}

// gotdAuth implementiert auth.UserAuthenticator über Channels. Phone wird
// einmal beim Auth-Start gepusht; Code + Password kommen via späteren
// LoginWithCode-Call ans Channel.
type gotdAuth struct {
	phone      string
	codeCh     chan string
	pwCh       chan string
	codeSentCh chan struct{} // close-on-first-SendCode-success
}

func (a *gotdAuth) Phone(ctx context.Context) (string, error) { return a.phone, nil }

func (a *gotdAuth) Code(ctx context.Context, sentCode *tg.AuthSentCode) (string, error) {
	// SMS ist raus → Login() entriegeln, dann auf Code warten.
	select {
	case <-a.codeSentCh:
	default:
		close(a.codeSentCh)
	}
	select {
	case s := <-a.codeCh:
		return s, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (a *gotdAuth) Password(ctx context.Context) (string, error) {
	select {
	case s := <-a.pwCh:
		if strings.TrimSpace(s) == "" {
			return "", auth.ErrPasswordNotProvided
		}
		return s, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (a *gotdAuth) AcceptTermsOfService(ctx context.Context, tos tg.HelpTermsOfService) error {
	return nil
}

func (a *gotdAuth) SignUp(ctx context.Context) (auth.UserInfo, error) {
	return auth.UserInfo{}, fmt.Errorf("gotd: Sign-Up nicht unterstützt")
}

func NewGotdClient(apiID int, apiHash string, sessionFile string) *GotdClient {
	if strings.TrimSpace(sessionFile) == "" {
		sessionFile = "telegrim.session.json"
	}
	return &GotdClient{
		apiID:       apiID,
		apiHash:     strings.TrimSpace(apiHash),
		sessionFile: sessionFile,
		peerCache:   map[int64]tg.InputPeerClass{},
	}
}

func (g *GotdClient) Connect(ctx context.Context) error {
	return g.ensureConn(ctx)
}

// Login speichert API-Credentials. Wenn auch eine Telefonnummer dabei ist,
// wird der persistente Auth-Flow gestartet: dieser triggert SendCode (SMS).
// Returnt nil sobald der Code per SMS unterwegs ist — der eigentliche
// SignIn passiert in LoginWithCode.
func (g *GotdClient) Login(ctx context.Context, settings AuthSettings) error {
	if settings.APIID <= 0 {
		return fmt.Errorf("api id fehlt oder ungültig")
	}
	if strings.TrimSpace(settings.APIHash) == "" {
		return fmt.Errorf("api hash fehlt")
	}
	g.mu.Lock()
	g.apiID = settings.APIID
	g.apiHash = strings.TrimSpace(settings.APIHash)
	g.mu.Unlock()

	phone := strings.TrimSpace(settings.Phone)
	if phone == "" {
		// Kein Phone → nur Credentials gespeichert. SMS kommt erst wenn
		// ein späterer Login-Call mit Phone reinkommt.
		return nil
	}
	return g.startAuthFlow(phone)
}

// startAuthFlow stößt einen persistenten gotd.Run() an mit unserem channel-
// basierten UserAuthenticator. Blockiert bis SMS-versendet-Signal kommt.
func (g *GotdClient) startAuthFlow(phone string) error {
	g.authMu.Lock()
	// Wenn schon ein Flow läuft → den abräumen + frisch starten. Sonst
	// kollidiert ein vorheriger Versuch mit neuem Phone.
	if g.authCancel != nil {
		g.authCancel()
		g.authCancel = nil
		g.auth = nil
	}
	if err := g.ensureSessionDir(); err != nil {
		g.authMu.Unlock()
		return err
	}
	appID, appHash, err := g.credentials()
	if err != nil {
		g.authMu.Unlock()
		return err
	}

	a := &gotdAuth{
		phone:      phone,
		codeCh:     make(chan string, 1),
		pwCh:       make(chan string, 1),
		codeSentCh: make(chan struct{}),
	}
	authCtx, cancel := context.WithCancel(context.Background())
	g.auth = a
	g.authCancel = cancel
	g.authDoneCh = make(chan error, 1)
	doneCh := g.authDoneCh
	sessionFile := g.sessionFile
	g.authMu.Unlock()

	client := gtg.NewClient(appID, appHash, gtg.Options{
		SessionStorage: &session.FileStorage{Path: sessionFile},
	})

	go func() {
		err := client.Run(authCtx, func(ctx context.Context) error {
			flow := auth.NewFlow(a, auth.SendCodeOptions{})
			if err := client.Auth().IfNecessary(ctx, flow); err != nil {
				return err
			}
			return nil
		})
		doneCh <- err
	}()

	// Auf SendCode-Quittung warten (max 30s).
	select {
	case <-a.codeSentCh:
		return nil
	case err := <-doneCh:
		// Fertig bevor Code gefragt wurde = vermutlich Fehler (bad credentials,
		// rate limit etc.). Saubere Diagnose zurückgeben.
		if err == nil {
			return nil
		}
		return formatGotdAuthErr(err)
	case <-time.After(30 * time.Second):
		return fmt.Errorf("gotd: timeout beim Code-Versand (Telegram nicht erreichbar?)")
	}
}

// LoginWithCode reicht Code (+ optional 2FA-Passwort) in den laufenden Auth-
// Flow ein und wartet auf den Endzustand. Wenn der Flow noch nicht läuft,
// wird er hier nachgestartet.
func (g *GotdClient) LoginWithCode(ctx context.Context, settings AuthSettings) error {
	if err := g.Login(ctx, settings); err != nil {
		return err
	}
	code := strings.TrimSpace(settings.Code)
	if code == "" {
		return fmt.Errorf("login-code fehlt")
	}

	g.authMu.Lock()
	a := g.auth
	doneCh := g.authDoneCh
	g.authMu.Unlock()
	if a == nil || doneCh == nil {
		return fmt.Errorf("gotd: kein aktiver Auth-Flow — zuerst Phone-Number senden")
	}

	// Code einreichen (non-blocking: Channel ist 1-Slot).
	select {
	case a.codeCh <- code:
	default:
	}
	// Password optional einreichen — Password() blockt sonst, wenn 2FA an ist.
	pw := strings.TrimSpace(settings.Password)
	if pw != "" {
		select {
		case a.pwCh <- pw:
		default:
		}
	} else {
		// Kein Passwort gegeben → ggf. mit auth.ErrPasswordNotProvided abbrechen.
		select {
		case a.pwCh <- "":
		default:
		}
	}

	select {
	case err := <-doneCh:
		g.authMu.Lock()
		if g.authCancel != nil {
			g.authCancel()
			g.authCancel = nil
		}
		g.auth = nil
		g.authMu.Unlock()
		if err != nil {
			return formatGotdAuthErr(err)
		}
		return nil
	case <-time.After(60 * time.Second):
		return fmt.Errorf("gotd: timeout beim SignIn")
	}
}

func formatGotdAuthErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, auth.ErrPasswordNotProvided) {
		return fmt.Errorf("2FA-Passwort fehlt: bitte Feld 'Passwort (2FA)' ausfüllen")
	}
	if errors.Is(err, auth.ErrPasswordInvalid) {
		return fmt.Errorf("2FA-Passwort ist ungültig")
	}
	return fmt.Errorf("telegram login fehlgeschlagen: %w", err)
}

func (g *GotdClient) StartQRLogin(ctx context.Context) (string, error) {
	if _, _, err := g.credentials(); err != nil {
		return "", err
	}
	var url string
	err := g.withClient(ctx, func(ctx context.Context, c *gtg.Client) error {
		appID, appHash, err := g.credentials()
		if err != nil {
			return err
		}
		token, err := qrlogin.NewQR(c.API(), appID, appHash, qrlogin.Options{}).Export(ctx)
		if err != nil {
			return fmt.Errorf("qr token export fehlgeschlagen: %w", err)
		}
		url = token.URL()
		return nil
	})
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(url) == "" {
		return "", fmt.Errorf("leere qr login url")
	}
	return url, nil
}

func (g *GotdClient) ListChatsInFolder(ctx context.Context, folderID int32) ([]Chat, error) {
	return g.ListChats(ctx)
}

func (g *GotdClient) ListFolders(ctx context.Context) ([]Folder, error) {
	return []Folder{{ID: 0, Title: "Alle"}}, nil
}

func (g *GotdClient) ListChats(ctx context.Context) ([]Chat, error) {
	var out []Chat
	err := g.withClient(ctx, func(ctx context.Context, c *gtg.Client) error {
		resp, err := c.API().MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{
			OffsetPeer: &tg.InputPeerEmpty{},
			Limit:      50,
		})
		if err != nil {
			return fmt.Errorf("dialogs laden fehlgeschlagen: %w", err)
		}

		dialogs, messages, chats, users := unpackDialogs(resp)
		msgByID := map[int]*tg.Message{}
		for _, m := range messages {
			if mm, ok := m.(*tg.Message); ok {
				msgByID[mm.ID] = mm
			}
		}

		userName := map[int64]string{}
		chatName := map[int64]string{}
		peerInputs := map[int64]tg.InputPeerClass{}

		for _, u := range users {
			uu, ok := u.(*tg.User)
			if !ok {
				continue
			}
			name := strings.TrimSpace(strings.TrimSpace(uu.FirstName + " " + uu.LastName))
			if name == "" {
				if v, ok := uu.GetUsername(); ok && strings.TrimSpace(v) != "" {
					name = "@" + strings.TrimSpace(v)
				}
			}
			if name == "" {
				name = fmt.Sprintf("User %d", uu.ID)
			}
			userName[uu.ID] = name
			if ah, ok := uu.GetAccessHash(); ok {
				peerInputs[uu.ID] = &tg.InputPeerUser{UserID: uu.ID, AccessHash: ah}
			}
		}

		for _, ch := range chats {
			switch cc := ch.(type) {
			case *tg.Chat:
				chatName[cc.ID] = strings.TrimSpace(cc.Title)
				peerInputs[-cc.ID] = &tg.InputPeerChat{ChatID: cc.ID}
			case *tg.Channel:
				chatName[cc.ID] = strings.TrimSpace(cc.Title)
				if ah, ok := cc.GetAccessHash(); ok {
					peerInputs[-1000000000000-cc.ID] = &tg.InputPeerChannel{ChannelID: cc.ID, AccessHash: ah}
				}
			}
		}

		rows := make([]Chat, 0, len(dialogs))
		for _, d := range dialogs {
			dialog, ok := d.(*tg.Dialog)
			if !ok {
				continue
			}
			id := encodePeerID(dialog.Peer)
			if id == 0 {
				continue
			}
			title := resolvePeerTitle(dialog.Peer, userName, chatName)
			last := ""
			if m := msgByID[dialog.TopMessage]; m != nil {
				last = strings.TrimSpace(m.Message)
			}
			if title == "" {
				title = fmt.Sprintf("Chat %d", id)
			}
			rows = append(rows, Chat{
				ID:     id,
				Title:  title,
				Last:   last,
				Unread: dialog.UnreadCount,
			})
		}

		g.mu.Lock()
		g.peerCache = peerInputs
		g.mu.Unlock()
		out = rows
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (g *GotdClient) ListMessages(ctx context.Context, chatID int64) ([]Message, error) {
	peer, err := g.resolvePeer(ctx, chatID)
	if err != nil {
		return nil, err
	}

	var out []Message
	err = g.withClient(ctx, func(ctx context.Context, c *gtg.Client) error {
		resp, err := c.API().MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
			Peer:  peer,
			Limit: 50,
		})
		if err != nil {
			return fmt.Errorf("messages laden fehlgeschlagen: %w", err)
		}

		msgs, chats, users := unpackMessages(resp)
		userName := map[int64]string{}
		for _, u := range users {
			if uu, ok := u.(*tg.User); ok {
				name := strings.TrimSpace(strings.TrimSpace(uu.FirstName + " " + uu.LastName))
				if name == "" {
					if v, ok := uu.GetUsername(); ok && strings.TrimSpace(v) != "" {
						name = "@" + strings.TrimSpace(v)
					}
				}
				if name == "" {
					name = fmt.Sprintf("User %d", uu.ID)
				}
				userName[uu.ID] = name
			}
		}
		chatName := map[int64]string{}
		for _, ch := range chats {
			switch cc := ch.(type) {
			case *tg.Chat:
				chatName[cc.ID] = strings.TrimSpace(cc.Title)
			case *tg.Channel:
				chatName[cc.ID] = strings.TrimSpace(cc.Title)
			}
		}

		rows := make([]Message, 0, len(msgs))
		for _, m := range msgs {
			mm, ok := m.(*tg.Message)
			if !ok {
				continue
			}
			sender := "Unbekannt"
			if from, ok := mm.GetFromID(); ok {
				sender = resolvePeerTitle(from, userName, chatName)
			} else {
				sender = resolvePeerTitle(mm.PeerID, userName, chatName)
			}
			if sender == "" {
				sender = "Unbekannt"
			}
			rows = append(rows, Message{
				ID:       int64(mm.ID),
				ChatID:   chatID,
				Sender:   sender,
				Text:     mm.Message,
				Outgoing: mm.Out,
			})
		}
		out = rows
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (g *GotdClient) ListMessagesBefore(ctx context.Context, chatID, beforeMsgID int64, limit int) ([]Message, error) {
	return nil, nil
}

func (g *GotdClient) SendMessage(ctx context.Context, chatID int64, text string) (Message, error) {
	peer, err := g.resolvePeer(ctx, chatID)
	if err != nil {
		return Message{}, err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return Message{}, fmt.Errorf("empty message")
	}

	err = g.withClient(ctx, func(ctx context.Context, c *gtg.Client) error {
		_, err := c.API().MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer:     peer,
			Message:  text,
			RandomID: time.Now().UnixNano(),
		})
		if err != nil {
			return fmt.Errorf("nachricht senden fehlgeschlagen: %w", err)
		}
		return nil
	})
	if err != nil {
		return Message{}, err
	}

	return Message{
		ID:       time.Now().UnixNano(),
		ChatID:   chatID,
		Sender:   "Du",
		Text:     text,
		Outgoing: true,
	}, nil
}

func (g *GotdClient) DownloadFile(ctx context.Context, fileID int32) (string, error) {
	return "", fmt.Errorf("gotd: download nicht unterstützt")
}

func (g *GotdClient) MarkChatRead(ctx context.Context, chatID int64) error {
	return nil
}

func (g *GotdClient) Close() error {
	g.authMu.Lock()
	if g.authCancel != nil {
		g.authCancel()
		g.authCancel = nil
	}
	g.auth = nil
	g.authMu.Unlock()

	g.connMu.Lock()
	if g.connStop != nil {
		g.connStop()
		g.connStop = nil
	}
	g.connMu.Unlock()
	return nil
}

// ensureConn stellt sicher dass eine persistente Verbindung zu Telegram läuft.
// Beim ersten Aufruf wird der Client gestartet; danach wird die bestehende
// Verbindung wiederverwendet. Falls die Verbindung abgebrochen ist, wird sie
// neu aufgebaut.
func (g *GotdClient) ensureConn(waitCtx context.Context) error {
	g.connMu.Lock()
	if g.connReady != nil {
		ready := g.connReady
		g.connMu.Unlock()
		select {
		case <-ready:
			return nil
		case <-waitCtx.Done():
			return waitCtx.Err()
		}
	}

	appID, appHash, err := g.credentials()
	if err != nil {
		g.connMu.Unlock()
		return err
	}
	if err := g.ensureSessionDir(); err != nil {
		g.connMu.Unlock()
		return err
	}

	_ = rand.Reader
	readyCh := make(chan struct{})
	g.connReady = readyCh
	connCtx, stop := context.WithCancel(context.Background())
	g.connStop = stop
	sessionFile := g.sessionFile
	g.connMu.Unlock()

	client := gtg.NewClient(appID, appHash, gtg.Options{
		SessionStorage: &session.FileStorage{Path: sessionFile},
	})

	go func() {
		_ = client.Run(connCtx, func(ctx context.Context) error {
			g.connMu.Lock()
			g.connAPI = client
			g.connCtx = ctx
			g.connMu.Unlock()
			close(readyCh)
			<-ctx.Done()
			return nil
		})
		// Verbindung beendet — zurücksetzen damit nächster withClient-Aufruf
		// eine neue Verbindung aufbaut.
		g.connMu.Lock()
		g.connAPI = nil
		g.connCtx = nil
		g.connReady = nil
		g.connMu.Unlock()
	}()

	select {
	case <-readyCh:
		return nil
	case <-waitCtx.Done():
		return waitCtx.Err()
	}
}

func (g *GotdClient) withClient(ctx context.Context, fn func(context.Context, *gtg.Client) error) error {
	if err := g.ensureConn(ctx); err != nil {
		return err
	}
	g.connMu.Lock()
	api := g.connAPI
	connCtx := g.connCtx
	g.connMu.Unlock()
	if api == nil {
		return fmt.Errorf("keine Verbindung zu Telegram")
	}
	err := fn(connCtx, api)
	if err != nil {
		// Bei FLOOD_WAIT die angegebene Wartezeit einhalten und einmal wiederholen.
		var wait int
		if _, scanErr := fmt.Sscanf(err.Error(), "dialogs laden fehlgeschlagen: rpcDoRequest: rpc error code 420: FLOOD_WAIT (%d)", &wait); scanErr != nil {
			fmt.Sscanf(err.Error(), "rpcDoRequest: rpc error code 420: FLOOD_WAIT (%d)", &wait)
		}
		if wait <= 0 {
			// generischer FLOOD_WAIT ohne Parse-Erfolg
			if strings.Contains(err.Error(), "FLOOD_WAIT") {
				wait = 5
			}
		}
		if wait > 0 {
			select {
			case <-time.After(time.Duration(wait+1) * time.Second):
			case <-ctx.Done():
				return ctx.Err()
			}
			return fn(connCtx, api)
		}
	}
	return err
}

func (g *GotdClient) credentials() (int, string, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.apiID <= 0 {
		return 0, "", fmt.Errorf("api id fehlt oder ungültig")
	}
	if strings.TrimSpace(g.apiHash) == "" {
		return 0, "", fmt.Errorf("api hash fehlt")
	}
	return g.apiID, g.apiHash, nil
}

func (g *GotdClient) ensureSessionDir() error {
	dir := filepath.Dir(g.sessionFile)
	if dir == "." || dir == "" {
		return nil
	}
	return os.MkdirAll(dir, 0o700)
}

func (g *GotdClient) resolvePeer(ctx context.Context, chatID int64) (tg.InputPeerClass, error) {
	g.mu.RLock()
	peer, ok := g.peerCache[chatID]
	g.mu.RUnlock()
	if ok {
		return peer, nil
	}
	if _, err := g.ListChats(ctx); err != nil {
		return nil, err
	}
	g.mu.RLock()
	peer, ok = g.peerCache[chatID]
	g.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("chat %d nicht gefunden", chatID)
	}
	return peer, nil
}

func unpackDialogs(v tg.MessagesDialogsClass) ([]tg.DialogClass, []tg.MessageClass, []tg.ChatClass, []tg.UserClass) {
	switch d := v.(type) {
	case *tg.MessagesDialogs:
		return d.Dialogs, d.Messages, d.Chats, d.Users
	case *tg.MessagesDialogsSlice:
		return d.Dialogs, d.Messages, d.Chats, d.Users
	default:
		return nil, nil, nil, nil
	}
}

func unpackMessages(v tg.MessagesMessagesClass) ([]tg.MessageClass, []tg.ChatClass, []tg.UserClass) {
	switch m := v.(type) {
	case *tg.MessagesMessages:
		return m.Messages, m.Chats, m.Users
	case *tg.MessagesMessagesSlice:
		return m.Messages, m.Chats, m.Users
	case *tg.MessagesChannelMessages:
		return m.Messages, m.Chats, m.Users
	default:
		return nil, nil, nil
	}
}

func encodePeerID(peer tg.PeerClass) int64 {
	switch p := peer.(type) {
	case *tg.PeerUser:
		return p.UserID
	case *tg.PeerChat:
		return -p.ChatID
	case *tg.PeerChannel:
		return -1000000000000 - p.ChannelID
	default:
		return 0
	}
}

func resolvePeerTitle(peer tg.PeerClass, userName map[int64]string, chatName map[int64]string) string {
	switch p := peer.(type) {
	case *tg.PeerUser:
		return userName[p.UserID]
	case *tg.PeerChat:
		return chatName[p.ChatID]
	case *tg.PeerChannel:
		return chatName[p.ChannelID]
	default:
		return ""
	}
}
