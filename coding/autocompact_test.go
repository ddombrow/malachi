package coding

import (
	"context"
	"iter"
	"strings"
	"testing"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/agent/session"
)

// contextLimitError is what a provider returns when the prompt is too large.
const contextLimitError = `400 {"error":{"message":"This model's maximum context length is 350000 tokens, however your messages resulted in 361203 tokens.","type":"invalid_request_error","code":"context_length_exceeded"}}`

func testSettings(window int) *Settings {
	return &Settings{Providers: map[string]ProviderConfig{
		"test": {Name: "test", DefaultModel: "m", ContextWindow: window},
	}}
}

// overflowProvider fails the first request with a context error and answers
// everything after it, so recovery can be observed end to end.
type overflowProvider struct {
	failFirst bool
	reply     string
	requests  []agent.Request
}

func (f *overflowProvider) Stream(_ context.Context, req agent.Request) iter.Seq[agent.AssistantEvent] {
	f.requests = append(f.requests, req)
	fail := f.failFirst && len(f.requests) == 1
	return func(yield func(agent.AssistantEvent) bool) {
		m := agent.NewAssistantMessage(req.Model)
		if fail {
			m.StopReason, m.ErrorMessage = agent.StopError, contextLimitError
			yield(&agent.AssistantError{Error: m})
			return
		}
		m.StopReason = agent.StopStop
		m.Content = []agent.Content{&agent.TextContent{Text: f.reply}}
		// The summariser accumulates TextDelta, not the final message, so a
		// provider that only emits AssistantDone summarizes to nothing.
		yield(&agent.TextDelta{Delta: f.reply})
		yield(&agent.AssistantDone{Message: m})
	}
}

// rejectProvider fails every request with the same error.
type rejectProvider struct {
	err   string
	calls int
}

func (f *rejectProvider) Stream(_ context.Context, req agent.Request) iter.Seq[agent.AssistantEvent] {
	f.calls++
	return func(yield func(agent.AssistantEvent) bool) {
		m := agent.NewAssistantMessage(req.Model)
		m.StopReason, m.ErrorMessage = agent.StopError, f.err
		yield(&agent.AssistantError{Error: m})
	}
}

func longTranscript(n int) []agent.Message {
	var msgs []agent.Message
	for i := 0; i < n; i++ {
		msgs = append(msgs, agent.NewUserText(strings.Repeat("please do the thing ", 40)))
		msgs = append(msgs, &agent.AssistantMessage{
			Model:      "m",
			StopReason: agent.StopStop,
			Content:    []agent.Content{&agent.TextContent{Text: strings.Repeat("done indeed ", 40)}},
		})
	}
	return msgs
}

// newTestSession opens a file-backed session in a temporary home, so tests
// never write sessions or logs into the developer's real ~/.malachi.
func newTestSession(t *testing.T, window int, p agent.Provider) *Session {
	t.Helper()
	s, err := Open(Options{
		Cwd: t.TempDir(), Home: t.TempDir(), Settings: testSettings(window), Model: "test/m", Provider: p,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// seedHistory gives a file-backed session a history the way a real one has
// it: written to the session file, with entry ids recorded. ReplaceMessages
// alone would leave messages a compaction cannot anchor its tail to.
func seedHistory(t *testing.T, s *Session, msgs []agent.Message) {
	t.Helper()
	for _, m := range msgs {
		e := session.NewMessageEntry(m)
		if !s.append(e) {
			t.Fatalf("seeding history: %v", s.PersistError())
		}
		s.rememberEntry(m, e.ID)
	}
	s.Harness.ReplaceMessages(msgs)
}

func TestRecoverOverflowRetriesAfterCompacting(t *testing.T) {
	fp := &overflowProvider{failFirst: true, reply: "SESSION SUMMARY\n## Goal\nkeep going\n"}
	s := newTestSession(t, 100_000, fp)
	seedHistory(t, s, longTranscript(40))

	if err := s.Prompt(context.Background(), "go on"); err != nil {
		t.Fatal(err)
	}
	// One summarisation and one retry after the rejected request.
	if len(fp.requests) < 3 {
		t.Fatalf("expected a rejected attempt, a summary and a retry, got %d requests", len(fp.requests))
	}
	if fp.requests[len(fp.requests)-1].Model != "m" {
		t.Error("retry used a different model")
	}
	retry := fp.requests[len(fp.requests)-1]
	if len(retry.Messages) == 0 {
		t.Fatal("retry sent an empty transcript")
	}
	// The summary must actually be in the retried context, or the retry is
	// just the request that was already rejected.
	found := false
	for _, m := range retry.Messages {
		if _, ok := m.(*agent.CompactionSummaryMessage); ok {
			found = true
		}
	}
	if !found {
		t.Error("retry did not carry the compaction summary")
	}
}

func TestRecoverOverflowDeclinesNonContextErrors(t *testing.T) {
	fp := &rejectProvider{err: `401 {"error":{"message":"invalid api key"}}`}
	s := newTestSession(t, 100_000, fp)
	seedHistory(t, s, longTranscript(40))

	if err := s.Prompt(context.Background(), "go on"); err != nil {
		t.Fatal(err)
	}
	// A bad key does not get better after paying for a summary.
	if fp.calls != 1 {
		t.Fatalf("expected exactly one provider call, got %d", fp.calls)
	}
}

func TestRecoverOverflowIsNotAttemptedTwice(t *testing.T) {
	// Every request fails on context: recovery must still happen only once,
	// or a session whose window is genuinely too small loops forever.
	fp := &rejectProvider{err: contextLimitError}
	s := newTestSession(t, 100_000, fp)
	seedHistory(t, s, longTranscript(40))

	if err := s.Prompt(context.Background(), "go on"); err != nil {
		t.Fatal(err)
	}
	if fp.calls > 2 {
		t.Fatalf("recovery retried more than once: %d provider calls", fp.calls)
	}
}

func TestAutoCompactionThresholdHoldsBackAReserve(t *testing.T) {
	s := newTestSession(t, 350_000, &overflowProvider{})
	got := s.AutoCompactionThreshold()
	if want := 350_000 - 350_000/8; got != want {
		t.Errorf("threshold = %d, want %d", got, want)
	}
	// The threshold must leave room to actually answer in.
	if got >= s.ContextWindow() {
		t.Errorf("threshold %d leaves no room in window %d", got, s.ContextWindow())
	}
}

func TestNeedsCompactionFalseForShortConversation(t *testing.T) {
	s := newTestSession(t, 100_000, &overflowProvider{})
	s.Harness.ReplaceMessages(longTranscript(3))
	if _, _, needed := s.NeedsCompaction(); needed {
		t.Error("a short conversation should never ask to be compacted")
	}
}

func TestNeedsCompactionFalseWhenNothingCanBeFolded(t *testing.T) {
	s := newTestSession(t, 1_000, &overflowProvider{})
	// Past the threshold, but with fewer messages than a compaction may fold.
	s.Harness.ReplaceMessages(longTranscript(2))
	estimated, threshold, needed := s.NeedsCompaction()
	if estimated <= threshold {
		t.Skipf("estimate %d did not exceed threshold %d", estimated, threshold)
	}
	if needed {
		t.Error("compaction cannot shorten this conversation and should not be attempted")
	}
}

func TestAutoCompactIsANoOpWhenNotNeeded(t *testing.T) {
	fp := &overflowProvider{}
	s := newTestSession(t, 100_000, fp)
	s.Harness.ReplaceMessages(longTranscript(3))
	if _, err := s.AutoCompact(context.Background(), nil); err == nil {
		t.Fatal("expected an error rather than a pointless compaction")
	}
	if len(fp.requests) != 0 {
		t.Errorf("a no-op compaction made %d provider calls", len(fp.requests))
	}
}
