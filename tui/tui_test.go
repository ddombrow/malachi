package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/ai/fake"
	"github.com/ddombrow/malachi/coding"
)

func newTestModel(t *testing.T) *model {
	t.Helper()
	s, err := coding.Open(coding.Options{
		Cwd: t.TempDir(), Home: t.TempDir(), Settings: &coding.Settings{},
		Provider: fake.New(fake.Text("hi")), NoSession: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	m := newModel(s, "")
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	return m
}

// shown returns the plain text of the transcript after running fn, which
// is how tests observe what the user would see in the viewport.
func shown(m *model, fn func() tea.Cmd) string {
	before := len(ansi.Strip(m.tr.text(m.r)))
	fn()
	return ansi.Strip(m.tr.text(m.r))[before:]
}

func TestStreamingTextIsLiveThenPrinted(t *testing.T) {
	m := newTestModel(t)
	partial := agent.NewAssistantMessage("m")
	partial.Content = []agent.Content{&agent.TextContent{Text: "Hello wor"}}
	m.Update(sessionEventMsg{&agent.MessageUpdateEvent{Message: partial, AssistantMessageEvent: &agent.TextDelta{Delta: "wor", Partial: partial}}})
	if v := m.View().Content; !strings.Contains(v, "Hello wor") {
		t.Fatalf("live area missing partial text:\n%s", v)
	}

	final := partial.Clone()
	final.Content = []agent.Content{&agent.TextContent{Text: "Hello world"}}
	final.Usage = agent.Usage{Input: 10, Output: 5, TotalTokens: 15}
	out := shown(m, func() tea.Cmd { return m.handleEvent(&agent.MessageEndEvent{Message: final}) })
	if !strings.Contains(out, "world") {
		t.Fatalf("final text not printed: %s", out)
	}
	if strings.Contains(m.live(), "Hello") {
		t.Fatal("completed message must leave the live area")
	}
	if !strings.Contains(m.statusLine(), "↑10 ↓5") {
		t.Fatalf("status: %s", m.statusLine())
	}
}

func TestToolLifecycle(t *testing.T) {
	m := newTestModel(t)
	args := map[string]any{"command": "go test ./..."}
	m.Update(sessionEventMsg{&agent.ToolExecutionStartEvent{ToolCallID: "c", ToolName: "bash", Args: args}})
	m.Update(sessionEventMsg{&agent.ToolExecutionUpdateEvent{ToolCallID: "c", ToolName: "bash", PartialResult: agent.TextResult("ok pkg/a")}})
	v := m.View().Content
	if !strings.Contains(v, "$ go test ./...") || !strings.Contains(v, "ok pkg/a") {
		t.Fatalf("running tool not shown:\n%s", v)
	}
	out := shown(m, func() tea.Cmd {
		return m.handleEvent(&agent.ToolExecutionEndEvent{ToolCallID: "c", ToolName: "bash", Result: agent.TextResult("ok pkg/a\nok pkg/b")})
	})
	if !strings.Contains(out, m.r.icons[iconBash]) || !strings.Contains(out, "ok pkg/b") {
		t.Fatalf("result not printed: %s", out)
	}
	if strings.Contains(m.live(), "$ go test") || m.last == nil {
		t.Fatal("finished tool must leave the live area and be kept for /last")
	}
}

func TestEditResultShowsDiff(t *testing.T) {
	m := newTestModel(t)
	res := agent.TextResult("Successfully replaced 1 block(s)")
	res.Details = map[string]any{"patch": "--- f\n+++ f\n@@ -1,1 +1,1 @@\n-old\n+new\n"}
	out := m.r.toolResult("edit", map[string]any{"path": "f", "edits": []any{1}}, res, false)
	if !strings.Contains(out, "-old") || !strings.Contains(out, "+new") || strings.Contains(out, "+++") {
		t.Fatalf("diff: %s", out)
	}
}

func TestSlashCommands(t *testing.T) {
	m := newTestModel(t)
	if out := shown(m, func() tea.Cmd { return m.command("/help") }); !strings.Contains(out, "/resume") {
		t.Fatalf("help: %s", out)
	}
	if out := shown(m, func() tea.Cmd { return m.command("/bogus") }); !strings.Contains(out, "unknown command") {
		t.Fatalf("unknown: %s", out)
	}
	if out := shown(m, func() tea.Cmd { return m.command("/thinking high") }); !strings.Contains(out, "high") || m.s.ThinkingLevel() != "high" {
		t.Fatalf("thinking: %s", out)
	}
}

func TestCopyLatestAssistantResponse(t *testing.T) {
	m := newTestModel(t)
	if out := shown(m, func() tea.Cmd { return m.command("/copy") }); !strings.Contains(out, "no assistant response") {
		t.Fatalf("copy without a response: %s", out)
	}

	a := agent.NewAssistantMessage("m")
	a.Content = []agent.Content{&agent.TextContent{Text: "A **copyable** answer."}}
	m.Update(sessionEventMsg{&agent.MessageEndEvent{Message: a}})
	if m.lastReply != "A **copyable** answer." {
		t.Fatalf("stored reply = %q", m.lastReply)
	}
	if out := shown(m, func() tea.Cmd { return m.command("/copy") }); !strings.Contains(out, "copied latest assistant response") {
		t.Fatalf("copy feedback: %s", out)
	}
}

func TestBashStatusMarks(t *testing.T) {
	m := newTestModel(t)
	for _, tc := range []struct {
		details map[string]any
		want    []string
	}{
		{map[string]any{"exit_code": 0}, []string{m.r.icons[iconBash]}},
		{map[string]any{"exit_code": float64(0)}, []string{m.r.icons[iconBash]}}, // resumed from JSON
		{map[string]any{"exit_code": 2}, []string{m.r.icons[iconBash], "✗ exit 2"}},
		{map[string]any{"cancelled": true, "exit_code": -1}, []string{m.r.icons[iconCancelled], "cancelled"}},
		{map[string]any{"timed_out": true, "exit_code": -1}, []string{m.r.icons[iconTimeout], "timed out"}},
	} {
		res := agent.TextResult("x")
		res.Details = tc.details
		out := m.r.toolResult("bash", map[string]any{"command": "c"}, res, false)
		for _, w := range tc.want {
			if !strings.Contains(out, w) {
				t.Errorf("%v: want %q in %q", tc.details, w, out)
			}
		}
		wantExit := strings.Contains(strings.Join(tc.want, " "), "✗ exit")
		if wantExit != strings.Contains(out, "✗ exit") {
			t.Errorf("%v: unexpected failure marker in %q", tc.details, out)
		}
	}
}

func TestIconsPerItemKind(t *testing.T) {
	m := newTestModel(t)
	for tool, kind := range map[string]string{
		"read": iconRead, "edit": iconEdit, "write": iconWrite,
		"bash": iconBash, "grep": iconGrep, "glob": iconGlob,
	} {
		if out := m.r.toolResult(tool, map[string]any{}, agent.TextResult(""), false); !strings.Contains(out, m.r.icons[kind]) {
			t.Errorf("%s: want %s in %q", tool, kind, out)
		}
	}
	if out := m.r.toolResult("read", map[string]any{"path": "x"}, agent.TextResult("File not found"), true); !strings.Contains(out, m.r.icons[iconError]) {
		t.Errorf("error icon missing: %q", out)
	}
	a := agent.NewAssistantMessage("m")
	a.Content = []agent.Content{&agent.ThinkingContent{Thinking: "hmm"}, &agent.TextContent{Text: "Done."}}
	out := m.r.assistantMessage(a)
	if !strings.Contains(out, m.r.icons[iconThinking]) || !strings.Contains(out, m.r.icons[iconReply]) {
		t.Errorf("reply icons missing: %q", out)
	}
}

// Continuation lines must align under the first line's text, past the icon.
func TestGutterAlignsMarkdown(t *testing.T) {
	m := newTestModel(t)
	a := agent.NewAssistantMessage("m")
	a.Content = []agent.Content{&agent.TextContent{Text: "First paragraph.\n\n- one\n- two"}}
	out := ansi.Strip(m.r.assistantMessage(a))
	lines := strings.Split(strings.TrimLeft(out, "\n"), "\n")
	glyph := m.r.icons[iconReply]
	indent := strings.Repeat(" ", lipgloss.Width(glyph)+1)
	if !strings.HasPrefix(lines[0], glyph+" First paragraph.") {
		t.Fatalf("first line: %q", lines[0])
	}
	for _, l := range lines[1:] {
		if l != "" && !strings.HasPrefix(l, indent) {
			t.Errorf("continuation not indented under text: %q", l)
		}
		if strings.HasSuffix(l, " ") {
			t.Errorf("trailing blank: %q", l)
		}
	}
}

func TestListReplyStartsOnIconLine(t *testing.T) {
	m := newTestModel(t)
	a := agent.NewAssistantMessage("m")
	a.Content = []agent.Content{&agent.TextContent{Text: "- **one**\n- two"}}
	out := strings.TrimLeft(ansi.Strip(m.r.assistantMessage(a)), "\n")
	if !strings.HasPrefix(out, m.r.icons[iconReply]+" • one") {
		t.Fatalf("got %q", out)
	}
}

func TestUserMessageWrapsInsideGutter(t *testing.T) {
	r := newRenderer(30, true, "emoji", "")
	out := strings.TrimLeft(ansi.Strip(r.userMessage("please read the readme and summarize it briefly")), "\n")
	lines := strings.Split(out, "\n")
	if len(lines) < 2 || !strings.HasPrefix(lines[0], "❯ please") || !strings.HasPrefix(lines[1], "  ") {
		t.Fatalf("got %q", out)
	}
	for _, l := range lines {
		if ansi.StringWidth(l) > 30 {
			t.Errorf("line too wide: %q", l)
		}
	}
}

func TestSummaryShortensPaths(t *testing.T) {
	r := newRenderer(80, true, "emoji", "/work/proj")
	if got := r.summary("read", map[string]any{"path": "/work/proj/src/a.go"}); got != "read src/a.go" {
		t.Fatalf("got %q", got)
	}
}

func TestViewportFollowsUnlessScrolledUp(t *testing.T) {
	m := newTestModel(t)
	for i := 0; i < 60; i++ {
		m.Update(printMsg{func(r *renderer) string { return item(fmt.Sprintf("line %d", i)) }})
	}
	if !m.vp.AtBottom() || !strings.Contains(m.View().Content, "line 59") {
		t.Fatal("should follow new output")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	m.Update(printMsg{func(r *renderer) string { return item("line 60") }})
	if m.vp.AtBottom() || strings.Contains(m.View().Content, "line 60") {
		t.Fatal("scrolled-up view must stay put")
	}
	if !strings.Contains(m.statusLine(), "ctrl+end") {
		t.Fatal("status should hint at more output below")
	}
	m.usage = agent.Usage{Input: 123456, Output: 98765, CacheRead: 55555, TotalTokens: 1}
	m.context = 777777
	m.Update(tea.WindowSizeMsg{Width: 60, Height: 30})
	if st := ansi.Strip(m.statusLine()); !strings.HasSuffix(st, "ctrl+end"+statusPad) || ansi.StringWidth(st) > 60 {
		t.Fatalf("hint must survive a narrow status line: %q", st)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnd, Mod: tea.ModCtrl})
	if !m.vp.AtBottom() {
		t.Fatal("ctrl+end should jump to the bottom")
	}
}

func TestResizeRewrapsTranscript(t *testing.T) {
	m := newTestModel(t)
	long := strings.Repeat("word ", 30)
	m.Update(printMsg{func(r *renderer) string { return r.userMessage(long) }})
	wide := strings.Count(ansi.Strip(m.tr.text(m.r)), "\n")
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 30})
	narrow := strings.Count(ansi.Strip(m.tr.text(m.r)), "\n")
	if narrow <= wide {
		t.Fatalf("narrower terminal should wrap to more lines (%d vs %d)", narrow, wide)
	}
}

func TestItemsAreSeparatedByBlankLines(t *testing.T) {
	m := newTestModel(t)
	m.Update(printMsg{func(r *renderer) string { return r.userMessage("hi") }})
	m.Update(printMsg{func(r *renderer) string { return "" }}) // empty turns add nothing
	m.Update(printMsg{func(r *renderer) string { return item("second") }})
	text := ansi.Strip(m.tr.text(m.r))
	if !strings.Contains(text, "❯ hi\n\nsecond") {
		t.Fatalf("got %q", text)
	}
	if lines := strings.Split(m.View().Content, "\n"); len(lines) != m.height {
		t.Fatalf("view must fill the screen exactly: %d lines for height %d", len(lines), m.height)
	}
}

func TestDotsIconSet(t *testing.T) {
	r := newRenderer(80, true, "dots", "")
	out := ansi.Strip(r.toolResult("bash", map[string]any{"command": "ls"}, agent.TextResult(""), false))
	if !strings.Contains(out, "⏺ $ ls") {
		t.Fatalf("dots: %q", out)
	}
	if newRenderer(80, true, "bogus", "").icons[iconReply] != iconSets["emoji"][iconReply] {
		t.Fatal("unknown icon set should fall back to emoji")
	}
}

func TestSpinnerStopsWhenIdle(t *testing.T) {
	m := newTestModel(t)
	_, cmd := m.Update(m.spin.Tick())
	if cmd != nil {
		t.Fatal("idle session must not re-arm spinner")
	}
	m.Update(sessionEventMsg{coding.CompactionStartEvent{Reason: coding.CompactionManual}})
	// A compaction is work too, and its line carries the spinner.
	release := make(chan struct{})
	defer close(release)
	busy := compactingModel(t, release)
	_, cmd = busy.Update(busy.spin.Tick())
	if cmd == nil {
		t.Fatal("a busy session must keep the spinner ticking")
	}
}

func TestWrapLinesUsesCells(t *testing.T) {
	lines := wrapLines(strings.Repeat("漢", 12), 20)
	if got := strings.Join(lines, ""); got != strings.Repeat("漢", 12) {
		t.Fatalf("lost text: %q", got)
	}
	for _, l := range lines {
		if w := ansi.StringWidth(l); w > 20 {
			t.Fatalf("line %q is %d cells", l, w)
		}
	}
}

func TestStatusLineFitsNarrowTerminal(t *testing.T) {
	m := newTestModel(t)
	m.usage = agent.Usage{Input: 123456, Output: 98765, CacheRead: 55555, TotalTokens: 1}
	m.context = 777777
	m.Update(tea.WindowSizeMsg{Width: 8, Height: 30})
	if st := ansi.Strip(m.statusLine()); ansi.StringWidth(st) > 8 {
		t.Fatalf("status too wide: %q (%d)", st, ansi.StringWidth(st))
	}
}

func TestBannerFitsNarrowTerminal(t *testing.T) {
	s := malachiTestSession(t)
	for _, w := range []int{7, 8, 10, 14, 30, 80} {
		r := newRenderer(w, true, "emoji", "")
		for _, resumed := range []int{0, 3} {
			for _, l := range strings.Split(ansi.Strip(r.banner(s, resumed)), "\n") {
				if got := ansi.StringWidth(l); got > w {
					t.Errorf("width=%d resumed=%d: line %q is %d cells", w, resumed, l, got)
				}
			}
		}
	}
}

// malachiTestSession returns a throwaway session for renderer tests.
func malachiTestSession(t *testing.T) *coding.Session {
	t.Helper()
	s, err := coding.Open(coding.Options{
		Cwd: t.TempDir(), Home: t.TempDir(), Settings: &coding.Settings{},
		Provider: fake.New(fake.Text("hi")), NoSession: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The status bar separates itself with a tinted full-width row rather than a
// blank line above it, so the input sits closer to the transcript.
func TestStatusBarIsTinted(t *testing.T) {
	m := newTestModel(t)
	if m.r.st.status.GetBackground() == nil {
		t.Fatal("status bar needs a background so it reads as its own row")
	}
	if w := ansi.StringWidth(ansi.Strip(m.statusLine())); w != m.width {
		t.Fatalf("status bar must span the full width to tint it: %d != %d", w, m.width)
	}
}

// The input's prompt must be the same glyph and indent as a user message in
// the transcript, so the box lines up with what it is replying to.
func TestInputPromptMatchesUserGutter(t *testing.T) {
	m := newTestModel(t)
	glyph := m.r.icons[iconUser]
	if m.input.Prompt != glyph+" " {
		t.Fatalf("prompt %q, want %q", m.input.Prompt, glyph+" ")
	}
	if got, want := lipgloss.Width(m.input.Prompt), m.r.gutterWidth(iconUser); got != want {
		t.Fatalf("prompt is %d cells, user gutter is %d", got, want)
	}
	// The dots set keeps its own user glyph, so the prompt follows the set.
	d := newTestModel(t)
	d.r = newRenderer(80, true, "dots", "")
	d.applyInputStyles()
	if d.input.Prompt != d.r.icons[iconUser]+" " {
		t.Fatalf("prompt did not follow the icon set: %q", d.input.Prompt)
	}
}

// Compaction never touches the transcript, so the status bar and a transcript
// marker are the only way the user learns the model's view was trimmed.
func TestTrimmingIsVisible(t *testing.T) {
	m := newTestModel(t)
	if strings.Contains(ansi.Strip(m.statusLine()), "cmp ") {
		t.Fatal("status bar shows cmp before anything has compacted")
	}

	// A run with large tool output trips the ceiling.
	history := []agent.Message{
		agent.NewUserText("look around"),
	}
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("c%d", i)
		a := agent.NewAssistantMessage("fake")
		a.Content = []agent.Content{&agent.ToolCall{ID: id, Name: "bash", Arguments: map[string]any{"command": "make"}}}
		a.StopReason = agent.StopToolUse
		history = append(history, a, &agent.ToolResultMessage{
			ToolCallID: id, ToolName: "bash",
			Content: []agent.Content{&agent.TextContent{Text: strings.Repeat("out ", 20_000)}},
			Details: map[string]any{"command": "make", "exit_code": 0},
		})
	}
	m.s.Harness.ReplaceMessages(history)
	if err := m.s.Prompt(context.Background(), "go on"); err != nil {
		t.Fatal(err)
	}
	m.Update(nil)

	c := m.s.Trim()
	if c.Seq == 0 {
		t.Fatal("a provider request with 80 kB of tool output must compact")
	}
	if st := ansi.Strip(m.statusLine()); !strings.Contains(st, "cmp ") {
		t.Errorf("status bar must show compaction: %q", st)
	}
	out := ansi.Strip(m.tr.text(m.r))
	if !strings.Contains(out, "trimmed 3 tool results") {
		t.Errorf("transcript marker missing or wrong: %q", out)
	}
	if !strings.Contains(out, "kB") || !strings.Contains(out, "ledger 4 entries") {
		t.Errorf("marker should report sizes and ledger size: %q", out)
	}
}

func TestTrimCommandForcesNextRequest(t *testing.T) {
	m := newTestModel(t)
	// Nothing to trim yet: say so instead of arming a no-op.
	m.command("/trim")
	if out := ansi.Strip(m.tr.text(m.r)); !strings.Contains(out, "nothing to trim") {
		t.Fatalf("want an honest nothing-to-compact line, got %q", out)
	}
	m.command("/trim 1024x")
	if out := ansi.Strip(m.tr.text(m.r)); !strings.Contains(out, "usage: /trim") {
		t.Errorf("bad argument should print usage, got %q", out)
	}

	history := []agent.Message{agent.NewUserText("look around")}
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("c%d", i)
		a := agent.NewAssistantMessage("fake")
		a.Content = []agent.Content{&agent.ToolCall{ID: id, Name: "bash", Arguments: map[string]any{"command": "make"}}}
		a.StopReason = agent.StopToolUse
		history = append(history, a, &agent.ToolResultMessage{
			ToolCallID: id, ToolName: "bash",
			Content: []agent.Content{&agent.TextContent{Text: strings.Repeat("out ", 20_000)}},
			Details: map[string]any{"command": "make", "exit_code": 0},
		})
	}
	m.s.Harness.ReplaceMessages(history)
	m.command("/trim")
	out := ansi.Strip(m.tr.text(m.r))
	if !strings.Contains(out, "trimming tool output before the next request") {
		t.Fatalf("want a pending-trim line, got %q", out)
	}
	if err := m.s.Prompt(context.Background(), "go on"); err != nil {
		t.Fatal(err)
	}
	m.Update(nil)
	if out := ansi.Strip(m.tr.text(m.r)); !strings.Contains(out, "trimmed") {
		t.Errorf("forced pass should have trimmed: %q", out)
	}
}

func TestRenderGauge(t *testing.T) {
	cases := []struct {
		full  float64
		cells string
	}{
		{0, "░░░░░░░░"},
		{0.5, "▓▓▓▓░░░░"},
		{1, "▓▓▓▓▓▓▓▓"},
		{2, "▓▓▓▓▓▓▓▓"}, // clamped: a full window is still a full bar
		{-1, "░░░░░░░░"},
		{0.06, "░░░░░░░░"}, // 0.48 of a cell, rounds down
		{0.07, "▓░░░░░░░"}, // 0.56 of a cell, rounds up
	}
	for _, c := range cases {
		got := renderGauge(c.full)
		if got != c.cells {
			t.Errorf("renderGauge(%v) = %q, want %q", c.full, got, c.cells)
		}
		if n := len([]rune(got)); n != gaugeWidth {
			t.Errorf("renderGauge(%v) is %d cells, want %d", c.full, n, gaugeWidth)
		}
	}
}

// The gauge tells you how close the last request came to the window, which is
// what /compact is for. Before anything has been sent it shows a local estimate,
// marked so it cannot be mistaken for one the provider reported.
func TestStatusBarShowsContextGauge(t *testing.T) {
	m := newTestModel(t)
	if st := ansi.Strip(m.statusLine()); !strings.Contains(st, "ctx ≈") {
		t.Fatalf("a resumed session should be sized before the first request: %q", st)
	}

	m.context, m.contextEstimated = 64_000, false // half of the default 128k window
	st := ansi.Strip(m.statusLine())
	if !strings.Contains(st, "ctx 64.0k") {
		t.Fatalf("status bar lost the ctx figure: %q", st)
	}
	if !strings.Contains(st, "▓▓▓▓░░░░ 50%") {
		t.Fatalf("status bar lost the gauge: %q", st)
	}

	// A reported request supersedes the estimate, and the sign goes away.
	if strings.Contains(st, "≈") {
		t.Errorf("a measured context should not be marked as an estimate: %q", st)
	}

	// Over the window still reads as full rather than overflowing the bar.
	m.context = 200_000
	if !strings.Contains(ansi.Strip(m.statusLine()), "▓▓▓▓▓▓▓▓ 100%") {
		t.Errorf("an over-full request should read 100%%: %q", ansi.Strip(m.statusLine()))
	}

	// It must not push the bar out of shape: the line still fits the terminal.
	for _, w := range []int{40, 80, 120} {
		m.width = w
		if got := ansi.StringWidth(ansi.Strip(m.statusLine())); got != w {
			t.Errorf("status bar is %d cells at width %d", got, w)
		}
	}
}

func TestBridgeNextUnblocksOnClose(t *testing.T) {
	m := newTestModel(t)
	cmd := m.bridge.next()
	m.bridge.close()
	if _, ok := cmd().(bridgeClosedMsg); !ok {
		t.Fatal("next() must return after close")
	}
}

// A prompt larger than the configured window means the configuration is wrong,
// not that the model is full, and the bar says so rather than sitting at a
// clamped 100% with no explanation.
func TestContextGaugeMarksAnOverWindowPrompt(t *testing.T) {
	m := newTestModel(t)
	window := m.s.ContextWindow()

	m.context = int64(window - 1)
	if st := ansi.Strip(m.statusLine()); !strings.Contains(st, "100%") || strings.Contains(st, "100%!") {
		t.Fatalf("just inside the window: %q", st)
	}
	m.context = int64(window) * 2
	if st := ansi.Strip(m.statusLine()); !strings.Contains(st, "▓▓▓▓▓▓▓▓ 100%!") {
		t.Fatalf("over the window should be marked: %q", st)
	}
}

// A summarisation reports the step it is on, and afterwards the transcript
// carries the summary itself: seeing what the model now believes is the point
// of compacting by hand.
func TestCompactShowsPhasesAndTheSummary(t *testing.T) {
	m := newTestModel(t)

	m.Update(sessionEventMsg{coding.CompactionStartEvent{Reason: coding.CompactionManual}})
	m.Update(sessionEventMsg{coding.CompactionProgressEvent{Reason: coding.CompactionManual, Phase: "summarizing"}})
	if live := ansi.Strip(m.live()); !strings.Contains(live, "compact · summarizing") {
		t.Errorf("live area should name the step: %q", live)
	}
	// No bar is ever drawn for a compaction: a summary has no length to be a
	// fraction of, so a bar would be a guess with a denominator attached.
	if strings.Contains(ansi.Strip(m.live()), "░") {
		t.Errorf("compaction should not draw a bar: %q", ansi.Strip(m.live()))
	}
	// A step carrying a count reports it; a bare step says it is still to come.
	m.Update(sessionEventMsg{coding.CompactionProgressEvent{Reason: coding.CompactionManual, Phase: "summarizing 3.1 kB"}})
	if live := ansi.Strip(m.live()); !strings.Contains(live, "summarizing 3.1 kB") {
		t.Errorf("live area should carry the count: %q", live)
	}

	m.Update(sessionEventMsg{coding.CompactionEndEvent{Reason: coding.CompactionManual, Result: &coding.SummarizeResult{
		Summary:      "## Goal\nFix the parser.",
		Replaced:     180,
		Kept:         20,
		TokensBefore: 255_200,
		Usage:        agent.Usage{TotalTokens: 1500},
		Warnings:     []string{"only 1 of 6 expected sections present"},
	}}})
	out := ansi.Strip(m.tr.text(m.r))
	for _, want := range []string{
		"compacted 180 messages into a summary · kept 20",
		"Fix the parser.",
		"only 1 of 6 expected sections present",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("transcript missing %q:\n%s", want, out)
		}
	}
	if m.phase != "" {
		t.Error("the end of a compaction should leave the live area idle")
	}
}

func TestWordmarkAppearsOnlyForNewSessions(t *testing.T) {
	s := malachiTestSession(t)
	wide := ansi.Strip(newRenderer(80, true, "emoji", "").banner(s, 0))
	if !strings.Contains(wide, "/\\/\\") {
		t.Errorf("a new session should draw the wordmark:\n%s", wide)
	}
	// A resumed session has history worth reading; art on top of it is noise.
	resumed := ansi.Strip(newRenderer(80, true, "emoji", "").banner(s, 3))
	if strings.Contains(resumed, "/\\/\\") {
		t.Errorf("a resumed session should not draw the wordmark:\n%s", resumed)
	}
	// Too narrow to hold it whole means not at all, rather than half a word.
	for _, w := range []int{7, 20, wordmarkWidth} {
		got := ansi.Strip(newRenderer(w, true, "emoji", "").banner(s, 0))
		if strings.Contains(got, "/\\/\\") {
			t.Errorf("width=%d cannot hold the wordmark (%d cells) but drew it", w, wordmarkWidth)
		}
	}
	if got := ansi.Strip(newRenderer(wordmarkWidth+2, true, "emoji", "").banner(s, 0)); !strings.Contains(got, "/\\/\\") {
		t.Errorf("width=%d fits the wordmark but it was omitted", wordmarkWidth+2)
	}
}

func TestIconSetsAreUniformWidth(t *testing.T) {
	// Gutters line up only because every icon in a set is the same width, and
	// emoji ones are two cells. A new icon that measures differently would
	// leave that row a column proud of the rest, which is easy to introduce
	// and hard to spot.
	want := map[string]int{"emoji": 2, "dots": 1}
	for set, icons := range iconSets {
		for kind, icon := range icons {
			// iconUser is the input prompt glyph, not a gutter icon, and is
			// deliberately one cell narrow. It has its own alignment test.
			if kind == iconUser {
				continue
			}
			if got := lipgloss.Width(icon); got != want[set] {
				t.Errorf("%s/%s = %q is %d cells, want %d", set, kind, icon, got, want[set])
			}
		}
	}
}

func TestSandboxCommandAndMarker(t *testing.T) {
	m := newTestModel(t)
	m.command("/sandbox")
	if out := m.tr.text(m.r); !strings.Contains(out, "writable:") || !strings.Contains(out, "hidden") {
		t.Fatalf("/sandbox printed:\n%s", out)
	}
	if strings.Contains(m.statusLine(), "unsandboxed") {
		t.Fatal("marker shown with the sandbox on")
	}

	s, err := coding.Open(coding.Options{
		Cwd: t.TempDir(), Home: t.TempDir(), Settings: &coding.Settings{},
		Provider: fake.New(), NoSession: true, Sandbox: "off",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	off := newModel(s, "")
	off.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	if !strings.Contains(off.statusLine(), "unsandboxed") {
		t.Fatalf("status without marker: %q", off.statusLine())
	}
	if !strings.Contains(off.tr.text(off.r), "Sandbox off") {
		t.Fatalf("no startup notice:\n%s", off.tr.text(off.r))
	}
}
