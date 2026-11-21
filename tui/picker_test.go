package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/ddombrow/malachi/ai/fake"
	"github.com/ddombrow/malachi/coding"
)

func press(m *model, keys ...string) {
	for _, k := range keys {
		switch k {
		case "enter":
			m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		case "esc":
			m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
		case "down":
			m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		case "up":
			m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
		case "backspace":
			m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
		default:
			for _, r := range k {
				m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
			}
		}
	}
}

// run applies a command's effect the way the program would.
func run(m *model, cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	if msg := cmd(); msg != nil {
		m.Update(msg)
	}
}

func screen(m *model) string { return ansi.Strip(m.View().Content) }

func TestThinkingPickerChoosesALevel(t *testing.T) {
	m := newTestModel(t)
	levels := m.s.Provider().ThinkingLevels
	if len(levels) < 2 {
		t.Skipf("provider has %d thinking levels", len(levels))
	}
	start := m.s.ThinkingLevel()
	run(m, m.command("/thinking"))
	if m.picker == nil {
		t.Fatal("bare /thinking opened no picker")
	}
	view := screen(m)
	for _, l := range levels {
		if !strings.Contains(view, l) {
			t.Fatalf("picker does not show %q:\n%s", l, view)
		}
	}
	if !strings.Contains(view, "› ● "+start) {
		t.Fatalf("the cursor must start on the current level %q:\n%s", start, view)
	}
	// Enter on an untouched picker keeps the level.
	press(m, "enter")
	if m.picker != nil || m.s.ThinkingLevel() != start {
		t.Fatalf("enter moved the level from %q to %q", start, m.s.ThinkingLevel())
	}

	run(m, m.command("/thinking"))
	want := levels[0]
	if want == start {
		want = levels[1]
	}
	press(m, want, "enter")
	if m.s.ThinkingLevel() != want {
		t.Fatalf("filtering for %q and choosing set %q", want, m.s.ThinkingLevel())
	}
	if m.picker != nil {
		t.Fatal("the picker stayed open after a choice")
	}
	if !strings.Contains(screen(m), "thinking level set to "+want) {
		t.Fatalf("the choice was not reported:\n%s", screen(m))
	}
}

func TestPickerEscChangesNothing(t *testing.T) {
	m := newTestModel(t)
	start := m.s.ThinkingLevel()
	run(m, m.command("/thinking"))
	if m.picker == nil {
		t.Skip("provider lists no thinking levels")
	}
	press(m, "down", "esc")
	if m.picker != nil || m.s.ThinkingLevel() != start {
		t.Fatalf("esc: picker open %v, level %q (was %q)", m.picker != nil, m.s.ThinkingLevel(), start)
	}
	// Keys go back to the input box.
	press(m, "x")
	if m.input.Value() != "x" {
		t.Fatalf("input after esc = %q", m.input.Value())
	}
}

// While a picker is open its keys are its own: esc during a run closes the
// picker rather than aborting the run, and typing does not reach the input.
func TestPickerKeepsItsKeys(t *testing.T) {
	m := newTestModel(t)
	m.picker = newPicker("t", "", []pickerItem{{label: "a", value: "a"}, {label: "b", value: "b"}}, func(string) tea.Cmd { return nil })
	press(m, "zz")
	if m.input.Value() != "" {
		t.Fatalf("typing reached the hidden input: %q", m.input.Value())
	}
	if !strings.Contains(screen(m), "no match") {
		t.Fatalf("a filter matching nothing must say so:\n%s", screen(m))
	}
	press(m, "enter")
	if m.picker == nil {
		t.Fatal("enter with nothing to choose closed the picker")
	}
	press(m, "backspace", "backspace", "b", "enter")
	if m.picker != nil {
		t.Fatal("enter on a match did not choose it")
	}
}

func TestModelPickerFillsWhenTheListArrives(t *testing.T) {
	t.Setenv("OPENCODE_API_KEY", "test") // the switch builds a real provider; it is never called
	m := newTestModel(t)
	cmd := m.modelCommand("")
	if m.picker == nil || !strings.Contains(screen(m), "loading") {
		t.Fatalf("bare /model must open a loading picker:\n%s", screen(m))
	}
	// The spinner keeps turning while the list loads, though no run is busy.
	before := screen(m)
	_, next := m.Update(m.spin.Tick())
	if next == nil {
		t.Fatal("the spinner stopped while the list was loading")
	}
	if screen(m) == before {
		t.Fatal("the loading picker did not show the spinner turn")
	}
	_ = cmd // fetching would reach the network; deliver a list instead
	p := m.picker
	m.Update(modelsMsg{p: p, ids: []string{"kimi-k3", "glm-5.2", "qwen4-coder"}})
	view := screen(m)
	if strings.Contains(view, "loading") || !strings.Contains(view, "qwen4-coder") {
		t.Fatalf("the list did not fill the picker:\n%s", view)
	}
	press(m, "glm", "enter")
	if m.s.Model() != "glm-5.2" {
		t.Fatalf("model = %q, want glm-5.2:\n%s", m.s.Model(), screen(m))
	}
}

// A fetch that ends after its picker was closed must not touch the picker
// open now: a failure would otherwise close it and report an old error.
func TestLateModelListIsIgnored(t *testing.T) {
	m := newTestModel(t)
	m.modelCommand("")
	p := m.picker
	press(m, "esc")
	run(m, m.command("/thinking"))
	thinking := m.picker
	out := shown(m, func() tea.Cmd {
		m.Update(modelsMsg{p: p, err: errors.New("endpoint down")})
		return nil
	})
	if m.picker != thinking {
		t.Fatal("a late model fetch closed the open picker")
	}
	if strings.Contains(out, "endpoint down") {
		t.Fatalf("a late model fetch reported its error: %s", out)
	}
}

// The picker takes the input box's place, so the screen never grows past
// the terminal however many choices there are.
func TestPickerFitsTheTerminal(t *testing.T) {
	m := newTestModel(t)
	var items []pickerItem
	for _, id := range strings.Fields(strings.Repeat("model-x ", 50)) {
		items = append(items, pickerItem{label: id, value: id})
	}
	for _, h := range []int{30, 12, 6, 3} {
		m.Update(tea.WindowSizeMsg{Width: 80, Height: h})
		m.picker = newPicker("many", "", items, func(string) tea.Cmd { return nil })
		m.Update(nil)
		if rows := strings.Count(m.View().Content, "\n") + 1; rows > h {
			t.Fatalf("height %d: view is %d rows", h, rows)
		}
		// The cursor stays on screen at the end of the list.
		press(m, "up")
		if !strings.Contains(screen(m), "› ") {
			t.Fatalf("height %d: the cursor scrolled off:\n%s", h, screen(m))
		}
		press(m, "esc")
	}
}

func TestResumePickerReopensASession(t *testing.T) {
	t.Setenv("OPENCODE_API_KEY", "test") // the resumed session builds a real provider; it is never called
	home, cwd := t.TempDir(), t.TempDir()
	open := func() *coding.Session {
		s, err := coding.Open(coding.Options{Cwd: cwd, Home: home, Settings: &coding.Settings{}, Provider: fake.New(fake.Text("hi"))})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	old := open()
	if err := old.Prompt(context.Background(), "the earlier task"); err != nil {
		t.Fatal(err)
	}
	oldPath := old.Path()
	old.Close()

	m := newModel(open(), "")
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	if err := m.s.Prompt(context.Background(), "this task"); err != nil {
		t.Fatal(err)
	}
	m.Update(nil)
	run(m, m.command("/resume"))
	if m.picker == nil || !strings.Contains(screen(m), "the earlier task") {
		t.Fatalf("bare /resume must list sessions:\n%s", screen(m))
	}
	press(m, "earlier", "enter")
	if m.s.Path() != oldPath {
		t.Fatalf("resumed %q, want %q", m.s.Path(), oldPath)
	}
}

func TestTrustPickerAnswersForThisSession(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "AGENTS.md"), []byte("project rules"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := coding.Open(coding.Options{Cwd: cwd, Home: home, Settings: &coding.Settings{}, Provider: fake.New(), NoSession: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	m := newModel(s, "")
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	out := shown(m, func() tea.Cmd { return m.command("/trust") })
	if m.picker == nil {
		t.Fatalf("bare /trust opened no picker: %s", out)
	}
	if !strings.Contains(out, "AGENTS.md") {
		t.Fatalf("the files the answer covers must be listed: %s", out)
	}
	press(m, "yes", "enter")
	if st := m.s.TrustState(); !st.Trusted() || st.Source != "session" {
		t.Fatalf("yes: %+v", st)
	}
	// Answering again starts on the answer in effect.
	m.command("/trust")
	if !strings.Contains(screen(m), "› ● yes") {
		t.Fatalf("the picker must start on yes:\n%s", screen(m))
	}
	press(m, "down", "enter")
	if m.s.TrustState().Trusted() {
		t.Fatalf("no: %+v", m.s.TrustState())
	}
	if store, err := coding.LoadTrustStore(home); err != nil || len(store.Entries) != 0 {
		t.Fatalf("the picker must not remember the answer: %+v %v", store, err)
	}
}

// With nothing to load there is nothing to decide.
func TestTrustPickerNeedsSomethingToLoad(t *testing.T) {
	m := newTestModel(t)
	m.command("/trust")
	if m.picker != nil {
		t.Fatal("bare /trust opened a picker for a directory with no project files")
	}
}

// Choosing parent from an unanswered prompt is a yes for this directory that
// is remembered for everything under its parent.
func TestTrustPickerParentRemembersForSiblings(t *testing.T) {
	home, repo := t.TempDir(), t.TempDir()
	pkg := filepath.Join(repo, "api")
	if err := os.Mkdir(pkg, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "AGENTS.md"), []byte("rules"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := coding.Open(coding.Options{Cwd: pkg, Home: home, Settings: &coding.Settings{}, Provider: fake.New(), NoSession: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	m := newModel(s, "")
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	if !m.s.TrustState().Pending {
		t.Fatalf("want an unanswered prompt: %+v", m.s.TrustState())
	}
	m.command("/trust")
	if !strings.Contains(screen(m), "remember yes for everything under") {
		t.Fatalf("the parent option must say how far it reaches:\n%s", screen(m))
	}
	out := shown(m, func() tea.Cmd {
		press(m, "parent", "enter")
		return nil
	})
	if st := m.s.TrustState(); !st.Trusted() || st.Source != "inherited" {
		t.Fatalf("parent: %+v\n%s", st, out)
	}
	if !coding.ResolveTrust(home, filepath.Join(repo, "web"), coding.TrustAsk, "").Trusted() {
		t.Fatal("a sibling does not inherit the parent's yes")
	}
}
