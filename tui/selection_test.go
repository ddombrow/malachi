package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestMain(m *testing.M) {
	clipboardTools = nil // never touch the developer's real clipboard
	// Nor their real ~/.malachi: sessions opened without an explicit Home
	// land in a throwaway directory.
	home, err := os.MkdirTemp("", "malachi-test-home-")
	if err != nil {
		panic(err)
	}
	os.Setenv("MALACHI_HOME", home)
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

// fakeClipboard installs a clipboard tool that writes what it is given to a
// file, and returns a function reading it back.
func fakeClipboard(t *testing.T) func() string {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "clip.txt")
	script := filepath.Join(dir, "clip")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncat > '"+out+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	clipboardTools = [][]string{{script}}
	t.Cleanup(func() { clipboardTools = nil })
	return func() string { data, _ := os.ReadFile(out); return string(data) }
}

// runCmd executes a command tree, so batched commands (like the clipboard
// write) actually run.
func runCmd(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	if batch, ok := cmd().(tea.BatchMsg); ok {
		for _, c := range batch {
			runCmd(c)
		}
	}
}

// selectionModel returns a model whose transcript starts with three
// known lines right below the banner.
func selectionModel(t *testing.T) (*model, int) {
	t.Helper()
	m := newTestModel(t)
	m.Update(printMsg{func(r *renderer) string {
		return item(r.gutter(iconReply, r.st.dim, "alpha one\nbeta two\ngamma three"))
	}})
	lines := strings.Split(ansi.Strip(m.vp.View()), "\n")
	for i, l := range lines {
		if strings.Contains(l, "alpha one") {
			return m, i
		}
	}
	t.Fatalf("alpha line not visible:\n%s", strings.Join(lines, "\n"))
	return nil, 0
}

func TestDragSelectsAndCopiesWithoutGutter(t *testing.T) {
	clip := fakeClipboard(t)
	m, row := selectionModel(t)

	// Drag from the far left of "alpha" to the end of "two".
	m.Update(tea.MouseClickMsg{X: 0, Y: row, Button: tea.MouseLeft})
	m.Update(tea.MouseMotionMsg{X: 5, Y: row + 1, Button: tea.MouseLeft})
	m.Update(tea.MouseMotionMsg{X: 10, Y: row + 1, Button: tea.MouseLeft})
	if got := m.selectedText(); got != "alpha one\nbeta two" {
		t.Fatalf("selection = %q", got)
	}
	if !strings.Contains(m.View().Content, "\x1b[7m") {
		t.Fatal("selection is not highlighted")
	}
	_, cmd := m.Update(tea.MouseReleaseMsg{X: 10, Y: row + 1, Button: tea.MouseLeft})
	runCmd(cmd)
	if got := clip(); got != "alpha one\nbeta two" {
		t.Fatalf("clipboard = %q", got)
	}
	if !strings.Contains(m.statusLine(), "copied selection") {
		t.Fatalf("status: %s", ansi.Strip(m.statusLine()))
	}
}

func TestPartialLineSelection(t *testing.T) {
	m, row := selectionModel(t)
	gutter := m.r.gutterWidth(iconReply)
	// Select "one" through "beta": from col gutter+6 on line 1 to gutter+3 on line 2.
	m.Update(tea.MouseClickMsg{X: gutter + 6, Y: row, Button: tea.MouseLeft})
	m.Update(tea.MouseMotionMsg{X: gutter + 3, Y: row + 1, Button: tea.MouseLeft})
	if got := m.selectedText(); got != "one\nbeta" {
		t.Fatalf("selection = %q", got)
	}
}

func TestClickWithoutDragDoesNotCopy(t *testing.T) {
	clip := fakeClipboard(t)
	m, row := selectionModel(t)
	m.Update(tea.MouseClickMsg{X: 4, Y: row, Button: tea.MouseLeft})
	_, cmd := m.Update(tea.MouseReleaseMsg{X: 4, Y: row, Button: tea.MouseLeft})
	runCmd(cmd)
	if m.sel.active || clip() != "" {
		t.Fatal("a plain click must not select or copy")
	}
}

func TestSelectionSurvivesNewOutputAndClearsOnKey(t *testing.T) {
	m, row := selectionModel(t)
	m.Update(tea.MouseClickMsg{X: 0, Y: row, Button: tea.MouseLeft})
	m.Update(tea.MouseMotionMsg{X: 20, Y: row, Button: tea.MouseLeft})
	m.Update(tea.MouseReleaseMsg{X: 20, Y: row, Button: tea.MouseLeft})
	m.Update(printMsg{func(r *renderer) string { return item("streamed later") }})
	if got := m.selectedText(); got != "alpha one" {
		t.Fatalf("selection moved when output arrived: %q", got)
	}
	m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	if m.sel.active {
		t.Fatal("typing should dismiss the selection")
	}
}

func TestCopyCommandUsesClipboardTool(t *testing.T) {
	clip := fakeClipboard(t)
	m := newTestModel(t)
	m.lastReply = "the answer"
	runCmd(m.command("/copy"))
	if got := clip(); got != "the answer" {
		t.Fatalf("clipboard = %q", got)
	}
}

func TestLinesWithoutGutterKeepTheirFirstColumns(t *testing.T) {
	m := newTestModel(t)
	if g := m.lineGutter("/\\/\\   __ _"); g != 0 {
		t.Fatalf("banner art has no gutter, got %d", g)
	}
	if g := m.lineGutter("💬 hello"); g != m.r.gutterWidth(iconReply) {
		t.Fatalf("icon line gutter = %d", g)
	}
	if g := m.lineGutter("   continued"); g != m.r.gutterWidth(iconReply) {
		t.Fatalf("indented line gutter = %d", g)
	}
}
