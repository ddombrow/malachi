package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/ai"
	"github.com/ddombrow/malachi/ai/fake"
	"github.com/ddombrow/malachi/coding"
)

// The rules for when work may start are coding.Session's, tested there. These
// tests check what the TUI does with them: what it says, and what happens to
// the text the user typed.

// longHistory is enough conversation to compact.
func longHistory() []agent.Message {
	var out []agent.Message
	for i := 0; i < 15; i++ {
		a := agent.NewAssistantMessage("m")
		a.Content = []agent.Content{&agent.TextContent{Text: "done, patched internal/scan.go"}}
		out = append(out, agent.NewUserText("please fix the parser in internal/scan.go"), a)
	}
	return out
}

const handover = "## Goal\nFix the parser in internal/scan.go.\n\n## Next Steps\nRun the tests, then wire the case into scan_test.go, keeping the scanner loop as it is."

// blockedSummary answers with a handover once release is closed.
func blockedSummary(release <-chan struct{}) fake.Script {
	return func(ctx context.Context, _ agent.Request, b *ai.Builder) {
		select {
		case <-release:
		case <-ctx.Done():
			return
		}
		b.Text(handover)
		b.Done(agent.StopStop)
	}
}

// compactingModel returns a model whose session is part-way through a
// /compact that finishes when release is closed.
func compactingModel(t *testing.T, release <-chan struct{}, after ...fake.Script) *model {
	t.Helper()
	s, err := coding.Open(coding.Options{
		Cwd: t.TempDir(), Home: t.TempDir(), Settings: &coding.Settings{},
		Provider: fake.New(append([]fake.Script{blockedSummary(release)}, after...)...), NoSession: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	s.Harness.ReplaceMessages(longHistory())
	m := newModel(s, "")
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	done := make(chan struct{})
	go func() {
		defer close(done)
		if cmd := m.command("/compact"); cmd != nil {
			cmd()
		}
	}()
	// Cleanups run last first, so this waits for the compaction (the test
	// releases it on return) before the temp directories it writes to are
	// removed.
	t.Cleanup(func() {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the compaction never finished")
		}
	})
	waitUntil(t, "the compaction to start", func() bool { return s.State().Compacting })
	return m
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// pump feeds session events into the model until done reports true.
func pump(t *testing.T, m *model, what string, done func() bool) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for !done() {
		select {
		case msg := <-m.bridge.ch:
			m.Update(msg)
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func TestPromptDuringCompactionIsHeldThenSent(t *testing.T) {
	release := make(chan struct{})
	m := compactingModel(t, release, fake.Text("answer"))

	out := shown(m, func() tea.Cmd { return m.submit("typed while the summary is written") })
	if !strings.Contains(out, "held") {
		t.Fatalf("no word that the prompt was held: %q", out)
	}
	if st := m.s.State(); st.Running || st.HeldPrompt != "typed while the summary is written" {
		t.Fatalf("session state: %+v", st)
	}

	close(release)
	// Events reach the model asynchronously; wait for what it shows.
	pump(t, m, "the held prompt's answer in the transcript", func() bool {
		return strings.Contains(ansi.Strip(m.tr.text(m.r)), "answer")
	})
	text := ansi.Strip(m.tr.text(m.r))
	if !strings.Contains(text, "compacted") || !strings.Contains(text, "typed while the summary is written") {
		t.Fatalf("transcript should show the summary and then the held prompt:\n%s", text)
	}
}

func TestSecondPromptDuringCompactionIsRefusedNotLost(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	m := compactingModel(t, release)
	m.submit("first")
	out := shown(m, func() tea.Cmd { return m.submit("second") })
	if m.s.State().HeldPrompt != "first" || m.input.Value() != "second" {
		t.Fatalf("held=%q input=%q", m.s.State().HeldPrompt, m.input.Value())
	}
	if !strings.Contains(out, "waiting") {
		t.Fatalf("refusal not explained: %q", out)
	}
}

func TestEscDuringCompactionGivesThePromptBack(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	m := compactingModel(t, release)
	m.submit("typed right before esc")
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})

	pump(t, m, "the cancellation to be reported", func() bool {
		return strings.Contains(ansi.Strip(m.tr.text(m.r)), "compaction cancelled")
	})
	if m.input.Value() != "typed right before esc" {
		t.Fatalf("held prompt was lost; input = %q", m.input.Value())
	}
	if !strings.Contains(ansi.Strip(m.tr.text(m.r)), "your prompt is back in the input") {
		t.Fatal("the user was not told where their prompt went")
	}
	if len(m.s.Harness.Messages()) != len(longHistory()) {
		t.Fatal("a cancelled compaction changed the transcript")
	}
}

func TestSessionChangesRefusedDuringCompaction(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	m := compactingModel(t, release)
	before := m.s
	for _, cmd := range []string{"/new", "/model glm-5.2", "/compact"} {
		out := shown(m, func() tea.Cmd { return m.command(cmd) })
		if !strings.Contains(out, "compaction") {
			t.Errorf("%s during a compaction: %q", cmd, out)
		}
	}
	if m.s != before {
		t.Fatal("/new swapped the session out from under a compaction")
	}
}

// The progress line follows the session's events, which arrive in order, so
// it cannot be left on screen by a step reported after the end.
func TestCompactionLineFollowsEvents(t *testing.T) {
	m := newTestModel(t)
	m.Update(sessionEventMsg{coding.CompactionStartEvent{Reason: coding.CompactionThreshold}})
	if live := ansi.Strip(m.live()); !strings.Contains(live, "auto · compacting") {
		t.Fatalf("live area: %q", live)
	}
	m.Update(sessionEventMsg{coding.CompactionProgressEvent{Reason: coding.CompactionThreshold, Phase: "summarizing 3.1 kB"}})
	if live := ansi.Strip(m.live()); !strings.Contains(live, "summarizing 3.1 kB") {
		t.Fatalf("live area should carry the step: %q", live)
	}
	m.Update(sessionEventMsg{coding.CompactionEndEvent{Reason: coding.CompactionThreshold, Result: &coding.SummarizeResult{Summary: handover, Replaced: 30, Kept: 20}}})
	if live := ansi.Strip(m.live()); strings.Contains(live, "compact") {
		t.Fatalf("the line should be gone once the compaction ended: %q", live)
	}
	text := ansi.Strip(m.tr.text(m.r))
	if !strings.Contains(text, "compacted automatically to fit the context window") || !strings.Contains(text, "Fix the parser") {
		t.Fatalf("transcript:\n%s", text)
	}
	// A progress event with no compaction under way is ignored.
	m.Update(sessionEventMsg{coding.CompactionProgressEvent{Phase: "writing"}})
	if m.phase != "" {
		t.Fatalf("stray progress put the line back: %q", m.phase)
	}
}
