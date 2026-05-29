package telegram

import (
	"context"
	"fmt"
	"strings"
)

type MockClient struct {
	chats    []Chat
	messages map[int64][]Message
	nextID   int64
	apiID    int
	apiHash  string
}

func NewMockClient() *MockClient {
	return &MockClient{
		chats: []Chat{
			{ID: 1, Title: "Max Byte", Last: "Kernel kompiliert. Warum auch leben?", Unread: 2},
			{ID: 2, Title: "Dr. Leo Kraft", Last: "Heute kein Training ist auch keine Lösung.", Unread: 0},
			{ID: 3, Title: "NOVA", Last: "Ich habe darüber nachgedacht. Leider.", Unread: 5},
		},
		messages: map[int64][]Message{
			1: {
				{ID: 1, ChatID: 1, Sender: "Max", Text: "Arch läuft. Noch.", Outgoing: false},
				{ID: 2, ChatID: 1, Sender: "Du", Text: "Das klingt wie eine Drohung.", Outgoing: true},
			},
			2: {
				{ID: 3, ChatID: 2, Sender: "Leo", Text: "Mobilität zuerst, Ego danach.", Outgoing: false},
			},
			3: {
				{ID: 4, ChatID: 3, Sender: "NOVA", Text: "Ein Go-TUI ist sinnvoll. Bis du Medien willst.", Outgoing: false},
			},
		},
		nextID: 100,
	}
}

func (m *MockClient) Connect(ctx context.Context) error {
	return nil
}

func (m *MockClient) Login(ctx context.Context, auth AuthSettings) error {
	if auth.APIID <= 0 {
		return fmt.Errorf("api id fehlt oder ungültig")
	}
	if strings.TrimSpace(auth.APIHash) == "" {
		return fmt.Errorf("api hash fehlt")
	}
	m.apiID = auth.APIID
	m.apiHash = strings.TrimSpace(auth.APIHash)
	return nil
}

func (m *MockClient) LoginWithCode(ctx context.Context, auth AuthSettings) error {
	if err := m.Login(ctx, auth); err != nil {
		return err
	}
	if strings.TrimSpace(auth.Phone) == "" {
		return fmt.Errorf("telefonnummer fehlt")
	}
	if strings.TrimSpace(auth.Code) == "" {
		return fmt.Errorf("login-code fehlt")
	}
	return nil
}

func (m *MockClient) StartQRLogin(ctx context.Context) (string, error) {
	if m.apiID <= 0 || strings.TrimSpace(m.apiHash) == "" {
		return "", fmt.Errorf("zuerst API ID/API Hash setzen und Login ausführen")
	}
	token := fmt.Sprintf("mock-%d-%d", m.apiID, m.nextID+1)
	return "tg://login?token=" + token, nil
}

func (m *MockClient) ListChats(ctx context.Context) ([]Chat, error) {
	return m.chats, nil
}

func (m *MockClient) ListChatsInFolder(ctx context.Context, folderID int32) ([]Chat, error) {
	return m.chats, nil
}

func (m *MockClient) ListFolders(ctx context.Context) ([]Folder, error) {
	return []Folder{
		{ID: 0, Title: "Alle"},
		{ID: 1, Title: "Personal"},
		{ID: 2, Title: "Work"},
		{ID: -1, Title: "📁 Archiv"},
	}, nil
}

func (m *MockClient) ListMessages(ctx context.Context, chatID int64) ([]Message, error) {
	return m.messages[chatID], nil
}

func (m *MockClient) ListMessagesBefore(ctx context.Context, chatID, beforeMsgID int64, limit int) ([]Message, error) {
	return nil, nil
}

func (m *MockClient) SendMessage(ctx context.Context, chatID int64, text string) (Message, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Message{}, fmt.Errorf("empty message")
	}

	m.nextID++
	msg := Message{
		ID:       m.nextID,
		ChatID:   chatID,
		Sender:   "Du",
		Text:     text,
		Outgoing: true,
	}

	m.messages[chatID] = append(m.messages[chatID], msg)

	for i := range m.chats {
		if m.chats[i].ID == chatID {
			m.chats[i].Last = text
			break
		}
	}

	return msg, nil
}

func (m *MockClient) DownloadFile(ctx context.Context, fileID int32) (string, error) {
	return "", fmt.Errorf("mock: kein download")
}

func (m *MockClient) MarkChatRead(ctx context.Context, chatID int64) error {
	for i := range m.chats {
		if m.chats[i].ID == chatID {
			m.chats[i].Unread = 0
			break
		}
	}
	return nil
}

func (m *MockClient) Close() error {
	return nil
}
