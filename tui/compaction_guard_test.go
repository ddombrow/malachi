package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/ddombrow/malachi/coding"
)

// startCompaction puts the model in the middle of a /compact without running
// the summarisation itself, which is what a slow model looks like to the UI.
func startCompaction(t *testing.T, m *model) {
	t.Helper()
	m.command("/compact")
	if !m.summarizing {
		t.Fatal("/compact did not start a summarisation")
	}
}

func TestPromptDuringCompactionIsHeldThenSent(t *testing.T) {
	m := newTestModel(t)
	startCompaction(t, m)

	out := shown(m, func() tea.Cmd { return m.submit("typed while the summary is written") })
	if m.running {
		t.Fatal("a run started on a transcript the compaction is about to replace")
	}
	if m.pendingPrompt != "typed while the summary is written" || !strings.Contains(out, "held") {
		t.Fatalf("prompt not held: pending=%q out=%q", m.pendingPrompt, out)
	}

	m.Update(compactDoneMsg{result: &coding.SummarizeResult{Summary: "## Goal\nx"}})
	if !m.running {
		t.Fatal("the held prompt was not sent once the compaction finished")
	}
	if m.pendingPrompt != "" || m.summarizing {
		t.Fatal("compaction state was not cleared")
	}
}

func TestSecondPromptDuringCompactionIsRefusedNotLost(t *testing.T) {
	m := newTestModel(t)
	startCompaction(t, m)
	m.submit("first")
	m.submit("second")
	if m.pendingPrompt != "first" || m.input.Value() != "second" {
		t.Fatalf("pending=%q input=%q", m.pendingPrompt, m.input.Value())
	}
}

func TestEscKeepsTheGuardUntilTheSummaryStops(t *testing.T) {
	m := newTestModel(t)
	startCompaction(t, m)
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	// The goroutine has not reported back yet: it may still be writing.
	m.submit("typed right after esc")
	if m.running {
		t.Fatal("esc reopened prompts before the summarisation had stopped")
	}
	m.Update(compactDoneMsg{err: context.Canceled})
	if m.running || m.summarizing {
		t.Fatal("a cancelled compaction must not send the held prompt")
	}
	if m.input.Value() != "typed right after esc" {
		t.Fatalf("held prompt was lost; input = %q", m.input.Value())
	}
}

func TestSessionChangesRefusedDuringCompaction(t *testing.T) {
	m := newTestModel(t)
	startCompaction(t, m)
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

func TestFinishedCompactionIsNotReportedAsCancelled(t *testing.T) {
	m := newTestModel(t)
	res := &coding.SummarizeResult{Summary: "done"}
	cmd := m.summarize("compact", func(ctx context.Context, _ func(string)) (*coding.SummarizeResult, error) {
		m.cancelPhase() // esc lands after the transcript was replaced
		return res, nil
	})
	done, ok := cmd().(compactDoneMsg)
	if !ok || done.result != res || done.err != nil {
		t.Fatalf("got %#v", done)
	}
}
