//go:build windows

package telegram

import (
	"context"
	"fmt"
)

// TDLibClient ist auf Windows nicht verfügbar — TDLib braucht CGo +
// libtdjson.dll Setup das aktuell nicht im Port-Scope liegt. Wir bauen einen
// Stub der bei jedem Call Fehler returnt, sodass main.go beim Backend-Wahl
// auf "gotd" oder "mock" fallback kann.
type TDLibClient struct{}

var _ Client = (*TDLibClient)(nil)

func NewTDLibClient(apiID int, apiHash string, dataDir string) *TDLibClient {
	return &TDLibClient{}
}

var errTDLibUnsupported = fmt.Errorf("tdlib: nicht unterstützt auf Windows — TELEGRIM_BACKEND=gotd setzen")

func (c *TDLibClient) Connect(ctx context.Context) error { return errTDLibUnsupported }
func (c *TDLibClient) Login(ctx context.Context, auth AuthSettings) error {
	return errTDLibUnsupported
}
func (c *TDLibClient) LoginWithCode(ctx context.Context, auth AuthSettings) error {
	return errTDLibUnsupported
}
func (c *TDLibClient) StartQRLogin(ctx context.Context) (string, error) {
	return "", errTDLibUnsupported
}
func (c *TDLibClient) ListChats(ctx context.Context) ([]Chat, error) { return nil, nil }
func (c *TDLibClient) ListChatsInFolder(ctx context.Context, folderID int32) ([]Chat, error) {
	return nil, nil
}
func (c *TDLibClient) ListFolders(ctx context.Context) ([]Folder, error) { return nil, nil }
func (c *TDLibClient) ListMessages(ctx context.Context, chatID int64) ([]Message, error) {
	return nil, nil
}
func (c *TDLibClient) ListMessagesBefore(ctx context.Context, chatID, beforeMsgID int64, limit int) ([]Message, error) {
	return nil, nil
}
func (c *TDLibClient) SendMessage(ctx context.Context, chatID int64, text string) (Message, error) {
	return Message{}, errTDLibUnsupported
}
func (c *TDLibClient) MarkChatRead(ctx context.Context, chatID int64) error { return nil }
func (c *TDLibClient) DownloadFile(ctx context.Context, fileID int32) (string, error) {
	return "", errTDLibUnsupported
}
func (c *TDLibClient) Close() error { return nil }
