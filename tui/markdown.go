package tui

import (
	"fmt"
	"image/color"
	"slices"
	"strconv"
	"strings"
	"sync"

	"charm.land/lipgloss/v2"
	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/charmbracelet/x/ansi"
	"github.com/rivo/uniseg"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	east "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
)

// The markdown renderer turns a reply into terminal text. It is our own
// rather than glamour because a reply is drawn while it streams: each
// top-level block is rendered on its own, so the blocks a stream has
// finished are drawn once and only the one still growing is redrawn per
// token (see markdownStream). Code is highlighted with chroma's lexers
// but our own colors; chroma's themes are left out.

var mdParser = goldmark.New(goldmark.WithExtensions(
	extension.Table, extension.Strikethrough, extension.TaskList, extension.Linkify,
)).Parser()

// mdStyle is how a run of markdown text looks. Lip Gloss styles are not used
// for runs: Lip Gloss underlines and strikes through one rune at a time,
// an escape sequence per character.
type mdStyle struct {
	bold, italic, underline, strike bool
	fg, bg                          color.Color
}

// on lays s over base: s's attributes win, base fills in the rest.
func (s mdStyle) on(base mdStyle) mdStyle {
	s.bold, s.italic = s.bold || base.bold, s.italic || base.italic
	s.underline, s.strike = s.underline || base.underline, s.strike || base.strike
	if s.fg == nil {
		s.fg = base.fg
	}
	if s.bg == nil {
		s.bg = base.bg
	}
	return s
}

func (s mdStyle) render(str string) string {
	if s == (mdStyle{}) || str == "" {
		return str
	}
	var a ansi.Style
	if s.bold {
		a = a.Bold()
	}
	if s.italic {
		a = a.Italic(true)
	}
	if s.underline {
		a = a.Underline(true)
	}
	if s.strike {
		a = a.Strikethrough(true)
	}
	if s.fg != nil {
		a = a.ForegroundColor(s.fg)
	}
	if s.bg != nil {
		a = a.BackgroundColor(s.bg)
	}
	return a.Styled(str)
}

// mdStyles are the markdown colors, chosen once per theme.
type mdStyles struct {
	h1, h2, h3, code, codeBlock, link, rule, quote, check mdStyle
	codeBg                                                mdStyle // behind a code block
	syn                                                   mdSyntax
}

// mdSyntax colors highlighted code.
type mdSyntax struct {
	keyword, str, num, comment, fn, typ, ins, del, head mdStyle
}

func newMDStyles(isDark bool) mdStyles {
	ld := lipgloss.LightDark(isDark)
	accent := ld(lipgloss.Color("97"), lipgloss.Color("141"))
	st := mdStyles{
		h1:        mdStyle{bold: true, underline: true, fg: accent},
		h2:        mdStyle{bold: true, fg: accent},
		h3:        mdStyle{bold: true},
		code:      mdStyle{fg: ld(lipgloss.Color("166"), lipgloss.Color("215"))},
		codeBlock: mdStyle{fg: ld(lipgloss.Color("238"), lipgloss.Color("252"))},
		link:      mdStyle{underline: true, fg: ld(lipgloss.Color("25"), lipgloss.Color("75"))},
		rule:      mdStyle{fg: ld(lipgloss.Color("250"), lipgloss.Color("238"))},
		quote:     mdStyle{fg: ld(lipgloss.Color("245"), lipgloss.Color("243"))},
		check:     mdStyle{fg: ld(lipgloss.Color("28"), lipgloss.Color("114"))},
	}
	st.codeBg = mdStyle{bg: ld(lipgloss.Color("255"), lipgloss.Color("235"))}
	st.syn.keyword = mdStyle{fg: accent}
	st.syn.str = mdStyle{fg: ld(lipgloss.Color("28"), lipgloss.Color("150"))}
	st.syn.num = mdStyle{fg: ld(lipgloss.Color("166"), lipgloss.Color("215"))}
	st.syn.comment = mdStyle{italic: true, fg: ld(lipgloss.Color("245"), lipgloss.Color("243"))}
	st.syn.fn = mdStyle{fg: ld(lipgloss.Color("25"), lipgloss.Color("117"))}
	st.syn.typ = mdStyle{fg: ld(lipgloss.Color("31"), lipgloss.Color("80"))}
	st.syn.ins = mdStyle{fg: ld(lipgloss.Color("28"), lipgloss.Color("114"))}
	st.syn.del = mdStyle{fg: ld(lipgloss.Color("160"), lipgloss.Color("203"))}
	st.syn.head = mdStyle{bold: true, fg: ld(lipgloss.Color("31"), lipgloss.Color("80"))}
	return st
}

// mdWidth is the width a reply's text wraps to, beside its icon gutter.
func (r *renderer) mdWidth() int { return max(20, r.width-r.gutterWidth(iconReply)-1) }

// markdown renders a whole document.
func (r *renderer) markdown(src string) string {
	if strings.TrimSpace(src) == "" {
		return src
	}
	return r.mdBlocks(src, r.mdWidth(), nil)
}

// mdStream caches the rendered part of a reply that has stopped changing.
type mdStream struct {
	src, out string // src is a prefix of the reply made of whole blocks
	code     codeCache
}

// codeCache holds the lexed complete lines of the code block a reply is
// streaming, so each token lexes only the line still being written.
type codeCache struct {
	lang, src string
	lines     [][]cell
}

// markdownStream renders a reply that is still arriving. Everything before
// the last top-level block is final (a later token can only extend the last
// block or start a new one), so it is rendered once and cached; each call
// parses and renders only the text after it.
func (r *renderer) markdownStream(src string) string {
	c := &r.stream
	if !strings.HasPrefix(src, c.src) {
		*c = mdStream{}
	}
	rest := src[len(c.src):]
	if cut := lastBlockStart(rest); cut > 0 {
		done := rest[:cut]
		if out := r.mdBlocks(done, r.mdWidth(), nil); out != "" {
			c.out = joinBlocks(c.out, out)
		}
		c.src += done
		rest = rest[cut:]
	}
	return joinBlocks(c.out, r.mdBlocks(rest, r.mdWidth(), &c.code))
}

func joinBlocks(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "\n\n" + b
}

// lastBlockStart is the byte offset where the last top-level block of src
// starts, or 0 when that cannot be told (one block, or a block such as a
// thematic break that keeps no source positions).
func lastBlockStart(src string) int {
	doc := mdParser.Parse(text.NewReader([]byte(src)))
	last := doc.LastChild()
	if last == nil || last == doc.FirstChild() {
		return 0
	}
	start := -1
	if fc, ok := last.(*ast.FencedCodeBlock); ok {
		// Its lines start after the opening fence, so step back a line.
		switch {
		case fc.Info != nil:
			start = fc.Info.Segment.Start
		case fc.Lines().Len() > 0:
			start = max(0, strings.LastIndexByte(src[:fc.Lines().At(0).Start], '\n'))
			if start > 0 {
				start = strings.LastIndexByte(src[:start], '\n') + 1
			}
		}
	} else {
		start = firstSegment(last)
	}
	if start <= 0 {
		return 0
	}
	return strings.LastIndexByte(src[:start], '\n') + 1
}

// firstSegment is the smallest source offset any part of n records, or -1.
func firstSegment(n ast.Node) int {
	best := -1
	_ = ast.Walk(n, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		off := -1
		if n.Type() == ast.TypeBlock && n.Lines().Len() > 0 {
			off = n.Lines().At(0).Start
		} else if t, ok := n.(*ast.Text); ok {
			off = t.Segment.Start
		}
		if off >= 0 && (best < 0 || off < best) {
			best = off
		}
		return ast.WalkContinue, nil
	})
	return best
}

// mdBlocks parses src and renders its top-level blocks, a blank line apart.
// A code cache is given only for the block still streaming.
func (r *renderer) mdBlocks(src string, width int, code *codeCache) string {
	b := []byte(src)
	doc := mdParser.Parse(text.NewReader(b), parser.WithContext(parser.NewContext()))
	w := mdWriter{src: b, st: r.md, code: code}
	if code == nil {
		w.codes = r.codes
	}
	return w.children(doc, width, "\n\n")
}

type mdWriter struct {
	src   []byte
	st    mdStyles
	code  *codeCache        // nil unless streaming
	codes map[string]string // where finished code blocks are kept for copying
}

// children renders each block child of n and joins them with sep.
func (w *mdWriter) children(n ast.Node, width int, sep string) string {
	var parts []string
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		if s := w.block(c, width); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, sep)
}

func (w *mdWriter) block(n ast.Node, width int) string {
	switch n := n.(type) {
	case *ast.Paragraph, *ast.TextBlock:
		return w.wrapInline(n, mdStyle{}, width)
	case *ast.Heading:
		st := w.st.h3
		switch n.Level {
		case 1:
			st = w.st.h1
		case 2:
			st = w.st.h2
		}
		return w.wrapInline(n, st, width)
	case *ast.ThematicBreak:
		return w.st.rule.render(strings.Repeat("─", width))
	case *ast.FencedCodeBlock:
		return w.codeBlock(n.Lines(), string(n.Language(w.src)), width)
	case *ast.CodeBlock, *ast.HTMLBlock:
		return w.codeBlock(n.Lines(), "", width)
	case *ast.Blockquote:
		inner := w.children(n, width-2, "\n\n")
		bar := w.st.quote.render("│") + " "
		lines := strings.Split(inner, "\n")
		for i, l := range lines {
			lines[i] = bar + l
		}
		return strings.Join(lines, "\n")
	case *ast.List:
		return w.list(n, width)
	case *east.Table:
		return w.table(n, width)
	}
	// Unknown blocks (an extension's) still show their text.
	if n.Type() == ast.TypeBlock && n.Lines().Len() > 0 {
		return w.codeBlock(n.Lines(), "", width)
	}
	return w.children(n, width, "\n\n")
}

// codeBlock draws code on a tinted panel, highlighted when its fence names a
// language chroma knows. It keeps its line breaks; a line too long for the
// screen is cut into rows rather than reflowed.
func (w *mdWriter) codeBlock(lines *text.Segments, lang string, width int) string {
	var b strings.Builder
	for i := range lines.Len() {
		seg := lines.At(i)
		b.Write(seg.Value(w.src))
	}
	source := strings.TrimRight(b.String(), "\r\n ")
	code := expandTabs(source, 4)
	if code == "" {
		return ""
	}
	hl := w.highlight(code, lang)
	// Lexers end the text with a newline of their own.
	for len(hl) > 1 && len(hl[len(hl)-1]) == 0 {
		hl = hl[:len(hl)-1]
	}
	// The block sits on a tinted panel the full width of the text, padded
	// by a row above and below and two columns either side. Copying trims
	// the padding. The top row holds the copy label (see codeAt).
	const padX = 2
	bg := w.st.codeBg
	top := bg.render(strings.Repeat(" ", width))
	if width >= len(copyLabel)+2*padX {
		top = bg.render(strings.Repeat(" ", width-len(copyLabel)-1)) + w.st.quote.on(bg).render(copyLabel) + bg.render(" ")
	}
	rows := []string{top}
	var plain []string
	for _, line := range hl {
		for _, row := range hardWrapCells(line, width-2*padX) {
			right := max(0, width-padX-cellsWidth(row))
			rows = append(rows, bg.render(strings.Repeat(" ", padX))+renderCells(row, bg)+bg.render(strings.Repeat(" ", right)))
			plain = append(plain, cellsText(row))
		}
	}
	if w.codes != nil {
		w.codes[codeKey(plain)] = source
	}
	return strings.Join(append(rows, bg.render(strings.Repeat(" ", width))), "\n")
}

// copyLabel sits at the right of a code block's top row; clicking it
// copies the block.
const copyLabel = "[copy]"

// codeKey identifies a code block by its rows as drawn, with the padding
// (and whatever indents the block on screen) trimmed, so a click on the
// drawn block can find the source it came from.
func codeKey(rows []string) string {
	trimmed := make([]string, len(rows))
	for i, r := range rows {
		trimmed[i] = strings.TrimSpace(r)
	}
	return strings.Join(trimmed, "\n")
}

func cellsText(cs []cell) string {
	var b strings.Builder
	for _, c := range cs {
		b.WriteString(c.g)
	}
	return b.String()
}

// panelSeq is the escape sequence that opens a code block's tint; every
// row of a panel carries it and no other text does.
func (r *renderer) panelSeq() string {
	return strings.TrimSuffix(r.md.codeBg.render("x"), "x\x1b[m")
}

// copyLabelAt reports whether raw, a rendered transcript line, is the top
// row of a code block, and the columns its copy label spans.
func (r *renderer) copyLabelAt(raw string) (start, end int, ok bool) {
	if !strings.Contains(raw, r.panelSeq()) {
		return 0, 0, false
	}
	// The viewport may pad the row, so find the label rather than
	// counting from the end.
	plain := ansi.Strip(raw)
	i := strings.LastIndex(plain, copyLabel)
	if i < 0 || strings.TrimSpace(plain[i+len(copyLabel):]) != "" {
		return 0, 0, false
	}
	start = ansi.StringWidth(plain[:i])
	end = start + len(copyLabel)
	// Only the gutter may sit before the label: the rest of the row is tint.
	if strings.TrimSpace(ansi.Cut(plain, r.gutterWidth(iconReply), start)) != "" {
		return 0, 0, false
	}
	return start, end, true
}

// codeAt returns the source of the code block whose copy label is at
// (line, col) of the content that line(i) reads, n lines long.
func (r *renderer) codeAt(line func(int) string, n, i, col int) (string, bool) {
	if i < 0 || i >= n {
		return "", false
	}
	start, end, ok := r.copyLabelAt(line(i))
	if !ok || col < start || col >= end {
		return "", false
	}
	seq := r.panelSeq()
	var rows []string
	for j := i + 1; j < n && strings.Contains(line(j), seq); j++ {
		rows = append(rows, ansi.Strip(line(j)))
	}
	if len(rows) == 0 {
		return "", false
	}
	src, ok := r.codes[codeKey(rows[:len(rows)-1])] // the last row is padding
	return src, ok
}

// highlight splits code into lines of styled cells. While streaming, the
// complete lines are lexed once and the line being written on its own;
// lexing all of it per token is milliseconds for a long block.
func (w *mdWriter) highlight(code, lang string) [][]cell {
	if w.code == nil {
		return w.lex(code, lang)
	}
	i := strings.LastIndexByte(code, '\n')
	head, tail := code[:i+1], code[i+1:]
	if c := w.code; c.lang != lang || c.src != head {
		lines := w.lex(head, lang)
		// head ends in a newline, which leaves an empty last line.
		*c = codeCache{lang: lang, src: head, lines: lines[:len(lines)-1]}
	}
	return append(slices.Clip(w.code.lines), w.lex(tail, lang)...)
}

// lex splits code into lines of styled cells.
func (w *mdWriter) lex(code, lang string) [][]cell {
	var lines [][]cell
	var line []cell
	add := func(s string, st *mdStyle) {
		for s != "" {
			if s[0] == '\n' {
				lines, line = append(lines, line), nil
				s = s[1:]
				continue
			}
			var g string
			var gw int
			if s[0] < 0x80 {
				g, s, gw = s[:1], s[1:], 1
			} else {
				g, s, gw, _ = uniseg.FirstGraphemeClusterInString(s, -1)
			}
			line = append(line, cell{g: g, w: gw, st: st})
		}
	}
	if it := tokenise(code, lang); it != nil {
		for t := it(); t != chroma.EOF; t = it() {
			add(t.Value, w.st.token(t.Type))
		}
	} else {
		add(code, &w.st.codeBlock)
	}
	return append(lines, line)
}

// tokenise lexes code as lang, or returns nil for a language chroma does
// not know (or none: guessing would color prose as code).
func tokenise(code, lang string) chroma.Iterator {
	if lang == "" {
		return nil
	}
	lexer := lexerFor(lang)
	if lexer == nil {
		return nil
	}
	it, err := chroma.Coalesce(lexer).Tokenise(nil, code)
	if err != nil {
		return nil
	}
	return it
}

// lexerCache holds lexerFor's answers. Finding a lexer by file name
// matches globs against every lexer chroma has, which costs more than
// lexing most code: a diff, highlighting each hunk, paid it per hunk.
var lexerCache sync.Map // lang → chroma.Lexer, nil for none

// lexerFor is chroma's lexer for lang, a language or file name, or nil.
func lexerFor(lang string) chroma.Lexer {
	if l, ok := lexerCache.Load(lang); ok {
		lexer, _ := l.(chroma.Lexer)
		return lexer
	}
	lexer := lexers.Get(lang)
	lexerCache.Store(lang, lexer)
	return lexer
}

// token is the style for a chroma token type: a handful of colors, not a
// chroma theme, so code reads like the rest of the transcript.
func (st *mdStyles) token(t chroma.TokenType) *mdStyle {
	switch {
	case t == chroma.NameFunction || t == chroma.NameFunctionMagic:
		return &st.syn.fn
	case t == chroma.KeywordType || t == chroma.NameBuiltin || t == chroma.NameClass:
		return &st.syn.typ
	case t == chroma.GenericInserted:
		return &st.syn.ins
	case t == chroma.GenericDeleted:
		return &st.syn.del
	case t == chroma.GenericHeading || t == chroma.GenericSubheading:
		return &st.syn.head
	case t.InCategory(chroma.Comment):
		if t.InSubCategory(chroma.CommentPreproc) {
			return &st.syn.keyword
		}
		return &st.syn.comment
	case t.InCategory(chroma.Keyword), t == chroma.OperatorWord:
		return &st.syn.keyword
	case t.InSubCategory(chroma.LiteralString):
		return &st.syn.str
	case t.InSubCategory(chroma.LiteralNumber), t == chroma.KeywordConstant:
		return &st.syn.num
	}
	return &st.codeBlock
}

// hardWrapCells cuts a line of code into rows of at most width cells.
func hardWrapCells(cs []cell, width int) [][]cell {
	width = max(width, 1)
	rows := [][]cell{nil}
	w := 0
	for _, c := range cs {
		if w+c.w > width && w > 0 {
			rows, w = append(rows, nil), 0
		}
		rows[len(rows)-1] = append(rows[len(rows)-1], c)
		w += c.w
	}
	return rows
}

func (w *mdWriter) list(l *ast.List, width int) string {
	n, last := 0, l.Start
	for c := l.FirstChild(); c != nil; c = c.NextSibling() {
		n++
	}
	if l.IsOrdered() {
		last += n - 1
	}
	markW := 2
	if l.IsOrdered() {
		markW = len(strconv.Itoa(last)) + 2
	}
	sep := "\n"
	if !l.IsTight {
		sep = "\n\n"
	}
	bullet := []string{"•", "◦", "▪"}[listDepth(l)%3]
	var items []string
	i := l.Start
	for c := l.FirstChild(); c != nil; c = c.NextSibling() {
		mark := bullet + " "
		if l.IsOrdered() {
			mark = fmt.Sprintf("%*d. ", markW-2, i)
			i++
		}
		body := w.children(c, width-markW, sep)
		pad := strings.Repeat(" ", markW)
		lines := strings.Split(body, "\n")
		for j, ln := range lines {
			switch {
			case j == 0:
				lines[j] = mark + ln
			case ln != "":
				lines[j] = pad + ln
			}
		}
		items = append(items, strings.Join(lines, "\n"))
	}
	return strings.Join(items, sep)
}

func listDepth(n ast.Node) int {
	d := 0
	for p := n.Parent(); p != nil; p = p.Parent() {
		if _, ok := p.(*ast.List); ok {
			d++
		}
	}
	return d
}

// table draws a GFM table in a box with a rule between rows, narrowing the widest columns (and wrapping their
// cells) until it fits.
func (w *mdWriter) table(t *east.Table, width int) string {
	var rows [][][]cell
	header := 0
	for r := t.FirstChild(); r != nil; r = r.NextSibling() {
		if _, ok := r.(*east.TableHeader); ok {
			header = 1
		}
		var row [][]cell
		for c := r.FirstChild(); c != nil; c = c.NextSibling() {
			st := mdStyle{bold: header == 1 && len(rows) == 0}
			row = append(row, w.cells(c, st))
		}
		rows = append(rows, row)
	}
	cols := len(t.Alignments)
	for _, row := range rows {
		cols = max(cols, len(row))
	}
	if cols == 0 {
		return ""
	}
	widths := make([]int, cols)
	for _, row := range rows {
		for i, c := range row {
			widths[i] = max(widths[i], cellsWidth(c))
		}
	}
	avail := width - 3*(cols-1) - 4 // separators and the outer border
	for sum(widths) > avail {
		wi := 0
		for i, cw := range widths {
			if cw > widths[wi] {
				wi = i
			}
		}
		if widths[wi] <= 3 {
			break
		}
		widths[wi]--
	}
	// rule is a border line: left corner, the joint between columns, right.
	rule := func(l, mid, r string) string {
		var b strings.Builder
		b.WriteString(l + "─")
		for i, cw := range widths {
			if i > 0 {
				b.WriteString("─" + mid + "─")
			}
			b.WriteString(strings.Repeat("─", cw))
		}
		b.WriteString("─" + r)
		return w.st.rule.render(b.String())
	}
	bar := func(s string) string { return w.st.rule.render(s) }
	out := []string{rule("┌", "┬", "┐")}
	for ri, row := range rows {
		wrapped := make([][]string, cols)
		height := 1
		for i := range cols {
			var c []cell
			if i < len(row) {
				c = row[i]
			}
			wrapped[i] = wrapCells(c, widths[i])
			height = max(height, len(wrapped[i]))
		}
		for h := range height {
			var b strings.Builder
			b.WriteString(bar("│") + " ")
			for i := range cols {
				if i > 0 {
					b.WriteString(bar(" │ "))
				}
				s := ""
				if h < len(wrapped[i]) {
					s = wrapped[i][h]
				}
				align := east.AlignLeft
				if i < len(t.Alignments) {
					align = t.Alignments[i]
				}
				b.WriteString(alignCell(s, widths[i], align))
			}
			b.WriteString(" " + bar("│"))
			out = append(out, b.String())
		}
		// A rule under every row, not just the header: a wrapped cell
		// otherwise reads as rows of its own.
		if ri < len(rows)-1 {
			out = append(out, rule("├", "┼", "┤"))
		}
	}
	out = append(out, rule("└", "┴", "┘"))
	return strings.Join(out, "\n")
}

func sum(xs []int) int {
	t := 0
	for _, x := range xs {
		t += x
	}
	return t
}

func alignCell(s string, width int, a east.Alignment) string {
	gap := max(0, width-ansi.StringWidth(s))
	switch a {
	case east.AlignRight:
		return strings.Repeat(" ", gap) + s
	case east.AlignCenter:
		l := gap / 2
		return strings.Repeat(" ", l) + s + strings.Repeat(" ", gap-l)
	}
	return s + strings.Repeat(" ", gap)
}

// A cell is one grapheme of inline text with its style; inline text is
// flattened to cells so wrapping can break anywhere without splitting an
// escape sequence, and every output line carries its own styling.
type cell struct {
	g     string
	w     int
	st    *mdStyle
	space bool
	br    bool // a hard line break
}

func (w *mdWriter) wrapInline(n ast.Node, base mdStyle, width int) string {
	return strings.Join(wrapCells(w.cells(n, base), width), "\n")
}

// cells flattens n's inline children, styled on top of base.
func (w *mdWriter) cells(n ast.Node, base mdStyle) []cell {
	var out []cell
	var walk func(n ast.Node, st mdStyle)
	add := func(s string, st mdStyle) {
		p := &st
		for s != "" {
			var g string
			var gw int
			if s[0] < 0x80 {
				g, s, gw = s[:1], s[1:], 1
			} else {
				g, s, gw, _ = uniseg.FirstGraphemeClusterInString(s, -1)
			}
			switch g {
			case "\n", "\r":
				out = append(out, cell{g: " ", w: 1, st: p, space: true})
			case " ", "\t":
				out = append(out, cell{g: " ", w: 1, st: p, space: true})
			default:
				out = append(out, cell{g: g, w: gw, st: p})
			}
		}
	}
	walk = func(n ast.Node, st mdStyle) {
		for c := n.FirstChild(); c != nil; c = c.NextSibling() {
			switch c := c.(type) {
			case *ast.Text:
				add(string(c.Value(w.src)), st)
				switch {
				case c.HardLineBreak():
					out = append(out, cell{br: true})
				case c.SoftLineBreak():
					add(" ", st)
				}
			case *ast.String:
				add(string(c.Value), st)
			case *ast.CodeSpan:
				var b strings.Builder
				for t := c.FirstChild(); t != nil; t = t.NextSibling() {
					if t, ok := t.(*ast.Text); ok {
						b.Write(t.Value(w.src))
					}
				}
				add(b.String(), w.st.code.on(st))
			case *ast.Emphasis:
				if c.Level >= 2 {
					walk(c, mdStyle{bold: true}.on(st))
				} else {
					walk(c, mdStyle{italic: true}.on(st))
				}
			case *east.Strikethrough:
				walk(c, mdStyle{strike: true}.on(st))
			case *ast.Link:
				walk(c, w.st.link.on(st))
				if dest := string(c.Destination); dest != "" && dest != plainText(c, w.src) {
					add(" ("+dest+")", w.st.quote)
				}
			case *ast.AutoLink:
				add(string(c.Label(w.src)), w.st.link.on(st))
			case *ast.Image:
				add("[image: ", w.st.quote)
				walk(c, w.st.link.on(st))
				add("]", w.st.quote)
			case *ast.RawHTML:
				for i := range c.Segments.Len() {
					seg := c.Segments.At(i)
					add(string(seg.Value(w.src)), st)
				}
			case *east.TaskCheckBox:
				if c.IsChecked {
					add("[x] ", w.st.check)
				} else {
					add("[ ] ", st)
				}
			default:
				walk(c, st)
			}
		}
	}
	walk(n, base)
	return out
}

// plainText is the unstyled text of n's inline content.
func plainText(n ast.Node, src []byte) string {
	var b strings.Builder
	_ = ast.Walk(n, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if t, ok := n.(*ast.Text); ok && entering {
			b.Write(t.Value(src))
		}
		return ast.WalkContinue, nil
	})
	return b.String()
}

func cellsWidth(cs []cell) int {
	w, line := 0, 0
	for _, c := range cs {
		if c.br {
			w, line = max(w, line), 0
			continue
		}
		line += c.w
	}
	return max(w, line)
}

// wrapCells word-wraps cells to width and renders each line.
func wrapCells(cs []cell, width int) []string {
	width = max(width, 1)
	var lines [][]cell
	var line []cell
	lineW := 0
	flush := func() {
		for len(line) > 0 && line[len(line)-1].space {
			line = line[:len(line)-1]
		}
		lines = append(lines, line)
		line, lineW = nil, 0
	}
	for i := 0; i < len(cs); {
		if cs[i].br {
			flush()
			i++
			continue
		}
		// The next run of spaces, then the word after it.
		j := i
		spaceW := 0
		for j < len(cs) && cs[j].space {
			spaceW += cs[j].w
			j++
		}
		k := j
		wordW := 0
		for k < len(cs) && !cs[k].space && !cs[k].br {
			wordW += cs[k].w
			k++
		}
		if lineW > 0 && lineW+spaceW+wordW > width {
			flush()
		} else if lineW > 0 {
			// Leading spaces of a line are dropped.
			line = append(line, cs[i:j]...)
			lineW += spaceW
		}
		for _, c := range cs[j:k] {
			if lineW+c.w > width && lineW > 0 {
				flush()
			}
			line = append(line, c)
			lineW += c.w
		}
		i = k
	}
	flush()
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = renderCells(l, mdStyle{})
	}
	return out
}

// renderCells styles a line, one escape sequence per run of same-styled
// cells, each laid over base.
func renderCells(cs []cell, base mdStyle) string {
	var b, run strings.Builder
	var cur *mdStyle
	emit := func() {
		if run.Len() == 0 {
			return
		}
		st := base
		if cur != nil {
			st = cur.on(base)
		}
		b.WriteString(st.render(run.String()))
		run.Reset()
	}
	for _, c := range cs {
		if c.st != cur {
			emit()
			cur = c.st
		}
		run.WriteString(c.g)
	}
	emit()
	return b.String()
}
