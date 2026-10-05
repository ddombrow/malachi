package coding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/agent/session"
	"github.com/ddombrow/malachi/ai"
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
	if len(phases) < 4 || !strings.HasPrefix(phases[0], "reading ") || phases[len(phases)-1] != "writing" {
		t.Errorf("phases should name each step, got %v", phases)
	}

	after := s.Harness.Messages()
	if len(after) != res.Kept+1 {
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

// A long summary has to be visibly alive, or it is indistinguishable from a
// hang: the only repaint a phase gets is a phase message.
func TestSummarizeReportsCharactersAsTheyArrive(t *testing.T) {
	// Unthrottled: the fake answers instantly, so the real interval would
	// collapse every delta into the first report.
	defer func(d time.Duration) { compactProgressInterval = d }(compactProgressInterval)
	compactProgressInterval = 0

	// Three deltas, because a single one cannot show the count advancing.
	s := summarizeFixture(t, func(_ context.Context, _ agent.Request, b *ai.Builder) {
		for range 3 {
			b.Text(strings.Repeat("word ", 100))
		}
		b.Done(agent.StopStop)
	})

	var phases []string
	if _, err := s.Summarize(context.Background(), "", func(p string) { phases = append(phases, p) }); err != nil {
		t.Fatalf("Summarize: %v", err)
	}

	var counts []float64
	for _, p := range phases {
		if !strings.HasPrefix(p, "summarizing ") {
			continue
		}
		var v float64
		var unit string
		text := strings.TrimPrefix(p, "summarizing ")
		if _, err := fmt.Sscanf(text, "%f %s", &v, &unit); err != nil {
			t.Fatalf("unreadable count %q: %v", text, err)
		}
		switch unit {
		case "B":
		case "kB":
			v *= 1 << 10
		case "MB":
			v *= 1 << 20
		default:
			t.Fatalf("unexpected unit in %q", text)
		}
		counts = append(counts, v)
	}
	if len(counts) == 0 {
		t.Fatalf("no character counts reported, phases were %v", phases)
	}
	// It has to rise, or the display is a spinner after all: a number that
	// never moves tells the user nothing a static label would not.
	if !slices.IsSorted(counts) || counts[0] == counts[len(counts)-1] {
		t.Errorf("character counts did not advance: %v", counts)
	}
}

// A resumed session's messages come from disk, not from this process's
// persistence listener. Compacting one must still record where the retained
// tail begins, or the next resume replays the summary alone.
func TestSummarizeAfterResumeKeepsTailOnDisk(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	var scripts []fake.Script
	for i := 0; i < 15; i++ {
		scripts = append(scripts, fake.Text("ok"))
	}
	s, err := Open(Options{Cwd: cwd, Home: home, Settings: &Settings{}, Provider: fake.New(scripts...)})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 15; i++ {
		if err := s.Prompt(context.Background(), fmt.Sprintf("turn %d", i)); err != nil {
			t.Fatal(err)
		}
	}

	resumed, err := Open(Options{Cwd: cwd, Home: home, Settings: &Settings{}, Continue: true, Provider: fake.New(fake.Text(goodSummary))})
	if err != nil {
		t.Fatal(err)
	}
	res, err := resumed.Summarize(context.Background(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.FirstKeptID == "" {
		t.Fatal("compaction of a resumed session did not find its tail on disk")
	}

	f, err := session.Load(resumed.Path())
	if err != nil {
		t.Fatal(err)
	}
	replayed := session.Replay(session.BranchPath(f.Entries(), f.TipID())).Messages
	inMemory := resumed.Harness.Messages()
	if len(replayed) != len(inMemory) {
		t.Fatalf("replay has %d messages, memory has %d: the retained tail was lost", len(replayed), len(inMemory))
	}
	for i := 1; i < len(inMemory); i++ {
		if agent.MessageText(replayed[i]) != agent.MessageText(inMemory[i]) {
			t.Fatalf("message %d differs: %q vs %q", i, agent.MessageText(replayed[i]), agent.MessageText(inMemory[i]))
		}
	}
}

// The summariser is asked for the user's goal, so it has to see the user.
func TestSummarizerSeesUserAndAssistantText(t *testing.T) {
	toolTurn := agent.NewAssistantMessage("m")
	toolTurn.Content = []agent.Content{
		&agent.TextContent{Text: "the bug is a missing lookahead; patching"},
		&agent.ToolCall{ID: "c1", Name: "edit", Arguments: map[string]any{"path": "SECRET-ARG"}},
	}
	toolTurn.StopReason = agent.StopToolUse
	body := transcriptJSON([]agent.Message{
		&agent.CompactionSummaryMessage{Summary: "PREVIOUS-SUMMARY"},
		agent.NewUserText("please fix trailing commas"),
		toolTurn,
		&agent.ToolResultMessage{ToolCallID: "c1", ToolName: "edit", Content: []agent.Content{&agent.TextContent{Text: "TOOL-OUTPUT"}}},
		assistantText("done"),
	})
	for _, want := range []string{`"role": "user", "text": "please fix trailing commas"`, "missing lookahead", `"text": "done"`} {
		if !strings.Contains(body, want) {
			t.Errorf("summariser input is missing %q:\n%s", want, body)
		}
	}
	for _, leak := range []string{"TOOL-OUTPUT", "SECRET-ARG", "PREVIOUS-SUMMARY"} {
		if strings.Contains(body, leak) {
			t.Errorf("summariser input should not contain %q", leak)
		}
	}
}

// The request Summarize actually sends carries the user's words.
func TestSummarizeRequestIncludesUserMessages(t *testing.T) {
	p := fake.New(fake.Text(goodSummary))
	s, err := Open(Options{Cwd: t.TempDir(), Home: t.TempDir(), Settings: &Settings{}, Provider: p, NoSession: true})
	if err != nil {
		t.Fatal(err)
	}
	s.Harness.ReplaceMessages(longConversation(15))
	if _, err := s.Summarize(context.Background(), "", nil); err != nil {
		t.Fatal(err)
	}
	sent := agent.MessageText(p.Requests[0].Messages[0])
	if !strings.Contains(sent, "please fix the parser in internal/scan.go") {
		t.Fatalf("the user's request never reached the summariser:\n%s", sent)
	}
}

// Cancelling after the summary arrived but before it was written changes
// nothing, like cancelling at any earlier point.
func TestSummarizeCancelledBeforeWritingChangesNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := summarizeFixture(t, fake.Text(goodSummary))
	before := len(s.Harness.Messages())
	_, err := s.Summarize(ctx, "", func(phase string) {
		if phase == "validating" {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if len(s.Harness.Messages()) != before {
		t.Fatal("a cancelled compaction replaced the transcript")
	}
}

// The summariser input is labelled JSON, so it has to parse as JSON and
// round-trip the text exactly, backslashes and control characters included.
func TestSummarizerInputIsValidJSON(t *testing.T) {
	tricky := "path C:\\new\\table, regex \\d+\\n, quote \", tab\there, <b>&amp;</b>\nnext line"
	body := transcriptJSON([]agent.Message{agent.NewUserText(tricky), assistantText("ok")})
	raw := strings.TrimSuffix(strings.TrimPrefix(body, "```json\n"), "\n```")
	var parsed []struct{ Role, Text string }
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, raw)
	}
	if len(parsed) != 2 || parsed[0].Text != tricky {
		t.Fatalf("text did not round-trip:\n got %q\nwant %q", parsed[0].Text, tricky)
	}
	if strings.Contains(raw, `\u003c`) {
		t.Error("HTML escaping obscures code for the model")
	}
}

// summaryServer is an OpenAI-compatible endpoint that answers every request
// with goodSummary and records the model it was asked for.
func summaryServer(t *testing.T, models *[]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Model string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		*models = append(*models, req.Model)
		chunk, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": goodSummary}, "finish_reason": "stop"}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", chunk)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSummarizeFollowsModelSwitch(t *testing.T) {
	var gotA, gotB []string
	a, b := summaryServer(t, &gotA), summaryServer(t, &gotB)
	settings := &Settings{DefaultProvider: "a", Providers: map[string]ProviderConfig{
		"a": {BaseURL: a.URL, DefaultModel: "model-a"},
		"b": {BaseURL: b.URL, DefaultModel: "model-b"},
	}}
	s, err := Open(Options{Cwd: t.TempDir(), Home: t.TempDir(), Settings: settings, NoSession: true})
	if err != nil {
		t.Fatal(err)
	}
	s.Harness.ReplaceMessages(longConversation(15))
	if err := s.SetModel("b/model-b"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Summarize(context.Background(), "", nil); err != nil {
		t.Fatal(err)
	}
	if len(gotA) != 0 || len(gotB) != 1 || gotB[0] != "model-b" {
		t.Fatalf("summary went to a=%v b=%v; want only b, as model-b", gotA, gotB)
	}
}

// One long agentic turn has no user message near the end. The tail must still
// open where a turn can be resumed, not on a result whose call was summarized.
func TestCompactBoundaryNeverOrphansAToolResult(t *testing.T) {
	messages := []agent.Message{agent.NewUserText("refactor everything")}
	for i := 0; i < 30; i++ {
		a := agent.NewAssistantMessage("m")
		a.StopReason = agent.StopToolUse
		a.Content = []agent.Content{
			&agent.ToolCall{ID: fmt.Sprintf("c%d-a", i), Name: "read", Arguments: map[string]any{}},
			&agent.ToolCall{ID: fmt.Sprintf("c%d-b", i), Name: "read", Arguments: map[string]any{}},
		}
		messages = append(messages, a,
			&agent.ToolResultMessage{ToolCallID: fmt.Sprintf("c%d-a", i), ToolName: "read"},
			&agent.ToolResultMessage{ToolCallID: fmt.Sprintf("c%d-b", i), ToolName: "read"})
	}
	// With 91 messages the naive cut (91-20) lands on a tool result.
	if _, ok := messages[len(messages)-compactKeepMessages].(*agent.ToolResultMessage); !ok {
		t.Fatal("fixture no longer exercises the orphan case")
	}

	keepFrom, err := compactBoundary(messages)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := messages[keepFrom].(*agent.ToolResultMessage); ok {
		t.Fatalf("tail opens on an orphaned tool result at %d", keepFrom)
	}
	tail := messages[keepFrom:]
	if got := agent.ProviderContext(tail); len(got) != len(tail) {
		t.Fatalf("the tail needed repair before sending (%d -> %d messages)", len(tail), len(got))
	}
	if len(tail) < compactKeepMessages {
		t.Fatalf("tail shrank to %d messages; stepping back should only ever keep more", len(tail))
	}
}

// compactKeepMessages is how much stays verbatim: the tail starts at the
// earliest user message inside that window, not the last one, which kept only
// the latest exchange.
func TestCompactBoundaryKeepsTheWholeWindowFromATurnBoundary(t *testing.T) {
	messages := longConversation(20) // 40 alternating messages
	keepFrom, err := compactBoundary(messages)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(messages) - keepFrom; got != compactKeepMessages {
		t.Fatalf("kept %d messages, want %d", got, compactKeepMessages)
	}
	// Odd window: the earliest user message inside it, so one fewer.
	messages = append(messages, agent.NewUserText("one more"))
	keepFrom, _ = compactBoundary(messages)
	if _, ok := messages[keepFrom].(*agent.UserMessage); !ok || len(messages)-keepFrom != compactKeepMessages-1 {
		t.Fatalf("kept %d messages starting with %T", len(messages)-keepFrom, messages[keepFrom])
	}
}
