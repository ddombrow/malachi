package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/ddombrow/malachi/agent"
)

func TestPastedTextIsCollapsedInEditorAndSubmittedWhole(t *testing.T) {
	m := newTestModel(t)
	pasted := strings.Join([]string{
		"first line", "second line", "third line", "fourth line", "fifth line",
		"sixth line", "seventh line", "eighth line", "ninth line", "tenth line",
		"eleventh line", "twelfth line",
	}, "\n")
	events := make(chan any, 32)
	unsubscribe := m.s.Subscribe(func(e any) { events <- e })
	defer unsubscribe()

	m.Update(tea.PasteMsg{Content: pasted})
	marker := "[Pasted 12 Lines]"
	if got := m.input.Value(); got != marker {
		t.Fatalf("editor value = %q, want %q", got, marker)
	}

	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	deadline := time.After(5 * time.Second)
	for {
		select {
		case e := <-events:
			end, ok := e.(*agent.MessageEndEvent)
			if !ok {
				continue
			}
			user, ok := end.Message.(*agent.UserMessage)
			if !ok {
				continue
			}
			if got := user.Content.String(); got != pasted {
				t.Fatalf("submitted user message = %q, want full paste %q", got, pasted)
			}
			return
		case <-deadline:
			t.Fatal("timed out waiting for submitted user message")
		}
	}
}

func TestMultilinePasteExpandsInPlace(t *testing.T) {
	m := newTestModel(t)
	m.input.SetValue("left right")
	m.input.SetCursorColumn(5)
	pasted := "one\ntwo\nthree"
	m.Update(tea.PasteMsg{Content: pasted})
	// Replacing a selection/cursor position keeps surrounding prompt text in
	// place; only the paste body is hidden behind the marker.
	visible := m.input.Value()
	if visible != "left [Pasted 3 Lines]right" {
		t.Fatalf("visible input = %q", visible)
	}
	if got, want := m.expandPastes(visible), "left "+pasted+"right"; got != want {
		t.Fatalf("expanded input = %q, want %q", got, want)
	}
}

func TestPastedLineCount(t *testing.T) {
	for _, tc := range []struct {
		text string
		want int
	}{
		{"", 0},
		{"one", 1},
		{"one\ntwo", 2},
		{"one\ntwo\n", 2},
		{"one\r\ntwo", 2},
	} {
		if got := pastedLineCount(tc.text); got != tc.want {
			t.Errorf("pastedLineCount(%q) = %d, want %d", tc.text, got, tc.want)
		}
	}
}

func TestUserMessageKeepsCopiedLineEndingsAsTranscriptLines(t *testing.T) {
	r := newRenderer(100, true, "emoji", "")
	out := strings.TrimLeft(ansi.Strip(r.userMessage("first\rsecond\r\nthird")), "\n")
	lines := strings.Split(out, "\n")
	if len(lines) != 3 {
		t.Fatalf("rendered pasted message as %d lines, want 3: %q", len(lines), out)
	}
	for i, want := range []string{"first", "second", "third"} {
		if !strings.Contains(lines[i], want) {
			t.Errorf("transcript line %d = %q, want %q", i+1, lines[i], want)
		}
	}
}

func TestUserMessagePreservesPastedWhitespace(t *testing.T) {
	r := newRenderer(100, true, "emoji", "")
	out := strings.TrimLeft(ansi.Strip(r.userMessage("  first  line\n    indented  text")), "\n")
	lines := strings.Split(out, "\n")
	if len(lines) != 2 {
		t.Fatalf("rendered pasted message as %d lines, want 2: %q", len(lines), out)
	}
	if !strings.HasSuffix(lines[0], "  first  line") {
		t.Errorf("first line lost its spacing: %q", lines[0])
	}
	if !strings.HasSuffix(lines[1], "    indented  text") {
		t.Errorf("indented line lost its spacing: %q", lines[1])
	}
}

func TestUserMessageExpandsCodeIndentationTabs(t *testing.T) {
	r := newRenderer(100, true, "emoji", "")
	text := "package tui\n\nfunc test() {\n\tif true {\n\t\tcall()\n\t}\n}"
	out := strings.TrimLeft(ansi.Strip(r.userMessage(text)), "\n")
	lines := strings.Split(out, "\n")
	for i, want := range map[int]string{
		3: "    if true {",
		4: "        call()",
	} {
		if !strings.HasSuffix(lines[i], want) {
			t.Errorf("line %d = %q, want indentation ending in %q", i+1, lines[i], want)
		}
	}
}
