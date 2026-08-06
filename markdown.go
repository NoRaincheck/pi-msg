package main

import (
	"strconv"
	"strings"
)

// maxXHTMLIM caps how much converted XHTML goes into a single message stanza.
// Blocks are grouped at block boundaries so each message is a self-contained
// XHTML-IM <body>; staying far below maxBody means the transport's byte
// splitter never has to cut into an XHTML element.
const maxXHTMLIM = 4000

// mdBlock is one parsed markdown block. plain() is the markup-stripped text
// for the XMPP <body> fallback; xhtml() is the XEP-0071 integration-set markup
// (already XML-escaped, no &nbsp; — real U+00A0 only).
type mdBlock interface {
	plain() string
	xhtml() string
}

// --- inline rendering ---

// inlineSpecials are the bytes that trigger inline handling; everything else
// is literal text copied verbatim (so multi-byte UTF-8 passes through intact).
const inlineSpecials = `\` + "`" + `*_[]<>&`

// renderInline converts one line's inline markup to either plain text or
// XHTML-IM markup, appended to b.
func renderInline(s string, b *strings.Builder, plain bool) {
	writeText := func(t string) {
		if plain {
			b.WriteString(t)
		} else {
			b.WriteString(escText(t))
		}
	}
	i := 0
	for i < len(s) {
		j := strings.IndexAny(s[i:], inlineSpecials)
		spec := len(s)
		if j >= 0 {
			spec = i + j
		}
		// A bare URL anywhere before the next special byte beats a literal run.
		if u := nextURLIndex(s, i); u >= 0 && u < spec {
			if u > i {
				writeText(s[i:u])
			}
			if url, n := matchBareURL(s[u:]); n > 0 {
				if plain {
					b.WriteString(url)
				} else {
					b.WriteString(`<a href="` + escAttr(url) + `">` + escText(url) + `</a>`)
				}
				i = u + n
			} else {
				writeText(s[u : u+1])
				i = u + 1
			}
			continue
		}
		if j < 0 {
			writeText(s[i:])
			return
		}
		if spec > i {
			writeText(s[i:spec])
			i = spec
		}
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s):
			// Backslash escape: emit the next character literally.
			writeText(s[i+1 : i+2])
			i += 2
		case c == '`':
			// Inline code span.
			if k := strings.IndexByte(s[i+1:], '`'); k >= 0 {
				code := s[i+1 : i+1+k]
				if plain {
					b.WriteString(code)
				} else {
					b.WriteString(`<span style="font-family:monospace">` + escText(code) + `</span>`)
				}
				i += k + 2
			} else {
				writeText("`")
				i++
			}
		case strings.HasPrefix(s[i:], "**") || strings.HasPrefix(s[i:], "__"):
			delim := s[i : i+2]
			if k := strings.Index(s[i+2:], delim); k >= 0 {
				inner := s[i+2 : i+2+k]
				if plain {
					renderInline(inner, b, true)
				} else {
					b.WriteString("<strong>")
					renderInline(inner, b, false)
					b.WriteString("</strong>")
				}
				i += k + 4
			} else {
				writeText(delim)
				i += 2
			}
		case c == '*' || c == '_':
			// Emphasis. Underscores only count when they delimit words
			// (so snake_case survives); asterisks are unconditional.
			if c == '_' && i > 0 && isWordByte(s[i-1]) {
				writeText("_")
				i++
				continue
			}
			if k := strings.IndexByte(s[i+1:], c); k >= 0 {
				if c == '_' && i+1+k+1 < len(s) && isWordByte(s[i+1+k+1]) {
					writeText("_")
					i++
					continue
				}
				inner := s[i+1 : i+1+k]
				if plain {
					renderInline(inner, b, true)
				} else {
					b.WriteString("<em>")
					renderInline(inner, b, false)
					b.WriteString("</em>")
				}
				i += k + 2
			} else {
				writeText(string(c))
				i++
			}
		case c == '[':
			// Link [text](url).
			if k := strings.IndexByte(s[i+1:], ']'); k >= 0 {
				text := s[i+1 : i+1+k]
				rest := s[i+1+k+1:]
				if strings.HasPrefix(rest, "(") {
					if end := strings.IndexByte(rest[1:], ')'); end >= 0 {
						url := rest[1 : 1+end]
						if safeURL(url) {
							if plain {
								renderInline(text, b, true)
								b.WriteString(" (")
								b.WriteString(url)
								b.WriteString(")")
							} else {
								b.WriteString(`<a href="` + escAttr(url) + `">`)
								renderInline(text, b, false)
								b.WriteString(`</a>`)
							}
						} else {
							// Unsafe scheme: keep it as inert text.
							renderInline(text, b, plain)
							b.WriteString(" ")
							writeText(url)
						}
						i += 1 + k + 1 + 1 + end + 1
						continue
					}
				}
			}
			writeText("[")
			i++
		case c == '<':
			// <url> autolink, else a literal (possibly hostile) tag.
			if u, n := matchAngleAutolink(s[i:]); n > 0 {
				if plain {
					b.WriteString(u)
				} else {
					b.WriteString(`<a href="` + escAttr(u) + `">` + escText(u) + `</a>`)
				}
				i += n
			} else {
				writeText("<")
				i++
			}
		case c == '>':
			writeText(">")
			i++
		case c == '&':
			writeText("&")
			i++
		default:
			writeText(string(c))
			i++
		}
	}
}

// isWordByte reports whether b is a letter, digit, or underscore.
func isWordByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_'
}

// safeURL accepts only schemes an IM client can hand off without scripting
// risk. Everything else (javascript:, data:, vbscript:, …) is treated as text.
func safeURL(u string) bool {
	low := strings.ToLower(u)
	return strings.HasPrefix(low, "http://") ||
		strings.HasPrefix(low, "https://") ||
		strings.HasPrefix(low, "mailto:")
}

// nextURLIndex returns the earliest index >= from at which a bare-URL scheme
// begins, or -1 if none. Used so URLs are linked even when no inline special
// byte follows them.
func nextURLIndex(s string, from int) int {
	low := strings.ToLower(s)
	best := -1
	for _, pat := range []string{"https://", "http://", "mailto:"} {
		if k := strings.Index(low[from:], pat); k >= 0 {
			k += from
			if best < 0 || k < best {
				best = k
			}
		}
	}
	return best
}

// matchBareURL reports whether s starts with a bare URL and returns the URL
// and the number of bytes it occupies (trailing punctuation trimmed).
func matchBareURL(s string) (string, int) {
	low := strings.ToLower(s)
	if !strings.HasPrefix(low, "http://") && !strings.HasPrefix(low, "https://") && !strings.HasPrefix(low, "mailto:") {
		return "", 0
	}
	end := len(s)
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\n', '\r', '<', '>', '"', '\'', '(', ')', '[', ']':
			end = i
			i = len(s)
		}
	}
	u := strings.TrimRight(s[:end], ".,;:!?")
	if u == "" || !safeURL(u) {
		return "", 0
	}
	return u, len(u)
}

// matchAngleAutolink handles <http://…> syntax; any other <…> (e.g. raw HTML)
// returns 0 so the caller renders it as literal escaped text.
func matchAngleAutolink(s string) (string, int) {
	if !strings.HasPrefix(s, "<") {
		return "", 0
	}
	if j := strings.IndexByte(s, '>'); j > 0 {
		u := s[1:j]
		if safeURL(u) {
			return u, j + 1
		}
	}
	return "", 0
}

// --- text helpers ---

// escText escapes the characters that are special in XML character data
// (the only entities XEP-0071 permits).
func escText(s string) string {
	if !strings.ContainsAny(s, "&<>") {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// escAttr escapes text for use inside an XML attribute value.
func escAttr(s string) string {
	return strings.ReplaceAll(escText(s), `"`, "&quot;")
}

// codeLineHTML escapes a code line and converts leading whitespace to NBSPs so
// indentation survives a client's whitespace collapsing (XHTML-IM forbids the
// &nbsp; entity; only the real U+00A0 character is allowed).
func codeLineHTML(ln string) string {
	trimmed := strings.TrimLeft(ln, " \t")
	n := len(ln) - len(trimmed)
	if n == 0 {
		return escText(ln)
	}
	return strings.Repeat("\u00a0", n) + escText(trimmed)
}

// --- blocks ---

type paragraphBlock struct{ text string }

func (p paragraphBlock) plain() string {
	var b strings.Builder
	renderInline(p.text, &b, true)
	return b.String()
}

func (p paragraphBlock) xhtml() string {
	var b strings.Builder
	b.WriteString("<p>")
	renderInline(p.text, &b, false)
	b.WriteString("</p>")
	return b.String()
}

type headingBlock struct {
	level int
	text  string
}

func (h headingBlock) plain() string {
	var b strings.Builder
	renderInline(h.text, &b, true)
	return b.String()
}

func (h headingBlock) xhtml() string {
	var b strings.Builder
	b.WriteString(`<p style="font-weight:bold">`)
	renderInline(h.text, &b, false)
	b.WriteString("</p>")
	return b.String()
}

type codeBlock struct{ lines []string }

func (c codeBlock) plain() string { return strings.Join(c.lines, "\n") }

func (c codeBlock) xhtml() string {
	var b strings.Builder
	b.WriteString(`<p style="font-family:monospace">`)
	for i, ln := range c.lines {
		if i > 0 {
			b.WriteString("<br/>")
		}
		b.WriteString(codeLineHTML(ln))
	}
	b.WriteString("</p>")
	return b.String()
}

type listItem struct {
	text string // first-line inline content
	cont string // continuation inline content
	sub  []mdBlock
}

type listBlock struct {
	ordered bool
	items   []listItem
}

func (l listBlock) plain() string {
	var b strings.Builder
	for i, it := range l.items {
		prefix := "• "
		if l.ordered {
			prefix = strconv.Itoa(i+1) + ". "
		}
		var sb strings.Builder
		if it.text != "" {
			renderInline(it.text, &sb, true)
		}
		if it.cont != "" {
			if sb.Len() > 0 {
				sb.WriteString(" ")
			}
			renderInline(it.cont, &sb, true)
		}
		b.WriteString(prefix)
		b.WriteString(sb.String())
		for _, s := range it.sub {
			b.WriteString("\n  ")
			b.WriteString(strings.ReplaceAll(s.plain(), "\n", "\n  "))
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func (l listBlock) xhtml() string {
	open, close := "<ul>", "</ul>"
	if l.ordered {
		open, close = "<ol>", "</ol>"
	}
	var b strings.Builder
	b.WriteString(open)
	for _, it := range l.items {
		b.WriteString("<li>")
		var sb strings.Builder
		if it.text != "" {
			renderInline(it.text, &sb, false)
		}
		if it.cont != "" {
			if sb.Len() > 0 {
				sb.WriteString(" ")
			}
			renderInline(it.cont, &sb, false)
		}
		b.WriteString(sb.String())
		for _, s := range it.sub {
			b.WriteString(s.xhtml())
		}
		b.WriteString("</li>")
	}
	b.WriteString(close)
	return b.String()
}

type quoteBlock struct{ blocks []mdBlock }

func (q quoteBlock) plain() string {
	var b strings.Builder
	for i, bl := range q.blocks {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString("> ")
		b.WriteString(strings.ReplaceAll(bl.plain(), "\n", "\n> "))
	}
	return b.String()
}

func (q quoteBlock) xhtml() string {
	var b strings.Builder
	b.WriteString("<blockquote>")
	for _, bl := range q.blocks {
		b.WriteString(bl.xhtml())
	}
	b.WriteString("</blockquote>")
	return b.String()
}

type tableBlock struct{ rows [][]string }

func (t tableBlock) plain() string {
	var b strings.Builder
	for i, r := range t.rows {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(strings.Join(r, " | "))
	}
	return b.String()
}

func (t tableBlock) xhtml() string {
	var b strings.Builder
	b.WriteString(`<p style="font-family:monospace">`)
	for i, r := range t.rows {
		if i > 0 {
			b.WriteString("<br/>")
		}
		for j, cell := range r {
			if j > 0 {
				b.WriteString(" | ")
			}
			b.WriteString(escText(cell))
		}
	}
	b.WriteString("</p>")
	return b.String()
}

// --- block parsing ---

type mdParser struct {
	lines []string
	i     int
}

func (p *mdParser) cur() string { return p.lines[p.i] }
func (p *mdParser) eof() bool   { return p.i >= len(p.lines) }

// parseMD parses markdown into a top-level block list.
func parseMD(text string) []mdBlock {
	p := &mdParser{lines: strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")}
	var out []mdBlock
	for !p.eof() {
		line := p.cur()
		switch {
		case isBlank(line):
			p.i++
		case isFence(line):
			out = append(out, p.parseCodeBlock())
		case isHeading(line):
			out = append(out, p.parseHeading())
		case isHR(line):
			p.i++
		case isTableStart(p):
			out = append(out, p.parseTable())
		case isQuote(line):
			out = append(out, p.parseQuote())
		case isListMarker(line):
			out = append(out, p.parseList())
		default:
			out = append(out, p.parseParagraph())
		}
	}
	return out
}

func (p *mdParser) parseParagraph() mdBlock {
	var parts []string
	for !p.eof() {
		line := p.cur()
		if isBlank(line) || isFence(line) || isHeading(line) || isHR(line) || isQuote(line) || isListMarker(line) || isTableStart(p) {
			break
		}
		parts = append(parts, strings.TrimSpace(line))
		p.i++
	}
	return paragraphBlock{text: strings.Join(parts, " ")}
}

func (p *mdParser) parseHeading() mdBlock {
	line := strings.TrimLeft(p.cur(), " ")
	level := 0
	for level < len(line) && level < 6 && line[level] == '#' {
		level++
	}
	text := strings.TrimSpace(line[level:])
	p.i++
	return headingBlock{level: level, text: text}
}

func (p *mdParser) parseCodeBlock() mdBlock {
	opener := strings.TrimSpace(p.cur())
	fenceChar := opener[0]
	fenceLen := 0
	for fenceLen < len(opener) && opener[fenceLen] == fenceChar {
		fenceLen++
	}
	closing := func(line string) bool {
		t := strings.TrimSpace(line)
		return len(t) >= fenceLen && strings.Trim(t, string(fenceChar)) == ""
	}
	p.i++
	var lines []string
	for !p.eof() && !closing(p.cur()) {
		lines = append(lines, p.cur())
		p.i++
	}
	if !p.eof() {
		p.i++ // consume closing fence
	}
	return codeBlock{lines: lines}
}

func (p *mdParser) parseQuote() mdBlock {
	var inner []string
	for !p.eof() {
		t := strings.TrimLeft(p.cur(), " \t")
		if !strings.HasPrefix(t, ">") {
			break
		}
		rest := strings.TrimPrefix(t, ">")
		inner = append(inner, strings.TrimPrefix(rest, " "))
		p.i++
	}
	return quoteBlock{blocks: parseMD(strings.Join(inner, "\n"))}
}

func (p *mdParser) parseList() mdBlock {
	indent, _, _, _ := listMarker(p.cur())
	return p.parseListAt(indent)
}

func (p *mdParser) parseListAt(base int) mdBlock {
	ordered := false
	var items []listItem
	first := true
	for !p.eof() {
		line := p.cur()
		if isBlank(line) {
			p.i++
			continue
		}
		indent, marker, content, ok := listMarker(line)
		if !ok || indent < base || indent != base {
			break
		}
		isOrd := isOrderedMarker(marker)
		if first {
			ordered = isOrd
			first = false
		} else if isOrd != ordered {
			break
		}
		item := listItem{text: content}
		p.i++
		for !p.eof() {
			l := p.cur()
			if isBlank(l) {
				break
			}
			_, _, _, lok := listMarker(l)
			ind := leadingIndent(l)
			if lok && ind == base {
				break // next item at this level
			}
			if ind <= base {
				break // end of this item's body
			}
			if lok {
				item.sub = append(item.sub, p.parseListAt(ind))
			} else {
				item.cont = strings.TrimSpace(l)
				p.i++
			}
		}
		items = append(items, item)
	}
	return listBlock{ordered: ordered, items: items}
}

func (p *mdParser) parseTable() mdBlock {
	var rows [][]string
	if cells, ok := splitTableRow(p.cur()); ok {
		rows = append(rows, cells)
		p.i++
	}
	if p.i < len(p.lines) && isTableSeparator(p.lines[p.i]) {
		p.i++ // skip the ---|:---: header separator
	}
	for p.i < len(p.lines) {
		line := p.cur()
		if cells, ok := splitTableRow(line); ok && !isTableSeparator(line) {
			rows = append(rows, cells)
			p.i++
		} else {
			break
		}
	}
	return tableBlock{rows: rows}
}

// --- line classification ---

func isBlank(line string) bool { return strings.TrimSpace(line) == "" }

// leadingIndent counts leading whitespace, treating a tab as 4 columns.
func leadingIndent(line string) int {
	n := 0
	for _, r := range line {
		switch r {
		case ' ':
			n++
		case '\t':
			n += 4
		default:
			return n
		}
	}
	return n
}

// listMarker splits a possible list-item line into its indentation, marker
// ("-", "1.", …), and content.
func listMarker(line string) (indent int, marker, content string, ok bool) {
	indent = leadingIndent(line)
	rest := strings.TrimLeft(line, " \t")
	if rest == "" {
		return 0, "", "", false
	}
	if strings.ContainsRune("-+*", rune(rest[0])) {
		if len(rest) >= 2 && (rest[1] == ' ' || rest[1] == '\t') {
			return indent, rest[:1], strings.TrimSpace(rest[1:]), true
		}
		return 0, "", "", false
	}
	j := 0
	for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
		j++
	}
	if j > 0 && j < len(rest) && (rest[j] == '.' || rest[j] == ')') &&
		j+1 < len(rest) && (rest[j+1] == ' ' || rest[j+1] == '\t') {
		return indent, rest[:j+1], strings.TrimSpace(rest[j+1:]), true
	}
	return 0, "", "", false
}

func isListMarker(line string) bool {
	_, _, _, ok := listMarker(line)
	return ok
}

func isOrderedMarker(marker string) bool {
	return marker != "" && marker[0] >= '0' && marker[0] <= '9'
}

func isHeading(line string) bool {
	t := strings.TrimLeft(line, " ")
	h := 0
	for h < len(t) && h < 6 && t[h] == '#' {
		h++
	}
	return h > 0 && h < len(t) && (t[h] == ' ' || t[h] == '\t')
}

func isFence(line string) bool {
	t := strings.TrimSpace(line)
	return strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~")
}

func isHR(line string) bool {
	t := strings.TrimSpace(line)
	if len(t) < 3 {
		return false
	}
	c := t[0]
	if !strings.ContainsRune("-*_", rune(c)) {
		return false
	}
	for i := 0; i < len(t); i++ {
		if t[i] != c {
			return false
		}
	}
	return true
}

func isQuote(line string) bool {
	return strings.HasPrefix(strings.TrimLeft(line, " \t"), ">")
}

// splitTableRow splits a |foo|bar| line into its cells.
func splitTableRow(line string) ([]string, bool) {
	t := strings.TrimSpace(line)
	if !strings.Contains(t, "|") {
		return nil, false
	}
	parts := strings.Split(t, "|")
	cells := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" && len(cells) == 0 {
			continue // leading empty cell from a leading |
		}
		cells = append(cells, p)
	}
	for len(cells) > 0 && cells[len(cells)-1] == "" {
		cells = cells[:len(cells)-1] // trailing empty cell
	}
	if len(cells) == 0 {
		return nil, false
	}
	return cells, true
}

// isTableSeparator reports whether a line is a markdown table separator row.
func isTableSeparator(line string) bool {
	cells, ok := splitTableRow(line)
	if !ok {
		return false
	}
	for _, c := range cells {
		c = strings.TrimSpace(c)
		if c == "" {
			return false
		}
		for _, r := range c {
			if r != '-' && r != ':' {
				return false
			}
		}
	}
	return true
}

// isTableStart reports whether the current line begins a pipe table (a row
// followed by a separator row).
func isTableStart(p *mdParser) bool {
	if _, ok := splitTableRow(p.cur()); !ok {
		return false
	}
	return p.i+1 < len(p.lines) && isTableSeparator(p.lines[p.i+1])
}

// --- message chunking ---

// richChunk is one self-contained XHTML-IM message: the plain-text <body>
// fallback and the escaped XHTML for the <html> wrapper.
type richChunk struct {
	plain string
	xhtml string
}

// renderRichMessage converts markdown text into chunks, each small enough to
// fit in a single message stanza and complete at block boundaries.
func renderRichMessage(text string) []richChunk {
	blocks := parseMD(text)
	var chunks []richChunk
	var plain, xhtml strings.Builder
	flush := func() {
		if xhtml.Len() == 0 {
			return
		}
		chunks = append(chunks, richChunk{
			plain: strings.TrimSpace(plain.String()),
			xhtml: xhtml.String(),
		})
		plain.Reset()
		xhtml.Reset()
	}
	for _, b := range blocks {
		bp := b.plain()
		bx := b.xhtml()
		if xhtml.Len() > 0 && xhtml.Len()+len(bx) > maxXHTMLIM {
			flush()
		}
		if plain.Len() > 0 {
			plain.WriteByte('\n')
		}
		plain.WriteString(bp)
		xhtml.WriteString(bx)
	}
	flush()
	return chunks
}
