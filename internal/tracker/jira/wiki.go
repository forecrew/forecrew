package jira

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Jira Data Center writes text in wiki markup (Jira's text formatting
// notation). wikiToMarkdown and markdownToWiki convert through the same
// nodes as ADF; a macro the converter does not know keeps its text.

var (
	wikiHeading = regexp.MustCompile(`^h([1-6])\.\s*(.*)$`)
	wikiItem    = regexp.MustCompile(`^([*#-]+)\s+(.*)$`)
	wikiRule    = regexp.MustCompile(`^-{4,}\s*$`)
	wikiBlock   = regexp.MustCompile(`^\{(code|noformat|quote|panel)(?::([^}]*))?\}(.*)$`)
	wikiMacro   = regexp.MustCompile(`\{[a-zA-Z]+(?::[^}]*)?\}`)
	wikiTable   = regexp.MustCompile(`^\|`)
)

// wikiToMarkdown converts wiki markup to Markdown.
func wikiToMarkdown(s string) string {
	return toMarkdown(fromWiki(s))
}

// markdownToWiki converts Markdown to wiki markup for Jira Data Center.
func markdownToWiki(md string) string {
	return toWiki(fromMarkdown(md))
}

// fromWiki parses wiki markup.
func fromWiki(s string) *node {
	lines := strings.Split(strings.ReplaceAll(strings.TrimSpace(s), "\r\n", "\n"), "\n")
	return &node{Type: tDoc, Content: wikiParseBlocks(lines)}
}

// wikiEnd finds the line that closes a {tag} block opened on line i, and
// the text of the block.
func wikiEnd(lines []string, i int, tag, first string) (int, []string) {
	closer := "{" + tag + "}"
	if before, _, ok := strings.Cut(first, closer); ok {
		return i, []string{before}
	}
	body := []string{}
	if first != "" {
		body = append(body, first)
	}
	for j := i + 1; j < len(lines); j++ {
		if before, _, ok := strings.Cut(lines[j], closer); ok {
			if before != "" {
				body = append(body, before)
			}
			return j, body
		}
		body = append(body, lines[j])
	}
	return len(lines), body
}

func wikiParseBlocks(lines []string) []*node {
	var out []*node
	for i := 0; i < len(lines); {
		var blocks []*node
		blocks, i = wikiBlockAt(lines, i)
		out = append(out, blocks...)
	}
	return out
}

// wikiBlockAt parses the blocks that start at lines[i]: one, or the content
// of a macro that keeps only its text. It returns the line after them.
func wikiBlockAt(lines []string, i int) ([]*node, int) {
	trimmed := strings.TrimSpace(lines[i])
	switch {
	case trimmed == "":
		return nil, i + 1
	case wikiBlock.MatchString(trimmed):
		return wikiMacroBlock(lines, i)
	case wikiHeading.MatchString(trimmed):
		m := wikiHeading.FindStringSubmatch(trimmed)
		return []*node{{Type: tHeading, Attrs: map[string]any{"level": int(m[1][0] - '0')}, Content: wikiParseInline(m[2])}}, i + 1
	case wikiRule.MatchString(trimmed):
		return []*node{{Type: tRule}}, i + 1
	case strings.HasPrefix(trimmed, "bq. "):
		return []*node{{Type: tQuote, Content: []*node{{Type: tParagraph, Content: wikiParseInline(trimmed[4:])}}}}, i + 1
	case wikiItem.MatchString(trimmed):
		var items []wikiListItem
		for ; i < len(lines) && wikiItem.MatchString(strings.TrimSpace(lines[i])); i++ {
			m := wikiItem.FindStringSubmatch(strings.TrimSpace(lines[i]))
			items = append(items, wikiListItem{markers: strings.ReplaceAll(m[1], "-", "*"), text: m[2]})
		}
		return wikiList(items, 1), i
	case wikiTable.MatchString(trimmed):
		table := &node{Type: tTable}
		for ; i < len(lines) && wikiTable.MatchString(strings.TrimSpace(lines[i])); i++ {
			table.Content = append(table.Content, wikiRow(strings.TrimSpace(lines[i])))
		}
		return []*node{table}, i
	}
	p, next := paragraph(lines, i, func(l string) bool { return wikiStartsBlock(strings.TrimSpace(l)) }, wikiParseInline)
	return []*node{p}, next
}

// wikiMacroBlock reads {code}, {noformat}, {quote} and {panel} blocks.
func wikiMacroBlock(lines []string, i int) ([]*node, int) {
	m := wikiBlock.FindStringSubmatch(strings.TrimSpace(lines[i]))
	end, body := wikiEnd(lines, i, m[1], m[3])
	switch m[1] {
	case "code", "noformat":
		code := &node{Type: tCode}
		if lang, _, _ := strings.Cut(m[2], "|"); m[1] == "code" && lang != "" && !strings.Contains(lang, "=") {
			code.Attrs = map[string]any{"language": lang}
		}
		if text := strings.Join(body, "\n"); text != "" {
			code.Content = []*node{{Type: tText, Text: text}}
		}
		return []*node{code}, end + 1
	case "quote":
		return []*node{{Type: tQuote, Content: wikiParseBlocks(body)}}, end + 1
	}
	return wikiParseBlocks(body), end + 1 // a panel keeps its text
}

func wikiStartsBlock(t string) bool {
	return wikiBlock.MatchString(t) || wikiHeading.MatchString(t) || wikiRule.MatchString(t) || strings.HasPrefix(t, "bq. ") ||
		wikiItem.MatchString(t) || wikiTable.MatchString(t)
}

type wikiListItem struct {
	markers string // * and #, one per level
	text    string
}

// wikiList builds the lists of items at depth: a run of items whose marker
// at depth is the same, with deeper items nested in the item before them.
func wikiList(items []wikiListItem, depth int) []*node {
	var out []*node
	var list *node
	for i := 0; i < len(items); {
		it := items[i]
		kind := tBullet
		if len(it.markers) >= depth && it.markers[depth-1] == '#' {
			kind = tOrdered
		}
		if list == nil || list.Type != kind {
			list = &node{Type: kind}
			out = append(out, list)
		}
		item := &node{Type: tItem}
		if len(it.markers) == depth {
			item.Content = []*node{{Type: tParagraph, Content: wikiParseInline(it.text)}}
			i++
		}
		j := i
		for j < len(items) && len(items[j].markers) > depth {
			j++
		}
		if j > i {
			item.Content = append(item.Content, wikiList(items[i:j], depth+1)...)
			i = j
		}
		list.Content = append(list.Content, item)
	}
	return out
}

func wikiRow(line string) *node {
	cellType := tCell
	sep := "|"
	if strings.HasPrefix(line, "||") {
		cellType, sep = tHeaderCell, "||"
	}
	row := &node{Type: tRow}
	for c := range strings.SplitSeq(strings.Trim(line, "|"), sep) {
		row.Content = append(row.Content, &node{Type: cellType, Content: []*node{{Type: tParagraph, Content: wikiParseInline(strings.Trim(c, "| "))}}})
	}
	return row
}

// wikiMarks are the inline marks of wiki markup. Underline, citation,
// superscript and subscript have no mark in Markdown and keep their text.
var wikiMarks = map[byte]string{'*': mStrong, '_': mEm, '-': mStrike, '+': "", '^': "", '~': ""}

// wikiOpens reports whether a mark character at i can open a span: at the
// start of a word, before a character that is not a space.
func wikiOpens(s string, i int) bool {
	return i+1 < len(s) && s[i+1] != ' ' && !wordAt(s, i-1, true)
}

// wikiCloses finds the end of a span opened at i.
func wikiCloses(s string, i int, c byte) int {
	for j := i + 2; j < len(s); j++ {
		if s[j] == '\n' {
			return -1
		}
		if s[j] == c && s[j-1] != ' ' && s[j-1] != '\\' && !wordAt(s, j+1, false) {
			return j
		}
	}
	return -1
}

func wikiParseInline(s string) []*node {
	p := inlineParser{}
	p.wiki(s, nil)
	return p.out
}

func (p *inlineParser) wiki(s string, marks []mark) {
	var text strings.Builder
	for i := 0; i < len(s); {
		if next, ok := p.wikiSpan(s, i, marks, &text); ok {
			i = next
			continue
		}
		text.WriteByte(s[i])
		i++
	}
	p.add(text.String(), marks)
}

// wikiSpan reads what starts at s[i]: a break, an escape, a macro, a link,
// an image or a marked span. ok is false for plain text.
func (p *inlineParser) wikiSpan(s string, i int, marks []mark, text *strings.Builder) (int, bool) {
	c := s[i]
	switch {
	case strings.HasPrefix(s[i:], `\\`), c == '\n':
		p.flush(text, marks)
		p.out = append(p.out, &node{Type: tBreak})
		if c == '\n' {
			return i + 1, true
		}
		return i + 2, true
	case c == '\\' && i+1 < len(s):
		return escaped(s, i, text), true
	case strings.HasPrefix(s[i:], "{{"):
		if j := strings.Index(s[i+2:], "}}"); j > 0 {
			p.flush(text, marks)
			p.add(s[i+2:i+2+j], with(marks, mark{Type: mCode}))
			return i + j + 4, true
		}
	case c == '{':
		if loc := wikiMacro.FindStringIndex(s[i:]); loc != nil && loc[0] == 0 { // {color:red} and the like
			return i + loc[1], true
		}
	case c == '[':
		if j := strings.IndexByte(s[i:], ']'); j > 1 {
			p.flush(text, marks)
			p.wikiLink(s[i+1:i+j], marks)
			return i + j + 1, true
		}
	case c == '!':
		if j := strings.IndexByte(s[i+1:], '!'); j > 0 && !strings.ContainsAny(s[i+1:i+1+j], " \n") {
			name, _, _ := strings.Cut(s[i+1:i+1+j], "|")
			text.WriteString("[image: " + name + "]")
			return i + j + 2, true
		}
	}
	return p.wikiMarked(s, i, marks, text)
}

// wikiMarked reads a citation or a span of a wiki mark at s[i].
func (p *inlineParser) wikiMarked(s string, i int, marks []mark, text *strings.Builder) (int, bool) {
	if strings.HasPrefix(s[i:], "??") {
		j := strings.Index(s[i+2:], "??")
		if j <= 0 {
			return 0, false
		}
		p.flush(text, marks)
		p.wiki(s[i+2:i+2+j], marks)
		return i + j + 4, true
	}
	t, ok := wikiMarks[s[i]]
	if !ok || !wikiOpens(s, i) {
		return 0, false
	}
	j := wikiCloses(s, i, s[i])
	if j <= 0 {
		return 0, false
	}
	p.flush(text, marks)
	inner := marks
	if t != "" {
		inner = with(marks, mark{Type: t})
	}
	p.wiki(s[i+1:j], inner)
	return j + 1, true
}

// wikiLink reads [text|url], [url] and [~user].
func (p *inlineParser) wikiLink(inner string, marks []mark) {
	if user, ok := strings.CutPrefix(inner, "~"); ok {
		p.out = append(p.out, &node{Type: tMention, Attrs: map[string]any{"id": user, "text": "@" + strings.TrimPrefix(user, "accountid:")}})
		return
	}
	text, href, ok := strings.Cut(inner, "|")
	if !ok {
		text, href = inner, inner
	}
	if !strings.Contains(href, ":") && !strings.HasPrefix(href, "/") { // an anchor or an attachment: no link
		p.add(text, marks)
		return
	}
	p.wiki(text, with(marks, mark{Type: mLink, Attrs: map[string]any{"href": strings.TrimSpace(href)}}))
}

// ---- wiki out

func toWiki(doc *node) string {
	return strings.TrimSpace(wikiBlocks(doc.Content))
}

func wikiBlocks(blocks []*node) string {
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if s := wikiBlockOut(b, ""); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "\n\n")
}

func wikiBlockOut(n *node, prefix string) string {
	switch n.Type {
	case tParagraph:
		return wikiInline(n.Content)
	case tHeading:
		return "h" + strconv.Itoa(max(1, min(6, atoi(n.attr("level"))))) + ". " + wikiInline(n.Content)
	case tBullet, tOrdered:
		return wikiListOut(n, prefix)
	case tCode:
		open := "{code}"
		if lang := n.attr("language"); lang != "" {
			open = "{code:" + lang + "}"
		}
		return open + "\n" + plain(n) + "\n{code}"
	case tQuote:
		return "{quote}\n" + wikiBlocks(n.Content) + "\n{quote}"
	case tRule:
		return "----"
	case tTable:
		rows := make([]string, 0, len(n.Content))
		for _, row := range n.Content {
			rows = append(rows, wikiRowOut(row))
		}
		return strings.Join(rows, "\n")
	case tText, tBreak, tMention:
		return wikiInline([]*node{n})
	}
	return unknownBlock(n, wikiBlocks, wikiInline)
}

// wikiListOut writes a list; a nested one adds its marker to prefix.
func wikiListOut(n *node, prefix string) string {
	marker := "*"
	if n.Type == tOrdered {
		marker = "#"
	}
	var lines []string
	for _, item := range n.Content {
		for _, c := range item.Content {
			if c.Type == tBullet || c.Type == tOrdered {
				lines = append(lines, wikiBlockOut(c, prefix+marker))
				continue
			}
			lines = append(lines, prefix+marker+" "+strings.ReplaceAll(wikiBlockOut(c, ""), "\n\n", "\n"))
		}
	}
	return strings.Join(lines, "\n")
}

func wikiRowOut(row *node) string {
	var b strings.Builder
	last := "|"
	for _, c := range row.Content {
		sep := "|"
		if c.Type == tHeaderCell {
			sep = "||"
		}
		b.WriteString(sep + strings.ReplaceAll(wikiBlocks(c.Content), "\n", " "))
		last = sep
	}
	b.WriteString(last)
	return b.String()
}

// wikiEscape escapes what wiki markup would read as a mark: always the
// brackets, braces, bars and the strong and em marks, and the other marks at
// a word's edge.
func wikiEscape(s string) string {
	var b strings.Builder
	for i, r := range s {
		switch {
		case strings.ContainsRune("*_{}[]|!", r):
			b.WriteByte('\\')
		case strings.ContainsRune("-+^~", r) && (!wordAt(s, i-1, true) || !wordAt(s, i+utf8.RuneLen(r), false)) && !unicode.IsSpace(nextRune(s, i+utf8.RuneLen(r))):
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func nextRune(s string, i int) rune {
	if i >= len(s) {
		return ' '
	}
	r, _ := utf8.DecodeRuneInString(s[i:])
	return r
}

func wikiInline(nodes []*node) string {
	var b strings.Builder
	for _, n := range nodes {
		switch n.Type {
		case tText:
			b.WriteString(wikiText(n))
		case tBreak:
			b.WriteString("\n")
		case tMention:
			if id := n.attr("id"); id != "" {
				b.WriteString("[~" + id + "]")
			} else {
				b.WriteString(wikiEscape(mentionText(n)))
			}
		case "inlineCard":
			b.WriteString("[" + n.attr("url") + "]")
		default:
			if len(n.Content) > 0 {
				b.WriteString(wikiInline(n.Content))
			} else {
				b.WriteString(wikiEscape(unknownLeaf(n)))
			}
		}
	}
	return b.String()
}

func wikiText(n *node) string {
	s := n.Text
	if _, ok := hasMark(n.Marks, mCode); ok {
		s = "{{" + strings.ReplaceAll(s, "}}", "} }") + "}}"
	} else {
		s = wikiEscape(s)
	}
	for _, t := range slicesReverse(markOrder) {
		m, ok := hasMark(n.Marks, t)
		if !ok {
			continue
		}
		switch t {
		case mEm:
			s = "_" + s + "_"
		case mStrong:
			s = "*" + s + "*"
		case mStrike:
			s = "-" + s + "-"
		case mLink:
			s = "[" + s + "|" + m.href() + "]"
		}
	}
	return s
}
