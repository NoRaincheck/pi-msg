package main

import (
	"strings"
	"testing"
)

func TestExtractText(t *testing.T) {
	if got := extractText("  hi  "); got != "hi" {
		t.Errorf("string content = %q, want hi", got)
	}
	content := []any{
		map[string]any{"type": "text", "text": "line one"},
		map[string]any{"type": "thinking", "thinking": "should be dropped"},
		map[string]any{"type": "text", "text": "line two"},
	}
	if got := extractText(content); got != "line one\nline two" {
		t.Errorf("array content = %q, want two joined text blocks (thinking dropped)", got)
	}
	if got := extractText(nil); got != "" {
		t.Errorf("nil content = %q, want empty", got)
	}
	// Non-text-only content yields empty.
	onlyThinking := []any{map[string]any{"type": "thinking", "thinking": "x"}}
	if got := extractText(onlyThinking); got != "" {
		t.Errorf("thinking-only content = %q, want empty", got)
	}
}

func TestSplitCommand(t *testing.T) {
	cases := []struct {
		in        string
		name, arg string
	}{
		{"/new", "new", ""},
		{"/model anthropic/claude", "model", "anthropic/claude"},
		{"/think high", "think", "high"},
		{"/COMPACT  keep the api notes ", "compact", "keep the api notes"},
	}
	for _, c := range cases {
		name, arg := splitCommand(c.in)
		if name != c.name || arg != c.arg {
			t.Errorf("splitCommand(%q) = (%q,%q), want (%q,%q)", c.in, name, arg, c.name, c.arg)
		}
	}
}

func TestMatchModel(t *testing.T) {
	res := Event{"data": map[string]any{"models": []any{
		map[string]any{"provider": "anthropic", "id": "claude-sonnet-5"},
		map[string]any{"provider": "google", "id": "gemini-2.5-pro"},
	}}}
	provider, id, ok := matchModel(res, "sonnet")
	if !ok || provider != "anthropic" || id != "claude-sonnet-5" {
		t.Errorf("matchModel(sonnet) = (%q,%q,%v), want anthropic/claude-sonnet-5", provider, id, ok)
	}
	if _, _, ok := matchModel(res, "nonesuch"); ok {
		t.Error("matchModel(nonesuch) matched unexpectedly")
	}
}

func TestToolLabel(t *testing.T) {
	cases := []struct {
		name string
		ev   Event
		want string
	}{
		{"bash with command", Event{"toolName": "bash", "args": map[string]any{"command": "npm test"}}, "! npm test"},
		{"bash collapses whitespace", Event{"toolName": "bash", "args": map[string]any{"command": "go  build\n./..."}}, "! go build ./..."},
		{"non-bash tool", Event{"toolName": "read_file"}, "! read_file"},
		{"missing name", Event{}, "running a tool…"},
		{"bash no command", Event{"toolName": "bash"}, "! bash"},
	}
	for _, c := range cases {
		if got := toolLabel(c.ev); got != c.want {
			t.Errorf("%s: toolLabel = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestTruncateLabel(t *testing.T) {
	if got := truncateLabel("short", 40); got != "short" {
		t.Errorf("short = %q, want unchanged", got)
	}
	long := "abcdefghij" // 10 runes
	if got := truncateLabel(long, 5); got != "abcd…" {
		t.Errorf("long = %q, want abcd…", got)
	}
	if got := truncateLabel("a\tb\nc  d", 40); got != "a b c d" {
		t.Errorf("whitespace = %q, want single-spaced", got)
	}
}

func TestComposePrompt(t *testing.T) {
	b := NewBridge(ResolvedAccount{Owner: "zach@x.com"}, false)
	// No stanza id → body verbatim.
	if got := b.composePrompt("hello there", "", ""); got != "hello there" {
		t.Errorf("composePrompt no stanza-id = %q, want 'hello there'", got)
	}
	// Stanza id + target JID → header lines, then blank line, then body.
	got := b.composePrompt("do it", "msg-7", "zach@x.com/phone")
	want := "stanza-id: msg-7\nreact-to: zach@x.com/phone\n\ndo it"
	if got != want {
		t.Errorf("composePrompt = %q, want %q", got, want)
	}
}

func TestPrettyDump(t *testing.T) {
	jsonl := strings.Join([]string{
		`{"type":"session","timestamp":"2024-12-03T14:00:00.000Z","cwd":"/proj"}`,
		`{"type":"message","timestamp":"2024-12-03T14:00:01.000Z","message":{"role":"user","content":"fix the build"}}`,
		`{"type":"message","timestamp":"2024-12-03T14:00:02.000Z","message":{"role":"assistant","content":[{"type":"text","text":"on it"},{"type":"toolCall","toolName":"bash"}]}}`,
		`{"type":"message","timestamp":"2024-12-03T14:00:03.000Z","message":{"role":"toolResult","toolName":"bash","content":[{"type":"text","text":"exit 0"}]}}`,
		`{"type":"model_change","timestamp":"2024-12-03T14:05:00.000Z","provider":"anthropic","modelId":"claude"}`,
	}, "\n")
	out := prettyDump([]byte(jsonl))
	for _, want := range []string{
		"TIME", "KIND", "DETAIL",
		"14:00:01", "user", "fix the build",
		"assistant", "on it ⚙ bash",
		"toolResult", "↳ bash: exit 0",
		"model", "anthropic/claude",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("prettyDump missing %q in:\n%s", want, out)
		}
	}
}
