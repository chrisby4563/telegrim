package config

import "testing"

func TestLoadDefaultBackendFallbackToTDLib(t *testing.T) {
	t.Setenv("TELEGRIM_BACKEND", "")
	cfg := LoadDefault()
	if cfg.Backend != "tdlib" {
		t.Fatalf("expected default backend tdlib, got %q", cfg.Backend)
	}
}

func TestLoadDefaultBackendGotd(t *testing.T) {
	t.Setenv("TELEGRIM_BACKEND", "gotd")
	cfg := LoadDefault()
	if cfg.Backend != "gotd" {
		t.Fatalf("expected backend gotd, got %q", cfg.Backend)
	}
}

func TestLoadDefaultBackendMock(t *testing.T) {
	t.Setenv("TELEGRIM_BACKEND", "mock")
	cfg := LoadDefault()
	if cfg.Backend != "mock" {
		t.Fatalf("expected backend mock, got %q", cfg.Backend)
	}
}
