package tui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/ai/fake"
	"github.com/ddombrow/malachi/coding"
)

// diffRepo is a repository with one commit of a.go, then a.go changed.
func diffRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@example.com", "GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	writeFile(t, filepath.Join(dir, "a.go"), "package a\n\nvar x = 1\n")
	git("init", "-q")
	git("add", "-A")
	git("commit", "-q", "-m", "first")
	writeFile(t, filepath.Join(dir, "a.go"), "package a\n\nvar x = 2\n")
	return dir
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newDiffModel(t *testing.T, cwd string, width int) *model {
	t.Helper()
	s, err := coding.Open(coding.Options{
		Cwd: cwd, Home: t.TempDir(), Settings: &coding.Settings{},
		Provider: fake.New(fake.Text("hi")), NoSession: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	m := newModel(s, "")
	m.Update(tea.WindowSizeMsg{Width: width, Height: 30})
	return m
}

// feed runs cmd and hands the pane's messages back to the model, as the
// program would, until no more come.
func feed(m *model, cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	switch msg := cmd().(type) {
	case tea.BatchMsg:
		for _, c := range msg {
			feed(m, c)
		}
	case diffFetchedMsg, diffDrawnMsg:
		_, next := m.Update(msg)
		feed(m, next)
	}
}

func TestDiffPaneShowsWorkingTree(t *testing.T) {
	dir := diffRepo(t)
	writeFile(t, filepath.Join(dir, "notes.md"), "hi\n")
	m := newDiffModel(t, dir, 120)
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'g', Mod: tea.ModCtrl})
	if !m.dp.open || m.contentWidth() != 60 || m.sidebarShown() {
		t.Fatalf("open %v, transcript %d wide", m.dp.open, m.contentWidth())
	}
	if !strings.Contains(ansi.Strip(m.View().Content), "Changes · loading…") {
		t.Fatalf("no loading heading:\n%s", ansi.Strip(m.View().Content))
	}
	feed(m, cmd)
	if m.dp.err != nil {
		t.Skipf("git cannot run here: %v", m.dp.err)
	}
	view := ansi.Strip(m.View().Content)
	for _, want := range []string{"Changes · 2 files +2 −1", "│ M a.go", "│ A notes.md", "│ a.go ", "notes.md (untracked)", "[ ask ]", "3 - var x = 1", "3 + var x = 2"} {
		if !strings.Contains(view, want) {
			t.Errorf("missing %q in:\n%s", want, view)
		}
	}
	prevRule := false
	for _, row := range strings.Split(m.View().Content, "\n")[:m.vp.Height()] {
		if w := ansi.StringWidth(row); w != 120 {
			t.Fatalf("row %d wide: %q", w, ansi.Strip(row))
		}
		rule := strings.HasPrefix(ansi.Cut(ansi.Strip(row), 62, 120), "────")
		if rule && prevRule {
			t.Fatalf("two rules in a row:\n%s", view)
		}
		prevRule = rule
	}
	// Closing gives the columns back.
	m.command("/diff")
	if m.dp.open || m.contentWidth() == 60 {
		t.Fatal("/diff did not close the pane")
	}
}

func TestDiffPaneCoversNarrowTerminal(t *testing.T) {
	m := newDiffModel(t, t.TempDir(), 80)
	m.print(func(r *renderer) string { return item("transcript text") })
	feed(m, m.toggleDiff())
	m.Update(nil)
	view := ansi.Strip(m.View().Content)
	if strings.Contains(view, "transcript text") || !strings.Contains(view, "not in a git repository") {
		t.Fatalf("pane should cover the transcript:\n%s", view)
	}
	if m.contentWidth() != 80 {
		t.Fatalf("transcript lost its width: %d", m.contentWidth())
	}
}

func TestDiffPaneRefetchesAfterTools(t *testing.T) {
	m := newDiffModel(t, t.TempDir(), 120)
	first := m.toggleDiff()
	// A tool ending while the first fetch runs asks for one more, not two.
	if m.diffEvent(&agent.ToolExecutionEndEvent{}) != nil || m.diffEvent(coding.AgentSettledEvent{}) != nil || !m.dp.again {
		t.Fatal("events during a fetch should queue one more")
	}
	feed(m, first)
	if m.dp.fetching || m.dp.again || m.dp.fetchGen != 2 {
		t.Fatalf("fetching %v again %v gen %d", m.dp.fetching, m.dp.again, m.dp.fetchGen)
	}
	m.toggleDiff()
	if m.diffEvent(&agent.ToolExecutionEndEvent{}) != nil {
		t.Fatal("a closed pane fetched")
	}
}

func TestDiffPaneScrollsUnderTheMouse(t *testing.T) {
	m := newDiffModel(t, t.TempDir(), 120)
	m.dp.open, m.dp.loaded = true, true
	m.dp.diff.Patch = "x"
	for range 100 {
		m.dp.lines = append(m.dp.lines, "row")
	}
	m.Update(nil)
	m.Update(tea.MouseWheelMsg{X: 100, Y: 3, Button: tea.MouseWheelDown})
	if m.dp.sc.YOffset() != wheelLines {
		t.Fatalf("pane offset %d", m.dp.sc.YOffset())
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
	if m.dp.sc.YOffset() <= wheelLines {
		t.Fatal("pgdown did not scroll the pane")
	}
	// The close button closes it.
	m.Update(tea.MouseClickMsg{X: 119, Y: 0, Button: tea.MouseLeft})
	if m.dp.open {
		t.Fatal("close button did not close the pane")
	}
}

func TestDiffPaneAsksAboutAFile(t *testing.T) {
	m := newDiffModel(t, diffRepo(t), 120)
	feed(m, m.toggleDiff())
	if m.dp.err != nil {
		t.Skipf("git cannot run here: %v", m.dp.err)
	}
	m.Update(nil)
	f := m.dp.files[0]
	y := m.diffTop() + f.row - m.dp.sc.YOffset()
	m.Update(tea.MouseClickMsg{X: 117, Y: y, Button: tea.MouseLeft})
	if got := m.input.Value(); got != "[diff a.go] " {
		t.Fatalf("input %q", got)
	}
	sent := m.expandPastes(m.input.Value() + "why?")
	if !strings.Contains(sent, "changes to a.go:\n\n```diff\ndiff --git a/a.go b/a.go\n") || !strings.Contains(sent, "+var x = 2\n```\n why?") {
		t.Fatalf("sent %q", sent)
	}
}

func TestDiffPaneSummaryJumpsToAFile(t *testing.T) {
	m := newDiffModel(t, t.TempDir(), 120)
	m.dp.open, m.dp.loaded = true, true
	m.dp.diff.Patch = "x"
	for i := range 3 {
		m.dp.files = append(m.dp.files, paneFile{path: fmt.Sprintf("f%d", i), status: 'M', row: i * 40})
	}
	for range 120 {
		m.dp.lines = append(m.dp.lines, "row")
	}
	m.Update(nil)
	if n := m.summaryRows(); n != 3 {
		t.Fatalf("summary rows %d", n)
	}
	m.Update(tea.MouseClickMsg{X: 80, Y: 2, Button: tea.MouseLeft}) // f1
	if m.dp.sc.YOffset() != 39 {                                    // f1's upper rule
		t.Fatalf("offset %d", m.dp.sc.YOffset())
	}
}
