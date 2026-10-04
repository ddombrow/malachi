package coding

import (
	"context"
	"iter"
	"strings"
	"testing"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/agent/session"
	"github.com/ddombrow/malachi/ai/fake"
)

// assistantText builds a finished assistant message saying one thing, which is
// all the summariser sees.
func assistantText(text string) agent.Message {
	m := agent.NewAssistantMessage("m")
	m.Content = []agent.Content{&agent.TextContent{Text: text}}
	return m
}

// longConversation builds a conversation with enough turns to have a prefix
// worth summarizing: alternating user text and assistant text.
func longConversation(turns int) []agent.Message {
	var out []agent.Message
	for i := 0; i < turns; i++ {
		out = append(out,
			agent.NewUserText("please fix the parser in internal/scan.go, it drops trailing commas"),
			assistantText("done, patched internal/scan.go and added a case in scan_test.go"))
	}
	return out
}

const goodSummary = `## Goal
Fix the parser dropping trailing commas in internal/scan.go.

## Key Decisions
Kept the existing scanner loop rather than rewriting it; the bug was a
single lookahead in internal/scan.go, not the tokenizer.

## Next Steps
Run go test ./internal/... and then wire the new case into scan_test.go.`

// summarizeFixture returns a session with a long conversation already loaded.
func summarizeFixture(t *testing.T, scripts ...fake.Script) *Session {
	t.Helper()
	s, err := Open(Options{
		Cwd: t.TempDir(), Home: t.TempDir(), Settings: &Settings{},
		Provider: fake.New(scripts...), NoSession: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	s.Harness.ReplaceMessages(longConversation(20))
	return s
}

// A summarisation replaces the conversation prefix with the summary and keeps
// the tail verbatim, in the shape a resume will see.
func TestSummarizeReplacesPrefixKeepsTail(t *testing.T) {
	s := summarizeFixture(t, fake.Text(goodSummary))
	before := s.Harness.Messages()

	var phases []string
	res, err := s.Summarize(context.Background(), "", func(p string) { phases = append(phases, p) })
	if err != nil {
		t.Fatal(err)
	}
	if len(phases) < 4 || phases[0] != "reading" || phases[len(phases)-1] != "writing" {
		t.Errorf("phases should name each step, got %v", phases)
	}

	after := s.Harness.Messages()
	if len(after) != res.Replaced-res.Replaced+res.Kept+1 {
		t.Fatalf("want 1 summary + %d kept, got %d messages", res.Kept, len(after))
	}
	summary, ok := after[0].(*agent.CompactionSummaryMessage)
	if !ok {
		t.Fatalf("first message should be the summary, got %T", after[0])
	}
	if summary.Summary != goodSummary {
		t.Errorf("summary not carried verbatim:\n%s", summary.Summary)
	}
	// The tail must be the messages the boundary chose, unmodified.
	tail := before[len(before)-res.Kept:]
	for i, want := range tail {
		if after[1+i] != want {
			t.Errorf("kept message %d was replaced, not retained", i)
		}
	}
	if res.Replaced == 0 || res.Kept == 0 {
		t.Errorf("a summarisation should report both sides: %+v", res)
	}
}

// The retained tail must open with a user message, so the model never sees a
// turn that starts mid-exchange.
func TestCompactBoundaryOpensTheTailOnAUserMessage(t *testing.T) {
	messages := longConversation(20)
	keepFrom, err := compactBoundary(messages)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := messages[keepFrom].(*agent.UserMessage); !ok {
		t.Fatalf("tail starts on %T, want a user message", messages[keepFrom])
	}
	// A conversation with no tool calls must still cut cleanly.
	if keepFrom <= 0 || keepFrom >= len(messages) {
		t.Fatalf("boundary %d is not inside the conversation (%d messages)", keepFrom, len(messages))
	}
}

// Nothing is changed unless the model produced something usable.
func TestSummarizeRefusesUnusableSummaries(t *testing.T) {
	s := summarizeFixture(t, fake.Text(""))
	before := s.Harness.Messages()
	if _, err := s.Summarize(context.Background(), "", nil); err == nil {
		t.Fatal("an empty summary should be refused")
	}
	if len(s.Harness.Messages()) != len(before) {
		t.Error("a refused summarisation must leave the transcript alone")
	}

	// A short conversation has no prefix worth replacing.
	short, err := Open(Options{
		Cwd: t.TempDir(), Home: t.TempDir(), Settings: &Settings{},
		Provider: fake.New(fake.Text(goodSummary)), NoSession: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	short.Harness.ReplaceMessages(longConversation(2))
	if _, err := short.Summarize(context.Background(), "", nil); err != ErrNothingToSummarize {
		t.Errorf("want ErrNothingToSummarize, got %v", err)
	}
}

// A summary that lost the structure is still applied, because it is better
// than nothing, but what it lost is reported rather than hidden.
func TestSummarizeReportsLoss(t *testing.T) {
	s := summarizeFixture(t, fake.Text("Sure! I fixed the parser bug you asked about earlier."))
	res, err := s.Summarize(context.Background(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) == 0 {
		t.Fatal("a summary with no sections and no identifiers should warn")
	}
	joined := strings.Join(res.Warnings, "; ")
	if !strings.Contains(joined, "sections") {
		t.Errorf("missing structure warning: %q", joined)
	}
	if !strings.Contains(joined, "identifiers") {
		t.Errorf("missing identifier warning: %q", joined)
	}
	// A complete summary should not warn at all.
	clean := summarizeFixture(t, fake.Text(goodSummary))
	res, err = clean.Summarize(context.Background(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("a well-formed summary should not warn: %v", res.Warnings)
	}
}

// Cancelling leaves nothing behind: the entry is written only after the summary
// is accepted.
func TestSummarizeCancelledChangesNothing(t *testing.T) {
	s := summarizeFixture(t, fake.Text(goodSummary))
	before := s.Harness.Messages()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Summarize(ctx, "", nil); err == nil {
		t.Fatal("a cancelled summarisation should fail")
	}
	if len(s.Harness.Messages()) != len(before) {
		t.Error("a cancelled summarisation must leave the transcript alone")
	}
}

// Instructions and a previous summary are both folded into the request.
func TestSummarizeFoldsInstructionsAndPreviousSummary(t *testing.T) {
	s := summarizeFixture(t, fake.Text(goodSummary), fake.Text(goodSummary))
	var seen []agent.Request
	s.runtime = recordingProvider{inner: s.runtime, seen: &seen}

	if _, err := s.Summarize(context.Background(), "focus on the auth bug", nil); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 {
		t.Fatalf("want one summarisation request, got %d", len(seen))
	}
	req := seen[0]
	body := agent.MessageText(req.Messages[0])
	if !strings.Contains(body, "focus on the auth bug") {
		t.Errorf("instructions were dropped:\n%s", body[:200])
	}
	if req.System == "" || !strings.Contains(req.System, "handover document") {
		t.Errorf("the summarisation needs its own system prompt, got %q", req.System)
	}
	if len(req.Tools) != 0 {
		t.Errorf("the summariser must not be offered tools, got %d", len(req.Tools))
	}
	// Every call the agent makes carries the routing and prompt-cache hint; a
	// gateway that requires the header answers 400 without it.
	if req.SessionID == "" {
		t.Error("the summarisation must send SessionID like every other request")
	}
	if req.SessionID != s.sessionID {
		t.Errorf("SessionID = %q, want the session's own %q", req.SessionID, s.sessionID)
	}
	// The dropped conversation should be in the body.
	if !strings.Contains(body, "internal/scan.go") {
		t.Errorf("the conversation was not included:\n%s", body[:300])
	}

	// A second compaction folds the first summary in. It needs new conversation
	// to have anything left to do, which is the honest behaviour: the first
	// compaction already reduced the transcript to a summary and a short tail.
	s.Harness.ReplaceMessages(append(s.Harness.Messages(), longConversation(20)...))
	if _, err := s.Summarize(context.Background(), "", nil); err != nil {
		t.Fatal(err)
	}
	second := agent.MessageText(seen[1].Messages[0])
	if !strings.Contains(second, "Previous handover") || !strings.Contains(second, goodSummary) {
		t.Errorf("the previous summary was not folded in:\n%s", second[:300])
	}
}

// The persisted entry is what a resume reads, so its shape is the contract
// with tau: a summary, the entry the tail begins at, and the cost.
func TestCompactionEntryShape(t *testing.T) {
	e := session.NewCompactionEntry("the summary", "entry-abc", 120_000,
		agent.Usage{Input: 1000, Output: 200, CacheRead: 300, TotalTokens: 1500},
		"openai", "gpt-5.1")

	if e.Type != session.TypeCompaction {
		t.Fatalf("type = %q, want %q", e.Type, session.TypeCompaction)
	}
	if e.String("summary") != "the summary" {
		t.Errorf("summary = %q", e.String("summary"))
	}
	if e.String("first_kept_entry_id") != "entry-abc" {
		t.Errorf("first_kept_entry_id = %q", e.String("first_kept_entry_id"))
	}
	if e.Int("tokens_before") != 120_000 {
		t.Errorf("tokens_before = %d", e.Int("tokens_before"))
	}
	if e.String("model") != "gpt-5.1" || e.String("provider") != "openai" {
		t.Errorf("model/provider missing: %v", e.Fields)
	}
	if e.Fields["usage"] == nil {
		t.Error("the summarisation's own cost should be recorded")
	}
	// An unknown tail leaves the field out rather than writing an empty id.
	plain := session.NewCompactionEntry("s", "", 0, agent.Usage{}, "", "")
	if _, ok := plain.Fields["first_kept_entry_id"]; ok {
		t.Error("an absent tail should omit first_kept_entry_id, not write it empty")
	}
}

// The end-to-end shape a resume depends on: after compaction the replayed
// conversation is the summary plus the tail, and the full history is still
// reachable.
func TestReplayHonoursCompactionAndReplayAllRecovers(t *testing.T) {
	keptID := session.NewID()
	dropped := []*session.Entry{
		session.NewSessionInfo("/work", ""),
		session.NewMessageEntry(agent.NewUserText("first question")),
		session.NewMessageEntry(assistantText("first answer")),
	}
	kept := []*session.Entry{
		session.NewMessageEntry(agent.NewUserText("second question")),
		session.NewMessageEntry(assistantText("second answer")),
	}
	kept[0].ID = keptID
	compaction := session.NewCompactionEntry("the handover", keptID, 100_000, agent.Usage{}, "openai", "gpt-5.1")

	path := append(append([]*session.Entry{}, dropped...), kept...)
	path = append(path, compaction)

	// The tail was written before the compaction entry, so this only works
	// because first_kept_entry_id points back at it.
	st := session.Replay(path)
	if len(st.Messages) != 3 {
		t.Fatalf("want summary + 2 kept, got %d messages", len(st.Messages))
	}
	summary, ok := st.Messages[0].(*agent.CompactionSummaryMessage)
	if !ok {
		t.Fatalf("first replayed message = %T, want a summary", st.Messages[0])
	}
	if summary.Summary != "the handover" {
		t.Errorf("summary = %q", summary.Summary)
	}
	if got := agent.MessageText(st.Messages[1]); got != "second question" {
		t.Errorf("the tail was not retained, got %q", got)
	}
	if got := agent.MessageText(st.Messages[2]); got != "second answer" {
		t.Errorf("the tail was not retained, got %q", got)
	}

	// Without the id, replay would lose a tail that was already on disk: the
	// compaction entry is the last one written, so nothing follows it.
	unpointed := session.NewCompactionEntry("the handover", "", 0, agent.Usage{}, "", "")
	st = session.Replay(append(append([]*session.Entry{}, dropped...), append(kept, unpointed)...))
	if len(st.Messages) != 1 {
		t.Fatalf("without the pointer only the summary should survive, got %d", len(st.Messages))
	}

	// The escape hatch recovers everything the summary replaced.
	full := session.ReplayAll(path)
	want := len(dropped) + len(kept) - 1 // the session-info entry is not a message
	if len(full.Messages) != want {
		t.Fatalf("ReplayAll recovered %d messages, want %d", len(full.Messages), want)
	}
	if got := agent.MessageText(full.Messages[0]); got != "first question" {
		t.Errorf("ReplayAll lost the beginning: %q", got)
	}
}

// entryIDFor only answers for messages that reached the disk.
func TestEntryIDForNamesPersistedMessages(t *testing.T) {
	s, err := Open(Options{
		Cwd: t.TempDir(), Home: t.TempDir(), Settings: &Settings{},
		Provider: fake.New(fake.Text("hello")), Model: "fake/model",
	})
	if err != nil {
		t.Fatal(err)
	}
	m := agent.NewUserText("write this down")
	s.rememberEntry(m, "entry-xyz")
	if got := s.entryIDFor(m); got != "entry-xyz" {
		t.Errorf("entryIDFor = %q, want entry-xyz", got)
	}
	if got := s.entryIDFor(agent.NewUserText("never written")); got != "" {
		t.Errorf("an unpersisted message should have no entry id, got %q", got)
	}
}

// recordingProvider captures the requests a session makes, so the
// summarisation's prompt can be inspected.
type recordingProvider struct {
	inner agent.Provider
	seen  *[]agent.Request
}

func (p recordingProvider) Stream(ctx context.Context, req agent.Request) iter.Seq[agent.AssistantEvent] {
	*p.seen = append(*p.seen, req)
	return p.inner.Stream(ctx, req)
}
