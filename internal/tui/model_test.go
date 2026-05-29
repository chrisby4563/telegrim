package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"telegrim/internal/config"
	"telegrim/internal/telegram"
)

func TestAvatarIconDeterministic(t *testing.T) {
	a := avatarIcon("Max Byte")
	b := avatarIcon("Max Byte")
	if a != b {
		t.Fatalf("avatar icon must be deterministic")
	}
}

func TestAvatarIconVariesBySeed(t *testing.T) {
	a := avatarIcon("Max Byte")
	b := avatarIcon("NOVA")
	if a == b {
		t.Fatalf("expected different seeds to produce different icons")
	}
}

func TestAvatarIconIsLargerThanTwoCells(t *testing.T) {
	if w := lipgloss.Width(avatarIcon("Max Byte")); w < 4 {
		t.Fatalf("expected larger avatar width, got %d", w)
	}
}

func TestUnreadBadgeEmptyAtZero(t *testing.T) {
	if unreadBadge(0) != "" {
		t.Fatalf("expected empty badge for unread=0")
	}
}

func TestUnreadBadgeCapsAt99Plus(t *testing.T) {
	badge := unreadBadge(140)
	if badge == "" {
		t.Fatalf("expected non-empty badge")
	}
}

func TestClampMsgScrollBounds(t *testing.T) {
	if got := clampMsgScroll(10, 3, -4); got != 0 {
		t.Fatalf("expected lower clamp 0, got %d", got)
	}
	if got := clampMsgScroll(10, 3, 99); got != 7 {
		t.Fatalf("expected upper clamp 7, got %d", got)
	}
}

func TestFitToWidthNeverEmptyWhenWidthPositive(t *testing.T) {
	got := fitToWidth("abc", 2)
	if got == "" {
		t.Fatalf("fitToWidth should render output for positive width")
	}
}

func TestSettingsAuthValidation(t *testing.T) {
	m := NewModel(config.LoadDefault(), telegram.NewMockClient())
	m.settingsInputs[0].SetValue("12345")
	m.settingsInputs[1].SetValue("hash")
	m.settingsInputs[2].SetValue("+491****4567")
	m.settingsInputs[3].SetValue("12345")
	m.settingsInputs[4].SetValue("secret2fa")
	auth, err := m.settingsAuth()
	if err != nil {
		t.Fatalf("unexpected auth error: %v", err)
	}
	if auth.APIID != 12345 || auth.APIHash != "hash" {
		t.Fatalf("unexpected auth parsed: %+v", auth)
	}
	if auth.Password != "secret2fa" {
		t.Fatalf("expected password to be parsed")
	}
}

func TestSettingsAuthNeedsPhoneAndCode(t *testing.T) {
	m := NewModel(config.LoadDefault(), telegram.NewMockClient())
	m.settingsInputs[0].SetValue("12345")
	m.settingsInputs[1].SetValue("hash")
	if _, err := m.settingsAuth(); err == nil {
		t.Fatalf("expected settingsAuth to require phone and code")
	}
}

func TestRenderSettingsShowsQRLoginButton(t *testing.T) {
	m := NewModel(config.LoadDefault(), telegram.NewMockClient())
	m.width = 120
	m.height = 30
	m.setActiveTab(tabSettings)
	lines := strings.Join(m.renderSettingsLines(m.layout()), "\n")
	if !strings.Contains(lines, "[QR Login]") {
		t.Fatalf("expected [QR Login] button in settings view")
	}
	if !strings.Contains(lines, "[Terminal Login]") {
		t.Fatalf("expected [Terminal Login] button in settings view")
	}
	if !strings.Contains(lines, "Passwort (2FA)") {
		t.Fatalf("expected password input prompt in settings view")
	}
}

func TestRenderASCIIQRReturnsBlocks(t *testing.T) {
	qr, err := renderASCIIQR("https://t.me/login/example")
	if err != nil {
		t.Fatalf("unexpected qr render error: %v", err)
	}
	hasBraille := false
	for _, r := range qr {
		if r >= 0x2800 && r <= 0x28FF {
			hasBraille = true
			break
		}
	}
	if !hasBraille && !strings.ContainsAny(qr, "█▀▄") {
		t.Fatalf("expected block or braille characters in qr output")
	}
}

func TestRenderASCIIQRIsCompactEnoughForTerminal(t *testing.T) {
	qr, err := renderASCIIQR("https://t.me/login/example")
	if err != nil {
		t.Fatalf("unexpected qr render error: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(qr), "\n")
	if len(lines) > 24 {
		t.Fatalf("expected compact qr <= 24 lines, got %d", len(lines))
	}
}

func TestRenderSettingsQRShowsSizeWarningInsteadOfTruncatedQR(t *testing.T) {
	m := NewModel(config.LoadDefault(), telegram.NewMockClient())
	m.width = 70
	m.height = 12
	m.setActiveTab(tabSettings)
	m.qrASCII = strings.Repeat("████████████████████████████\n", 22)
	m.qrLoginURL = "tg://login?token=test"

	lines := strings.Join(m.renderSettingsLines(m.layout()), "\n")
	if !strings.Contains(lines, "QR passt nicht") {
		t.Fatalf("expected size warning when QR does not fit")
	}
}

func TestSettingsF3TriggersQRLoginCmd(t *testing.T) {
	m := NewModel(config.LoadDefault(), telegram.NewMockClient())
	m.setActiveTab(tabSettings)
	m.settingsInputs[0].SetValue("123")
	m.settingsInputs[1].SetValue("hash")

	// F7 ist jetzt der TG-QR-Hotkey (F3 wechselt nur den Tab).
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyF7})
	if cmd == nil {
		t.Fatalf("expected command for F7 TG QR login")
	}
	msg := cmd()
	qrMsg, ok := msg.(qrLoginReadyMsg)
	if !ok {
		t.Fatalf("expected qrLoginReadyMsg, got %T", msg)
	}
	if qrMsg.err != nil {
		t.Fatalf("unexpected qr login error: %v", qrMsg.err)
	}
	if strings.TrimSpace(qrMsg.url) == "" {
		t.Fatalf("expected qr url in message")
	}
	_ = next
}
