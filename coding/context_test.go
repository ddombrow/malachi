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
		Provider: p, Model: "fake", PrepareRequest: newCodingContextPreparer(nil).prepare,
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
	// The marker replaces the output in place; nothing else is added. A read's
	// outcome says nothing its call does not, so the marker carries no facts.
	if want := "[trimmed read tool output: 45000 bytes]"; oldInRequest.Text() != want {
		t.Fatalf("marker = %q, want %q", oldInRequest.Text(), want)
	}
	if len(requestMessages) != len(history)+1 {
		t.Fatalf("trimming must not add messages: %d vs %d", len(requestMessages), len(history)+1)
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
	cache := newCodingContextPreparer(nil)
	request := agent.Request{Messages: messages}
	prepared := cache.prepare(request)
	if prepared.Messages[1] != assistant || prepared.Messages[2] != result {
		t.Fatal("unmodified history entries should be shared, not deep-copied")
	}
	firstTotal := cache.totalSize
	cache.prepare(request)
	if cache.totalSize != firstTotal || len(cache.results) != 1 {
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
	if len(cache.results) != 2 || cache.totalSize != firstTotal+len("two-two") {
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
	if len(cache.results) != 1 || cache.totalSize != len("replacement-result") {
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
	p := newCodingContextPreparer(log)

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

	// Forcing it produces stats.
	if !p.forceBudget(0) {
		t.Fatal("forceBudget should find output to compact")
	}
	view := p.prepare(agent.Request{Messages: messages}).Messages
	got := log.get()
	if got.Seq == 0 || got.Results != 1 || got.Before <= got.After || got.After <= 0 {
		t.Fatalf("want one compacted result with before>after, got %+v", got)
	}
	if text := agent.MessageText(view[4]); text != "[trimmed read tool output: 20000 bytes]" {
		t.Errorf("trimmed read marker = %q", text)
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
	p := newCodingContextPreparer(log)

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

	// The trim went well below the ceiling, so modest growth trims nothing.
	turn("c8", 20_000)
	p.prepare(agent.Request{Messages: messages})
	if got := log.get(); got.Seq != 1 || got.NewResults != 0 || got.Results != first.Results {
		t.Fatalf("growth under the ceiling must not trim: %+v", got)
	}

	// Growth that pushes past the ceiling again is a real event: more results
	// join the trimmed set.
	turn("c9", 20_000)
	turn("c10", 20_000)
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

// Trimming must not rewrite the request on every turn: the provider caches the
// prompt prefix, and a changed early message re-sends everything after it
// uncached. Once trimmed, a result stays trimmed, an automatic trim goes well
// below the ceiling, and requests between trims only append.
func TestTrimIsStickyAndKeepsThePrefixStable(t *testing.T) {
	p := newCodingContextPreparer(&trimLog{})
	messages := []agent.Message{agent.NewUserText("go")}
	var prev []agent.Message
	rewrites := 0
	for i := 0; i < 40; i++ {
		id := fmt.Sprintf("c%d", i)
		a := agent.NewAssistantMessage("fake")
		a.Content = []agent.Content{&agent.ToolCall{ID: id, Name: "grep", Arguments: map[string]any{"pattern": "x"}}}
		a.StopReason = agent.StopToolUse
		messages = append(messages, a, &agent.ToolResultMessage{
			ToolCallID: id, ToolName: "grep",
			Content: []agent.Content{&agent.TextContent{Text: strings.Repeat("x", 8_000)}},
		})
		view := p.prepare(agent.Request{Messages: messages}).Messages
		for j := range prev {
			if agent.MessageText(prev[j]) != agent.MessageText(view[j]) {
				rewrites++
				if strings.HasPrefix(agent.MessageText(prev[j]), "[trimmed ") {
					t.Fatalf("request %d un-trimmed result %d", i, j)
				}
				break
			}
		}
		prev = view
	}
	// 320 kB through a 64 kB ceiling trimmed to 32 kB is a handful of trims,
	// not one per request.
	if rewrites == 0 || rewrites > 10 {
		t.Fatalf("prefix rewritten on %d of 40 requests", rewrites)
	}

	// A forced trim to an exact budget trims to it, and what it trimmed stays
	// trimmed for the requests after it.
	if !p.forceBudget(0) {
		t.Fatal("forceBudget should find output to trim")
	}
	p.prepare(agent.Request{Messages: messages})
	if p.forceBudget(16_000) {
		t.Fatal("after trimming everything but the last result, nothing but markers is left above the last result")
	}
	view := p.prepare(agent.Request{Messages: messages}).Messages
	for j, m := range view[:len(view)-1] {
		if r, ok := m.(*agent.ToolResultMessage); ok && !strings.HasPrefix(r.Text(), "[trimmed ") {
			t.Fatalf("result %d came back after a forced trim", j)
		}
	}
}

// A trim marker keeps how the call ended, read from the result alone. The
// call's arguments are already in the request beside it and are not repeated.
func TestTrimFactsKeepTheOutcome(t *testing.T) {
	result := func(isError bool, text string, details map[string]any) *agent.ToolResultMessage {
		return &agent.ToolResultMessage{ToolName: "bash", IsError: isError,
			Content: []agent.Content{&agent.TextContent{Text: text}}, Details: details}
	}
	cases := []struct {
		name   string
		result *agent.ToolResultMessage
		want   string
	}{
		{"exit code", result(false, "ok", map[string]any{"command": "make", "exit_code": 0}), "exit 0"},
		{"failure keeps the first error line",
			result(true, "tests failed\nlong detail", map[string]any{"exit_code": 1}), `exit 1: "tests failed"`},
		{"timeout", result(true, "", map[string]any{"timed_out": true}), "timed out"},
		{"edit's first changed line", result(false, "done", map[string]any{"path": "/a.go", "first_changed_line": 12}),
			"first changed line 12"},
		{"error without details", result(true, "no such file", nil), `"no such file"`},
		{"a plain read says nothing", result(false, "contents", map[string]any{"path": "/a.go"}), ""},
	}
	for _, tc := range cases {
		if got := trimFact(tc.result); got != tc.want {
			t.Errorf("%s: fact = %q, want %q", tc.name, got, tc.want)
		}
	}

	long := result(true, strings.Repeat("x", 10_000), nil)
	if got := trimFact(long); len(got) > maxTrimFactFieldBytes+8 {
		t.Errorf("a long error line must be truncated: %d bytes", len(got))
	}
	marker := trimMarker(result(false, "", nil), toolResultSize{bytes: 900, fact: "exit 0"})
	if marker != "[trimmed bash tool output: 900 bytes; exit 0]" {
		t.Errorf("marker = %q", marker)
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
