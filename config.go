package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Account is one Bonjour account the bridge can connect as, as stored in the
// config file. The bridge connects peer-to-peer (serverless messaging,
// XEP-0174) to the owner's Bonjour IM client over the LAN; there is no XMPP
// server anywhere. Only jid and owner are required; the rest have defaults.
// Old server-only keys (room, resource, uploadService, pingInterval, …) are
// silently ignored by the JSON decoder, so pre-existing configs keep loading.
type Account struct {
	// JID is the bare JID of the bot account, e.g. "pi@mymac.local". It is the
	// identity pi-msg advertises over Bonjour and presents in the peer stream.
	JID string `json:"jid"`
	// Owner is the JID of the human this account relays to, and the only JID
	// whose messages drive the agent. pi-msg connects directly to this JID's
	// advertised Bonjour IM instance.
	Owner string `json:"owner"`
	// BonjourService is the DNS-SD service type browsed to find the owner's
	// Bonjour IM client. Defaults to "_presence._tcp".
	BonjourService string `json:"bonjourService,omitempty"`
	// BonjourName, when set, narrows discovery to a service instance whose name
	// contains this string — useful when several users advertise.
	BonjourName string `json:"bonjourName,omitempty"`
	// DiscoverTimeout is how long each Bonjour discovery attempt waits for the
	// owner's instance before failing. A Go duration string ("10s"). Defaults
	// to "10s".
	DiscoverTimeout string `json:"discoverTimeout,omitempty"`
	// ToolActivity mirrors a one-line notice each time a tool starts.
	ToolActivity bool `json:"toolActivity,omitempty"`
	// Reactions, when true, enables XEP-0444 emoji reactions on owner
	// messages: the run lifecycle maps to 👀 (picked up) / ✅ (done) / ⛔
	// (aborted), and the agent may react deliberately via the send_reaction
	// tool. Off by default so it doesn't double up with read receipts.
	Reactions bool `json:"reactions,omitempty"`
	// Model is the model pattern to launch pi with (e.g.
	// "anthropic/claude-sonnet-latest"). Optional.
	Model string `json:"model,omitempty"`
	// Workdir is the working directory for the pi agent. Defaults to the
	// process cwd.
	Workdir string `json:"workdir,omitempty"`
	// Avatar is a path to a local image (PNG/JPEG/GIF) whose SHA-1 hash is
	// advertised as the bot's XEP-0153 photo hash in the Bonjour TXT record
	// (the "phsh" key). Optional; a missing/invalid file is a logged warning,
	// not fatal.
	Avatar string `json:"avatar,omitempty"`
	// Alias is the display alias advertised as the XEP-0174 "1st" key in the
	// bot's Bonjour TXT record, shown as the contact's name in Bonjour IM
	// clients (e.g. Adium). Optional; absent means clients fall back to the
	// JID.
	Alias string `json:"alias,omitempty"`
}

// Config is the on-disk config: an arbitrary number of named accounts.
// "default" is used when no account is selected.
type Config struct {
	Accounts map[string]Account `json:"accounts"`
}

// ResolvedAccount is a fully-resolved account ready to connect with, defaults
// applied.
type ResolvedAccount struct {
	Name            string
	JID             string
	Owner           string
	BonjourService  string
	BonjourName     string
	DiscoverTimeout time.Duration
	ToolActivity    bool
	Reactions       bool
	Model           string
	Workdir         string
	Avatar          string
	Alias           string
}

const (
	defaultAccount = "default"
	// defaultBonjourService is the DNS-SD service type browsed to find a local
	// Bonjour IM user (XEP-0174 serverless messaging), as advertised by Adium
	// and Messages.
	defaultBonjourService = "_presence._tcp"
	// defaultDiscoverTimeout is how long each Bonjour discovery attempt waits.
	defaultDiscoverTimeout = 10 * time.Second
)

// configPath returns the config file path: $PI_MSG_CONFIG or
// ~/.config/pi-msg/config.json.
func configPath() string {
	if p := os.Getenv("PI_MSG_CONFIG"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".config", "pi-msg", "config.json")
	}
	return filepath.Join(home, ".config", "pi-msg", "config.json")
}

// errNoConfig is returned by loadConfig when the config file does not exist,
// so main can distinguish "not set up" from a real read/parse error.
var errNoConfig = errors.New("pi-msg: no config file")

// loadConfig reads and parses the config file. It returns errNoConfig
// (wrapped) if the file does not exist.
func loadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w at %s", errNoConfig, path)
		}
		return nil, fmt.Errorf("pi-msg: cannot read config at %s: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("pi-msg: config at %s is not valid JSON: %w", path, err)
	}
	if cfg.Accounts == nil {
		return nil, fmt.Errorf("pi-msg: config at %s must have an \"accounts\" object", path)
	}
	return &cfg, nil
}

// resolveAccount selects and validates an account. Selection order:
// requested (if present in the file) -> "default". It returns a
// human-readable error on any misconfiguration.
func resolveAccount(cfg *Config, requested string) (ResolvedAccount, error) {
	if len(cfg.Accounts) == 0 {
		return ResolvedAccount{}, errors.New("pi-msg: config has no accounts")
	}

	name := defaultAccount
	if _, ok := cfg.Accounts[requested]; requested != "" && ok {
		name = requested
	}
	acct, ok := cfg.Accounts[name]
	if !ok {
		names := accountNames(cfg)
		if requested != "" {
			return ResolvedAccount{}, fmt.Errorf("pi-msg: account %q not found and no %q account defined", requested, defaultAccount)
		}
		return ResolvedAccount{}, fmt.Errorf("pi-msg: no %q account defined (set PI_MSG_ACCOUNT to one of: %s)", defaultAccount, strings.Join(names, ", "))
	}

	var missing []string
	if acct.JID == "" {
		missing = append(missing, "jid")
	}
	if acct.Owner == "" {
		missing = append(missing, "owner")
	}
	if len(missing) > 0 {
		return ResolvedAccount{}, fmt.Errorf("pi-msg: account %q is missing required field(s): %s", name, strings.Join(missing, ", "))
	}

	bonjourService := acct.BonjourService
	if bonjourService == "" {
		bonjourService = defaultBonjourService
	}
	discoverTimeout := defaultDiscoverTimeout
	if s := strings.TrimSpace(acct.DiscoverTimeout); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return ResolvedAccount{}, fmt.Errorf("pi-msg: account %q has invalid discoverTimeout %q: %w", name, s, err)
		}
		discoverTimeout = d
	}

	return ResolvedAccount{
		Name:            name,
		JID:             acct.JID,
		Owner:           acct.Owner,
		BonjourService:  bonjourService,
		BonjourName:     strings.TrimSpace(acct.BonjourName),
		DiscoverTimeout: discoverTimeout,
		ToolActivity:    acct.ToolActivity,
		Reactions:       acct.Reactions,
		Model:           acct.Model,
		Workdir:         acct.Workdir,
		Avatar:          strings.TrimSpace(acct.Avatar),
		Alias:           strings.TrimSpace(acct.Alias),
	}, nil
}

// accountNames returns the configured account names (unsorted).
func accountNames(cfg *Config) []string {
	names := make([]string, 0, len(cfg.Accounts))
	for n := range cfg.Accounts {
		names = append(names, n)
	}
	return names
}
