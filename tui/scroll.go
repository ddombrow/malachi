package tui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// wheelLines is how far one notch of the mouse wheel scrolls.
const wheelLines = 3

// scroller is the window onto the transcript: the lines to show and how far
// down them the view is. It holds the lines it is given rather than a copy,
// and looks only at the ones on screen, so setting them costs nothing however
// long the session has run. (bubbles' viewport measures every line each time
// its content is set, which made each keystroke and each streamed token cost
// time in proportion to the whole transcript.)
type scroller struct {
	width, height int
	// head is the finished transcript and tail the live area under it.
	head, tail []string
	yOffset    int
}

// SetLines replaces the content: head followed by tail. Blank lines at the
// very top are dropped. A view left past the end goes to the bottom.
func (s *scroller) SetLines(head, tail []string) {
	for len(head) > 0 && head[0] == "" {
		head = head[1:]
	}
	if len(head) == 0 {
		for len(tail) > 0 && tail[0] == "" {
			tail = tail[1:]
		}
	}
	s.head, s.tail = head, tail
	s.clamp()
}

func (s *scroller) SetWidth(w int)  { s.width = w }
func (s *scroller) SetHeight(h int) { s.height = h }
func (s *scroller) Width() int      { return s.width }
func (s *scroller) Height() int     { return s.height }
func (s *scroller) YOffset() int    { return s.yOffset }

// TotalLineCount is how many lines there are to scroll through.
func (s *scroller) TotalLineCount() int { return len(s.head) + len(s.tail) }

// Line returns content line i.
func (s *scroller) Line(i int) string {
	if i < len(s.head) {
		return s.head[i]
	}
	return s.tail[i-len(s.head)]
}

func (s *scroller) maxYOffset() int { return max(0, s.TotalLineCount()-s.height) }

func (s *scroller) clamp() { s.yOffset = max(0, min(s.yOffset, s.maxYOffset())) }

// AtBottom reports whether the last line is in view.
func (s *scroller) AtBottom() bool { return s.yOffset >= s.maxYOffset() }

func (s *scroller) GotoTop()         { s.yOffset = 0 }
func (s *scroller) GotoBottom()      { s.yOffset = s.maxYOffset() }
func (s *scroller) ScrollUp(n int)   { s.yOffset -= n; s.clamp() }
func (s *scroller) ScrollDown(n int) { s.yOffset += n; s.clamp() }
func (s *scroller) PageUp()          { s.ScrollUp(s.height) }
func (s *scroller) PageDown()        { s.ScrollDown(s.height) }

// View renders exactly height rows, each exactly width cells: lines too long
// are cut, short ones and the rows past the end are padded with spaces.
func (s *scroller) View() string {
	if s.width <= 0 || s.height <= 0 {
		return ""
	}
	rows := make([]string, s.height)
	total := s.TotalLineCount()
	for i := range rows {
		var line string
		if n := s.yOffset + i; n < total {
			// A tab has no fixed width, so it would break the padding.
			line = strings.ReplaceAll(s.Line(n), "\t", "    ")
		}
		w := ansi.StringWidth(line)
		if w > s.width {
			line = ansi.Cut(line, 0, s.width)
			w = ansi.StringWidth(line)
		}
		rows[i] = line + strings.Repeat(" ", max(0, s.width-w))
	}
	return strings.Join(rows, "\n")
}
