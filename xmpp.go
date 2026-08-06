package main

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"mellium.im/xmlstream"
	"mellium.im/xmpp"
	"mellium.im/xmpp/jid"
	"mellium.im/xmpp/ping"
	"mellium.im/xmpp/stanza"
)

// maxBody is a soft cap for a single outgoing message body; longer text is
// split on newline / word boundaries so peers don't reject oversized stanzas.
const maxBody = 50000

const chatStatesNS = "http://jabber.org/protocol/chatstates"

// Receipt namespaces: XEP-0184 message delivery receipts and XEP-0333 chat
// markers. The bridge honors whichever an incoming owner message requests.
const (
	receiptsNS    = "urn:xmpp:receipts"
	chatMarkersNS = "urn:xmpp:chat-markers:0"
)

// reactionsNS is XEP-0444 message reactions: the agent reacts to an owner
// message with emoji (e.g. 👀 picked up, ✅ done, ⛔ aborted).
const reactionsNS = "urn:xmpp:reactions:0"

// newStanzaID generates a random stanza id.
func newStanzaID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// bareJid returns the bare (localpart@domain) form of a JID, lowercased.
func bareJid(full string) string {
	if slash := strings.IndexByte(full, '/'); slash >= 0 {
		full = full[:slash]
	}
	return strings.ToLower(full)
}

// chunk splits text into pieces no longer than max, preferring newline then
// word boundaries.
func chunk(text string, max int) []string {
	if len(text) <= max {
		return []string{text}
	}
	var chunks []string
	rest := text
	for len(rest) > max {
		cut := strings.LastIndexByte(rest[:max], '\n')
		if cut < max/2 {
			cut = strings.LastIndexByte(rest[:max], ' ')
		}
		if cut < max/2 {
			cut = max
		}
		chunks = append(chunks, rest[:cut])
		rest = strings.TrimLeft(rest[cut:], " \t\r\n")
	}
	if rest != "" {
		chunks = append(chunks, rest)
	}
	return chunks
}

// InboundMessage is a received message the bridge should act on. In serverless
// (1:1) mode it is always the owner, delivered over a direct peer session.
type InboundMessage struct {
	Body      string // message text
	FromOwner bool   // sender is the configured owner
	Direct    bool   // arrived as a 1:1 chat (always true in serverless mode)
	ID        string // stanza id (used as the XEP-0444 reaction target)
	From      string // full from-JID, so a reaction routes back to that resource
}

// XMPPBridge owns a single account's serverless XMPP connection: it maintains
// the direct peer sessions with the owner (an outbound connection it initiates,
// plus any inbound connections the owner's client opens against our listener),
// delivers relevant incoming messages via onMsg, and exposes send/presence/
// chat-state helpers the bridge calls from other goroutines.
type XMPPBridge struct {
	acct      ResolvedAccount
	ownerBare string
	onMsg     func(InboundMessage)
	logf      func(level, msg string)

	mu       sync.Mutex
	sessions map[*xmpp.Session]struct{} // all live peer sessions
	sendSess *xmpp.Session              // preferred session for sends (the outbound one)
	online   bool
	show     string // presence <show>: "" (available) or "dnd"/"away"/… (availability axis)
	presence string // presence <status> free text (activity axis)
	adv      *serverlessAdvertised

	seen      map[string]struct{}
	seenOrder []string

	// avatarHash is the lowercase hex SHA-1 of the configured avatar image,
	// advertised as the "phsh" key in the Bonjour TXT record (XEP-0153).
	avatarHash string

	// msgHistory maps stanza IDs to their source JID (inbound and outbound) so
	// send_reaction can target arbitrary messages by ID. Capped at 500 entries;
	// oldest is evicted when full.
	msgHistory map[string]msgHistoryEntry
}

// NewXMPPBridge constructs a bridge. onMsg is called for each message that
// should drive the agent; logf receives diagnostics.
func NewXMPPBridge(acct ResolvedAccount, onMsg func(InboundMessage), logf func(level, msg string)) *XMPPBridge {
	b := &XMPPBridge{
		acct:       acct,
		ownerBare:  bareJid(acct.Owner),
		onMsg:      onMsg,
		logf:       logf,
		presence:   "listening (" + nowStamp() + ")",
		seen:       make(map[string]struct{}),
		msgHistory: make(map[string]msgHistoryEntry),
	}
	b.loadAvatar()
	return b
}

// loadAvatar reads the configured avatar image and precomputes its SHA-1 hash,
// advertised as the Bonjour TXT "phsh" key (XEP-0153). A missing path or
// unreadable file is a logged warning, not fatal — the bridge just runs without
// a photo hash.
func (b *XMPPBridge) loadAvatar() {
	if b.acct.Avatar == "" {
		return
	}
	data, err := os.ReadFile(b.acct.Avatar)
	if err != nil {
		b.log("warning", "avatar not loaded: "+err.Error())
		return
	}
	if len(data) == 0 {
		b.log("warning", "avatar not loaded: file is empty: "+b.acct.Avatar)
		return
	}
	sum := sha1.Sum(data)
	b.avatarHash = hex.EncodeToString(sum[:])
	b.log("info", fmt.Sprintf("avatar hash %s (%d bytes) — advertised as phsh", b.avatarHash, len(data)))
}

func (b *XMPPBridge) log(level, msg string) {
	if b.logf != nil {
		b.logf(level, msg)
	}
}

// Run advertises the bot's own Bonjour IM presence, accepts inbound serverless
// connections, and maintains the outbound peer session to the owner with
// exponential backoff until ctx is canceled. onConnected (may be nil) is invoked
// after each successful outbound connect, once presence has been announced.
func (b *XMPPBridge) Run(ctx context.Context, onConnected func()) {
	if own, err := jid.Parse(b.acct.JID); err == nil {
		if adv, err := advertiseAndListen(ctx, own, b.avatarHash, b.acct.Alias); err == nil {
			b.mu.Lock()
			b.adv = adv
			b.mu.Unlock()
			b.log("info", fmt.Sprintf("advertising %s on _presence._tcp port %d", b.acct.JID, adv.port))
			go adv.serve(ctx, xmpp.HandlerFunc(b.handle))
			defer adv.shutdown()
		} else {
			b.log("warning", "advertise/listen failed: "+err.Error())
		}
	} else {
		b.log("warning", "advertise: bad jid: "+err.Error())
	}

	backoff := time.Second
	for {
		err := b.serve(ctx, onConnected)
		if ctx.Err() != nil {
			return
		}
		b.log("warning", fmt.Sprintf("connection to owner lost: %v; reconnecting in %s", err, backoff))
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// serve establishes one outbound peer session to the owner and runs its read
// loop until it drops. The owner's client keeps it open for bidirectional
// traffic, so our sends and their replies share it; inbound connections the
// owner's client opens against our listener are handled separately by the
// advertised server.
func (b *XMPPBridge) serve(ctx context.Context, onConnected func()) error {
	own, err := jid.Parse(b.acct.JID)
	if err != nil {
		return fmt.Errorf("invalid jid %q: %w", b.acct.JID, err)
	}
	peer, err := jid.Parse(b.acct.Owner)
	if err != nil {
		return fmt.Errorf("invalid owner jid %q: %w", b.acct.Owner, err)
	}
	endpoint, err := b.discover(ctx)
	if err != nil {
		return err
	}
	b.log("info", fmt.Sprintf("discovered owner %s at %s:%d", peer.String(), endpoint.Host, endpoint.Port))

	session, err := connectServerless(ctx, own, peer, endpoint)
	if err != nil {
		return err
	}
	b.addSession(session)
	defer b.removeSession(session)

	// Announce presence to the owner's client over the session.
	if err := b.encodePresence("", b.presence); err != nil {
		b.log("warning", "presence failed: "+err.Error())
	}

	b.log("info", fmt.Sprintf("online as %s, connected to %s", b.acct.JID, b.acct.Owner))
	if onConnected != nil {
		onConnected()
	}

	serveErr := session.Serve(xmpp.HandlerFunc(b.handle))
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if serveErr != nil {
		return serveErr
	}
	return fmt.Errorf("stream closed")
}

// discover returns the owner's Bonjour IM endpoint, found via mDNS using the
// account's discovery settings. The target is the owner's bare JID: in
// XEP-0174 each Bonjour IM instance is advertised under its user's JID.
func (b *XMPPBridge) discover(ctx context.Context) (*bonjourEndpoint, error) {
	return discoverBonjour(ctx, b.acct.BonjourService, b.acct.Owner, b.acct.BonjourName, b.acct.DiscoverTimeout)
}

func (b *XMPPBridge) addSession(s *xmpp.Session) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sessions == nil {
		b.sessions = make(map[*xmpp.Session]struct{})
	}
	b.sessions[s] = struct{}{}
	b.online = true
	b.sendSess = s
}

func (b *XMPPBridge) removeSession(s *xmpp.Session) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.sessions, s)
	if b.sendSess == s {
		b.sendSess = nil
		for other := range b.sessions {
			b.sendSess = other
			break
		}
	}
	if len(b.sessions) == 0 {
		b.online = false
	}
}

// Send delivers a chat message to the owner, splitting long text across
// stanzas.
func (b *XMPPBridge) Send(text string) string { return b.SendChatTo(b.acct.Owner, text) }

// SendChatTo posts a 1:1 chat message to an arbitrary JID, splitting long text.
// Returns the stanza ID of the last chunk sent, or "" if nothing was sent.
func (b *XMPPBridge) SendChatTo(to, text string) string {
	if b.currentSession() == nil {
		b.log("warning", "send skipped: not online")
		return ""
	}
	var lastID string
	for _, part := range chunk(text, maxBody) {
		id, err := b.encodeChat(to, part, stanza.ChatMessage)
		if err != nil {
			b.log("error", "send failed: "+err.Error())
			break
		}
		lastID = id
	}
	return lastID
}

// SendRich converts markdown text to XHTML-IM (XEP-0071) and sends it to `to`
// as one or more styled messages, each carrying a plain-text <body> fallback
// so clients without XHTML-IM (or with formatting disabled) still read the
// content. Returns the stanza ID of the last message sent, or "" if nothing
// was sent.
func (b *XMPPBridge) SendRich(to, md string) string {
	if b.currentSession() == nil {
		b.log("warning", "send skipped: not online")
		return ""
	}
	var lastID string
	for _, chunk := range renderRichMessage(md) {
		id, err := b.encodeXHTMLChat(to, chunk.plain, chunk.xhtml)
		if err != nil {
			b.log("error", "send failed: "+err.Error())
			break
		}
		lastID = id
	}
	return lastID
}

// SetPresence announces presence with a show (availability axis: "" = available,
// "dnd" = busy, …) and a status label (activity axis), remembering both for
// re-assertion on reconnect. Redundant no-change calls are dropped so streaming
// deltas don't spray identical updates. In serverless mode the availability is
// carried by the mDNS TXT "status=" key (visible to the owner's roster), and a
// best-effort in-stream presence is also sent to the peer's client.
func (b *XMPPBridge) SetPresence(show, status string) {
	b.mu.Lock()
	if show == b.show && status == b.presence {
		b.mu.Unlock()
		return // unchanged; skip
	}
	b.show = show
	b.presence = status
	adv := b.adv
	b.mu.Unlock()

	if adv != nil {
		adv.setStatus(serverlessStatus(show))
	}
	if err := b.encodePresence(show, status); err != nil {
		b.log("warning", "presence failed: "+err.Error())
	}
}

// serverlessStatus maps an XMPP <show> value to the XEP-0174 TXT status key.
func serverlessStatus(show string) string {
	switch show {
	case "":
		return "avail"
	case "dnd":
		return "dnd"
	default:
		return "away" // away / xa / chat
	}
}

// GoOffline broadcasts an unavailable presence so the owner's client stops
// showing the bot online, carrying an optional status describing why. The mDNS
// registration is torn down by Run on shutdown. Safe to call when already
// offline (no-op).
func (b *XMPPBridge) GoOffline(status string) {
	if err := b.encodeUnavailable(status); err != nil {
		b.log("warning", "offline presence failed: "+err.Error())
	}
}

// ChatState sends an XEP-0085 chat-state notification to the owner (the
// "typing…" indicator). "composing" shows typing; "active" clears it.
func (b *XMPPBridge) ChatState(state string) {
	if b.currentSession() == nil {
		return
	}
	if err := b.encodeChatState(b.acct.Owner, state, stanza.ChatMessage); err != nil {
		b.log("warning", "chatstate failed: "+err.Error())
	}
}

func (b *XMPPBridge) currentSession() *xmpp.Session {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.online || b.sendSess == nil {
		return nil
	}
	return b.sendSess
}

// incomingMsg is a received message stanza reduced to the fields the bridge
// cares about.
type incomingMsg struct {
	from        string
	typ         string
	body        string
	id          string
	delay       bool // carried an XEP-0203 <delay/> (server-replayed history)
	wantReceipt bool // carried a XEP-0184 <request/> (delivery receipt)
	markable    bool // carried a XEP-0333 <markable/> (chat marker)
}

// handle is the mellium read-loop callback for one inbound stanza.
func (b *XMPPBridge) handle(t xmlstream.TokenReadEncoder, start *xml.StartElement) error {
	switch start.Name.Local {
	case "message":
		toks, err := xmlstream.ReadAll(t)
		if err != nil {
			return err
		}
		m := incomingMsg{
			from: attr(start.Attr, "from"),
			typ:  attr(start.Attr, "type"),
			id:   attr(start.Attr, "id"),
			body: childText(toks, "body"),
		}
		_, m.delay = element(toks, "urn:xmpp:delay", "delay")
		_, m.wantReceipt = element(toks, receiptsNS, "request")
		_, m.markable = element(toks, chatMarkersNS, "markable")
		b.dispatch(m)
		return nil
	case "presence":
		// Presence in serverless messaging is carried by mDNS, not stanzas;
		// drain any the peer's client sends so the stream advances.
		_, err := xmlstream.Copy(xmlstream.Discard(), t)
		return err
	case "iq":
		toks, err := xmlstream.ReadAll(t)
		if err != nil {
			return err
		}
		// Answer XEP-0199 ping requests so a peer keepalive sees us as alive.
		// (Responses to our own pings are correlated by the session before
		// reaching this handler, so they never arrive here.)
		if attr(start.Attr, "type") == "get" {
			if _, ok := element(toks, ping.NS, "ping"); ok {
				return b.encodePong(attr(start.Attr, "from"), attr(start.Attr, "id"))
			}
		}
		return nil
	default:
		// Anything else: drain so the stream advances.
		_, err := xmlstream.Copy(xmlstream.Discard(), t)
		return err
	}
}

// encodePong replies to an XEP-0199 ping with an empty result IQ echoing the
// request id back to its sender.
func (b *XMPPBridge) encodePong(to, id string) error {
	session := b.currentSession()
	if session == nil {
		return fmt.Errorf("not online")
	}
	resp := stanza.IQ{ID: id, Type: stanza.ResultIQ}
	if to != "" {
		toJID, err := jid.Parse(to)
		if err != nil {
			return fmt.Errorf("invalid ping sender %q: %w", to, err)
		}
		resp.To = toJID
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return session.Encode(ctx, resp)
}

// dispatch applies delivery policy and forwards a message to onMsg. Serverless
// messaging is strictly 1:1, so only direct chat from the owner is handled.
func (b *XMPPBridge) dispatch(m incomingMsg) {
	if m.typ == "groupchat" {
		return // no rooms in serverless mode
	}
	b.dispatchDirect(m)
}

// dispatchDirect forwards a 1:1 chat message from the owner.
func (b *XMPPBridge) dispatchDirect(m incomingMsg) {
	// Only direct chat (or type-less) messages, and only from the owner. An
	// empty "from" can only be the peer we're connected to (there is no server
	// to relay from anywhere else), so it falls back to the owner.
	if m.typ != "" && m.typ != "chat" && m.typ != "normal" {
		return
	}
	if from := bareJid(m.from); from != "" && from != b.ownerBare {
		return
	}
	if strings.TrimSpace(m.body) == "" {
		return // chat-states, receipts, empty
	}
	// Drop replayed history so a blip doesn't reprocess old messages.
	if m.delay {
		return
	}
	if m.id != "" && b.seenDuplicate(m.id) {
		return
	}
	// Record the inbound message in history so send_reaction can target it by ID.
	if m.id != "" {
		b.recordMessage(m.id, m.from)
	}
	// The agent is about to take this in — acknowledge it as read/delivered.
	b.sendReceipts(m)
	b.onMsg(InboundMessage{Body: m.body, FromOwner: true, Direct: true, ID: m.id, From: m.from})
}

// seenDuplicate reports whether id was already handled, recording it if not.
// Bounded to the most recent 500 ids.
func (b *XMPPBridge) seenDuplicate(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.seen[id]; ok {
		return true
	}
	b.seen[id] = struct{}{}
	b.seenOrder = append(b.seenOrder, id)
	if len(b.seenOrder) > 500 {
		evicted := b.seenOrder[0]
		b.seenOrder = b.seenOrder[1:]
		delete(b.seen, evicted)
	}
	return false
}

// --- stanza encoders ---

func (b *XMPPBridge) encodeChat(to, body string, typ stanza.MessageType) (string, error) {
	session := b.currentSession()
	if session == nil {
		return "", fmt.Errorf("not online")
	}
	toJID, err := jid.Parse(to)
	if err != nil {
		return "", fmt.Errorf("invalid recipient %q: %w", to, err)
	}
	id := newStanzaID()
	msg := struct {
		stanza.Message
		Body string `xml:"body"`
	}{
		Message: stanza.Message{ID: id, To: toJID, Type: typ},
		Body:    body,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	b.recordMessage(id, to)
	return id, session.Encode(ctx, msg)
}

// xhtmlIMWrapper is the <html xmlns='http://jabber.org/protocol/xhtml-im'>
// element; Raw carries the pre-escaped XHTML-IM <body> content verbatim.
type xhtmlIMWrapper struct {
	Raw string `xml:",innerxml"`
}

// encodeXHTMLChat sends a 1:1 chat message whose styled body is an XHTML-IM
// (XEP-0071) <html> wrapper rendering in rich clients like Adium, alongside a
// plain-text <body> carrying the same content as a fallback. The XHTML must
// already be escaped and limited to the XEP-0071 integration set.
func (b *XMPPBridge) encodeXHTMLChat(to, plain, xhtml string) (string, error) {
	session := b.currentSession()
	if session == nil {
		return "", fmt.Errorf("not online")
	}
	toJID, err := jid.Parse(to)
	if err != nil {
		return "", fmt.Errorf("invalid recipient %q: %w", to, err)
	}
	id := newStanzaID()
	msg := struct {
		stanza.Message
		Body string          `xml:"body"`
		HTML *xhtmlIMWrapper `xml:"http://jabber.org/protocol/xhtml-im html,omitempty"`
	}{
		Message: stanza.Message{ID: id, To: toJID, Type: stanza.ChatMessage},
		Body:    plain,
		HTML:    &xhtmlIMWrapper{Raw: `<body xmlns="http://www.w3.org/1999/xhtml">` + xhtml + `</body>`},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	b.recordMessage(id, to)
	return id, session.Encode(ctx, msg)
}

func (b *XMPPBridge) encodeChatState(to, state string, typ stanza.MessageType) error {
	session := b.currentSession()
	if session == nil {
		return fmt.Errorf("not online")
	}
	toJID, err := jid.Parse(to)
	if err != nil {
		return fmt.Errorf("invalid recipient %q: %w", to, err)
	}
	msg := struct {
		stanza.Message
		State struct {
			XMLName xml.Name
		}
	}{
		Message: stanza.Message{To: toJID, Type: typ},
	}
	msg.State.XMLName = xml.Name{Space: chatStatesNS, Local: state}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return session.Encode(ctx, msg)
}

// sendReceipts acknowledges an accepted owner message: a XEP-0184 delivery
// receipt if the sender requested one, and a XEP-0333 "displayed" chat marker
// if the message was markable — a genuine read receipt, since the agent is
// about to act on it. Sent to the message's full from-JID so it routes back to
// the originating resource. Best-effort; failures are logged, not fatal.
func (b *XMPPBridge) sendReceipts(m incomingMsg) {
	if m.id == "" || m.from == "" {
		return
	}
	if m.wantReceipt {
		if err := b.encodeReceipt(m.from, receiptsNS, "received", m.id); err != nil {
			b.log("warning", "delivery receipt failed: "+err.Error())
		}
	}
	if m.markable {
		if err := b.encodeReceipt(m.from, chatMarkersNS, "displayed", m.id); err != nil {
			b.log("warning", "chat marker failed: "+err.Error())
		}
	}
}

// encodeReceipt sends a bodyless message to `to` carrying a single ack element
// (namespace ns, local name) whose `id` attribute references the acknowledged
// message forID — the wire form shared by XEP-0184 receipts and XEP-0333
// markers.
func (b *XMPPBridge) encodeReceipt(to, ns, local, forID string) error {
	session := b.currentSession()
	if session == nil {
		return fmt.Errorf("not online")
	}
	toJID, err := jid.Parse(to)
	if err != nil {
		return fmt.Errorf("invalid recipient %q: %w", to, err)
	}
	msg := struct {
		stanza.Message
		Ack struct {
			XMLName xml.Name
			ID      string `xml:"id,attr"`
		}
	}{
		Message: stanza.Message{To: toJID, Type: stanza.ChatMessage},
	}
	msg.Ack.XMLName = xml.Name{Space: ns, Local: local}
	msg.Ack.ID = forID
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return session.Encode(ctx, msg)
}

// msgHistoryEntry records an inbound or outbound message stanza in the
// history ring buffer, so the bridge can resolve a stanza ID to its source
// JID without the agent having to remember it.
type msgHistoryEntry struct {
	FromJID   string
	Timestamp time.Time
}

// msgHistoryCap is the maximum number of stanza IDs retained in history.
const msgHistoryCap = 500

// recordMessage records a stanza ID -> JID mapping in the history ring
// buffer, evicting the oldest entry if the buffer is full.
func (b *XMPPBridge) recordMessage(id, fromJID string) {
	if id == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, exists := b.msgHistory[id]; exists {
		b.msgHistory[id] = msgHistoryEntry{FromJID: fromJID, Timestamp: time.Now()}
		return
	}
	if len(b.msgHistory) >= msgHistoryCap {
		// Evict the oldest entry.
		var oldestKey string
		var oldestTime time.Time
		for k, v := range b.msgHistory {
			if oldestKey == "" || v.Timestamp.Before(oldestTime) {
				oldestKey, oldestTime = k, v.Timestamp
			}
		}
		delete(b.msgHistory, oldestKey)
	}
	b.msgHistory[id] = msgHistoryEntry{FromJID: fromJID, Timestamp: time.Now()}
}

// lookupMessage returns the from-JID for a recorded stanza ID, or "" if not found.
func (b *XMPPBridge) lookupMessage(id string) string {
	if id == "" {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if e, ok := b.msgHistory[id]; ok {
		return e.FromJID
	}
	return ""
}

// SendReaction reacts to message forID (authored by `to`) with the given emoji,
// per XEP-0444. Each stanza carries the full reaction set for the
// (agent, message) pair, so a later call replaces an earlier one; calling with
// no emoji sends an empty <reactions>, clearing any prior reaction.
// Best-effort: a missing target or an encode failure is logged, not fatal.
func (b *XMPPBridge) SendReaction(to, forID string, emojis ...string) {
	if to == "" || forID == "" {
		return
	}
	if err := b.encodeReaction(to, forID, emojis); err != nil {
		b.log("warning", "reaction failed: "+err.Error())
	}
}

// SendReactionTo reacts to message forID (authored by `to`) with a single
// emoji, taking explicit target parameters. Unlike SendReaction, it accepts a
// single required emoji string. Calling with an empty emoji clears the reaction.
func (b *XMPPBridge) SendReactionTo(to, forID, emoji string) {
	b.SendReaction(to, forID, emoji)
}

// encodeReaction sends a bodyless message to `to` carrying an XEP-0444
// <reactions id='forID'> element with one <reaction> child per emoji. An empty
// emojis slice yields an empty <reactions>, which clears the reaction set.
func (b *XMPPBridge) encodeReaction(to, forID string, emojis []string) error {
	session := b.currentSession()
	if session == nil {
		return fmt.Errorf("not online")
	}
	toJID, err := jid.Parse(to)
	if err != nil {
		return fmt.Errorf("invalid recipient %q: %w", to, err)
	}
	type reaction struct {
		XMLName xml.Name `xml:"reaction"`
		Text    string   `xml:",chardata"`
	}
	msg := struct {
		stanza.Message
		Reactions struct {
			XMLName   xml.Name
			ID        string `xml:"id,attr"`
			Reactions []reaction
		}
	}{
		Message: stanza.Message{To: toJID, Type: stanza.ChatMessage},
	}
	msg.Reactions.XMLName = xml.Name{Space: reactionsNS, Local: "reactions"}
	msg.Reactions.ID = forID
	for _, e := range emojis {
		if e == "" {
			continue
		}
		msg.Reactions.Reactions = append(msg.Reactions.Reactions, reaction{Text: e})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return session.Encode(ctx, msg)
}

// encodePresence announces presence with an optional show and status. An empty
// "to" targets the peer connected on the current session (there is no roster).
func (b *XMPPBridge) encodePresence(show, status string) error {
	session := b.currentSession()
	if session == nil {
		return fmt.Errorf("not online")
	}
	p := struct {
		XMLName xml.Name `xml:"presence"`
		Show    string   `xml:"show,omitempty"`
		Status  string   `xml:"status,omitempty"`
	}{Show: show, Status: status}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return session.Encode(ctx, p)
}

// encodeUnavailable broadcasts a roster-wide unavailable presence, marking the
// bot offline, with an optional <status> line describing why.
func (b *XMPPBridge) encodeUnavailable(status string) error {
	session := b.currentSession()
	if session == nil {
		return fmt.Errorf("not online")
	}
	p := struct {
		XMLName xml.Name `xml:"presence"`
		Type    string   `xml:"type,attr"`
		Status  string   `xml:"status,omitempty"`
	}{Type: "unavailable", Status: status}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return session.Encode(ctx, p)
}

// --- token helpers ---

// attr returns the value of the first attribute named local, or "".
func attr(attrs []xml.Attr, local string) string {
	for _, a := range attrs {
		if a.Name.Local == local {
			return a.Value
		}
	}
	return ""
}

// element returns the first child start-element among toks matching space and
// local name.
func element(toks []xml.Token, space, local string) (xml.StartElement, bool) {
	for _, tok := range toks {
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == local && se.Name.Space == space {
			return se, true
		}
	}
	return xml.StartElement{}, false
}

// childText returns the character data immediately following the first start
// element with the given local name, or "".
func childText(toks []xml.Token, local string) string {
	for i, tok := range toks {
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != local {
			continue
		}
		if i+1 < len(toks) {
			if cd, ok := toks[i+1].(xml.CharData); ok {
				return string(cd)
			}
		}
	}
	return ""
}
