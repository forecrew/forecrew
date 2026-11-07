package jira

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// node is a document in the form Markdown, ADF and wiki markup share. Its
// types and marks are those of ADF, which has the most of them; a type the
// converters do not know keeps its text.
type node struct {
	Type    string         `json:"type"`
	Text    string         `json:"text,omitempty"`
	Attrs   map[string]any `json:"attrs,omitempty"`
	Marks   []mark         `json:"marks,omitempty"`
	Content []*node        `json:"content,omitempty"`
}

// mark is a style of a text node: strong, em, code, strike or link.
type mark struct {
	Type  string         `json:"type"`
	Attrs map[string]any `json:"attrs,omitempty"`
}

// Node types.
const (
	tDoc        = "doc"
	tParagraph  = "paragraph"
	tHeading    = "heading"
	tBullet     = "bulletList"
	tOrdered    = "orderedList"
	tItem       = "listItem"
	tCode       = "codeBlock"
	tQuote      = "blockquote"
	tRule       = "rule"
	tText       = "text"
	tBreak      = "hardBreak"
	tMention    = "mention"
	tTable      = "table"
	tRow        = "tableRow"
	tCell       = "tableCell"
	tHeaderCell = "tableHeader"
)

// Mark types, in the order they nest from the outside in.
const (
	mLink   = "link"
	mStrike = "strike"
	mStrong = "strong"
	mEm     = "em"
	mCode   = "code"
)

var markOrder = []string{mLink, mStrike, mStrong, mEm, mCode}

func (n *node) attr(key string) string {
	switch v := n.Attrs[key].(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case int:
		return strconv.Itoa(v)
	}
	return ""
}

func (m mark) href() string {
	s, _ := m.Attrs["href"].(string)
	return s
}

func hasMark(marks []mark, t string) (mark, bool) {
	for _, m := range marks {
		if m.Type == t {
			return m, true
		}
	}
	return mark{}, false
}

// plain returns the text of a node and everything in it.
func plain(n *node) string {
	var b strings.Builder
	var walk func(*node)
	walk = func(n *node) {
		b.WriteString(n.Text)
		if n.Type == tMention {
			b.WriteString(mentionText(n))
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

func mentionText(n *node) string {
	t := n.attr("text")
	if t == "" {
		t = n.attr("id")
	}
	if t != "" && !strings.HasPrefix(t, "@") {
		t = "@" + t
	}
	return t
}

// ---- Markdown out

// toMarkdown renders a document as Markdown.
func toMarkdown(doc *node) string {
	return strings.TrimSpace(mdBlocks(doc.Content))
}

func mdBlocks(blocks []*node) string {
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if s := mdBlock(b); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "\n\n")
}

func indent(s, first, rest string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		switch {
		case i == 0:
			lines[i] = first + lines[i]
		case lines[i] != "":
			lines[i] = rest + lines[i]
		}
	}
	return strings.Join(lines, "\n")
}

func mdBlock(n *node) string {
	switch n.Type {
	case tParagraph:
		return mdInline(n.Content)
	case tHeading:
		level := max(1, min(6, atoi(n.attr("level"))))
		return strings.Repeat("#", level) + " " + mdInline(n.Content)
	case tBullet, tOrdered:
		items := make([]string, 0, len(n.Content))
		start := max(1, atoi(n.attr("order")))
		for i, item := range n.Content {
			marker := "- "
			if n.Type == tOrdered {
				marker = strconv.Itoa(start+i) + ". "
			}
			body := mdBlocks(item.Content)
			if item.Type != tItem {
				body = mdBlock(item)
			}
			items = append(items, indent(strings.ReplaceAll(body, "\n\n", "\n"), marker, strings.Repeat(" ", len(marker))))
		}
		return strings.Join(items, "\n")
	case tCode:
		return "```" + n.attr("language") + "\n" + plain(n) + "\n```"
	case tQuote:
		return indent(mdBlocks(n.Content), "> ", "> ")
	case tRule:
		return "---"
	case tTable:
		rows := make([]string, 0, len(n.Content))
		for i, row := range n.Content {
			cells := make([]string, 0, len(row.Content))
			for _, c := range row.Content {
				cells = append(cells, strings.ReplaceAll(mdBlocks(c.Content), "\n", " "))
			}
			rows = append(rows, "| "+strings.Join(cells, " | ")+" |")
			if i == 0 {
				rows = append(rows, "|"+strings.Repeat(" --- |", len(cells)))
			}
		}
		return strings.Join(rows, "\n")
	case tText, tBreak, tMention:
		return mdInline([]*node{n})
	}
	return unknownBlock(n, mdBlocks, mdInline)
}

// unknownBlock keeps the text of a node the converters do not know: its
// blocks, its inline content, its text, or at least its type.
func unknownBlock(n *node, blocks, inline func([]*node) string) string {
	switch {
	case len(n.Content) > 0 && isBlock(n.Content[0]):
		return blocks(n.Content)
	case len(n.Content) > 0:
		return inline(n.Content)
	case n.Text != "":
		return inline([]*node{{Type: tText, Text: n.Text}})
	}
	return unknownLeaf(n)
}

func unknownLeaf(n *node) string {
	for _, k := range []string{"text", "url", "shortName", "alt", "id"} {
		if v := n.attr(k); v != "" {
			return v
		}
	}
	return "[" + n.Type + "]"
}

func isBlock(n *node) bool {
	switch n.Type {
	case tText, tBreak, tMention, "emoji", "inlineCard", "date", "status", "media", "mediaInline":
		return false
	}
	return true
}

// mdEscape escapes the characters the Markdown parser would read as marks.
// An underscore only marks at a word's edge, so snake_case stays as it is,
// and a star between spaces is a star.
func mdEscape(s string) string {
	var b strings.Builder
	for i, r := range s {
		switch r {
		case '\\', '`', '[':
			b.WriteByte('\\')
		case '*':
			if nextRune(s, i+1) != ' ' || i == 0 || s[i-1] != ' ' {
				b.WriteByte('\\')
			}
		case '~':
			if strings.HasPrefix(s[i:], "~~") {
				b.WriteByte('\\')
			}
		case '_':
			if !wordAt(s, i-1, true) || !wordAt(s, i+1, false) {
				b.WriteByte('\\')
			}
		}
		b.WriteRune(r)
	}
	return b.String()
}

// wordAt reports whether the rune that ends (before) or starts at i is a
// letter or digit.
func wordAt(s string, i int, before bool) bool {
	if i < 0 || i >= len(s) {
		return false
	}
	var r rune
	if before {
		r, _ = utf8.DecodeLastRuneInString(s[:i+1])
	} else {
		r, _ = utf8.DecodeRuneInString(s[i:])
	}
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

func mdInline(nodes []*node) string {
	var b strings.Builder
	for _, n := range nodes {
		switch n.Type {
		case tText:
			b.WriteString(mdText(n))
		case tBreak:
			b.WriteString("\n")
		case tMention:
			b.WriteString(mdEscape(mentionText(n)))
		case "inlineCard":
			b.WriteString(n.attr("url"))
		default:
			if len(n.Content) > 0 {
				b.WriteString(mdInline(n.Content))
			} else {
				b.WriteString(mdEscape(unknownLeaf(n)))
			}
		}
	}
	return b.String()
}

func mdText(n *node) string {
	s := n.Text
	if _, ok := hasMark(n.Marks, mCode); ok {
		fence := "`"
		for strings.Contains(s, fence) {
			fence += "`"
		}
		s = fence + s + fence
	} else {
		s = mdEscape(s)
	}
	for _, t := range slicesReverse(markOrder) {
		m, ok := hasMark(n.Marks, t)
		if !ok {
			continue
		}
		switch t {
		case mEm:
			s = "*" + s + "*"
		case mStrong:
			s = "**" + s + "**"
		case mStrike:
			s = "~~" + s + "~~"
		case mLink:
			s = "[" + s + "](" + m.href() + ")"
		}
	}
	return s
}

func slicesReverse(in []string) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[len(in)-1-i] = v
	}
	return out
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// ---- Markdown in

var (
	mdHeading = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	mdRule    = regexp.MustCompile(`^(?:-{3,}|\*{3,}|_{3,})\s*$`)
	mdItem    = regexp.MustCompile(`^(\s*)([-*+]|\d{1,9}[.)])\s+(.*)$`)
	mdFence   = regexp.MustCompile("^(```+|~~~+)\\s*([\\w+#.-]*)\\s*$")
)

// fromMarkdown parses the Markdown Forecrew writes: paragraphs, headings,
// lists, code, quotes, rules and the common inline marks. A line break in a
// paragraph is a hard break, as people mean it in a ticket.
func fromMarkdown(s string) *node {
	lines := strings.Split(strings.ReplaceAll(strings.TrimSpace(s), "\r\n", "\n"), "\n")
	return &node{Type: tDoc, Attrs: map[string]any{"version": 1}, Content: mdParseBlocks(lines)}
}

func mdParseBlocks(lines []string) []*node {
	var out []*node
	for i := 0; i < len(lines); {
		var n *node
		if n, i = mdBlockAt(lines, i); n != nil {
			out = append(out, n)
		}
	}
	return out
}

// mdBlockAt parses the block that starts at lines[i], and returns it and the
// line after it.
func mdBlockAt(lines []string, i int) (*node, int) {
	trimmed := strings.TrimSpace(lines[i])
	switch {
	case trimmed == "":
		return nil, i + 1
	case mdFence.MatchString(trimmed):
		return mdCodeBlock(lines, i)
	case mdHeading.MatchString(trimmed):
		m := mdHeading.FindStringSubmatch(trimmed)
		return &node{Type: tHeading, Attrs: map[string]any{"level": len(m[1])}, Content: mdParseInline(m[2])}, i + 1
	case mdRule.MatchString(trimmed):
		return &node{Type: tRule}, i + 1
	case strings.HasPrefix(trimmed, ">"):
		var quoted []string
		for ; i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), ">"); i++ {
			q := strings.TrimPrefix(strings.TrimSpace(lines[i]), ">")
			quoted = append(quoted, strings.TrimPrefix(q, " "))
		}
		return &node{Type: tQuote, Content: mdParseBlocks(quoted)}, i
	case mdItem.MatchString(lines[i]):
		return mdParseList(lines, i)
	}
	return paragraph(lines, i, startsBlock, mdParseInline)
}

// paragraph reads lines up to a blank one or the start of another block. A
// line that looks like a block and is not one is a paragraph of its own.
func paragraph(lines []string, i int, starts func(string) bool, inline func(string) []*node) (*node, int) {
	var para []string
	for ; i < len(lines) && strings.TrimSpace(lines[i]) != "" && !starts(lines[i]); i++ {
		para = append(para, strings.TrimSpace(lines[i]))
	}
	if len(para) == 0 {
		para, i = append(para, strings.TrimSpace(lines[i])), i+1
	}
	return &node{Type: tParagraph, Content: inline(strings.Join(para, "\n"))}, i
}

func mdCodeBlock(lines []string, i int) (*node, int) {
	m := mdFence.FindStringSubmatch(strings.TrimSpace(lines[i]))
	j := i + 1
	for j < len(lines) && strings.TrimSpace(lines[j]) != m[1] {
		j++
	}
	code := &node{Type: tCode}
	if text := strings.Join(lines[i+1:min(j, len(lines))], "\n"); text != "" {
		code.Content = []*node{{Type: tText, Text: text}}
	}
	if m[2] != "" {
		code.Attrs = map[string]any{"language": m[2]}
	}
	return code, j + 1
}

func startsBlock(line string) bool {
	t := strings.TrimSpace(line)
	return mdFence.MatchString(t) || mdHeading.MatchString(t) || mdRule.MatchString(t) || strings.HasPrefix(t, ">") || mdItem.MatchString(line)
}

// mdParseList reads a list from lines[i]: its items and what is indented
// under them.
func mdParseList(lines []string, i int) (*node, int) {
	m := mdItem.FindStringSubmatch(lines[i])
	base := len(m[1])
	ordered := m[2] != "-" && m[2] != "*" && m[2] != "+"
	list := &node{Type: tBullet}
	if ordered {
		list.Type = tOrdered
		if n := atoi(strings.TrimRight(m[2], ".)")); n > 1 {
			list.Attrs = map[string]any{"order": n}
		}
	}
	for i < len(lines) {
		m := mdItem.FindStringSubmatch(lines[i])
		if m == nil || len(m[1]) != base || (m[2] != "-" && m[2] != "*" && m[2] != "+") != ordered {
			break
		}
		width := len(m[1]) + len(m[2]) + 1
		body := []string{m[3]}
		i++
		for i < len(lines) {
			l := lines[i]
			lead := len(l) - len(strings.TrimLeft(l, " "))
			if strings.TrimSpace(l) == "" || lead < width {
				break
			}
			body = append(body, l[min(width, lead):])
			i++
		}
		list.Content = append(list.Content, &node{Type: tItem, Content: mdParseBlocks(body)})
	}
	return list, i
}

// mdParseInline parses inline Markdown.
func mdParseInline(s string) []*node {
	p := inlineParser{}
	p.parse(s, nil)
	return p.out
}

type inlineParser struct {
	out []*node
}

// add appends text with marks, joining it to the text before when the marks
// are the same.
func (p *inlineParser) add(text string, marks []mark) {
	if text == "" {
		return
	}
	if n := len(p.out); n > 0 && p.out[n-1].Type == tText && sameMarks(p.out[n-1].Marks, marks) {
		p.out[n-1].Text += text
		return
	}
	p.out = append(p.out, &node{Type: tText, Text: text, Marks: cloneMarks(marks)})
}

func cloneMarks(m []mark) []mark {
	if len(m) == 0 {
		return nil
	}
	return append([]mark(nil), m...)
}

func sameMarks(a, b []mark) bool {
	if len(a) != len(b) {
		return false
	}
	for _, x := range a {
		y, ok := hasMark(b, x.Type)
		if !ok || x.href() != y.href() {
			return false
		}
	}
	return true
}

func with(marks []mark, m mark) []mark {
	return append(cloneMarks(marks), m)
}

// closing finds the end of a span that starts at i: the next delim not
// escaped, with text in between.
func closing(s string, i int, delim string) int {
	for j := i; j < len(s); j++ {
		if s[j] == '\\' {
			j++
			continue
		}
		if strings.HasPrefix(s[j:], delim) && j > i {
			return j
		}
	}
	return -1
}

func (p *inlineParser) parse(s string, marks []mark) {
	var text strings.Builder
	for i := 0; i < len(s); {
		if next, ok := p.mdSpan(s, i, marks, &text); ok {
			i = next
			continue
		}
		text.WriteByte(s[i])
		i++
	}
	p.add(text.String(), marks)
}

// flush adds the text read so far.
func (p *inlineParser) flush(text *strings.Builder, marks []mark) {
	p.add(text.String(), marks)
	text.Reset()
}

// escaped reads a backslash escape at s[i] into text.
func escaped(s string, i int, text *strings.Builder) int {
	_, size := utf8.DecodeRuneInString(s[i+1:])
	text.WriteString(s[i+1 : i+1+size])
	return i + 1 + size
}

// mdSpan reads what starts at s[i]: an escape, a break, code or a marked
// span. ok is false for plain text.
func (p *inlineParser) mdSpan(s string, i int, marks []mark, text *strings.Builder) (int, bool) {
	switch c := s[i]; {
	case c == '\\' && i+1 < len(s) && s[i+1] != '\n':
		return escaped(s, i, text), true
	case c == '\n':
		p.flush(text, marks)
		p.out = append(p.out, &node{Type: tBreak})
		return i + 1, true
	case c == '`':
		fence := s[i : i+len(s[i:])-len(strings.TrimLeft(s[i:], "`"))]
		j := strings.Index(s[i+len(fence):], fence)
		if j < 0 {
			return 0, false
		}
		p.flush(text, marks)
		p.add(s[i+len(fence):i+len(fence)+j], with(marks, mark{Type: mCode}))
		return i + 2*len(fence) + j, true
	}
	inner, m, end, ok := mdMarked(s, i)
	if !ok {
		return 0, false
	}
	p.flush(text, marks)
	p.parse(inner, with(marks, m))
	return end, true
}

// mdMarked reads a strong, strike, em or link span at s[i].
func mdMarked(s string, i int) (string, mark, int, bool) {
	c := s[i]
	switch {
	case strings.HasPrefix(s[i:], "**"), strings.HasPrefix(s[i:], "~~"):
		t := mStrong
		if c == '~' {
			t = mStrike
		}
		if j := closing(s, i+2, s[i:i+2]); j >= 0 {
			return s[i+2 : j], mark{Type: t}, j + 2, true
		}
	case c == '*' || (c == '_' && !wordAt(s, i-1, true)):
		if j := closing(s, i+1, string(c)); j >= 0 && (c == '*' || !wordAt(s, j+1, false)) && s[i+1] != ' ' {
			return s[i+1 : j], mark{Type: mEm}, j + 1, true
		}
	case c == '[':
		end := closing(s, i+1, "]")
		if end < 0 || !strings.HasPrefix(s[end:], "](") {
			break
		}
		if cl := strings.IndexByte(s[end+2:], ')'); cl >= 0 {
			return s[i+1 : end], mark{Type: mLink, Attrs: map[string]any{"href": s[end+2 : end+2+cl]}}, end + 3 + cl, true
		}
	}
	return "", mark{}, 0, false
}
