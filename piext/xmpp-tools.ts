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
// fire-and-forget notify) — which matters for file uploads that are slow and
// can fail.
//
// Which tools are registered is chosen by pi-msg via the PI_MSG_TOOLS env var
// (comma-separated); this mirrors the account's config (e.g. send_reaction is
// gated on the `reactions` opt-in). This is the "structured tool call instead
// of in-band text" path from issue #8 / docs/subagents.md. Routing (`to:`)
// intentionally stays prompt-injected; only discrete side-effect actions move
// to tools.
//
// Types are erased by jiti at load time, so the `import type` never resolves at
// runtime; only the value import (`typebox`) is resolved, against Pi's own deps.

import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { Type } from "typebox";

// Marks a confirm as a pi-msg action relay rather than a real user dialog.
// Kept in sync with relayPrefix in extension.go.
const RELAY_PREFIX = "pi-msg-relay:";

// Minimal structural type for the one UI method we use, so we don't depend on
// the exact exported type name.
type ConfirmUI = { confirm(title: string, message?: string): Promise<boolean> };

export default function xmppTools(pi: ExtensionAPI) {
	// Captured on session_start; used by tool handlers to reach pi-msg.
	let ui: ConfirmUI | undefined;

	pi.on("session_start", (_event, ctx) => {
		ui = ctx.ui as unknown as ConfirmUI;
	});

	// Which tools to register, chosen by pi-msg via PI_MSG_TOOLS (comma list).
	// Unset (e.g. running the extension standalone) enables both.
	const raw = process.env.PI_MSG_TOOLS;
	const enabled =
		raw === undefined ? new Set(["file", "reaction"]) : new Set(raw.split(",").map((s) => s.trim()));

	// relay hands an action to pi-msg and blocks for its boolean result.
	async function relay(action: string, args: Record<string, unknown>): Promise<boolean> {
		if (!ui) {
			throw new Error("no relay channel to pi-msg (session not started)");
		}
		return ui.confirm(RELAY_PREFIX + JSON.stringify({ action, ...args }), "pi-msg action");
	}

	if (enabled.has("reaction")) {
		pi.registerTool({
			name: "send_reaction",
			label: "React (XMPP)",
			description:
				"React to the human's most recent chat message with a single emoji, over XMPP (XEP-0444). Use for a lightweight acknowledgement in place of a text reply.",
			promptSnippet: "React to the latest chat message with an emoji",
			promptGuidelines: [
				"Use send_reaction to acknowledge the human's message with one emoji (e.g. 👀 for seen, ✅ for done) when a full text reply isn't warranted.",
			],
			parameters: Type.Object({
				emoji: Type.String({ description: "A single emoji, e.g. 👀 or ✅" }),
			}),
			async execute(_toolCallId, params) {
				const emoji = String((params as { emoji?: string }).emoji ?? "").trim();
				if (!emoji) {
					throw new Error("emoji is required");
				}
				if (!(await relay("react", { emoji }))) {
					throw new Error("pi-msg could not send the reaction (no message to react to?)");
				}
				return {
					content: [{ type: "text", text: `Reacted with ${emoji}.` }],
					details: { emoji },
				};
			},
		});
	}

	if (enabled.has("file")) {
		pi.registerTool({
			name: "send_file",
			label: "Send file (XMPP)",
			description:
				"Upload a local file and deliver it to the human over XMPP (XEP-0363 HTTP Upload). The path must be absolute and readable on this host. Defaults to the current conversation; pass `to` to target a specific allowed JID.",
			promptSnippet: "Send a local file (log, diff, image) to the human over chat",
			promptGuidelines: [
				"Use send_file to deliver a real local file to the human; give an absolute path. It is for files, not for pasting text.",
			],
			parameters: Type.Object({
				path: Type.String({ description: "Absolute path to a local file on this host" }),
				to: Type.Optional(Type.String({ description: "Destination JID; defaults to the current conversation" })),
			}),
			async execute(_toolCallId, params) {
				const p = params as { path?: string; to?: string };
				const path = String(p.path ?? "").trim();
				if (!path) {
					throw new Error("path is required");
				}
				if (!(await relay("file", { path, to: p.to ?? "" }))) {
					throw new Error(`pi-msg could not send the file ${path}`);
				}
				return {
					content: [{ type: "text", text: `Sent file ${path}.` }],
					details: { path },
				};
			},
		});
	}
}
