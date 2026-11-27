package tui

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
)

// Diffs are drawn from unified patches: edit's own (coding.UnifiedDiff)
// and git's. A patch is parsed into files, hunks and lines first, so the
// drawing knows each line's number and which side it is on. Rows are
// highlighted as the file's language, added and removed lines sit on a
// tint, and in a removed line paired with an added one the words that
// changed sit on a stronger tint.

// diffFile is one file's part of a patch.
type diffFile struct {
	oldPath, newPath string // "" for /dev/null: a file added or deleted
	binary           bool
	hunks            []diffHunk
	raw              string // the file's part of the patch, as it came
}

// path is the name the file goes by after the change.
func (f *diffFile) path() string {
	if f.newPath != "" {
		return f.newPath
	}
	return f.oldPath
}

type diffHunk struct {
	oldStart, oldLines, newStart, newLines int
	section                                string // what git prints after the second @@: the enclosing function
	lines                                  []diffLine
}

// diffLine is one line of a hunk. kind is ' ', '+' or '-'; old and new
// are its line numbers, 0 on the side that lacks it.
type diffLine struct {
	kind     byte
	text     string
	old, new int
}

// parsePatch reads a unified patch. Lines it does not understand (git's
// index and mode lines, say) are skipped, so a patch it half-understands
// still draws what it can.
func parsePatch(patch string) []diffFile {
	var files []diffFile
	var cur *diffFile
	var h *diffHunk
	gitPrefixes := false        // a diff --git header: paths carry a/ and b/
	oldLeft, newLeft := 0, 0    // lines the open hunk still has
	sawOld := false             // the current file has had its --- line
	var starts []int            // where each file's text begins in patch
	pos, next := 0, 0           // the current line's offset, and the next's
	start := func() *diffFile { // a file whose header has begun
		files = append(files, diffFile{})
		starts = append(starts, pos)
		cur, h, sawOld = &files[len(files)-1], nil, false
		return cur
	}
	for line := range strings.SplitSeq(strings.TrimSuffix(patch, "\n"), "\n") {
		pos, next = next, next+len(line)+1
		line = strings.TrimSuffix(line, "\r")
		if h != nil && (oldLeft > 0 || newLeft > 0) {
			// Inside a hunk, its counts say where it ends: a removed line
			// reading "-- x" is not a file header.
			kind := byte(' ')
			text := line
			if line != "" {
				kind, text = line[0], line[1:]
			}
			switch {
			case kind == ' ' && oldLeft > 0 && newLeft > 0:
				h.lines = append(h.lines, diffLine{kind: ' ', text: text, old: h.oldStart + h.oldLines - oldLeft, new: h.newStart + h.newLines - newLeft})
				oldLeft, newLeft = oldLeft-1, newLeft-1
				continue
			case kind == '-' && oldLeft > 0:
				h.lines = append(h.lines, diffLine{kind: '-', text: text, old: h.oldStart + h.oldLines - oldLeft})
				oldLeft--
				continue
			case kind == '+' && newLeft > 0:
				h.lines = append(h.lines, diffLine{kind: '+', text: text, new: h.newStart + h.newLines - newLeft})
				newLeft--
				continue
			case kind == '\\': // "\ No newline at end of file"
				continue
			}
			// A line the counts did not expect ends the hunk early.
			oldLeft, newLeft = 0, 0
		}
		switch {
		case strings.HasPrefix(line, "diff --git "):
			gitPrefixes = true
			f := start()
			if a, b, ok := strings.Cut(strings.TrimPrefix(line, "diff --git "), " b/"); ok {
				f.oldPath, f.newPath = strings.TrimPrefix(a, "a/"), b
			}
		case strings.HasPrefix(line, "--- ") && h == nil && cur != nil && !sawOld:
			cur.oldPath, sawOld = patchFilePath(line[4:], gitPrefixes), true
		case strings.HasPrefix(line, "--- "):
			start().oldPath = patchFilePath(line[4:], gitPrefixes)
			sawOld = true
		case strings.HasPrefix(line, "+++ ") && cur != nil && h == nil:
			cur.newPath = patchFilePath(line[4:], gitPrefixes)
		case strings.HasPrefix(line, "rename from ") && cur != nil:
			cur.oldPath = strings.TrimPrefix(line, "rename from ")
		case strings.HasPrefix(line, "rename to ") && cur != nil:
			cur.newPath = strings.TrimPrefix(line, "rename to ")
		case strings.HasPrefix(line, "new file mode") && cur != nil:
			cur.oldPath = ""
		case strings.HasPrefix(line, "deleted file mode") && cur != nil:
			cur.newPath = ""
		case strings.HasPrefix(line, "Binary files ") && cur != nil:
			cur.binary = true
		case strings.HasPrefix(line, "@@ "):
			hunk, ok := parseHunkHeader(line)
			if !ok {
				continue
			}
			if cur == nil {
				start()
			}
			cur.hunks = append(cur.hunks, hunk)
			h = &cur.hunks[len(cur.hunks)-1]
			oldLeft, newLeft = hunk.oldLines, hunk.newLines
		}
	}
	for i := range files {
		end := len(patch)
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		files[i].raw = patch[starts[i]:end]
	}
	return files
}

// patchFilePath is the path on a ---/+++ line: without git's a/ or b/,
// the timestamp diff -u appends, or /dev/null.
func patchFilePath(s string, gitPrefixes bool) string {
	s, _, _ = strings.Cut(s, "\t")
	if s == "/dev/null" {
		return ""
	}
	if gitPrefixes && len(s) > 2 && (s[:2] == "a/" || s[:2] == "b/") {
		s = s[2:]
	}
	return s
}

// parseHunkHeader reads "@@ -o,n +o,n @@ section". A missing count is 1.
func parseHunkHeader(line string) (diffHunk, bool) {
	rest := strings.TrimPrefix(line, "@@ ")
	ranges, section, ok := strings.Cut(rest, " @@")
	if !ok {
		return diffHunk{}, false
	}
	oldR, newR, ok := strings.Cut(ranges, " ")
	if !ok || !strings.HasPrefix(oldR, "-") || !strings.HasPrefix(newR, "+") {
		return diffHunk{}, false
	}
	span := func(s string) (start, n int, ok bool) {
		a, b, has := strings.Cut(s, ",")
		start, err := strconv.Atoi(a)
		if err != nil {
			return 0, 0, false
		}
		n = 1
		if has {
			if n, err = strconv.Atoi(b); err != nil {
				return 0, 0, false
			}
		}
		return start, n, true
	}
	var h diffHunk
	var ok1, ok2 bool
	h.oldStart, h.oldLines, ok1 = span(oldR[1:])
	h.newStart, h.newLines, ok2 = span(newR[1:])
	h.section = strings.TrimSpace(section)
	return h, ok1 && ok2
}

// diffStyles are the diff's colors, chosen once per theme.
type diffStyles struct {
	addBg, delBg, addEmph, delEmph mdStyle // row tints, and the changed words'
	addSign, delSign, num          mdStyle
	hunk, section, file            mdStyle
	button                         mdStyle // a file heading's [ ask ]
}

// The dark tints are hex because the 256-color palette has no green or red
// dark enough to sit behind code quietly; Bubble Tea downsamples them on
// terminals without true color.
func newDiffStyles(isDark bool) diffStyles {
	ld := lipgloss.LightDark(isDark)
	return diffStyles{
		addBg:   mdStyle{bg: ld(lipgloss.Color("194"), lipgloss.Color("#13261a"))},
		delBg:   mdStyle{bg: ld(lipgloss.Color("224"), lipgloss.Color("#2b1517"))},
		addEmph: mdStyle{bg: ld(lipgloss.Color("157"), lipgloss.Color("#22482d"))},
		delEmph: mdStyle{bg: ld(lipgloss.Color("217"), lipgloss.Color("#58252a"))},
		addSign: mdStyle{fg: ld(lipgloss.Color("28"), lipgloss.Color("114"))},
		delSign: mdStyle{fg: ld(lipgloss.Color("160"), lipgloss.Color("203"))},
		num:     mdStyle{fg: ld(lipgloss.Color("245"), lipgloss.Color("243"))},
		hunk:    mdStyle{fg: ld(lipgloss.Color("31"), lipgloss.Color("80"))},
		section: mdStyle{fg: ld(lipgloss.Color("245"), lipgloss.Color("243"))},
		file:    mdStyle{bold: true, fg: ld(lipgloss.Color("97"), lipgloss.Color("141"))},
		button:  mdStyle{bold: true, fg: ld(lipgloss.Color("25"), lipgloss.Color("117"))},
	}
}

// diffOpts is how a diff is drawn.
type diffOpts struct {
	width   int  // columns each row gets
	maxRows int  // rows to draw before stopping; 0 draws them all
	files   bool // a heading per file, for a patch that may span several
	wrap    bool // wrap long lines rather than cut them
}

// diffView draws patch and reports how many of its lines (hunk headers
// included) it left out to stay within maxRows.
func (r *renderer) diffView(patch string, o diffOpts) (string, int) {
	files := parsePatch(patch)
	width := max(o.width, 8)
	numW := diffNumWidth(files)
	var rows []string
	left := 0 // lines not drawn once full
	for fi := range files {
		f := &files[fi]
		budget := -1
		if o.maxRows > 0 {
			budget = max(0, o.maxRows-len(rows))
		}
		if o.files {
			if budget == 0 {
				left++
			} else {
				rows = append(rows, r.ds.file.render(truncateWidth(fileHeading(f), width)))
				if budget > 0 {
					budget--
				}
			}
		}
		fr, l := r.fileRows(f, numW, width, o.wrap, budget)
		rows, left = append(rows, fr...), left+l
	}
	return strings.Join(rows, "\n"), left
}

// diffNumWidth is the digits the line numbers of files take, so every
// file's rows line up.
func diffNumWidth(files []diffFile) int {
	numW := 1
	for _, f := range files {
		for _, h := range f.hunks {
			numW = max(numW, len(strconv.Itoa(h.oldStart+h.oldLines)), len(strconv.Itoa(h.newStart+h.newLines)))
		}
	}
	return numW
}

// fileRows draws one file's hunks, at most budget rows of them (all, when
// budget is negative), and reports how many lines it left out.
func (r *renderer) fileRows(f *diffFile, numW, width int, wrap bool, budget int) ([]string, int) {
	ds := &r.ds
	var rows []string
	full := func() bool { return budget >= 0 && len(rows) >= budget }
	left := 0
	if f.binary && len(f.hunks) == 0 {
		if full() {
			left++
		} else {
			rows = append(rows, ds.section.render("binary file"))
		}
	}
	lang := filepath.Base(f.path())
	for hi := range f.hunks {
		h := &f.hunks[hi]
		if full() {
			left += 1 + len(h.lines)
			continue
		}
		head := fmt.Sprintf("@@ -%d,%d +%d,%d @@", h.oldStart, h.oldLines, h.newStart, h.newLines)
		row := ds.hunk.render(head)
		if h.section != "" && len(head)+1 < width {
			row += " " + ds.section.render(truncateWidth(h.section, width-len(head)-1))
		}
		rows = append(rows, row)
		cells := r.hunkCells(h, lang)
		for li, l := range h.lines {
			if full() {
				left += len(h.lines) - li
				break
			}
			rows = append(rows, r.diffRows(l, cells[li], numW, width, wrap)...)
		}
	}
	if budget >= 0 && len(rows) > budget {
		rows = rows[:budget] // a wrapped line ran past the end
	}
	return rows, left
}

// counts are the lines f adds and removes.
func (f *diffFile) counts() (adds, dels int) {
	for _, h := range f.hunks {
		for _, l := range h.lines {
			switch l.kind {
			case '+':
				adds++
			case '-':
				dels++
			}
		}
	}
	return adds, dels
}

// status is git's letter for what happened to f: added, deleted, renamed
// or modified.
func (f *diffFile) status() byte {
	switch {
	case f.oldPath == "" && f.newPath != "":
		return 'A'
	case f.newPath == "" && f.oldPath != "":
		return 'D'
	case f.oldPath != f.newPath:
		return 'R'
	}
	return 'M'
}

// fileHeading names a file in a multi-file diff, and what happened to it.
func fileHeading(f *diffFile) string {
	switch {
	case f.oldPath == "" && f.newPath != "":
		return f.newPath + " (new)"
	case f.newPath == "" && f.oldPath != "":
		return f.oldPath + " (deleted)"
	case f.oldPath != f.newPath:
		return f.oldPath + " → " + f.newPath
	}
	return f.path()
}

// hunkCells highlights a hunk's lines. Each side is lexed whole, so a
// string or comment that spans lines colors right, and a removed line
// paired with the added line that replaced it marks the words that changed.
func (r *renderer) hunkCells(h *diffHunk, lang string) [][]cell {
	var oldSrc, newSrc []string
	for _, l := range h.lines {
		text := expandTabs(l.text, 4)
		if l.kind != '+' {
			oldSrc = append(oldSrc, text)
		}
		if l.kind != '-' {
			newSrc = append(newSrc, text)
		}
	}
	lexSide := func(src []string) [][]cell {
		if len(src) == 0 {
			return nil
		}
		return lexCells(strings.Join(src, "\n"), lang, &r.md)
	}
	oldCells, newCells := lexSide(oldSrc), lexSide(newSrc)
	out := make([][]cell, len(h.lines))
	oi, ni := 0, 0
	for i, l := range h.lines {
		switch l.kind {
		case '-':
			out[i] = lineCells(oldCells, oi)
			oi++
		case '+':
			out[i] = lineCells(newCells, ni)
			ni++
		default:
			out[i] = lineCells(newCells, ni)
			oi, ni = oi+1, ni+1
		}
	}
	// A run of removed lines followed by a run of added ones is a change:
	// pair them in order.
	emph := map[emphKey]*mdStyle{}
	for i := 0; i < len(h.lines); {
		if h.lines[i].kind != '-' {
			i++
			continue
		}
		dels := i
		for i < len(h.lines) && h.lines[i].kind == '-' {
			i++
		}
		adds := i
		for i < len(h.lines) && h.lines[i].kind == '+' {
			i++
		}
		for k := 0; k < min(adds-dels, i-adds); k++ {
			r.markChanged(out[dels+k], out[adds+k], emph)
		}
	}
	return out
}

func lineCells(lines [][]cell, i int) []cell {
	if i < len(lines) {
		return lines[i]
	}
	return nil
}

// markChanged puts the words that differ between a removed line and the
// added line that replaced it on the stronger tint. Lines that share too
// little are left alone: marking all of both says nothing.
func (r *renderer) markChanged(old, new []cell, emph map[emphKey]*mdStyle) {
	a, b := diffTokens(old), diffTokens(new)
	const maxTokens = 400 // the table is quadratic
	if len(a) == 0 || len(b) == 0 || len(a) > maxTokens || len(b) > maxTokens {
		return
	}
	keepA, keepB := lcsTokens(a, b)
	same, total := 0, 0
	for i, t := range a {
		if !t.space {
			total += t.end - t.start
			if keepA[i] {
				same += t.end - t.start
			}
		}
	}
	for i, t := range b {
		if !t.space {
			total += t.end - t.start
			if keepB[i] {
				same += t.end - t.start
			}
		}
	}
	if total == 0 || same == total || same*5 < total*2 { // under 40% kept
		return
	}
	mark := func(cs []cell, toks []diffToken, keep []bool, bg *mdStyle) {
		for i, t := range toks {
			if keep[i] {
				continue
			}
			for j := t.start; j < t.end; j++ {
				cs[j].st = emphStyle(cs[j].st, bg, emph)
			}
		}
	}
	mark(old, a, keepA, &r.ds.delEmph)
	mark(new, b, keepB, &r.ds.addEmph)
}

// emphKey is a syntax style laid on one of the emphasis tints.
type emphKey struct {
	st *mdStyle
	bg *mdStyle
}

// emphStyle is st on the emphasis tint bg, made once per pair so runs of
// cells still share a pointer (renderCells breaks runs on it).
func emphStyle(st, bg *mdStyle, cache map[emphKey]*mdStyle) *mdStyle {
	k := emphKey{st, bg}
	if e, ok := cache[k]; ok {
		return e
	}
	e := bg.on(mdStyle{})
	if st != nil {
		e = bg.on(*st)
	}
	cache[k] = &e
	return &e
}

// diffToken is a word, a run of spaces, or one other character: the
// units a changed line is compared in. start and end index its cells.
type diffToken struct {
	text       string
	start, end int
	space      bool
}

func diffTokens(cs []cell) []diffToken {
	class := func(g string) int {
		r, _ := utf8.DecodeRuneInString(g)
		switch {
		case unicode.IsSpace(r):
			return 1
		case r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r):
			return 2
		}
		return 3
	}
	var toks []diffToken
	for i := 0; i < len(cs); {
		c := class(cs[i].g)
		j := i + 1
		for c != 3 && j < len(cs) && class(cs[j].g) == c {
			j++
		}
		var b strings.Builder
		for _, x := range cs[i:j] {
			b.WriteString(x.g)
		}
		toks = append(toks, diffToken{text: b.String(), start: i, end: j, space: c == 1})
		i = j
	}
	return toks
}

// lcsTokens marks the tokens of a and b that are in their longest common
// subsequence.
func lcsTokens(a, b []diffToken) (keepA, keepB []bool) {
	n, m := len(a), len(b)
	t := make([]int, (n+1)*(m+1))
	at := func(i, j int) *int { return &t[i*(m+1)+j] }
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i].text == b[j].text {
				*at(i, j) = *at(i+1, j+1) + 1
			} else {
				*at(i, j) = max(*at(i+1, j), *at(i, j+1))
			}
		}
	}
	keepA, keepB = make([]bool, n), make([]bool, m)
	for i, j := 0, 0; i < n && j < m; {
		switch {
		case a[i].text == b[j].text:
			keepA[i], keepB[j] = true, true
			i, j = i+1, j+1
		case *at(i+1, j) >= *at(i, j+1):
			i++
		default:
			j++
		}
	}
	return keepA, keepB
}

// diffRows draws one line of a hunk: its number (the new side's, or the
// old side's for a removed line), its sign, then its code, as one row or,
// wrapping, several.
func (r *renderer) diffRows(l diffLine, cs []cell, numW, width int, wrap bool) []string {
	ds := &r.ds
	var bg, sign mdStyle
	num := l.new
	switch l.kind {
	case '+':
		bg, sign = ds.addBg, ds.addSign.on(ds.addBg)
	case '-':
		bg, sign, num = ds.delBg, ds.delSign.on(ds.delBg), l.old
	}
	gutterW := numW + 3 // "12 + "
	codeW := max(1, width-gutterW)
	var lines [][]cell
	switch {
	case cellsWidth(cs) <= codeW:
		lines = [][]cell{cs}
	case wrap:
		lines = hardWrapCells(cs, codeW)
	default:
		lines = [][]cell{append(cutCells(cs, codeW-1), cell{g: "…", w: 1, st: &ds.num})}
	}
	rows := make([]string, len(lines))
	for i, line := range lines {
		n := strings.Repeat(" ", numW)
		if i == 0 {
			n = fmt.Sprintf("%*d", numW, num)
		}
		s := " "
		if i == 0 && l.kind != ' ' {
			s = string(l.kind)
		}
		row := ds.num.on(bg).render(n+" ") + sign.render(s) + bg.render(" ") + renderCells(line, bg)
		if l.kind != ' ' {
			// A tinted row runs the full width, so a change reads as a band.
			row += bg.render(strings.Repeat(" ", max(0, codeW-cellsWidth(line))))
		}
		rows[i] = row
	}
	return rows
}

// cutCells keeps the cells of cs that fit in width columns.
func cutCells(cs []cell, width int) []cell {
	w := 0
	for i, c := range cs {
		if w+c.w > width {
			return cs[:i:i]
		}
		w += c.w
	}
	return cs
}
