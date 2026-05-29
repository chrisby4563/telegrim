package telegram

import (
	"context"
	"testing"
)

func TestMockStartQRLoginNeedsCredentials(t *testing.T) {
	c := NewMockClient()
	if _, err := c.StartQRLogin(context.Background()); err == nil {
		t.Fatalf("expected StartQRLogin to fail without credentials")
	}
}

func TestMockStartQRLoginReturnsURLAfterLoginData(t *testing.T) {
	c := NewMockClient()
	err := c.Login(context.Background(), AuthSettings{APIID: 123, APIHash: "hash", Phone: "+491701234567", Code: "12345"})
	if err != nil {
		t.Fatalf("unexpected login validation error: %v", err)
	}
	url, err := c.StartQRLogin(context.Background())
	if err != nil {
		t.Fatalf("unexpected start qr login error: %v", err)
	}
	if url == "" {
		t.Fatalf("expected qr login url")
	}
}

func TestMockLoginNeedsPhoneAndCode(t *testing.T) {
	c := NewMockClient()
	if err := c.LoginWithCode(context.Background(), AuthSettings{APIID: 123, APIHash: "hash"}); err == nil {
		t.Fatalf("expected login error when phone/code are missing")
	}
}
