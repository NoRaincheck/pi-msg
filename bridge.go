package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// typingRefresh re-sends the "composing" chat state before clients auto-clear
// it (~30s), so the typing indicator stays lit while the agent works.
const typingRefresh = 20 * time.Second

// Bridge wires a serverless XMPP connection to a `pi --mode rpc` child: owner
// chat becomes pi commands, and pi's events become chat replies / presence /
// typing.
type Bridge struct {
	acct  ResolvedAccount
	debug bool

	xmpp *XMPPBridge
	rpc  *RPCClient
	ctx  context.Context

	mu             sync.Mutex
	streamingRun   bool
	repliedThisRun bool
	shuttingDown   bool
	directTurn     bool   // active turn arrived as a 1:1 owner DM (drives typing)
	reactTo        string // full JID of the owner message the current run reacts to
	reactID        string // stanza id of that message (XEP-0444 target); "" disables

	lifecycleReactTo string // snapshot of reactTo at run start, for lifecycle auto-reacts
	lifecycleReactID string // snapshot of reactID at run start; never overwritten by deliverReply

	typingMu   sync.Mutex
	typingStop chan struct{}
}

// NewBridge constructs a bridge for the resolved account.
func NewBridge(acct ResolvedAccount, debug bool) *Bridge {
	return &Bridge{acct: acct, debug: debug}
}

func (b *Bridge) log(level, msg string) {
	if level == "info" && !b.debug {
		return
	}
	fmt.Fprintf(os.Stderr, "[pi-msg] %s: %s\n", level, msg)
}

// Run starts pi and the XMPP connection and drives the event loop until the
// context is canceled or pi exits.
func (b *Bridge) Run(ctx context.Context) error {
	b.ctx = ctx

	b.xmpp = NewXMPPBridge(b.acct, b.onInbound, b.log)

	// Materialise the companion extension so pi can register the XMPP tools.
	extPath, err := writeTempExtension()
	if err != nil {
		return err
	}
	defer os.Remove(extPath)

	b.rpc = NewRPCClient("", b.acct.Model, b.acct.Workdir, extPath, func(line string) {
		if b.debug {
			b.log("info", "pi stderr: "+line)
		}
	})
	// Tell the companion extension which tools to register. Only the reaction
	// tool survives serverless messaging — send_file needs a server-side
	// XEP-0363 upload component, which XEP-0174 does not have.
	tools := []string{"reaction"}
	b.rpc.env = []string{"PI_MSG_TOOLS=" + strings.Join(tools, ",")}

	// Bring up XMPP first so we can report problems, then start pi.
	// No connect callback: the bot appearing online (presence "listening") is
	// the startup signal now, in place of a chat banner.
	go b.xmpp.Run(ctx, nil)
	if err := b.rpc.Start(); err != nil {
		return err
	}
	b.log("info", fmt.Sprintf("bridging account %q (%s) to owner %s", b.acct.Name, b.acct.JID, b.acct.Owner))

	for {
		select {
		case <-ctx.Done():
			b.shutdown("interrupted (SIGINT/SIGTERM)")
			return nil
		case ev, ok := <-b.rpc.Events():
			if !ok {
				return b.onPiExit()
			}
			b.handleRPCEvent(ev)
		}
	}
}

func (b *Bridge) onPiExit() error {
	if b.rpc.StoppedIntentionally() {
		return nil
	}
	// pi died on its own (crash): XMPP is still connected, so clear the typing
	// indicator and — unlike the graceful lifecycle, which is presence-only — post
	// a loud chat message so the crash isn't missed, then drop presence carrying
	// the same reason as the offline status. The message goes first, while online.
	b.stopTyping()
	err := b.rpc.ExitErr()
	if err != nil {
		b.reply(fmt.Sprintf("🔴 pi crashed: %v. Bridge shutting down.", err))
		b.xmpp.GoOffline(fmt.Sprintf("offline — pi crashed: %v (%s)", err, nowStamp()))
		return fmt.Errorf("pi exited: %v", err)
	}
	b.reply("🔴 pi exited unexpectedly (no error reported). Bridge shutting down.")
	b.xmpp.GoOffline("offline — pi exited unexpectedly (" + nowStamp() + ")")
	return fmt.Errorf("pi exited unexpectedly")
}

// nowStamp is a short local timestamp for presence status lines.
func nowStamp() string { return time.Now().Format("2006-01-02 15:04:05 MST") }

func (b *Bridge) shutdown(reason string) {
	b.mu.Lock()
	if b.shuttingDown {
		b.mu.Unlock()
		return
	}
	b.shuttingDown = true
	b.mu.Unlock()
	b.log("info", "shutting down: "+reason)
	// Clear the typing indicator (sends chat-state "active") while still online,
	// so the owner isn't left seeing "typing…" against an offline bot.
	b.stopTyping()
	b.xmpp.GoOffline(fmt.Sprintf("offline — session ended (%s) at %s", reason, nowStamp()))
	b.rpc.Stop()
}

// --- pi event handling ---

// The bridge conveys agent state on three orthogonal axes so they don't all
// mean "busy" (see docs): the typing indicator = "a message is arriving right
// now" (lit only while assistant text streams); presence <show> = availability
// (dnd while a run is in flight, available when idle); presence <status> = the
// current activity label (thinking / running a tool / replying / retrying).
func (b *Bridge) handleRPCEvent(ev Event) {
	switch ev.Type() {
	case "agent_start":
		b.setStreaming(true)
		b.setReplied(false)
		b.xmpp.SetPresence("dnd", "thinking…")
		b.lifecycleReact("👀") // picked up (opt-in via the reactions flag)
	case "agent_settled":
		b.setStreaming(false)
		b.stopTyping()
		b.xmpp.SetPresence("", "listening ("+nowStamp()+")")
		b.lifecycleReact("✅") // done
		// The reply text + typing/presence already signal "done". Only nudge if
		// the run produced no message, so silence isn't mistaken for a hang.
		if !b.replied() {
			b.reply("✅ done (no reply) — your turn")
		}
	case "message_update":
		b.handleStreamDelta(ev)
	case "tool_execution_start":
		// A tool is running, not text streaming: drop the typing bubble and
		// label the activity.
		b.stopTyping()
		b.xmpp.SetPresence("dnd", toolLabel(ev))
	case "auto_retry_start":
		b.stopTyping()
		b.xmpp.SetPresence("dnd", "retrying (transient error)…")
	case "auto_retry_end":
		b.xmpp.SetPresence("dnd", "thinking…")
	case "message_end":
		msg := ev.Obj("message")
		if msg == nil || msg.Str("role") != "assistant" {
			return
		}
		if text := FixToolCallXML(extractText(msg["content"])); text != "" {
			b.deliverReply(text)
			b.setReplied(true)
		}
	case "extension_error":
		b.reply("⚠️ extension error: " + orUnknown(ev.Str("error")))
	case "extension_ui_request":
		b.handleUIRequest(ev)
	}
}

// handleUIRequest routes companion-extension tool-action relays and otherwise
// cancels interactive dialogs (nobody is at the TUI to answer them) so pi
// doesn't block. A `confirm` whose title carries the sentinel is a relayed tool
// action, not a real user dialog — see handleToolRelay.
func (b *Bridge) handleUIRequest(ev Event) {
	id := ev.Str("id")
	method := ev.Str("method")
	if method == "confirm" {
		if payload, ok := strings.CutPrefix(ev.Str("title"), relayPrefix); ok {
			b.handleToolRelay(id, payload)
			return
		}
	}
	switch method {
	case "select", "confirm", "input", "editor":
		if id != "" {
			b.rpc.CancelUI(id)
			b.reply(fmt.Sprintf("⚠️ pi asked for input (%s) — auto-dismissed (no interactive UI over chat).", method))
		}
	case "notify":
		if b.debug {
			if m := ev.Str("message"); m != "" {
				b.reply("ℹ️ " + m)
			}
		}
	}
}

// handleToolRelay performs an XMPP-side action requested by an agent tool call
// in the companion extension, then answers the blocking confirm so the tool
// (and thus the LLM) learns whether it succeeded. The JSON payload names the
// action and its arguments.
func (b *Bridge) handleToolRelay(id, payload string) {
	var cmd struct {
		Action    string `json:"action"`
		Emoji     string `json:"emoji"`
		MessageID string `json:"messageId"`
		From      string `json:"from"`
	}
	if err := json.Unmarshal([]byte(payload), &cmd); err != nil {
		b.log("warning", "bad tool-relay payload: "+err.Error())
		b.rpc.RespondUI(id, false)
		return
	}
	switch cmd.Action {
	case "react":
		to, rid := cmd.From, cmd.MessageID
		if to == "" && rid != "" {
			// No explicit from-JID: look up the cached one.
			to = b.xmpp.lookupMessage(rid)
		}
		if rid == "" {
			// No explicit message ID: fall back to the current run's target.
			b.mu.Lock()
			to, rid = b.reactTo, b.reactID
			b.mu.Unlock()
		}
		b.log("info", fmt.Sprintf("tool-relay react: emoji=%q target to=%q id=%q", cmd.Emoji, to, rid))
		b.xmpp.SendReaction(to, rid, cmd.Emoji)
		// Success iff we had a target; reactions are instant.
		ok := to != "" && rid != ""
		if !ok && cmd.MessageID != "" {
			b.log("warning", fmt.Sprintf("reaction target %q not found in message history and no from-JID supplied", cmd.MessageID))
		}
		b.rpc.RespondUI(id, ok)
	default:
		b.log("warning", "unknown tool-relay action: "+cmd.Action)
		b.rpc.RespondUI(id, false)
	}
}

// --- chat command handling ---

// onInbound routes a delivered message. Runs on an XMPP read goroutine;
// commands that need a response block only this handler, not pi's event
// stream. Serverless messaging is strictly 1:1, so every accepted message is
// from the owner and drives the agent.
func (b *Bridge) onInbound(m InboundMessage) {
	b.setDirectTurn(m.Direct)
	b.handleCanonical(m.Body, m.From, m.ID)
}

// handleCanonical handles a trusted (owner) message: control commands dispatch
// directly; anything else becomes a prompt. reactTo is the full JID of the
// message's author (used by reactions), reactID its stanza ID.
func (b *Bridge) handleCanonical(text, reactTo, reactID string) {
	t := strings.TrimSpace(text)
	if t == "" {
		return
	}
	if strings.HasPrefix(t, "/") && b.handleCommand(t) {
		return
	}
	// A real prompt: point lifecycle/agent reactions at the message that drove it.
	b.setLifecycleReactTarget(reactTo, reactID)
	b.rpc.Prompt(b.composePrompt(t, reactID, reactTo), b.steerBehavior())
	// Immediate "got it, working" ack; agent_start confirms it shortly (deduped).
	// Typing is no longer lit here — it now tracks literal text streaming.
	b.xmpp.SetPresence("dnd", "thinking…")
}

// handleCommand runs a recognized control command and returns true. Unknown
// "/…" input (extension commands, /skill:name, /template) returns false so the
// caller forwards it to pi as a prompt.
func (b *Bridge) handleCommand(t string) bool {
	name, arg := splitCommand(t)
	switch name {
	case "new":
		if b.streaming() {
			b.rpc.Abort()
		}
		b.settleLocally()
		res, err := b.rpc.NewSession(b.ctx)
		b.reportResult(err, res, "🆕 new session ready", "/new")
	case "compact":
		res, err := b.rpc.Compact(b.ctx, arg)
		b.reportResult(err, res, "🗜️ context compacted", "/compact")
	case "think":
		res, err := b.rpc.SetThinkingLevel(b.ctx, arg)
		b.reportResult(err, res, "🧠 thinking level: "+arg, "/think")
	case "model":
		b.handleModel(arg)
	case "abort", "stop":
		b.rpc.Abort()
		b.settleLocally()
		b.lifecycleReact("⛔") // aborted
		b.reply("⛔ aborted")
	case "quit", "exit":
		b.shutdown("requested over chat")
	case "dump":
		b.dumpSession(arg)
	default:
		return false
	}
	return true
}

// dumpSession sends the current session's transcript to the owner, straight
// from disk — no LLM turn. It reads the session file path from pi's get_state,
// then relays the file: verbatim JSONL by default, or record-by-record indented
// JSON with role/type headers when arg is "pretty".
func (b *Bridge) dumpSession(arg string) {
	res, err := b.rpc.GetState(b.ctx)
	if err != nil {
		b.reply("⚠️ /dump failed: " + err.Error())
		return
	}
	if !res.success() {
		b.reply("⚠️ /dump failed: " + res.errText())
		return
	}
	path := res.Obj("data").Str("sessionFile")
	if path == "" {
		b.reply("⚠️ /dump: no session file (session persistence is disabled)")
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		b.reply("⚠️ /dump: cannot read session file: " + err.Error())
		return
	}
	if len(raw) == 0 {
		b.reply("📄 session is empty")
		return
	}
	if strings.EqualFold(strings.TrimSpace(arg), "pretty") {
		b.reply(fmt.Sprintf("📄 session dump (pretty) — %s", path))
		pretty := prettyDump(raw)
		// When the dump is too large to fit in one message, the transport
		// splits it at newline/word boundaries — which can split the code fence
		// markers, breaking markdown rendering on the receiving client.  Split
		// into multiple self-contained code blocks instead.
		if len(pretty) <= maxBody {
			b.reply(pretty)
		} else {
			for _, chunk := range splitPrettyDump(pretty) {
				b.reply(chunk)
			}
		}
		return
	}
	b.reply(fmt.Sprintf("📄 raw session dump — %s (%d bytes)", path, len(raw)))
	b.reply(string(raw))
}

// prettyDump reformats a session's JSONL into a compact table — one row per
// record with its index, time, kind (message role, or record type), and a
// one-line detail preview. Wrapped in a code fence so styling-aware clients
// render it monospace.
func prettyDump(raw []byte) string {
	type row struct{ idx, tm, kind, detail string }
	var rows []row
	kindW := len("KIND")
	i := 0
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(line), &obj) != nil {
			continue
		}
		tm, kind, detail := recordRow(Event(obj))
		if len(kind) > kindW {
			kindW = len(kind)
		}
		// Collapse whitespace/newlines so each record stays one row, but keep the
		// full detail (no truncation).
		rows = append(rows, row{strconv.Itoa(i), tm, kind, strings.Join(strings.Fields(detail), " ")})
		i++
	}
	if len(rows) == 0 {
		return "(no records)"
	}
	var sb strings.Builder
	sb.WriteString("```\n")
	fmt.Fprintf(&sb, "%3s  %-8s  %-*s  %s\n", "#", "TIME", kindW, "KIND", "DETAIL")
	for _, r := range rows {
		fmt.Fprintf(&sb, "%3s  %-8s  %-*s  %s\n", r.idx, r.tm, kindW, r.kind, r.detail)
	}
	sb.WriteString("```")
	return sb.String()
}

// splitPrettyDump splits a code-fenced pretty table into multiple
// self-contained code blocks, each small enough to fit in one message.
func splitPrettyDump(dump string) []string {
	// Strip the outer ``` fences
	body := strings.TrimPrefix(dump, "```\n")
	body = strings.TrimSuffix(body, "\n```")
	lines := strings.Split(body, "\n")
	if len(lines) < 2 {
		return []string{dump}
	}
	header := lines[0] // "  #  TIME  KIND  DETAIL"
	rows := lines[1:]

	// Reserve ~100 bytes per chunk for fence + header overhead
	const overhead = 100
	var chunks []string
	start := 0
	for i := 0; i <= len(rows); i++ {
		size := 0
		for j := start; j < i && j < len(rows); j++ {
			size += len(rows[j]) + 1
		}
		if size+overhead > maxBody && i > start {
			// Emit chunk [start, i)
			var sb strings.Builder
			sb.WriteString("```\n")
			sb.WriteString(header)
			sb.WriteByte('\n')
			for _, r := range rows[start:i] {
				sb.WriteString(r)
				sb.WriteByte('\n')
			}
			sb.WriteString("```")
			chunks = append(chunks, sb.String())
			start = i
		}
		_ = size
	}
	// Remaining rows
	if start < len(rows) {
		var sb strings.Builder
		sb.WriteString("```\n")
		sb.WriteString(header)
		sb.WriteByte('\n')
		for _, r := range rows[start:] {
			sb.WriteString(r)
			sb.WriteByte('\n')
		}
		sb.WriteString("```")
		chunks = append(chunks, sb.String())
	}
	return chunks
}

// recordRow summarizes one session JSONL record into (time, kind, detail) for
// the pretty table. Kind is the message role for message records, else the
// record type; detail is a one-line preview appropriate to the record.
func recordRow(e Event) (tm, kind, detail string) {
	if ts := e.Str("timestamp"); len(ts) >= 19 {
		tm = ts[11:19] // HH:MM:SS from the ISO timestamp
	}
	switch typ := e.Str("type"); typ {
	case "message":
		msg := e.Obj("message")
		role := msg.Str("role")
		if role == "toolResult" {
			return tm, "toolResult", "↳ " + msg.Str("toolName") + ": " + contentText(msg["content"])
		}
		return tm, role, contentText(msg["content"])
	case "model_change":
		return tm, "model", e.Str("provider") + "/" + e.Str("modelId")
	case "thinking_level_change":
		return tm, "thinking", e.Str("thinkingLevel")
	case "compaction":
		return tm, "compaction", "compacted: " + e.Str("summary")
	case "session", "session_info":
		if n := e.Str("name"); n != "" {
			return tm, typ, n
		}
		return tm, typ, e.Str("cwd")
	default:
		return tm, typ, ""
	}
}

// contentText renders a message's content (string or block array) to a compact
// one-line preview: text verbatim, tool calls as "⚙ <name>", thinking as 💭.
func contentText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, it := range c {
			m, ok := it.(map[string]any)
			if !ok {
				continue
			}
			e := Event(m)
			switch e.Str("type") {
			case "text":
				parts = append(parts, e.Str("text"))
			case "thinking":
				parts = append(parts, "💭")
			case "toolCall":
				detail := "⚙ " + e.Str("toolName")
				if args := e.Obj("args"); args != nil {
					detail += " " + compactArgs(args)
				}
				parts = append(parts, detail)
			default:
				parts = append(parts, "["+e.Str("type")+"]")
			}
		}
		return strings.Join(parts, " ")
	default:
		return ""
	}
}

// compactArgs renders a tool-call arg map as a compact one-liner: key1=val1 key2=val2
// Values are collapsed: strings in full, numbers as-is, booleans as true/false,
// nested objects/arrays as [...] placeholder.
func compactArgs(args Event) string {
	var pairs []string
	for k, v := range args {
		switch val := v.(type) {
		case string:
			val = strings.Join(strings.Fields(val), " ")
			if len(val) > 40 {
				val = val[:37] + "…"
			}
			pairs = append(pairs, k+"="+val)
		case float64:
			pairs = append(pairs, k+"="+strconv.FormatFloat(val, 'f', -1, 64))
		case bool:
			pairs = append(pairs, k+"="+strconv.FormatBool(val))
		default:
			pairs = append(pairs, k+"=[…]")
		}
	}
	sort.Strings(pairs)
	return strings.Join(pairs, " ")
}

// composePrompt assembles the text sent to pi. Serverless messaging is
// strictly 1:1, so the prompt is the owner's message verbatim, plus the stanza
// ID and target JID the agent needs to call send_reaction (messageId and
// from-JID).
func (b *Bridge) composePrompt(body, reactID, reactTo string) string {
	var sb strings.Builder
	if reactID != "" {
		fmt.Fprintf(&sb, "stanza-id: %s\n", reactID)
		if reactTo != "" {
			fmt.Fprintf(&sb, "react-to: %s\n", reactTo)
		}
		sb.WriteString("\n")
	}
	sb.WriteString(body)
	return sb.String()
}

// reply sends a bridge-generated notice (banner, command results, shutdown,
// errors) to the owner's 1:1 — the primary channel. Agent replies go through
// deliverReply instead.
func (b *Bridge) reply(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	b.xmpp.Send(text)
}

// deliverReply routes one agent-produced message. The agent's markdown output
// is converted to XHTML-IM (XEP-0071) so the owner's rich client (e.g. Adium)
// renders it styled, with a plain-text fallback built in. Serverless messaging
// is strictly 1:1, so the text goes to the owner.
func (b *Bridge) deliverReply(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	stanzaID := b.xmpp.SendRich(b.acct.Owner, text)
	// Update reaction target to the just-sent message so subsequent
	// send_reaction calls target the agent's own message.
	if stanzaID != "" {
		b.setReactTarget(b.acct.Owner, stanzaID)
	}
}

func (b *Bridge) setDirectTurn(v bool) { b.mu.Lock(); b.directTurn = v; b.mu.Unlock() }
func (b *Bridge) isDirectTurn() bool   { b.mu.Lock(); defer b.mu.Unlock(); return b.directTurn }

func (b *Bridge) handleModel(arg string) {
	if arg == "" {
		b.reply("usage: /model <provider/id> or /model <search>")
		return
	}
	if strings.Contains(arg, "/") {
		provider, rest, _ := strings.Cut(arg, "/")
		res, err := b.rpc.SetModel(b.ctx, provider, rest)
		b.reportResult(err, res, "🤖 model set: "+arg, "/model")
		return
	}
	// Fuzzy: fetch models and match by substring.
	res, err := b.rpc.GetAvailableModels(b.ctx)
	if err != nil {
		b.reply("⚠️ /model failed: " + err.Error())
		return
	}
	provider, id, ok := matchModel(res, arg)
	if !ok {
		b.reply(fmt.Sprintf("no model matches %q. Try /model provider/id.", arg))
		return
	}
	set, err := b.rpc.SetModel(b.ctx, provider, id)
	b.reportResult(err, set, fmt.Sprintf("🤖 model set: %s/%s", provider, id), "/model")
}

// reportResult sends okMsg on success, or a formatted failure for command cmd.
func (b *Bridge) reportResult(err error, res Event, okMsg, cmd string) {
	if err != nil {
		b.reply(fmt.Sprintf("⚠️ %s failed: %s", cmd, err.Error()))
		return
	}
	if res.success() {
		b.reply(okMsg)
		return
	}
	b.reply(fmt.Sprintf("⚠️ %s failed: %s", cmd, res.errText()))
}

// handleStreamDelta maps an assistant streaming delta (message_update) to the
// typing indicator and status label. Typing is lit only between text_start and
// text_end — i.e. only while words are actually being produced — so a "typing…"
// bubble genuinely predicts an imminent message rather than "busy".
func (b *Bridge) handleStreamDelta(ev Event) {
	ame := ev.Obj("assistantMessageEvent")
	if ame == nil {
		return
	}
	switch ame.Str("type") {
	case "thinking_start":
		b.xmpp.SetPresence("dnd", "thinking…")
	case "text_start":
		b.xmpp.SetPresence("dnd", "replying…")
		b.startTyping()
	case "text_end":
		b.stopTyping()
	}
}

// toolLabel renders a short "running <tool>" status from a tool_execution_start
// event, appending a command snippet for bash.
func toolLabel(ev Event) string {
	name := ev.Str("toolName")
	if name == "" {
		return "running a tool…"
	}
	if name == "bash" {
		if args := ev.Obj("args"); args != nil {
			if cmd := strings.TrimSpace(args.Str("command")); cmd != "" {
				return "! " + truncateLabel(cmd, 80)
			}
		}
	}
	return "! " + name
}

// truncateLabel collapses newlines and rune-safely caps s to max characters for
// use in a one-line presence status.
func truncateLabel(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

// --- typing indicator ---

func (b *Bridge) startTyping() {
	// Typing is a 1:1-owner chat-state. Serverless messaging is strictly 1:1,
	// so every turn drives it (unless the turn was already handled inline).
	if !b.isDirectTurn() {
		return
	}
	b.typingMu.Lock()
	defer b.typingMu.Unlock()
	b.xmpp.ChatState("composing")
	if b.typingStop != nil {
		return
	}
	stop := make(chan struct{})
	b.typingStop = stop
	go func() {
		tk := time.NewTicker(typingRefresh)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				b.xmpp.ChatState("composing")
			}
		}
	}()
}

// stopTyping is unconditional so a running indicator can always be cleared
// (avoiding a stuck "composing" if the reply channel flips mid-turn). It only
// emits the "active" chat-state if typing was actually running.
func (b *Bridge) stopTyping() {
	b.typingMu.Lock()
	defer b.typingMu.Unlock()
	if b.typingStop != nil {
		close(b.typingStop)
		b.typingStop = nil
		b.xmpp.ChatState("active")
	}
}

// settleLocally resets run-scoped UI (streaming flag, typing indicator,
// presence) when a control command ends the current run directly. Pi answers
// `abort` with an `error`(aborted) event rather than `agent_settled`, so the
// normal agent_settled cleanup never fires for an aborted run — otherwise the
// typing goroutine keeps re-asserting "composing" (and presence stays
// "working…") into the next session. Idempotent and mutex-guarded, so it's
// safe if a late agent_settled also arrives.
func (b *Bridge) settleLocally() {
	b.setStreaming(false)
	b.stopTyping()
	b.xmpp.SetPresence("", "listening ("+nowStamp()+")")
}

// --- small state accessors ---

// setReactTarget records which message the next run's agent-driven reactions
// (send_reaction tool) attach to. Called before each prompt and updated by
// deliverReply so agent reactions target its own outgoing messages.
func (b *Bridge) setReactTarget(to, id string) {
	b.mu.Lock()
	b.reactTo, b.reactID = to, id
	b.mu.Unlock()
}

// setLifecycleReactTarget records both the regular react target AND a
// snapshot for lifecycle auto-reacts (👀✅⛔). The lifecycle snapshot is never
// overwritten by deliverReply, so agent_settled's ✅ always targets the
// original triggering message.
func (b *Bridge) setLifecycleReactTarget(to, id string) {
	b.mu.Lock()
	b.reactTo, b.reactID = to, id
	b.lifecycleReactTo, b.lifecycleReactID = to, id
	b.mu.Unlock()
}

// sendReaction sends a XEP-0444 reaction (emoji set) to the current run's
// target message. No-ops when no target is set (e.g. a room turn, where 1:1
// reaction tracking doesn't apply). Passing no emoji clears the reaction. This
// is the ungated path used for deliberate, agent-driven reactions.
func (b *Bridge) sendReaction(emojis ...string) {
	b.mu.Lock()
	to, id := b.reactTo, b.reactID
	b.mu.Unlock()
	if to == "" || id == "" {
		return
	}
	b.xmpp.SendReaction(to, id, emojis...)
}

// lifecycleReact maps a run-lifecycle beat to a reaction, but only when the
// per-account reactions flag is on — auto-reacting on every run can be noisy,
// so it's opt-in. Deliberate agent-driven reactions go through sendReaction and
// share the same flag gate at their call site.
func (b *Bridge) lifecycleReact(emojis ...string) {
	if !b.acct.Reactions {
		return
	}
	b.mu.Lock()
	to, id := b.lifecycleReactTo, b.lifecycleReactID
	b.mu.Unlock()
	if to == "" || id == "" {
		return
	}
	b.xmpp.SendReaction(to, id, emojis...)
}

func (b *Bridge) setStreaming(v bool) { b.mu.Lock(); b.streamingRun = v; b.mu.Unlock() }
func (b *Bridge) streaming() bool     { b.mu.Lock(); defer b.mu.Unlock(); return b.streamingRun }
func (b *Bridge) setReplied(v bool)   { b.mu.Lock(); b.repliedThisRun = v; b.mu.Unlock() }
func (b *Bridge) replied() bool       { b.mu.Lock(); defer b.mu.Unlock(); return b.repliedThisRun }

// steerBehavior returns "steer" when a run is already in flight, else "".
func (b *Bridge) steerBehavior() string {
	if b.streaming() {
		return "steer"
	}
	return ""
}

// --- pure helpers ---

// extractText pulls the plain-text portion out of an assistant message's
// content, which is either a string or an array of typed content blocks.
func extractText(content any) string {
	switch c := content.(type) {
	case string:
		return strings.TrimSpace(c)
	case []any:
		var parts []string
		for _, item := range c {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if m["type"] == "text" {
				if s, ok := m["text"].(string); ok {
					parts = append(parts, s)
				}
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	}
	return ""
}

// splitCommand splits "/name arg..." into a lowercased name and trimmed arg.
func splitCommand(t string) (name, arg string) {
	body := strings.TrimPrefix(t, "/")
	if sp := strings.IndexByte(body, ' '); sp >= 0 {
		return strings.ToLower(body[:sp]), strings.TrimSpace(body[sp+1:])
	}
	return strings.ToLower(body), ""
}

// matchModel finds the first available model whose "provider/id" contains the
// query (case-insensitive), from a get_available_models response.
func matchModel(res Event, query string) (provider, id string, ok bool) {
	data := res.Obj("data")
	if data == nil {
		return "", "", false
	}
	models, _ := data["models"].([]any)
	q := strings.ToLower(query)
	for _, m := range models {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		p, _ := mm["provider"].(string)
		i, _ := mm["id"].(string)
		if strings.Contains(strings.ToLower(p+"/"+i), q) {
			return p, i, true
		}
	}
	return "", "", false
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
