// xmpp-tools.ts — companion Pi extension for pi-msg.
//
// pi-msg runs the agent as `pi --mode rpc -e <this file>`. It owns the XMPP
// connection; the agent (Pi) is a separate process. A registered tool's handler
// therefore can't touch the socket directly — so it relays the action to pi-msg
// over the RPC extension-UI channel and blocks for the result.
//
// Relay transport: `ctx.ui.confirm(title, message)`. In RPC mode this emits an
// `extension_ui_request` (method "confirm") on stdout and blocks until the
// client sends back `extension_ui_response {confirmed}`. We smuggle a JSON
// action through the sentinel-prefixed `title`; pi-msg recognises the sentinel,
// performs the real XMPP action, and answers `confirmed: true/false`. That
// gives each tool a genuine success/failure to report to the LLM (unlike a
// fire-and-forget notify).
//
// Which tools are registered is chosen by pi-msg via the PI_MSG_TOOLS env var
// (comma-separated); this mirrors the account's config (e.g. send_reaction is
// gated on the `reactions` opt-in). This is the "structured tool call instead
// of in-band text" path from issue #8 / docs/subagents.md. Serverless messaging
// has no XEP-0363 upload component, so send_file was removed; send_reaction
// (XEP-0444) is the only side-effect tool.
//
// Types are erased by jiti at load time, so the `import type` never resolves at
// runtime; only the value import (`typebox`) is resolved, against Pi's own deps.

import type { ExtensionAPI } from "@earendil-works/pi-coding-agent"
import { Type } from "typebox"

// Marks a confirm as a pi-msg action relay rather than a real user dialog.
// Kept in sync with relayPrefix in extension.go.
const RELAY_PREFIX = "pi-msg-relay:"

// Minimal structural type for the one UI method we use, so we don't depend on
// the exact exported type name.
type ConfirmUI = { confirm(title: string, message?: string): Promise<boolean> }

export default function xmppTools(pi: ExtensionAPI) {
  // Captured on session_start; used by tool handlers to reach pi-msg.
  let ui: ConfirmUI | undefined

  pi.on("session_start", (_event, ctx) => {
    ui = ctx.ui as unknown as ConfirmUI
  })

  // Inject the agent's identity ($PI_MSG_ACCOUNT) at the top of every system
  // prompt so it's the first thing the agent reads. Prevents identity confusion
  // in multi-persona fleets where several agents share the same project context.
  pi.on("before_agent_start", async (event) => {
    const account = process.env.PI_MSG_ACCOUNT
    if (!account) return
    return {
      systemPrompt: `You are **${account}**. This is your identity in Zach\'s fleet.

${event.systemPrompt}`,
    }
  })

  // Which tools to register, chosen by pi-msg via PI_MSG_TOOLS (comma list).
  // Unset (e.g. running the extension standalone) enables both.
  const raw = process.env.PI_MSG_TOOLS
  const enabled = raw === undefined ? new Set(["reaction"]) : new Set(raw.split(",").map((s) => s.trim()))

  // relay hands an action to pi-msg and blocks for its boolean result.
  async function relay(action: string, args: Record<string, unknown>): Promise<boolean> {
    if (!ui) {
      throw new Error("no relay channel to pi-msg (session not started)")
    }
    return ui.confirm(RELAY_PREFIX + JSON.stringify({ action, ...args }), "pi-msg action")
  }

  if (enabled.has("reaction")) {
    pi.registerTool({
      name: "send_reaction",
      label: "React (XMPP)",
      description:
        "React to a chat message with a single emoji over XMPP (XEP-0444). By default reacts to the most recent incoming message; pass messageId to target an arbitrary message by its stanza ID.",
      promptSnippet: "React to a chat message with an emoji",
      promptGuidelines: [
        "Use send_reaction to acknowledge a message with one emoji (e.g. 👀 for seen, ✅ for done).",
        "To react to a specific message, include its stanza ID as messageId. The from-JID is resolved from the message history cache; if that fails you may also supply the from-JID explicitly.",
      ],
      parameters: Type.Object({
        emoji: Type.String({ description: "A single emoji, e.g. 👀 or ✅" }),
        messageId: Type.Optional(
          Type.String({
            description:
              "Optional XMPP stanza ID of the target message; omitting targets the most recent incoming message",
          }),
        ),
        from: Type.Optional(
          Type.String({
            description:
              "Optional full JID of the target message's author; resolved automatically from message history cache when messageId is provided",
          }),
        ),
      }),
      async execute(_toolCallId, params) {
        const p = params as { emoji?: string; messageId?: string; from?: string }
        const emoji = String(p.emoji ?? "").trim()
        if (!emoji) {
          throw new Error("emoji is required")
        }
        const args: Record<string, unknown> = { emoji }
        if (p.messageId) {
          args.messageId = p.messageId
        }
        if (p.from) {
          args.from = p.from
        }
        if (!(await relay("react", args))) {
          throw new Error(
            "pi-msg could not send the reaction" +
              (p.messageId
                ? " (target message not found in history; try supplying from-JID explicitly)"
                : " (no message to react to?)"),
          )
        }
        return {
          content: [{ type: "text", text: `Reacted with ${emoji}.` }],
          details: { emoji, ...(p.messageId ? { messageId: p.messageId } : {}) },
        }
      },
    })
  }
}
