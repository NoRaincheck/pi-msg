# Subagents for beltino (running under pi-msg / `pi --mode rpc`)

Status: proposed · 2026-07-23

## Context

`beltino` is a `pi` coding-agent persona that now runs **headless** as a systemd
service: `pi-msg` spawns `pi --mode rpc` and bridges its JSONL event stream to
XMPP, so Zach drives it entirely from a chat client. It replaced the previous
setup (interactive `pi` in a tmux session + the `pi-xmpp` TUI extension).

beltino's old subagent system was built for that retired world. It lives in the
`zachpmanson/beltino` repo and assumes an interactive host:

- `scripts/spawn-agent.sh` opens a **tmux window** (`tmux new-window -t pi …`)
  and runs each subagent as its own interactive `pi` + `pi-xmpp` instance.
- each subagent gets **its own XMPP account** (`register-xmpp-agent.sh` /
  `unregister-xmpp-agent.sh`, with `s-` prefix guards), reports into a MUC room
  (`testing@muc.chat.zachmanson.com`), and is tasked via `tmux send-keys`.
- the `.pi/skills/Subagent Management` skill and `wiki/Subagents.md` document
  this lifecycle; `.pi/extensions/harness-expose.ts` re-exposes `/new`,
  `/compact`, `/reload` etc. as tools.

Under `pi --mode rpc` there is **no tmux session and no `pi-xmpp`**, so
`spawn-agent.sh` has no `pi` window to attach to, and the per-subagent XMPP
accounts are built on the extension `pi-msg` replaced. The whole subsystem is
now inconsistent with how the parent runs.

Requirements for the replacement:

1. subagents have their own internal thinking (isolated context).
2. the common case is **summon → run → done** (fire-and-forget).
3. a subagent that gets stuck can have a **2-way conversation** to get
   clarification — ideally reaching Zach.

## Decision

**Adopt the `pi-subagents` plugin (nicobailon). Do not build a bespoke subagent
system, and do not use XMPP/MUC for parent↔subagent traffic.** For the flow
beltino needs, the plugin works **as-is under `pi --mode rpc` with no change to
pi-msg** — the summon→run→done→relay and clarification loops all ride
message-injection paths pi-msg already carries.

`pi` itself has **no native subagent primitive**, but there are two off-the-shelf
implementations. We chose between them deliberately:

| | **nicobailon** `npm:pi-subagents` (chosen) | **tintinweb** `npm:@tintinweb/pi-subagents` |
|---|---|---|
| Child process model | Separate `pi` **subprocess** per agent | **In-process** via `AgentSession` SDK |
| Isolation | Process-level — a crashing/looping child can't take down beltino | Shared fate with beltino's single process |
| Headless fit | Explicit `!ctx.hasUI` fallbacks; less TUI-dependent | Flagship UX (live widget, FleetView, conversation viewer) is TUI-only and inert over XMPP |
| Tool surface | `subagent(...)` with single/parallel/chain + management actions | Claude-Code-identical `Agent` / `get_subagent_result` / `steer_subagent` |

Both avoid XMPP/MUC for parent↔child, both deliver results by injecting a
follow-up into the parent session, and both do worktree isolation, agent memory,
and scheduling. We pick **nicobailon** because beltino is a **long-lived,
always-on, headless** persona: subprocess isolation matters more than a terminal
UX beltino can't render, and the explicit headless fallbacks fit a no-UI host.
(tintinweb is the stronger pick for *interactive* terminal use — its FleetView /
viewer / widget are the best part, and exactly the part XMPP throws away.)

## What `pi-subagents` (nicobailon) provides

(Verified against the 0.35.1 source.)

- **Isolated child sessions** — each subagent is a separate `pi` subprocess with
  its own context window. Requirement 1, for free.
- **Foreground runs** — block and return the child's final output inline.
- **Background / async runs** — return a `runId` immediately and notify on
  completion; lets beltino stay responsive to Zach. Requirement 2.
- **Parallel and chain** — `tasks: [...]` (default 4 concurrent) and
  `chain: [...]` (each result becomes `{previous}`).
- **Builtin agents** — scout, researcher, planner, worker, reviewer,
  context-builder, oracle, delegate. Inherit the parent default model unless
  overridden.
- **Management actions** on the same tool — `resume`, `steer`, `status`,
  `schedule`, `create`/`update`.
- **Result delivery by injection** —
  `pi.sendMessage({customType:"subagent-notification"}, {deliverAs:"followUp", triggerTurn:true})`
  (`src/extension/index.ts:360`). This is the load-bearing mechanism for
  headless operation (see below).
- **Worktree isolation, agent memory (project/local/user), scheduling, a
  `doctor` self-check, and an intercom supervisor channel.**

## Why this works headless with no pi-msg change

The human-facing pieces split in two:

- **Message injection — works with beltino as-is.** Background completion,
  the intercom supervisor channel, and child steering all inject into beltino's
  own session. A completion `followUp` with `triggerTurn:true` **triggers a
  beltino turn even when idle**; beltino's response then flows out through
  pi-msg's normal `message_end` relay to Zach — an unprompted "your subagent
  finished" ping, exactly what's wanted. This rides the same injection path
  pi-msg already uses for Zach's inbound messages.
- **Interactive UI dialogs — avoided.** The `clarify: true` TUI preview and the
  `/agents`-style `ctx.ui.select/confirm` prompts are the only things that would
  hit an `extension_ui_request`, which pi-msg currently cancels (`rpc.go` /
  `bridge.go`, "nobody's at the TUI"). We simply don't use them (see caveats).

## How beltino works with it

### A. Foreground (quick, blocking)

```
Zach → beltino:  "review the diff on the auth branch"
beltino calls:   subagent({ agent: "reviewer", task: "review the diff on branch auth-refactor" })
                 (blocks; child streaming is invisible to Zach, but the result returns inline)
beltino → Zach:  "Reviewer found 3 issues: …"
```

### B. Background / async (the primary pattern)

```
Zach → beltino:  "have a worker implement the caching plan, ping me when done"
beltino calls:   subagent({ agent: "worker", task: "…", async: true })   → returns a runId
beltino → Zach:  "Started worker (run a1b2). I'll let you know."
   … beltino stays responsive to Zach …
   … child finishes → followUp subagent-notification → triggers a beltino turn …
beltino → Zach:  "✅ worker done (run a1b2): implemented X, touched 4 files. Summary: …"
```

### C. Parallel / chain

`subagent({ tasks: [...] })` for fan-out, `subagent({ chain: [...] })` for
sequential steps. Use for multi-part work.

## Clarification loop (requirement 3) — notify → relay → resume/steer

Neither plugin offers a *blocking* child→human dialog, so beltino uses a
non-blocking loop that is pure message-injection (no pi-msg change):

```
child (async) finishes or pauses needing info
   → its notification says "need decision: Redis vs in-memory?"
beltino → Zach:  "worker is stuck: Redis or in-memory for the cache?"
Zach → beltino:  "Redis"
beltino calls:   subagent({ action: "resume", runId: "a1b2", message: "Use Redis" })
   (or action:"steer" to redirect a still-running async child live)
   → child continues with the answer
```

`resume` revives paused/completed/failed children with a follow-up message;
`steer` gives live guidance to a running async run.

## What Zach can drive directly over chat

pi-msg forwards `/`-prefixed chat as a prompt, and pi dispatches extension
commands immediately, so these work over XMPP:

- `/run <task>` — launch a single agent
- `/chain …`, `/parallel …`, `/run-chain <saved>` — workflows
- `/subagents-doctor` — diagnose setup
- `/subagents-stop` — stop runs
- `/subagent-cost` — parent + child token/cost for the session

**Inert over XMPP** (TUI-only, do not rely on): `/subagents-fleet`, the live
widget, the conversation viewer, and `clarify: true`.

## Migration plan

In the **`zachpmanson/beltino`** repo:

1. `pi install npm:pi-subagents` (write to project settings so it's tracked and
   reinstalled on activation).
2. Delete the retired subsystem:
   - `scripts/spawn-agent.sh`, `cleanup-agent.sh`, `register-xmpp-agent.sh`,
     `unregister-xmpp-agent.sh`, `subagent-context.md`
   - `.pi/skills/Subagent Management/`
   - `wiki/Subagents.md` (replace with a short page pointing at `pi-subagents`)
   - `.pi/extensions/harness-expose.ts` (RPC + pi-msg already cover
     new-session/compact/reload/etc.)
3. Config:
   - ensure beltino's default-model provider is authed (builtin agents inherit
     the parent default model).
   - optionally set `subagents.defaultModel` for cheaper children.
   - leave `agentScope` at user-only (avoids the project-agent confirm prompt).
   - leave `subagent_wait` enabled.
4. Run `/subagents-doctor` from beltino to self-verify discovery, async paths,
   and the intercom bridge.

In the **`pi-msg`** repo: nothing required. See the optional enhancement below.

## Optional / future: hard clarification via pi-msg

The loop above is non-blocking: a child that needs input finishes (or pauses)
and is `resume`d. If you later want *hard* clarification — the child literally
blocks mid-run until Zach answers — that needs two things that are out of scope
now:

1. **pi-msg change**: stop blanket-cancelling `extension_ui_request`; route
   `input`/`confirm`/`select` dialogs to Zach over XMPP and feed the reply back
   as `extension_ui_response` (keyed by the request `id`; honour the optional
   `timeout`). Note `ctx.hasUI` is `true` in RPC mode, so these dialogs *do*
   fire — pi-msg is what currently drops them. This benefits any pi-msg
   deployment, not just beltino.
2. **agent-side**: a child that actually raises such a dialog (neither plugin
   does at runtime today — their `ctx.ui` calls are admin menus), so it would
   need custom agent prompting or a small child extension.

## To verify before/while implementing

- Confirm the background completion `followUp` with `triggerTurn:true` wakes
  beltino and relays to Zach when beltino is otherwise idle (the whole async
  pattern rests on this).
- Confirm `resume`/`steer` behave as expected against a real async run under
  `pi --mode rpc`.
- Confirm scheduling behaves under the long-lived service (jobs are
  session-scoped; reset on `/new`) before depending on cron fires.
- Check the installed `pi-subagents` version against 0.35.1; APIs referenced
  here are from that release.

## References

- pi `docs/rpc.md` — RPC protocol, framing, and the Extension UI Protocol.
- pi `docs/sdk.md` — `AgentSession` / `createAgentSession` (the in-process model
  tintinweb uses; not used here).
- pi `examples/extensions/subagent/` — the bundled reference implementation.
- `pi-subagents` 0.35.1 (chosen) — github.com/nicobailon/pi-subagents.
- `@tintinweb/pi-subagents` 0.14.3 (considered) — github.com/tintinweb/pi-subagents.
- `pi-msg` `rpc.go` / `bridge.go` — current extension-dialog cancelling behaviour
  (only relevant to the optional hard-clarification enhancement).
