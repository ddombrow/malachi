package coding

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/ai/fake"
)

func TestCodingRequestTrimsOldResultsWithoutChangingTranscript(t *testing.T) {
	cwd := t.TempDir()
	oldText, recentText := strings.Repeat("old", 15_000), strings.Repeat("new", 15_000)
	oldCall := &agent.ToolCall{ID: "old-call", Name: "read", Arguments: map[string]any{"path": "old.go"}}
	recentCall := &agent.ToolCall{ID: "recent-call", Name: "edit", Arguments: map[string]any{"path": "new.go"}}
	oldResult := &agent.ToolResultMessage{
		ToolCallID: "old-call", ToolName: "read",
		Content: []agent.Content{&agent.TextContent{Text: oldText}},
		Details: map[string]any{"path": cwd + "/old.go"},
	}
	recentResult := &agent.ToolResultMessage{
		ToolCallID: "recent-call", ToolName: "edit",
		Content: []agent.Content{&agent.TextContent{Text: recentText}},
		Details: map[string]any{"path": cwd + "/new.go", "first_changed_line": 7},
	}
	toolTurn := func(call *agent.ToolCall) *agent.AssistantMessage {
		m := agent.NewAssistantMessage("fake")
		m.Content = []agent.Content{call}
		m.StopReason = agent.StopToolUse
		return m
	}
	history := []agent.Message{
		agent.NewUserText("inspect and edit"),
		toolTurn(oldCall), oldResult,
		toolTurn(recentCall), recentResult,
	}
	p := fake.New(fake.Text("done"))
	h := agent.NewHarness(agent.HarnessConfig{
		Provider: p, Model: "fake", PrepareRequest: newCodingContextPreparer(cwd, nil).prepare,
	}, history)

	if err := h.Prompt(context.Background(), agent.NewUserText("continue")); err != nil {
		t.Fatal(err)
	}
	if len(p.Requests) != 1 {
		t.Fatalf("provider calls = %d", len(p.Requests))
	}
	requestMessages := p.Requests[0].Messages
	results := toolResultsByID(requestMessages)
	oldInRequest, recentInRequest := results["old-call"], results["recent-call"]
	if oldInRequest == nil || recentInRequest == nil {
		t.Fatalf("tool results not paired in request: %#v", results)
	}
	if !strings.Contains(oldInRequest.Text(), "trimmed read tool output") {
		t.Fatalf("old result should compact: %.100q", oldInRequest.Text())
	}
	if oldInRequest == oldResult {
		t.Fatal("compaction should copy only the result being changed")
	}
	if recentInRequest.Text() != recentText {
		t.Fatal("most recent result should remain intact")
	}
	if oldInRequest.Details.(map[string]any)["path"] != cwd+"/old.go" {
		t.Fatal("compaction should retain result details")
	}

	transcriptResults := toolResultsByID(h.Messages())
	if transcriptResults["old-call"].Text() != oldText || transcriptResults["recent-call"].Text() != recentText {
		t.Fatal("request compaction must not mutate the canonical transcript")
	}
	ledger := ""
	for _, message := range requestMessages {
		if user, ok := message.(*agent.UserMessage); ok && strings.Contains(user.Content.String(), "<untrusted_tool_ledger>") {
			ledger = user.Content.String()
		}
	}
	if ledger == "" {
		t.Fatal("request should include the deterministic activity ledger")
	}
	if !strings.Contains(ledger, `read "old.go"`) || !strings.Contains(ledger, `edit "new.go"`) {
		t.Fatalf("ledger omitted tool facts: %q", ledger)
	}
}

func TestCodingContextCacheReusesAppendOnlyPrefixAndInvalidatesReplacement(t *testing.T) {
	cwd := t.TempDir()
	call := &agent.ToolCall{ID: "first", Name: "read", Arguments: map[string]any{"path": "a.go"}}
	assistant := agent.NewAssistantMessage("fake")
	assistant.Content = []agent.Content{call}
	result := &agent.ToolResultMessage{
		ToolCallID: "first", ToolName: "read",
		Content: []agent.Content{&agent.TextContent{Text: "one"}},
		Details: map[string]any{"path": filepath.Join(cwd, "a.go")},
	}
	messages := []agent.Message{agent.NewUserText("start"), assistant, result}
	cache := newCodingContextPreparer(cwd, nil)
	request := agent.Request{Messages: messages}
	prepared := cache.prepare(request)
	if prepared.Messages[1] != assistant || prepared.Messages[2] != result {
		t.Fatal("unmodified history entries should be shared, not deep-copied")
	}
	firstTotal, firstRecords := cache.totalSize, len(cache.records)
	cache.prepare(request)
	if cache.totalSize != firstTotal || len(cache.results) != 1 || len(cache.records) != firstRecords {
		t.Fatal("preparing the same prefix should reuse cached facts")
	}

	call2 := &agent.ToolCall{ID: "second", Name: "write", Arguments: map[string]any{"path": "b.go"}}
	assistant2 := agent.NewAssistantMessage("fake")
	assistant2.Content = []agent.Content{call2}
	result2 := &agent.ToolResultMessage{
		ToolCallID: "second", ToolName: "write",
		Content: []agent.Content{&agent.TextContent{Text: "two-two"}},
		Details: map[string]any{"path": filepath.Join(cwd, "b.go")},
	}
	appended := append(append([]agent.Message(nil), messages...), assistant2, result2)
	cache.prepare(agent.Request{Messages: appended})
	if len(cache.results) != 2 || cache.totalSize != firstTotal+len("two-two") || len(cache.records) != 2 {
		t.Fatal("cache should process only newly appended messages")
	}

	replacementCall := &agent.ToolCall{ID: "replacement", Name: "edit", Arguments: map[string]any{"path": "c.go"}}
	replacementAssistant := agent.NewAssistantMessage("fake")
	replacementAssistant.Content = []agent.Content{replacementCall}
	replacementResult := &agent.ToolResultMessage{
		ToolCallID: "replacement", ToolName: "edit",
		Content: []agent.Content{&agent.TextContent{Text: "replacement-result"}},
		Details: map[string]any{"path": filepath.Join(cwd, "c.go")},
	}
	replaced := []agent.Message{agent.NewUserText("new history"), replacementAssistant, replacementResult}
	cache.prepare(agent.Request{Messages: replaced})
	if len(cache.results) != 1 || cache.totalSize != len("replacement-result") || len(cache.records) != 1 ||
		!strings.Contains(cache.records[0], `edit "c.go"`) {
		t.Fatal("cache should rebuild when the transcript prefix is replaced")
	}
}

func TestCompactionDoesNotRewriteSavedSession(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	firstText, secondText := strings.Repeat("first", 8_000), strings.Repeat("second", 7_000)
	write(t, cwd, "first.txt", firstText)
	write(t, cwd, "second.txt", secondText)
	p := fake.New(
		fake.ToolCalls("", fake.Call{ID: "r1", Name: "read", Args: map[string]any{"path": "first.txt"}},
			fake.Call{ID: "r2", Name: "read", Args: map[string]any{"path": "second.txt"}}),
		fake.Text("finished"),
	)
	s, err := Open(testOpts(t, home, cwd, p))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Prompt(context.Background(), "read both files"); err != nil {
		t.Fatal(err)
	}
	requestResults := toolResultsByID(p.Requests[1].Messages)
	if !strings.Contains(requestResults["r1"].Text(), "trimmed read tool output") {
		t.Fatal("provider request should compact the old result")
	}
	if requestResults["r2"].Text() != secondText {
		t.Fatal("provider request should retain the recent result")
	}

	resumed, err := Open(Options{Cwd: cwd, Home: home, Settings: &Settings{}, Provider: fake.New(fake.Text("ok")), Continue: true})
	if err != nil {
		t.Fatal(err)
	}
	savedResults := toolResultsByID(resumed.Harness.Messages())
	if savedResults["r1"].Text() != firstText || savedResults["r2"].Text() != secondText {
		t.Fatal("saved session must retain the original tool outputs")
	}
}

func TestTrimPreservesErrorsAndImagesAreCounted(t *testing.T) {
	tooLarge := strings.Repeat("x", defaultToolResultBudget+100)
	errorResult := &agent.ToolResultMessage{
		ToolCallID: "error", ToolName: "bash", IsError: true,
		Content: []agent.Content{&agent.TextContent{Text: tooLarge}},
	}
	imageResult := &agent.ToolResultMessage{
		ToolCallID: "image", ToolName: "read",
		Content: []agent.Content{&agent.ImageContent{Data: strings.Repeat("A", defaultToolResultBudget+100), MimeType: "image/png"}},
	}
	errorMessages := []agent.Message{errorResult, &agent.ToolResultMessage{
		ToolCallID: "recent", ToolName: "read", Content: []agent.Content{&agent.TextContent{Text: "recent"}},
	}}
	trimmedErrors, n, _, _, _ := trimToolResults(errorMessages, 1024)
	if n == 0 {
		t.Fatal("oversized error should compact")
	}
	gotError := trimmedErrors[0].(*agent.ToolResultMessage)
	if !gotError.IsError || !strings.Contains(gotError.Text(), "trimmed bash tool error") {
		t.Fatalf("error status lost during compaction: %+v", gotError)
	}
	imageMessages := []agent.Message{imageResult, &agent.ToolResultMessage{
		ToolCallID: "recent", ToolName: "read", Content: []agent.Content{&agent.TextContent{Text: "recent"}},
	}}
	trimmedImages, n, _, _, _ := trimToolResults(imageMessages, 1024)
	if n == 0 {
		t.Fatal("oversized image should compact")
	}
	gotImage := trimmedImages[0].(*agent.ToolResultMessage)
	if !strings.Contains(gotImage.Text(), "trimmed read tool output") {
		t.Fatalf("image bytes must count toward budget: %q", gotImage.Text())
	}
}

func TestTrimRetainsOversizedMostRecentResult(t *testing.T) {
	old := &agent.ToolResultMessage{
		ToolCallID: "old", ToolName: "read",
		Content: []agent.Content{&agent.TextContent{Text: strings.Repeat("o", 1024)}},
	}
	recentText := strings.Repeat("r", defaultToolResultBudget+100)
	recent := &agent.ToolResultMessage{
		ToolCallID: "recent", ToolName: "read",
		Content: []agent.Content{&agent.TextContent{Text: recentText}},
	}
	compacted, n, _, _, _ := trimToolResults([]agent.Message{old, recent}, defaultToolResultBudget)
	if n == 0 {
		t.Fatal("older result should compact")
	}
	if compacted[0].(*agent.ToolResultMessage).Text() == strings.Repeat("o", 1024) {
		t.Fatal("older result should be compacted")
	}
	if compacted[1].(*agent.ToolResultMessage).Text() != recentText {
		t.Fatal("most recent result must stay intact even when over budget")
	}
}

// Compaction is reported to the UI even though nothing is persisted, and a
// forced pass applies to exactly one request.
func TestTrimStatsAndForcedBudget(t *testing.T) {
	cwd := t.TempDir()
	log := &trimLog{}
	p := newCodingContextPreparer(cwd, log)

	small := agent.NewAssistantMessage("fake")
	small.Content = []agent.Content{&agent.ToolCall{ID: "a", Name: "bash", Arguments: map[string]any{"command": "ls"}}}
	small.StopReason = agent.StopToolUse
	read := agent.NewAssistantMessage("fake")
	read.Content = []agent.Content{&agent.ToolCall{ID: "b", Name: "read", Arguments: map[string]any{"path": "big.go"}}}
	read.StopReason = agent.StopToolUse
	build := agent.NewAssistantMessage("fake")
	build.Content = []agent.Content{&agent.ToolCall{ID: "c", Name: "bash", Arguments: map[string]any{"command": "make"}}}
	build.StopReason = agent.StopToolUse
	toolResult := func(id, name string, n int, details map[string]any) agent.Message {
		return &agent.ToolResultMessage{ToolCallID: id, ToolName: name,
			Content: []agent.Content{&agent.TextContent{Text: strings.Repeat("x", n)}}, Details: details}
	}
	messages := []agent.Message{
		agent.NewUserText("go"),
		small,
		toolResult("a", "bash", 2, map[string]any{"command": "ls", "exit_code": 0}),
		read,
		// Large, and old enough to be compacted.
		toolResult("b", "read", 20_000, map[string]any{"path": filepath.Join(cwd, "big.go")}),
		build,
		// The most recent result is retained however large it is.
		toolResult("c", "bash", 30_000, map[string]any{"command": "make", "exit_code": 1}),
	}

	// Under the default ceiling: nothing fires, so the UI stays silent.
	p.prepare(agent.Request{Messages: messages})
	if got := log.get(); got.Seq != 0 {
		t.Fatalf("compacted below the default ceiling: %+v", got)
	}

	// Forcing it produces stats, including the ledger entry count.
	if !p.forceBudget(0) {
		t.Fatal("forceBudget should find output to compact")
	}
	p.prepare(agent.Request{Messages: messages})
	got := log.get()
	if got.Seq == 0 || got.Results != 1 || got.Before <= got.After || got.After <= 0 {
		t.Fatalf("want one compacted result with before>after, got %+v", got)
	}
	if got.LedgerEntries != 3 {
		t.Errorf("ledger entries = %d, want 3 (bash, read, bash)", got.LedgerEntries)
	}

	// The override lasts one request; the next pass is back to the default.
	seq := got.Seq
	p.prepare(agent.Request{Messages: messages})
	if after := log.get(); after.Seq != seq {
		t.Errorf("a second pass fired without the ceiling being exceeded: %+v", after)
	}

	// Nothing left to compact reports honestly instead of arming a no-op.
	if p.forceBudget(1 << 20) {
		t.Error("forceBudget claimed work above the total output size")
	}
}

// A pass that re-applies the same markers is not an event: the transcript is
// never rewritten, so every request trims the same results again. Reporting
// that once per request produced pairs of identical markers.
func TestRepeatedPassesReportOnlyNewlyTrimmedResults(t *testing.T) {
	log := &trimLog{}
	p := newCodingContextPreparer(t.TempDir(), log)

	var messages []agent.Message
	turn := func(id string, n int) {
		a := agent.NewAssistantMessage("fake")
		a.Content = []agent.Content{&agent.ToolCall{ID: id, Name: "bash", Arguments: map[string]any{"command": "make"}}}
		a.StopReason = agent.StopToolUse
		messages = append(messages, a, &agent.ToolResultMessage{
			ToolCallID: id, ToolName: "bash",
			Content: []agent.Content{&agent.TextContent{Text: strings.Repeat("x", n)}},
			Details: map[string]any{"command": "make", "exit_code": 0},
		})
	}
	messages = append(messages, agent.NewUserText("go"))
	// Well over the default ceiling, so several results must be trimmed.
	for i := 0; i < 8; i++ {
		turn(fmt.Sprintf("c%d", i), 20_000)
	}

	p.prepare(agent.Request{Messages: messages})
	first := log.get()
	if first.Seq != 1 || first.NewResults != first.Results || first.Results < 2 {
		t.Fatalf("first pass should trim several results exactly once: %+v", first)
	}

	// The transcript is unchanged, so the same results are re-trimmed and
	// nothing is new. This is the case that produced paired markers.
	p.prepare(agent.Request{Messages: messages})
	if got := log.get(); got.Seq != 1 || got.NewResults != 0 || got.Results != first.Results {
		t.Fatalf("a repeat pass must not report again: %+v", got)
	}

	// Growth that pushes the ceiling over another result is a real event: one
	// more result joins the trimmed set.
	turn("c8", 20_000)
	p.prepare(agent.Request{Messages: messages})
	second := log.get()
	if second.Seq != 2 || second.NewResults < 1 || second.Results <= first.Results {
		t.Fatalf("a newly trimmed result should be reported once: %+v", second)
	}

	// And once more with no change: silent again.
	p.prepare(agent.Request{Messages: messages})
	if got := log.get(); got.Seq != 2 || got.NewResults != 0 {
		t.Errorf("still no new trimming, so still no report: %+v", got)
	}
}

func TestCodingLedgerIsBoundedAndDeterministic(t *testing.T) {
	cwd := t.TempDir()
	call := &agent.ToolCall{ID: "c", Name: "bash", Arguments: map[string]any{"command": "go test ./..."}}
	assistant := agent.NewAssistantMessage("fake")
	assistant.Content = []agent.Content{call}
	result := &agent.ToolResultMessage{
		ToolCallID: "c", ToolName: "bash",
		Content: []agent.Content{&agent.TextContent{Text: "tests failed\nlong detail"}},
		Details: map[string]any{"command": "go test ./...", "exit_code": 1},
	}
	messages := []agent.Message{assistant, result}
	cache := newCodingContextPreparer(cwd, nil)
	cache.update(messages)
	first, _ := ledgerText(cache.records)
	second, _ := ledgerText(cache.records)
	if first == "" || first != second || len(first) > maxLedgerBytes {
		t.Fatalf("ledger should be deterministic and bounded: len=%d", len(first))
	}
	if !strings.Contains(first, `bash "go test ./..." — exit 1`) {
		t.Fatalf("ledger missing command result: %q", first)
	}
}

func toolResultsByID(messages []agent.Message) map[string]*agent.ToolResultMessage {
	results := make(map[string]*agent.ToolResultMessage)
	for _, m := range messages {
		if result, ok := m.(*agent.ToolResultMessage); ok {
			results[result.ToolCallID] = result
		}
	}
	return results
}

// The count reported is what was sent: the byte cap can leave fewer records
// than the record cap.
func TestLedgerCountsWhatFits(t *testing.T) {
	var records []string
	for i := 0; i < maxLedgerRecords; i++ {
		records = append(records, fmt.Sprintf("read %03d %s", i, strings.Repeat("x", 400)))
	}
	text, n := ledgerText(records)
	if n == 0 || n >= maxLedgerRecords || len(text) > maxLedgerBytes {
		t.Fatalf("sent %d of %d records in %d bytes", n, len(records), len(text))
	}
	if got := strings.Count(text, "\nread "); got != n {
		t.Fatalf("reported %d records, text holds %d", n, got)
	}
	if !strings.Contains(text, fmt.Sprintf("read %03d", maxLedgerRecords-1)) {
		t.Fatal("the newest record was dropped")
	}
	if _, n := ledgerText(records[:3]); n != 3 {
		t.Fatalf("small ledger reported %d", n)
	}
}
