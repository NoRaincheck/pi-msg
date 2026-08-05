# pi-msg

> Fork of [zachpmanson/pi-msg](https://github.com/zachpmanson/pi-msg/tree/main), modified
> to connect **only** to a local XMPP server discovered via Bonjour — no remote
> XMPP/Jabber server, no password auth.

Drive the [Pi](https://pi.dev) coding agent **entirely from a chat client** — 1:1
or in a group chat (MUC) — over a **local XMPP service discovered via Bonjour
(mDNS/DNS-SD)**. pi-msg never contacts a remote XMPP/Jabber server: it browses
your LAN for Bonjour IM users (`_presence._tcp`, as advertised by Adium and
Messages), connects to them anonymously, and relays your messages to the agent
and back.

`pi-msg` launches `pi --mode rpc`, then bridges Pi's JSONL event stream to XMPP
(via [mellium.im/xmpp](https://mellium.im/xmpp)): the assistant's replies are relayed
to you as chat messages, and your chat messages drive the agent — plain prompts **and**
slash commands, exactly as if you'd typed them into Pi locally.

Because it runs Pi in RPC mode, commands like `/new` work over chat (an earlier
in-process-extension version couldn't do this — `sendUserMessage` can't invoke Pi's
command layer).

## How it works

```mermaid
sequenceDiagram
    participant You as You (XMPP client)
    participant Bridge as pi-msg
    participant Pi as pi --mode rpc
    You->>Bridge: "fix the build"
    Bridge->>Pi: prompt
    Pi-->>Bridge: message_end event
    Bridge-->>You: assistant text
    You->>Bridge: "/new"
    Bridge->>Pi: {type:"new_session"}
    Note over Pi: fresh session
```

- Each finished **assistant message** → sent to you as chat.
- Agent state shows on three independent signals (1:1): a **typing indicator** while a
  reply is actually being written, presence **`<show>`** (`dnd` while busy, available
  when idle), and a presence **status** label of the current activity (`thinking…`,
  `running: <cmd>`, `replying…`, `retrying…`, `listening`). When a run settles with
  **no** text you get a `✅ done (no reply) — your turn` nudge.
- Messages you send are acknowledged with **read receipts** — XEP-0184 delivery
  receipts and XEP-0333 chat markers (`displayed`) — when the agent takes them in, if
  your client requests them.
- Your chat messages → routed to Pi:

| You send | Becomes |
| --- | --- |
| plain text | a prompt to the agent |
| `/skill:name …`, `/template …`, any extension command | a prompt (Pi expands/runs it) |
| `/new` | `new_session` (fresh session; connection stays up) |
| `/compact [instructions]` | `compact` |
| `/model <provider/id>` or `/model <search>` | `set_model` |
| `/think <off\|low\|medium\|high\|…>` | `set_thinking_level` |
| `/abort` (or `/stop`) | `abort` |
| `/dump` (or `/dump pretty`) | send the session transcript to the owner — raw JSONL, or `pretty` for indented per-record JSON (no LLM turn) |
| `/quit` (or `/exit`) | shut down the bridge and Pi |

## Configuration

Create `~/.config/pi-msg/config.json` (override the path with `PI_MSG_CONFIG`), then
`chmod 600` it:

```json
{
  "accounts": {
    "default": {
      "jid": "pi@mymac.local",
      "owner": "you@mymac.local",
      "model": "anthropic/claude-sonnet-latest",
      "workdir": "/path/to/your/project"
    }
  }
}
```

Per-account fields:

| field | required | default | notes |
| --- | --- | --- | --- |
| `jid` | yes | — | bare JID of the bot account (e.g. `pi@mymac.local`) |
| `owner` | yes | — | the human this account relays to; the **canonical** (trusted) driver |
| `bonjourService` | no | `_presence._tcp` | DNS-SD service type to browse — Bonjour IM (`_presence._tcp`) first, with `_xmpp-client._tcp` / `_jabber._tcp` as fallbacks when unset |
| `bonjourName` | no | — | narrow discovery to a service instance whose name contains this string (for LANs with several local servers) |
| `discoverTimeout` | no | `10s` | how long each Bonjour discovery attempt waits for a server (Go duration) |
| `resource` | no | `pi-msg` | XMPP resource (client-session label) |
| `model` | no | Pi's default | model pattern passed to `pi --model` |
| `workdir` | no | current dir | working directory for the agent (also where Pi discovers `AGENTS.md`/`CLAUDE.md`) |
| `room` | no | — | a bare MUC JID (or an **array** of them) to also join for **group chat** (see below) |
| `nick` | no | JID localpart | occupant nickname used in the room(s) |
| `roomTrigger` | no | `nick` | address prefix that makes a room message a prompt (e.g. `pi` → `pi: …`) |
| `uploadService` | no | auto-probed | XEP-0363 upload component JID for file transfer (set it if your local server has one) |
| `pingInterval` | no | `60s` | keepalive cadence (Go duration): XEP-0199 server ping + XEP-0410 MUC self-ping; `0` disables |
| `reactions` | no | `false` | XEP-0444 emoji reactions on 1:1 owner messages: lifecycle → 👀 picked up / ✅ done / ⛔ aborted, and enables the agent-driven `send_reaction` tool (see [Agent tools](#agent-tools)) |
| `avatar` | no | — | path to a local image (PNG/JPEG/GIF) published as the bot's XEP-0153 vCard profile picture on connect |

Multiple accounts: add more keys under `accounts`; `default` is used unless you set
`PI_MSG_ACCOUNT=<name>`. In 1:1 mode only the `owner` JID may drive the agent.

## Bonjour (local XMPP) only

pi-msg connects **only** to a local XMPP service advertised over Bonjour. On
every connect (and reconnect) it:

1. **Discovers** — browses Bonjour IM (`_presence._tcp`, where each online user
   advertises an instance like `you@my-mac`), falling back to `_xmpp-client._tcp`
   and then legacy `_jabber._tcp`, and dials the resolved address:port.
   Discovery is fresh each attempt, so a user that comes online later on the
   LAN is picked up.
2. **Authenticates anonymously** — no password anywhere. It tries SASL
   `ANONYMOUS` first (so standard resource binding applies); if the service
   offers no ANONYMOUS mechanism it connects with **no SASL at all**, using a
   resource-binding feature that doesn't require prior authentication.
3. **Uses opportunistic STARTTLS** — it attempts TLS first, accepting
   self-signed certificates (the LAN is trusted); if the service doesn't offer
   TLS it falls back to plaintext.

See who's online and what config to write with:

```bash
just discover        # or: ./pi-msg --discover
```

which lists every discovered service (e.g. `crn@mbpro  mbpro.local:5298`) and
prints the `jid`/`owner` to put in config.

Requirements:

- **mDNS must work on the host** — macOS resolves and advertises Bonjour out of
  the box; Linux needs `avahi` (or `systemd-resolved` with mDNS enabled).
- **A Bonjour IM user (e.g. Adium/Messages) must be online**, or a local XMPP
  server advertising `_xmpp-client._tcp` with anonymous / no-auth client
  connections (e.g. Prosody with `authentication = "anonymous"`, or an
  ejabberd anonymous-only virtual host).
- The bot and owner JIDs are like `pi@<host>` — the domain is the machine's
  local hostname, **not** a registered internet domain.

## Group chat (MUC)

Set `room` on an account (a single MUC JID, or an array of them) and pi-msg
**also** joins each. **The owner's 1:1 stays the primary channel** — joining a
room is purely additive and doesn't change 1:1 behaviour (typing indicator,
lifecycle notices, and unsolicited output all still go to the owner). Each reply
goes back to whichever channel the message arrived on, including the specific
room when several are joined. Room messages are handled on **two independent
axes**:

- **Trigger** — does the message start/steer a turn?
  - the **owner** → always
  - anyone else who **addresses the bot by name** (`pi: …` / `pi, …`) → always
  - all other chatter → never (it's buffered as ambient context)
- **Authority** — is the content trusted?
  - the **owner** → canonical (authoritative)
  - everyone else, even when addressing the bot → untrusted *commentary*; the agent is
    told to use its judgment and is under no obligation to act on it

Untriggered messages are buffered and, on the next turn, prepended to the prompt as a
clearly-labeled *"room commentary — non-canonical"* block, then the buffer clears.

**Reply routing (explicit `from:`/`to:`).** When an account has room access, routing is
fully explicit — no guessing. Each prompt the agent receives leads with a header naming
the message's origin:

```
from: <channel jid>     # the room (group msg) or the owner (DM) — reply here to answer in place
sender: <person jid>    # room messages only, when the real JID is known — reply here to DM them
<message body>
```

And **every** agent reply must begin with a `to: <jid>` line naming its destination:

- `to: <room jid>` → the group chat (groupchat)
- `to: <owner or occupant jid>` → that person, 1:1

One reply may contain **several `to:` blocks** — each `to:` line starts a new message, so
the agent can fan a single turn out to multiple destinations:

```
to: team@muc.chat.zachmanson.com
Deploying now — back in 5.
to: zach@chat.zachmanson.com
(privately: the staging creds are stale, heads up)
```

Destinations are **allowlisted**: the owner, joined room(s), and real JIDs currently seen
in a room. A reply whose `to:` is missing or points anywhere else is sent to the owner, so
nothing is silently lost — the agent can't message arbitrary users. In a pure 1:1 account
(no room) there are no prefixes; replies just go to the owner.

**File transfer.** The agent sends files with the **`send_file`** tool (a structured tool
call, not in-band text — see [Agent tools](#agent-tools) below): pi-msg uploads the file via
**XEP-0363 HTTP Upload** and sends the resulting URL as an **XEP-0066** out-of-band message,
so the recipient's client shows a downloadable file. The destination is allowlisted (owner,
joined rooms, known occupants) exactly like a `to:` reply. The upload component is discovered
automatically (`upload.<domain>` / `httpupload.<domain>`) or set explicitly via the
`uploadService` config field.

**The room must be non-anonymous** (ejabberd: *"Present real Jabber IDs to → anyone"*,
optionally *members-only*). The owner is recognized by real JID; in a semi-anonymous
room real JIDs are hidden, so the owner can't be distinguished and every message falls
through to the untrusted/ambient tiers.

## Agent tools

Beyond reply text, the agent gets structured **tools** (registered by a small companion
extension that pi-msg loads into `pi --mode rpc`, which relays each call back to pi-msg to
perform the XMPP action):

| Tool | What it does | Enabled when |
| --- | --- | --- |
| `send_reaction` | React to the human's latest message with an emoji (XEP-0444) | `reactions` is on |
| `send_file` | Upload a local file and deliver it (XEP-0363 + XEP-0066); dest defaults to the current conversation, allowlisted | always |

Reply **routing** (`to:`) stays an in-band text convention (above); only these discrete
side-effect actions are tools.

## Run

```bash
go build -o pi-msg . && ./pi-msg     # from the repo
```

Set `PI_MSG_DEBUG=1` to print connection/status/stderr diagnostics. On startup the bot
simply comes **online** in your roster (presence `listening`); on shutdown or a pi crash it
goes **offline** with a `<status>` describing why and when — pi-msg no longer posts chat
banners for these lifecycle events.

Requirements: Go ≥ 1.26 (to build), and a `pi` on `PATH` that's logged into a provider
(`pi` → `/login`).

## Notes

- Pi runs tools autonomously (no built-in approval prompts). If some other extension
  raises a dialog (`select`/`confirm`/`input`/`editor`), pi-msg auto-dismisses it
  (nobody's at the TUI) and tells you over chat — so approval-gated tools are declined
  over the bridge.
- Auth is **anonymous**: pi-msg tries SASL `ANONYMOUS` first, then falls back to no
  authentication at all (with a resource-binding feature that doesn't require auth).
  STARTTLS is opportunistic — attempted first and skipped if the server doesn't offer
  it; self-signed certificates are accepted, since the connection is local-only.
