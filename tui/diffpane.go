package tui

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/coding"
)

// The diff pane shows what changed in the repository since its last
// commit: a summary of the files at the top, then each file's diff under
// a heading of its own, whose [ ask ] puts that diff in the prompt. On a terminal wide enough to split it takes the right half, in
// place of the sidebar; on a narrower one it covers the transcript. What
// it shows is the session's (coding.Session.WorkingTreeDiff), fetched when
// the pane opens and again after each tool finishes, since any of them may
// have written a file. Fetching and drawing both run off the UI's
// goroutine: a large diff takes git, and the highlighter, a while.
//
// While the pane is open the scroll keys move it; the mouse wheel scrolls
// whichever side it is over.

const keyDiff = "ctrl+g"

// diffFetchTimeout bounds one fetch, so a wedged git cannot leave the pane
// loading for good.
const diffFetchTimeout = 30 * time.Second

type diffPane struct {
	open bool

	// fetchGen numbers fetches; a result from any but the latest is
	// dropped. again asks for another once the running one is in, since
	// files changed while it ran.
	fetchGen        int
	fetching, again bool

	loaded bool // a fetch has come back
	diff   coding.WorkingTreeDiff
	err    error

	// lines are the diff drawn drawnWidth columns wide; a draw at
	// drawingWidth is under way, numbered drawGen.
	lines                    []string
	drawnWidth, drawingWidth int
	drawGen                  int
	files                    []paneFile

	sc scroller
}

// paneFile is one file of the drawn diff.
type paneFile struct {
	path       string // its name, or "old → new" for a rename
	status     byte   // A, D, R or M
	adds, dels int
	raw        string // its part of the patch, for [ ask ]
	row        int    // its name's row in the pane's lines, between two rules
	untracked  bool
}

// askLabel sits at the right of each file's heading; clicking it asks about
// that file's diff.
const askLabel = "[ ask ]"

// maxAskBytes caps the diff [ ask ] puts in a prompt.
const maxAskBytes = 64 << 10

type diffFetchedMsg struct {
	gen  int
	diff coding.WorkingTreeDiff
	err  error
}

type diffDrawnMsg struct {
	gen, width int
	lines      []string
	files      []paneFile
}

// diffSplit reports whether the pane goes beside the transcript rather
// than over it.
func (m *model) diffSplit() bool { return m.width >= sidebarMinWidth }

// diffPaneWidth is the columns the pane takes, its border included.
func (m *model) diffPaneWidth() int {
	if m.diffSplit() {
		return m.width / 2
	}
	return m.width
}

// diffInnerWidth is the columns the diff itself gets.
func (m *model) diffInnerWidth() int {
	if m.diffSplit() {
		return m.diffPaneWidth() - lipgloss.Width(sidebarBorder)
	}
	return m.width
}

// diffCovers reports whether the pane hides the transcript.
func (m *model) diffCovers() bool { return m.dp.open && !m.diffSplit() }

func (m *model) toggleDiff() tea.Cmd {
	m.dp.open = !m.dp.open
	m.r = newRenderer(m.contentWidth(), m.isDark, m.s.Settings().Icons, m.s.Cwd())
	m.sel = selection{}
	if !m.dp.open {
		return nil
	}
	m.dp.sc.GotoTop()
	return m.fetchDiff()
}

// fetchDiff asks the session for the working tree's diff, or, with a fetch
// already running, for another once it is in.
func (m *model) fetchDiff() tea.Cmd {
	if !m.dp.open {
		return nil
	}
	if m.dp.fetching {
		m.dp.again = true
		return nil
	}
	m.dp.fetching = true
	m.dp.fetchGen++
	gen, s := m.dp.fetchGen, m.s
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), diffFetchTimeout)
		defer cancel()
		d, err := s.WorkingTreeDiff(ctx)
		return diffFetchedMsg{gen: gen, diff: d, err: err}
	}
}

func (m *model) diffFetched(msg diffFetchedMsg) tea.Cmd {
	m.dp.fetching = false
	var cmds []tea.Cmd
	if msg.gen == m.dp.fetchGen {
		old := m.dp.diff
		changed := !m.dp.loaded || msg.diff.Patch != old.Patch || msg.diff.NoRepo != old.NoRepo ||
			msg.diff.Truncated != old.Truncated || !slices.Equal(msg.diff.Untracked, old.Untracked) ||
			(msg.err == nil) != (m.dp.err == nil)
		m.dp.loaded, m.dp.diff, m.dp.err = true, msg.diff, msg.err
		if changed {
			m.dp.drawnWidth, m.dp.drawingWidth = 0, 0 // redraw, at whatever width
			cmds = append(cmds, m.drawDiff())
		}
	}
	if m.dp.again {
		m.dp.again = false
		cmds = append(cmds, m.fetchDiff())
	}
	return tea.Batch(cmds...)
}

// drawDiff draws the fetched patch at the pane's width, unless it is drawn
// or being drawn at that width already.
func (m *model) drawDiff() tea.Cmd {
	width := m.diffInnerWidth()
	if !m.dp.open || !m.dp.loaded || width <= 0 || width == m.dp.drawnWidth || width == m.dp.drawingWidth {
		return nil
	}
	m.dp.drawGen++
	m.dp.drawingWidth = width
	gen, r, patch := m.dp.drawGen, m.r, m.dp.diff.Patch
	untracked := map[string]bool{}
	for _, p := range m.dp.diff.Untracked {
		untracked[p] = true
	}
	return func() tea.Msg {
		// The renderer's styles are only read here, so drawing beside the
		// UI's own use of it is safe.
		lines, files := r.diffPaneRows(patch, untracked, width)
		return diffDrawnMsg{gen: gen, width: width, lines: lines, files: files}
	}
}

func (m *model) diffDrawn(msg diffDrawnMsg) {
	if msg.gen != m.dp.drawGen {
		return
	}
	m.dp.lines, m.dp.files = msg.lines, msg.files
	m.dp.drawnWidth, m.dp.drawingWidth = msg.width, 0
}

// diffPaneRows draws each file of patch under a heading of its own: its
// name between two rules. The first file goes without the upper one: the
// rule under the summary is already there.
func (r *renderer) diffPaneRows(patch string, untracked map[string]bool, width int) ([]string, []paneFile) {
	parsed := parsePatch(patch)
	numW := diffNumWidth(parsed)
	rule := r.st.dim.Render(strings.Repeat("─", width))
	var rows []string
	files := make([]paneFile, len(parsed))
	for i := range parsed {
		f := &parsed[i]
		adds, dels := f.counts()
		if i > 0 {
			rows = append(rows, rule)
		}
		files[i] = paneFile{path: fileName(f), status: f.status(), adds: adds, dels: dels, raw: f.raw, row: len(rows), untracked: untracked[f.path()]}
		rows = append(rows, r.fileHeadingRow(&files[i], width), rule)
		body, _ := r.fileRows(f, numW, width, true, -1)
		rows = append(rows, body...)
	}
	return rows, files
}

// fileName is how the pane names f: its path, or both for a rename.
func fileName(f *diffFile) string {
	if f.status() == 'R' {
		return f.oldPath + " → " + f.newPath
	}
	return f.path()
}

// fileState is what the heading says happened to f; nothing for a change.
func fileState(f *paneFile) string {
	switch {
	case f.untracked:
		return "untracked"
	case f.status == 'A':
		return "new"
	case f.status == 'D':
		return "deleted"
	case f.status == 'R':
		return "renamed"
	}
	return ""
}

// fileHeadingRow is a file's heading in the diff: its name, what happened
// to it, and [ ask ] at the right.
func (r *renderer) fileHeadingRow(f *paneFile, width int) string {
	ds := &r.ds
	state := ""
	if s := fileState(f); s != "" {
		state = " (" + s + ")"
	}
	room := max(1, width-len(askLabel)-ansi.StringWidth(state)-1)
	name := ds.file.render(truncateLeft(f.path, room)) + ds.section.render(state)
	gap := max(1, width-ansi.StringWidth(name)-len(askLabel))
	return name + strings.Repeat(" ", gap) + ds.button.render(askLabel)
}

// diffEvent refreshes the pane after an event that may have changed files.
func (m *model) diffEvent(e any) tea.Cmd {
	switch e.(type) {
	case *agent.ToolExecutionEndEvent, coding.AgentSettledEvent:
		return m.fetchDiff()
	}
	return nil
}

// summaryRows is how many rows the file summary takes: a row per file, up
// to a third of the pane, the last of them saying how many did not fit.
func (m *model) summaryRows() int {
	if len(m.dp.files) == 0 {
		return 0
	}
	return min(len(m.dp.files), max(1, (m.vp.Height()-1)/3))
}

// diffTop is the pane row the scrolling diff starts on: under the
// heading, the summary and the rule beneath it.
func (m *model) diffTop() int {
	if n := m.summaryRows(); n > 0 {
		return 1 + n + 1
	}
	return 1
}

// sizeDiff fits the pane's scroller to the screen; refresh calls it.
func (m *model) sizeDiff() {
	m.dp.sc.SetWidth(m.diffInnerWidth())
	m.dp.sc.SetHeight(max(0, m.vp.Height()-m.diffTop()))
	m.dp.sc.SetLines(m.dp.lines, nil)
}

// diffHeading is the pane's top row: what it shows, how much, and a close
// button at the right.
func (m *model) diffHeading() string {
	st := m.r.st
	width := m.diffInnerWidth()
	closeBtn := st.dim.Render("^g ") + st.accent.Render("×")
	text := st.accent.Render("Changes")
	switch d := m.dp; {
	case !d.loaded:
		text += st.dim.Render(" · loading…")
	case d.err != nil:
	case d.diff.NoRepo:
	case d.diff.Patch == "":
	default:
		adds, dels := 0, 0
		for _, f := range d.files {
			adds, dels = adds+f.adds, dels+f.dels
		}
		noun := "files"
		if len(d.files) == 1 {
			noun = "file"
		}
		text += st.dim.Render(fmt.Sprintf(" · %d %s ", len(d.files), noun)) +
			st.toolOK.Render(fmt.Sprintf("+%d", adds)) + " " + st.toolErr.Render(fmt.Sprintf("−%d", dels))
		if d.diff.Truncated {
			text += st.dim.Render(" · cut short")
		}
	}
	text = ansi.Truncate(text, max(0, width-lipgloss.Width(closeBtn)-1), "…")
	return text + strings.Repeat(" ", max(1, width-lipgloss.Width(text)-lipgloss.Width(closeBtn))) + closeBtn
}

// diffNote is what the pane says under its heading when there is no diff
// to scroll through, and how; "" when there is one, or nothing yet.
func (m *model) diffNote() (string, lipgloss.Style) {
	st := m.r.st
	switch d := m.dp; {
	case !d.loaded:
	case d.err != nil:
		return d.err.Error(), st.errorText
	case d.diff.NoRepo:
		return "not in a git repository", st.dim
	case d.diff.Patch == "":
		return "no uncommitted changes", st.dim
	}
	return "", st.dim
}

// diffLines are the pane's rows, each exactly its width, border included.
func (m *model) diffLines(height int) []string {
	width := m.diffInnerWidth()
	pad := func(s string) string {
		s = ansi.Truncate(s, width, "…")
		return s + strings.Repeat(" ", max(0, width-lipgloss.Width(s)))
	}
	rows := []string{pad(m.diffHeading())}
	if note, style := m.diffNote(); note != "" {
		for _, l := range wordWrap(note, width) {
			rows = append(rows, pad(style.Render(l)))
		}
	} else if v := m.dp.sc.View(); v != "" {
		rows = append(rows, m.summaryLines(width)...)
		if len(rows) > 1 {
			rows = append(rows, m.r.st.dim.Render(strings.Repeat("─", width)))
		}
		rows = append(rows, strings.Split(v, "\n")...)
	}
	for len(rows) < height {
		rows = append(rows, strings.Repeat(" ", width))
	}
	rows = rows[:height]
	if m.diffSplit() {
		border := m.r.st.dim.Render(sidebarBorder)
		for i := range rows {
			rows[i] = border + rows[i]
		}
	}
	return rows
}

// summaryLines list the files, a row each: what happened to it, its name
// and its counts. Clicking one scrolls the diff to it.
func (m *model) summaryLines(width int) []string {
	n := m.summaryRows()
	files := m.dp.files
	shown := files
	if len(files) > n {
		shown = files[:n-1]
	}
	st, ds := m.r.st, &m.r.ds
	var rows []string
	for _, f := range shown {
		letter := ds.file
		switch f.status {
		case 'A':
			letter = ds.addSign
		case 'D':
			letter = ds.delSign
		case 'R':
			letter = ds.hunk
		}
		counts := st.toolOK.Render(fmt.Sprintf("+%d", f.adds)) + " " + st.toolErr.Render(fmt.Sprintf("−%d", f.dels))
		name := truncateLeft(f.path, max(1, width-4-lipgloss.Width(counts)))
		gap := max(1, width-2-lipgloss.Width(name)-lipgloss.Width(counts))
		rows = append(rows, letter.render(string(f.status))+" "+name+strings.Repeat(" ", gap)+counts)
	}
	if len(files) > n {
		rows = append(rows, st.dim.Render(fmt.Sprintf("… %d more files", len(files)-len(shown))))
	}
	for i, r := range rows {
		rows[i] = r + strings.Repeat(" ", max(0, width-lipgloss.Width(r)))
	}
	return rows
}

// diffHit reports whether (x, y) is on the pane.
func (m *model) diffHit(x, y int) bool {
	return m.dp.open && y >= 0 && y < m.vp.Height() && (m.diffCovers() || x >= m.contentWidth())
}

// diffClick handles a click on the pane: the close button closes it, a
// file in the summary scrolls the diff to it, and a file's [ ask ] asks
// about it.
func (m *model) diffClick(x, y int) tea.Cmd {
	width := m.diffInnerWidth()
	x -= m.width - width // the pane's own column
	n, top := m.summaryRows(), m.diffTop()
	switch {
	case y == 0:
		if x >= width-lipgloss.Width("^g ×") {
			return m.toggleDiff()
		}
	case y <= n:
		if i := y - 1; i < len(m.dp.files) && (len(m.dp.files) <= n || i < n-1) {
			m.dp.sc.SetYOffset(m.dp.files[i].row - 1) // from its upper rule
		}
	case y >= top:
		row := m.dp.sc.YOffset() + y - top
		for i, f := range m.dp.files {
			if f.row == row && x >= width-len(askLabel) {
				return m.askDiff(i)
			}
		}
	}
	return nil
}

// askDiff puts a file's diff in the prompt, as a marker standing for it
// the way a long paste does, for the user to ask about.
func (m *model) askDiff(i int) tea.Cmd {
	f := m.dp.files[i]
	raw := f.raw
	if len(raw) > maxAskBytes {
		raw = raw[:strings.LastIndexByte(raw[:maxAskBytes], '\n')+1] + "… (diff cut short)\n"
	}
	marker := "[diff " + f.path + "]"
	content := fmt.Sprintf("the uncommitted changes to %s:\n\n```diff\n%s```\n", f.path, raw)
	m.input.InsertString(marker + " ")
	m.pastes = append(m.pastes, pastedText{marker: marker, content: content})
	return m.input.Focus()
}

// diffKey scrolls the pane with the transcript's scroll keys.
func (m *model) diffKey(key string) bool {
	sc := &m.dp.sc
	switch key {
	case "pgup":
		sc.PageUp()
	case "pgdown":
		sc.PageDown()
	case "shift+up":
		sc.ScrollUp(3)
	case "shift+down":
		sc.ScrollDown(3)
	case "ctrl+home":
		sc.GotoTop()
	case "ctrl+end":
		sc.GotoBottom()
	default:
		return false
	}
	return true
}
