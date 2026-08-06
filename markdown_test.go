package main

import (
	"strings"
	"testing"
)

// renderXHTML renders a markdown doc to the concatenated XHTML-IM of its blocks.
func renderXHTML(t *testing.T, md string) string {
	t.Helper()
	var b strings.Builder
	for _, bl := range parseMD(md) {
		b.WriteString(bl.xhtml())
	}
	return b.String()
}

func renderPlain(md string) string {
	var b strings.Builder
	for _, bl := range parseMD(md) {
		b.WriteString(bl.plain())
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

func TestMDParagraph(t *testing.T) {
	if got := renderXHTML(t, "hello world"); got != "<p>hello world</p>" {
		t.Errorf("paragraph = %q, want <p>hello world</p>", got)
	}
}

func TestMDHeading(t *testing.T) {
	if got := renderXHTML(t, "# Title"); got != `<p style="font-weight:bold">Title</p>` {
		t.Errorf("heading = %q", got)
	}
	if got := renderXHTML(t, "### deep"); got != `<p style="font-weight:bold">deep</p>` {
		t.Errorf("h3 = %q", got)
	}
}

func TestMDBoldItalicCode(t *testing.T) {
	got := renderXHTML(t, "a **bold** *it* `code`")
	for _, want := range []string{
		"<strong>bold</strong>",
		"<em>it</em>",
		`<span style="font-family:monospace">code</span>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("inline = %q, missing %q", got, want)
		}
	}
}

func TestMDUnderscoreNotEmphasis(t *testing.T) {
	got := renderXHTML(t, "use snake_case here")
	if strings.Contains(got, "<em>") {
		t.Errorf("snake_case should not be emphasized: %q", got)
	}
}

func TestMDCodeBlock(t *testing.T) {
	md := "```go\nfunc main() {}\n```"
	got := renderXHTML(t, md)
	want := `<p style="font-family:monospace">func main() {}</p>`
	if got != want {
		t.Errorf("code block = %q, want %q", got, want)
	}
}

func TestMDCodeBlockPreservesIndent(t *testing.T) {
	md := "```\n    indented\n        deep\n```"
	got := renderXHTML(t, md)
	if !strings.Contains(got, "\u00a0\u00a0\u00a0\u00a0indented") {
		t.Errorf("leading spaces must survive as NBSP: %q", got)
	}
	if !strings.Contains(got, "\u00a0\u00a0\u00a0\u00a0\u00a0\u00a0\u00a0\u00a0deep") {
		t.Errorf("deep indent must survive as NBSP: %q", got)
	}
	if strings.Contains(got, "&nbsp;") {
		t.Errorf("XHTML-IM forbids the &nbsp; entity: %q", got)
	}
	if !strings.Contains(got, "<br/>") {
		t.Errorf("code lines should join with <br/>: %q", got)
	}
}

func TestMDList(t *testing.T) {
	got := renderXHTML(t, "- a\n- b")
	if got != "<ul><li>a</li><li>b</li></ul>" {
		t.Errorf("list = %q", got)
	}
}

func TestMDOrderedList(t *testing.T) {
	got := renderXHTML(t, "1. first\n2. second")
	if got != "<ol><li>first</li><li>second</li></ol>" {
		t.Errorf("ordered list = %q", got)
	}
}

func TestMDNestedList(t *testing.T) {
	got := renderXHTML(t, "- a\n  - b\n    - c")
	if !strings.Contains(got, "<ul><li>a<ul><li>b<ul><li>c</li></ul></li></ul></li></ul>") {
		t.Errorf("nested list = %q", got)
	}
}

func TestMDLink(t *testing.T) {
	got := renderXHTML(t, "see [the docs](https://x.dev)")
	if !strings.Contains(got, `<a href="https://x.dev">the docs</a>`) {
		t.Errorf("link = %q", got)
	}
}

func TestMDUnsafeLinkIsInert(t *testing.T) {
	got := renderXHTML(t, "[x](javascript:alert(1))")
	if strings.Contains(got, "<a href=") {
		t.Errorf("unsafe link must not become a link: %q", got)
	}
	if !strings.Contains(got, "javascript") {
		t.Errorf("unsafe URL text should still be present: %q", got)
	}
}

func TestMDBareURL(t *testing.T) {
	got := renderXHTML(t, "see https://example.com/a?b=1 now")
	if !strings.Contains(got, `<a href="https://example.com/a?b=1">https://example.com/a?b=1</a>`) {
		t.Errorf("bare url = %q", got)
	}
}

func TestMDAngleAutolink(t *testing.T) {
	got := renderXHTML(t, "<https://example.com>")
	if !strings.Contains(got, `<a href="https://example.com">`) {
		t.Errorf("angle autolink = %q", got)
	}
}

func TestMDBlockquote(t *testing.T) {
	got := renderXHTML(t, "> quoted line")
	if got != "<blockquote><p>quoted line</p></blockquote>" {
		t.Errorf("blockquote = %q", got)
	}
}

func TestMDEscaping(t *testing.T) {
	got := renderXHTML(t, "a < b & c > d")
	want := "<p>a &lt; b &amp; c &gt; d</p>"
	if got != want {
		t.Errorf("escaping = %q, want %q", got, want)
	}
}

func TestMDRawHTMLIsLiteral(t *testing.T) {
	got := renderXHTML(t, "raw <b>bold</b> here")
	if strings.Contains(got, "<b>") {
		t.Errorf("raw HTML must not pass through: %q", got)
	}
	if !strings.Contains(got, "&lt;b&gt;bold&lt;/b&gt;") {
		t.Errorf("raw HTML should render as escaped text: %q", got)
	}
}

func TestMDTable(t *testing.T) {
	md := "| name | value |\n| --- | ---: |\n| a | 1 |\n| b | 2 |"
	got := renderXHTML(t, md)
	want := `<p style="font-family:monospace">name | value<br/>a | 1<br/>b | 2</p>`
	if got != want {
		t.Errorf("table = %q, want %q", got, want)
	}
}

func TestMDHRIsSkipped(t *testing.T) {
	got := renderXHTML(t, "---\n\nnext")
	if strings.Contains(got, "<hr") {
		t.Errorf("hr must not be emitted: %q", got)
	}
}

func TestMDPlainFallback(t *testing.T) {
	cases := []struct{ md, want string }{
		{"**bold** text", "bold text"},
		{"# Head", "Head"},
		{"- a\n- b", "• a\n• b"},
		{"1. a\n2. b", "1. a\n2. b"},
		{"`code`", "code"},
		{"> quoted", "> quoted"},
		{"```\nline1\nline2\n```", "line1\nline2"},
		{"[x](https://x.dev)", "x (https://x.dev)"},
	}
	for _, c := range cases {
		if got := renderPlain(c.md); got != c.want {
			t.Errorf("plain(%q) = %q, want %q", c.md, got, c.want)
		}
	}
}

// balancedTags reports whether the string's XHTML tags open and close evenly.
func balancedTags(s string) bool {
	open := 0
	for i := 0; i < len(s); i++ {
		if s[i] != '<' {
			continue
		}
		if strings.HasPrefix(s[i:], "<br/>") {
			continue
		}
		if strings.HasPrefix(s[i:], "<!--") {
			if j := strings.Index(s[i:], "-->"); j < 0 {
				return false
			}
			continue
		}
		j := strings.IndexByte(s[i:], '>')
		if j < 0 {
			return false
		}
		tag := s[i : i+j+1]
		if strings.HasPrefix(tag, "</") {
			open--
		} else if !strings.HasSuffix(tag, "/>") && !strings.HasPrefix(tag, "<!") {
			open++
		}
	}
	return open == 0
}

func TestRenderRichMessageChunks(t *testing.T) {
	// Enough paragraphs to force multiple chunks.
	para := "The quick brown fox jumps over the lazy dog and keeps on going."
	var md strings.Builder
	for i := 0; i < 300; i++ {
		md.WriteString(para)
		md.WriteString("\n\n")
	}
	chunks := renderRichMessage(md.String())
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if len(c.xhtml) > maxXHTMLIM {
			t.Errorf("chunk %d xhtml length %d exceeds cap %d", i, len(c.xhtml), maxXHTMLIM)
		}
		if !balancedTags(c.xhtml) {
			t.Errorf("chunk %d has unbalanced tags:\n%s", i, c.xhtml)
		}
		if strings.Contains(c.xhtml, "<html") || strings.Contains(c.xhtml, "<body") {
			t.Errorf("chunk %d must not contain wrapper elements:\n%s", i, c.xhtml)
		}
		if strings.TrimSpace(c.plain) == "" {
			t.Errorf("chunk %d has empty plain fallback", i)
		}
	}
}

func TestRenderRichMessageShortSingleChunk(t *testing.T) {
	chunks := renderRichMessage("**hi** there")
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	if chunks[0].plain != "hi there" {
		t.Errorf("plain = %q, want 'hi there'", chunks[0].plain)
	}
	if !strings.Contains(chunks[0].xhtml, "<strong>hi</strong>") {
		t.Errorf("xhtml = %q, want strong", chunks[0].xhtml)
	}
}
