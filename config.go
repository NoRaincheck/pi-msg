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

// roomList is the set of MUC JIDs from the "room" config field. It accepts
// either a single JID string ("room": "a@muc…") or an array of JID strings
// ("room": ["a@muc…", "b@muc…"]) so older single-room configs keep working.
type roomList []string

func (r *roomList) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*r = roomList{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return fmt.Errorf("\"room\" must be a JID string or an array of JID strings")
	}
	*r = many
	return nil
}

// Account is one Bonjour account the bridge can connect as, as stored in the
// config file. The bridge only ever connects to a local XMPP server discovered
// over Bonjour (mDNS/DNS-SD); there is no remote-server mode. Only jid and
// owner are required; the rest have defaults.
type Account struct {
	// JID is the bare JID of the bot account, e.g. "pi@mymac.local".
	JID string `json:"jid"`
	// Owner is the JID of the human this account relays to. In 1:1 mode it is
	// also the only JID whose messages drive the agent. In room mode it is the
	// canonical (trusted) participant.
	Owner string `json:"owner"`
	// BonjourService is the DNS-SD service type browsed to find the local XMPP
	// server. Defaults to "_xmpp-client._tcp" (with a legacy "_jabber._tcp"
	// fallback when unset).
	BonjourService string `json:"bonjourService,omitempty"`
	// BonjourName, when set, narrows discovery to a service instance whose name
	// contains this string — useful when several local servers advertise.
	BonjourName string `json:"bonjourName,omitempty"`
	// DiscoverTimeout is how long each Bonjour discovery attempt waits for a
	// server before failing. A Go duration string ("10s"). Defaults to "10s".
	DiscoverTimeout string `json:"discoverTimeout,omitempty"`
	// Resource is the XMPP client-session label. Defaults to "pi-msg".
	Resource string `json:"resource,omitempty"`
	// ToolActivity mirrors a one-line notice each time a tool starts.
	ToolActivity bool `json:"toolActivity,omitempty"`
	// Reactions, when true, enables XEP-0444 emoji reactions on 1:1 owner
	// messages: the run lifecycle maps to 👀 (picked up) / ✅ (done) / ⛔
	// (aborted), and the agent may react deliberately via a "react: <emoji>"
	// line. Off by default so it doesn't double up with read receipts + presence.
	Reactions bool `json:"reactions,omitempty"`
	// RoomReactions, when true, enables XEP-0444 emoji reactions on room
	// messages (both owner and addressed non-owner commentary). Independent of
	// the 1:1 reactions flag — you can opt into one, both, or neither.
	RoomReactions bool `json:"roomReactions,omitempty"`
	// Model is the model pattern to launch pi with (e.g.
	// "anthropic/claude-sonnet-latest"). Optional.
	Model string `json:"model,omitempty"`
	// Workdir is the working directory for the pi agent. Defaults to the
	// process cwd.
	Workdir string `json:"workdir,omitempty"`

	// Room, when set, additionally joins these bare MUC JIDs (e.g.
	// "team@muc.chat.example.com") and relays group chat. Accepts a single JID
	// string or an array of JID strings. The owner can still DM the bot 1:1 in
	// either mode; each reply goes back to whichever channel the message arrived
	// on.
	Room roomList `json:"room,omitempty"`
	// Nick is the occupant nickname used in the rooms. Defaults to the JID
	// localpart.
	Nick string `json:"nick,omitempty"`
	// RoomTrigger is the case-insensitive address prefix that makes a room
	// message a prompt for the agent (e.g. "pi" matches "pi: …" / "pi, …").
	// Defaults to Nick.
	RoomTrigger string `json:"roomTrigger,omitempty"`
	// UploadService is the XEP-0363 HTTP-upload component JID used for file
	// transfer. Optional; if unset the bridge probes "upload.<domain>" and
	// "httpupload.<domain>".
	UploadService string `json:"uploadService,omitempty"`
	// PingInterval is how often to send an XEP-0199 keepalive ping to the
	// server (and, in room mode, an XEP-0410 self-ping to each joined room) to
	// detect silent disconnects. A Go duration string ("60s", "2m"). Defaults
	// to "60s"; "0" disables keepalive.
	PingInterval string `json:"pingInterval,omitempty"`
	// Avatar is a path to a local image (PNG/JPEG/GIF) published as the bot's
	// XEP-0153 vCard avatar on connect. Optional; a missing/invalid file is a
	// logged warning, not fatal.
	Avatar string `json:"avatar,omitempty"`
}

// Config is the on-disk config: an arbitrary number of named accounts.
// "default" is used when no account is selected.
type Config struct {
	Accounts map[string]Account `json:"accounts"`
}

// ResolvedAccount is a fully-resolved account ready to connect with, defaults
// applied. RoomMode reports whether any room was set.
type ResolvedAccount struct {
	Name            string
	JID             string
	Owner           string
	BonjourService  string
	BonjourName     string
	DiscoverTimeout time.Duration
	Resource        string
	ToolActivity    bool
	Reactions       bool
	RoomReactions   bool
	Model           string
	Workdir         string
	Rooms           []string
	Nick            string
	RoomTrigger     string
	UploadService   string
	PingInterval    time.Duration
	Avatar          string
}

// RoomMode reports whether this account operates in MUC (group-chat) mode.
func (a ResolvedAccount) RoomMode() bool { return len(a.Rooms) > 0 }

const (
	defaultAccount  = "default"
	defaultResource = "pi-msg"
	// defaultPingInterval is the keepalive cadence when pingInterval is unset.
	defaultPingInterval = 60 * time.Second
	// defaultBonjourService is the DNS-SD service type browsed to find a local
	// XMPP server when bonjourService is unset: the Bonjour IM / serverless
	// messaging type used by Adium and Messages.
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

// localpart returns the part of a bare JID before '@', or the whole string if
// there is no '@'.
func localpart(jid string) string {
	if at := strings.IndexByte(jid, '@'); at >= 0 {
		return jid[:at]
	}
	return jid
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

	var rooms []string
	seen := make(map[string]bool)
	for _, rm := range acct.Room {
		rm = strings.TrimSpace(rm)
		if rm == "" || seen[rm] {
			continue
		}
		seen[rm] = true
		rooms = append(rooms, rm)
	}

	nick := acct.Nick
	if nick == "" {
		nick = localpart(acct.JID)
	}
	trigger := acct.RoomTrigger
	if trigger == "" {
		trigger = nick
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
	resource := acct.Resource
	if resource == "" {
		resource = defaultResource
	}
	pingInterval := defaultPingInterval
	if s := strings.TrimSpace(acct.PingInterval); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return ResolvedAccount{}, fmt.Errorf("pi-msg: account %q has invalid pingInterval %q: %w", name, s, err)
		}
		pingInterval = d
	}

	return ResolvedAccount{
		Name:            name,
		JID:             acct.JID,
		Owner:           acct.Owner,
		BonjourService:  bonjourService,
		BonjourName:     strings.TrimSpace(acct.BonjourName),
		DiscoverTimeout: discoverTimeout,
		Resource:        resource,
		ToolActivity:    acct.ToolActivity,
		Reactions:       acct.Reactions,
		RoomReactions:   acct.RoomReactions,
		Model:           acct.Model,
		Workdir:         acct.Workdir,
		Rooms:           rooms,
		Nick:            nick,
		RoomTrigger:     trigger,
		UploadService:   strings.TrimSpace(acct.UploadService),
		PingInterval:    pingInterval,
		Avatar:          strings.TrimSpace(acct.Avatar),
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
