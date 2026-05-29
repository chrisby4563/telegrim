package telegram

import "context"

// Platform-Konstanten für Multi-Backend-Unterstützung.
// "tg" = Telegram (TDLib), "wa" = WhatsApp (whatsmeow). Leerer String wird wie
// "tg" behandelt (Default für Legacy-Codepfade).
const (
	PlatformTG = "tg"
	PlatformWA = "wa"
)

type Chat struct {
	ID       int64
	Platform string // "tg" oder "wa"; leer = TG (Legacy).
	Title    string
	Last     string
	Unread   int
	// Presence-Info nur sinnvoll für 1:1-Chats; bei Gruppen/Kanälen Online=false.
	IsPrivate bool
	Online    bool
	// LastDate = Unix-Timestamp der letzten Nachricht; 0 wenn unbekannt.
	// Wird primär zur Sortierung verwendet (neueste oben).
	LastDate int32
	// IsChannel markiert read-only Broadcasts: TG-Kanäle, WA-Status. Werden
	// in einem separaten "Aktuelles"-Tab gerendert, nicht in der normalen
	// Kontaktliste.
	IsChannel bool
}

// Folder repräsentiert einen Telegram-Chat-Folder oder eine Pseudo-Liste.
// ID-Konvention: 0 = Main (Default), -1 = Archive, >0 = User-Folder.
type Folder struct {
	ID    int32
	Title string
}

// MessageStatus codiert den Versandzustand einer ausgehenden Nachricht.
// 0 = unbekannt/eingehend. Reihenfolge entspricht Fortschritt.
type MessageStatus int

const (
	MessageStatusNone MessageStatus = iota
	MessageStatusPending
	MessageStatusSent
	MessageStatusDelivered
	MessageStatusRead
)

type Message struct {
	ID       int64
	ChatID   int64
	Platform string // "tg" oder "wa"; leer = TG (Legacy).
	Sender   string
	Text     string
	Outgoing bool
	Status   MessageStatus `json:",omitempty"`
	// ExternalID = ID des Backends (z.B. whatsmeow MessageKey.ID). Wird zum
	// Dedupen in HistorySync-Pfaden verwendet. Leer = unbekannt (z.B. alte
	// persistierte Messages vor diesem Feld).
	ExternalID string `json:",omitempty"`
	// Date = Unix-Timestamp wann die Nachricht gesendet wurde.
	Date int32
	// Media ist nil bei Text-only Messages. Caption (falls vorhanden) steht
	// zusätzlich in Text.
	Media *Media
}

type MediaKind int

const (
	MediaNone MediaKind = iota
	MediaPhoto
	MediaVideo
	MediaAnimation // GIF
	MediaDocument
)

func (k MediaKind) Icon() string {
	switch k {
	case MediaPhoto:
		return "📷"
	case MediaVideo:
		return "🎬"
	case MediaAnimation:
		return "🎞"
	case MediaDocument:
		return "📎"
	default:
		return ""
	}
}

func (k MediaKind) Label() string {
	switch k {
	case MediaPhoto:
		return "Foto"
	case MediaVideo:
		return "Video"
	case MediaAnimation:
		return "GIF"
	case MediaDocument:
		return "Datei"
	default:
		return ""
	}
}

// Media beschreibt eine Datei-Anhang einer Nachricht. FileID wird zum Download
// via Client.DownloadFile genutzt; LocalPath ist gesetzt sobald die Datei
// lokal vorhanden ist (TDLib cached selbst auf Disk).
type Media struct {
	Kind      MediaKind
	FileID    int32
	FileName  string
	MimeType  string
	Size      int64
	Width     int32
	Height    int32
	Duration  int32 // Sekunden, nur Video/Animation
	LocalPath string
}

type AuthSettings struct {
	APIID    int
	APIHash  string
	Phone    string
	Code     string
	Password string
}

type Client interface {
	Connect(ctx context.Context) error
	Login(ctx context.Context, auth AuthSettings) error
	LoginWithCode(ctx context.Context, auth AuthSettings) error
	StartQRLogin(ctx context.Context) (string, error)
	ListChats(ctx context.Context) ([]Chat, error)
	ListChatsInFolder(ctx context.Context, folderID int32) ([]Chat, error)
	ListFolders(ctx context.Context) ([]Folder, error)
	ListMessages(ctx context.Context, chatID int64) ([]Message, error)
	// ListMessagesBefore lädt ältere Nachrichten vor einer bekannten Message-ID.
	// Rückgabe ist chronologisch (älteste zuerst). Leere Slice = keine älteren mehr.
	ListMessagesBefore(ctx context.Context, chatID, beforeMsgID int64, limit int) ([]Message, error)
	SendMessage(ctx context.Context, chatID int64, text string) (Message, error)
	// MarkChatRead setzt den ungelesen-Zähler eines Chats auf 0 und schickt
	// (wenn möglich) eine Lesebestätigung ans Backend, damit es synchron mit
	// dem Original-Client läuft.
	MarkChatRead(ctx context.Context, chatID int64) error
	// DownloadFile lädt eine Datei (per TDLib-FileID) und gibt nach Abschluss
	// den lokalen Pfad zurück. Falls schon im Cache, sofortige Rückgabe.
	DownloadFile(ctx context.Context, fileID int32) (string, error)
	Close() error
}
