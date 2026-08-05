package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeConfig(t *testing.T, cfg Config) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestResolveAccountDefaults(t *testing.T) {
	cfg := &Config{Accounts: map[string]Account{
		"default": {JID: "pi@mymac.local", Owner: "zach@mymac.local"},
	}}
	got, err := resolveAccount(cfg, "")
	if err != nil {
		t.Fatalf("resolveAccount: %v", err)
	}
	if got.BonjourService != "_presence._tcp" {
		t.Errorf("BonjourService = %q, want _presence._tcp", got.BonjourService)
	}
	if got.DiscoverTimeout != 10*time.Second {
		t.Errorf("DiscoverTimeout = %s, want 10s", got.DiscoverTimeout)
	}
	if got.JID != "pi@mymac.local" || got.Owner != "zach@mymac.local" {
		t.Errorf("JID/Owner = %q/%q, want pi@mymac.local/zach@mymac.local", got.JID, got.Owner)
	}
	if got.Avatar != "" {
		t.Errorf("Avatar = %q, want empty", got.Avatar)
	}
}

func TestResolveAccountBonjourOptions(t *testing.T) {
	cfg := &Config{Accounts: map[string]Account{
		"default": {
			JID: "pi@mymac.local", Owner: "zach@mymac.local",
			BonjourService: "_jabber._tcp", BonjourName: "chat", DiscoverTimeout: "30s",
		},
	}}
	got, err := resolveAccount(cfg, "")
	if err != nil {
		t.Fatalf("resolveAccount: %v", err)
	}
	if got.BonjourService != "_jabber._tcp" {
		t.Errorf("BonjourService = %q, want _jabber._tcp", got.BonjourService)
	}
	if got.BonjourName != "chat" {
		t.Errorf("BonjourName = %q, want chat", got.BonjourName)
	}
	if got.DiscoverTimeout != 30*time.Second {
		t.Errorf("DiscoverTimeout = %s, want 30s", got.DiscoverTimeout)
	}
	if _, err := resolveAccount(&Config{Accounts: map[string]Account{
		"default": {JID: "a@x.local", Owner: "o@x.local", DiscoverTimeout: "soon"},
	}}, ""); err == nil {
		t.Error("expected error for invalid discoverTimeout, got nil")
	}
}

func TestResolveAccountSelection(t *testing.T) {
	cfg := &Config{Accounts: map[string]Account{
		"default": {JID: "a@x.local", Owner: "o@x.local"},
		"work":    {JID: "b@x.local", Owner: "o@x.local"},
	}}
	got, err := resolveAccount(cfg, "work")
	if err != nil {
		t.Fatalf("resolveAccount: %v", err)
	}
	if got.Name != "work" || got.JID != "b@x.local" {
		t.Errorf("selected %q/%q, want work/b@x.local", got.Name, got.JID)
	}
	// Unknown requested falls back to default.
	got, err = resolveAccount(cfg, "nope")
	if err != nil {
		t.Fatalf("resolveAccount fallback: %v", err)
	}
	if got.Name != "default" {
		t.Errorf("fallback selected %q, want default", got.Name)
	}
}

func TestResolveAccountMissingFields(t *testing.T) {
	cfg := &Config{Accounts: map[string]Account{
		"default": {JID: "a@x.local"},
	}}
	if _, err := resolveAccount(cfg, ""); err == nil {
		t.Fatal("expected error for missing owner, got nil")
	}
}

func TestLoadConfigMissing(t *testing.T) {
	_, err := loadConfig(filepath.Join(t.TempDir(), "nope.json"))
	if err == nil {
		t.Fatal("expected errNoConfig, got nil")
	}
}

func TestLoadConfigRoundTrip(t *testing.T) {
	path := writeConfig(t, Config{Accounts: map[string]Account{
		"default": {JID: "a@x.local", Owner: "o@x.local"},
	}})
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if _, ok := cfg.Accounts["default"]; !ok {
		t.Error("default account not loaded")
	}
}

func TestOldServerOnlyKeysIgnored(t *testing.T) {
	// Pre-existing configs with server-only keys (room, resource,
	// pingInterval, …) must keep loading — the decoder ignores unknown keys.
	var cfg Config
	raw := `{"accounts":{"default":{
		"jid":"pi@x.local","owner":"o@x.local",
		"room":"team@muc.x","resource":"pi-msg","nick":"pi","pingInterval":"2m"
	}}}`
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("unmarshal with server-only keys: %v", err)
	}
	got, err := resolveAccount(&cfg, "")
	if err != nil {
		t.Fatalf("resolveAccount with server-only keys: %v", err)
	}
	if got.JID != "pi@x.local" || got.Owner != "o@x.local" {
		t.Errorf("resolved JID/Owner = %q/%q, want pi@x.local/o@x.local", got.JID, got.Owner)
	}
}
