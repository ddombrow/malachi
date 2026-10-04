package coding

import (
	"context"
	"strings"
	"testing"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/ai/fake"
)

func TestCodingRequestCompactsOldResultsWithoutChangingTranscript(t *testing.T) {
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
		Provider: p, Model: "fake", PrepareRequest: codingRequestPreparer(cwd),
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
	if !strings.Contains(oldInRequest.Text(), "compacted read tool output") {
		t.Fatalf("old result should compact: %.100q", oldInRequest.Text())
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
		if user, ok := message.(*agent.UserMessage); ok && strings.Contains(user.Content.String(), "Session activity ledger") {
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
	if !strings.Contains(requestResults["r1"].Text(), "compacted read tool output") {
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

func TestCompactionPreservesErrorsAndImagesAreCounted(t *testing.T) {
	tooLarge := strings.Repeat("x", toolResultContextBudget+100)
	errorResult := &agent.ToolResultMessage{
		ToolCallID: "error", ToolName: "bash", IsError: true,
		Content: []agent.Content{&agent.TextContent{Text: tooLarge}},
	}
	imageResult := &agent.ToolResultMessage{
		ToolCallID: "image", ToolName: "read",
		Content: []agent.Content{&agent.ImageContent{Data: strings.Repeat("A", toolResultContextBudget+100), MimeType: "image/png"}},
	}
	errorMessages := []agent.Message{errorResult, &agent.ToolResultMessage{
		ToolCallID: "recent", ToolName: "read", Content: []agent.Content{&agent.TextContent{Text: "recent"}},
	}}
	compactedErrors, didCompactErrors := compactToolResults(errorMessages, 1024)
	if !didCompactErrors {
		t.Fatal("oversized error should compact")
	}
	gotError := compactedErrors[0].(*agent.ToolResultMessage)
	if !gotError.IsError || !strings.Contains(gotError.Text(), "compacted bash tool error") {
		t.Fatalf("error status lost during compaction: %+v", gotError)
	}
	imageMessages := []agent.Message{imageResult, &agent.ToolResultMessage{
		ToolCallID: "recent", ToolName: "read", Content: []agent.Content{&agent.TextContent{Text: "recent"}},
	}}
	compactedImages, didCompactImages := compactToolResults(imageMessages, 1024)
	if !didCompactImages {
		t.Fatal("oversized image should compact")
	}
	gotImage := compactedImages[0].(*agent.ToolResultMessage)
	if !strings.Contains(gotImage.Text(), "compacted read tool output") {
		t.Fatalf("image bytes must count toward budget: %q", gotImage.Text())
	}
}

func TestCompactionRetainsOversizedMostRecentResult(t *testing.T) {
	old := &agent.ToolResultMessage{
		ToolCallID: "old", ToolName: "read",
		Content: []agent.Content{&agent.TextContent{Text: strings.Repeat("o", 1024)}},
	}
	recentText := strings.Repeat("r", toolResultContextBudget+100)
	recent := &agent.ToolResultMessage{
		ToolCallID: "recent", ToolName: "read",
		Content: []agent.Content{&agent.TextContent{Text: recentText}},
	}
	compacted, didCompact := compactToolResults([]agent.Message{old, recent}, toolResultContextBudget)
	if !didCompact {
		t.Fatal("older result should compact")
	}
	if compacted[0].(*agent.ToolResultMessage).Text() == strings.Repeat("o", 1024) {
		t.Fatal("older result should be compacted")
	}
	if compacted[1].(*agent.ToolResultMessage).Text() != recentText {
		t.Fatal("most recent result must stay intact even when over budget")
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
	first, second := codingLedger(messages, cwd), codingLedger(messages, cwd)
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
