package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/ddombrow/malachi/agent"
)

func TestScrollerViewIsExactlyItsSize(t *testing.T) {
	var s scroller
	s.SetWidth(6)
	s.SetHeight(3)
	s.SetLines([]string{"", "ab", "a\tb", "\x1b[1mlong line here\x1b[0m"}, []string{"", "世界世界"})
	if got := s.TotalLineCount(); got != 5 {
		t.Fatalf("leading blank line kept: %d lines", got)
	}
	for _, off := range []int{0, 1, 2, 99} {
		s.yOffset = off
		s.clamp()
		rows := strings.Split(s.View(), "\n")
		if len(rows) != 3 {
			t.Fatalf("offset %d: %d rows, want 3", off, len(rows))
		}
		for _, r := range rows {
			if w := lipgloss.Width(r); w != 6 {
				t.Fatalf("offset %d: row %q is %d cells, want 6", off, r, w)
			}
		}
	}
	if !s.AtBottom() || s.YOffset() != 2 {
		t.Fatalf("an offset past the end must clamp to the bottom, got %d", s.YOffset())
	}
}

func TestScrollerScrolling(t *testing.T) {
	var s scroller
	s.SetWidth(10)
	s.SetHeight(4)
	lines := strings.Split(strings.Repeat("line\n", 19)+"line", "\n")
	s.SetLines(lines[:15], lines[15:])
	s.GotoBottom()
	if s.YOffset() != 16 || !s.AtBottom() {
		t.Fatalf("bottom = %d", s.YOffset())
	}
	s.PageUp()
	s.ScrollUp(1)
	if s.YOffset() != 11 || s.AtBottom() {
		t.Fatalf("after page up and one line: %d", s.YOffset())
	}
	s.ScrollUp(100)
	if s.YOffset() != 0 {
		t.Fatalf("scrolled above the top: %d", s.YOffset())
	}
	s.PageDown()
	s.ScrollDown(100)
	if !s.AtBottom() {
		t.Fatal("scrolling down must stop at the bottom")
	}
	// Content shrinking under a scrolled view (the live area emptying) must
	// not leave it past the end.
	s.SetLines(lines[:6], nil)
	if s.YOffset() != 2 {
		t.Fatalf("offset after shrink = %d, want 2", s.YOffset())
	}
}

// A tab in streaming text used to be counted as no width when wrapping and
// drawn as four cells, so the line ran past the edge of the transcript.
func TestStreamingTabsStayWithinTheWidth(t *testing.T) {
	m := newTestModel(t)
	partial := agent.NewAssistantMessage("m")
	partial.Content = []agent.Content{&agent.TextContent{Text: strings.Repeat("a\tb ", 80)}}
	m.Update(sessionEventsMsg{&agent.MessageUpdateEvent{Message: partial}})
	for _, l := range strings.Split(m.live(), "\n") {
		if strings.Contains(l, "\t") {
			t.Fatalf("tab left in live line %q", l)
		}
		if w := lipgloss.Width(l); w > m.contentWidth() {
			t.Fatalf("live line is %d cells in a %d-cell transcript: %q", w, m.contentWidth(), l)
		}
	}
}

func TestQueuedEventsAreHandledInOneUpdate(t *testing.T) {
	m := newTestModel(t)
	for _, text := range []string{"one", "two", "three"} {
		m.bridge.ch <- &agent.MessageEndEvent{Message: agent.NewUserText(text)}
	}
	msg, ok := m.bridge.next()().(sessionEventsMsg)
	if !ok || len(msg) != 3 {
		t.Fatalf("next() = %#v, want the three queued events together", msg)
	}
	out := shown(m, func() tea.Cmd { _, cmd := m.Update(msg); return cmd })
	if i, j, k := strings.Index(out, "one"), strings.Index(out, "two"), strings.Index(out, "three"); i < 0 || j < i || k < j {
		t.Fatalf("events out of order or missing:\n%s", out)
	}
}
