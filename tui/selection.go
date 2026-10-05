package tui

import (
	"os/exec"
	"runtime"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// The TUI captures the mouse for wheel scrolling, which means a plain drag
// never reaches the terminal's own selection. So selection is done here:
// drag in the transcript to highlight, release to copy. Positions are in
// content coordinates (transcript line, cell column), so a selection stays
// put while the view scrolls or new output streams in below it.

// textPos is a cell in the viewport's content.
type textPos struct{ line, col int }

func (p textPos) before(q textPos) bool {
	return p.line < q.line || (p.line == q.line && p.col < q.col)
}

type selection struct {
	anchor, head textPos
	dragging     bool // button is down
	active       bool // the drag moved, so there is a range to show/copy
}

// bounds returns the selection ordered start-to-end.
func (s selection) bounds() (start, end textPos) {
	if s.head.before(s.anchor) {
		return s.head, s.anchor
	}
	return s.anchor, s.head
}

// span returns the half-open cell range [c0, c1) selected on content line
// i, or ok=false if the line is outside the selection. gutter is how many
// leading columns hold an icon or indentation rather than text (0 for lines
// without one); those are never selected.
func (s selection) span(i, lineWidth, gutter int) (c0, c1 int, ok bool) {
	if !s.active {
		return 0, 0, false
	}
	start, end := s.bounds()
	if i < start.line || i > end.line {
		return 0, 0, false
	}
	c0, c1 = 0, lineWidth
	if i == start.line {
		c0 = start.col
	}
	if i == end.line {
		c1 = min(c1, end.col+1)
	}
	c0 = max(c0, gutter)
	return c0, c1, c1 > c0
}

// lineGutter returns how many leading columns of a plain-text transcript
// line are icon gutter: the gutter width when those columns are blank or an
// icon, else 0 (the banner, for example, has no gutter).
func (m *model) lineGutter(plain string) int {
	w := m.r.gutterWidth(iconReply)
	head := strings.TrimSpace(ansi.Cut(plain, 0, w))
	if head == "" {
		return w
	}
	for _, glyph := range m.r.icons {
		if head == glyph {
			return w
		}
	}
	return 0
}

// contentPos converts a screen cell in the viewport to a content position.
func (m *model) contentPos(x, y int) textPos {
	y = max(0, min(y, m.vp.Height()-1))
	line := min(m.vp.YOffset()+y, max(0, m.vp.TotalLineCount()-1))
	return textPos{line: line, col: max(0, x)}
}

// handleMouse runs selection on left-button drags inside the viewport.
func (m *model) handleMouse(msg tea.MouseMsg) tea.Cmd {
	mouse := msg.Mouse()
	switch msg.(type) {
	case tea.MouseClickMsg:
		m.sel = selection{}
		if mouse.Button != tea.MouseLeft || mouse.Y >= m.vp.Height() {
			return nil
		}
		p := m.contentPos(mouse.X, mouse.Y)
		m.sel = selection{anchor: p, head: p, dragging: true}
	case tea.MouseMotionMsg:
		if !m.sel.dragging {
			return nil
		}
		// Dragging past the top or bottom edge scrolls, so a selection can
		// be longer than the screen.
		switch {
		case mouse.Y < 0 || (mouse.Y == 0 && m.vp.YOffset() > 0):
			m.vp.ScrollUp(1)
		case mouse.Y >= m.vp.Height():
			m.vp.ScrollDown(1)
		}
		m.sel.head = m.contentPos(mouse.X, mouse.Y)
		m.sel.active = m.sel.head != m.sel.anchor
	case tea.MouseReleaseMsg:
		if !m.sel.dragging {
			return nil
		}
		m.sel.dragging = false
		if !m.sel.active {
			m.sel = selection{} // a click, not a drag
			return nil
		}
		text := m.selectedText()
		if text == "" {
			m.sel = selection{}
			return nil
		}
		return m.copyText(text, "copied selection")
	}
	return nil
}

// selectedText returns the selection as plain text: styling stripped, icon
// gutter removed, trailing blanks trimmed.
func (m *model) selectedText() string {
	if !m.sel.active {
		return ""
	}
	lines := strings.Split(m.vp.GetContent(), "\n")
	start, end := m.sel.bounds()
	var out []string
	for i := start.line; i <= end.line && i < len(lines); i++ {
		plain := ansi.Strip(lines[i])
		c0, c1, ok := m.sel.span(i, ansi.StringWidth(plain), m.lineGutter(plain))
		if !ok {
			out = append(out, "")
			continue
		}
		out = append(out, strings.TrimRight(ansi.Cut(plain, c0, c1), " "))
	}
	return strings.Trim(strings.Join(out, "\n"), "\n")
}

// highlightSelection paints the selection onto the rendered viewport rows.
func (m *model) highlightSelection(view string) string {
	if !m.sel.active {
		return view
	}
	rows := strings.Split(view, "\n")
	style := lipgloss.NewStyle().Reverse(true)
	for i, row := range rows {
		plain := ansi.Strip(row)
		c0, c1, ok := m.sel.span(m.vp.YOffset()+i, ansi.StringWidth(plain), m.lineGutter(plain))
		if ok {
			rows[i] = lipgloss.StyleRanges(row, lipgloss.NewRange(c0, c1, style))
		}
	}
	return strings.Join(rows, "\n")
}

// copyText puts text on the clipboard two ways: OSC 52 through the terminal
// (works over SSH, needs terminal support) and the OS clipboard tool when
// one is installed (works where OSC 52 is off, e.g. Terminal.app). The
// status bar confirms briefly.
func (m *model) copyText(text, note string) tea.Cmd {
	m.flash, m.flashUntil = note, time.Now().Add(2*time.Second)
	return tea.Batch(tea.SetClipboard(text), nativeCopy(text))
}

// nativeCopy pipes text to the platform clipboard tool, if any.
func nativeCopy(text string) tea.Cmd {
	return func() tea.Msg {
		for _, argv := range clipboardTools {
			if _, err := exec.LookPath(argv[0]); err != nil {
				continue
			}
			cmd := exec.Command(argv[0], argv[1:]...)
			cmd.Stdin = strings.NewReader(text)
			if cmd.Run() == nil {
				return nil
			}
		}
		return nil
	}
}

// clipboardTools are tried in order; tests replace them so running the
// suite never touches the real clipboard.
var clipboardTools = clipboardCommands()

func clipboardCommands() [][]string {
	switch runtime.GOOS {
	case "darwin":
		return [][]string{{"pbcopy"}}
	case "windows":
		return [][]string{{"clip"}}
	}
	return [][]string{{"wl-copy"}, {"xclip", "-selection", "clipboard"}, {"xsel", "--clipboard", "--input"}}
}
