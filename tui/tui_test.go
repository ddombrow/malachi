package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
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

// printed runs a command tree and collects everything sent to scrollback.
func printed(cmd tea.Cmd) string {
	if cmd == nil {
		return ""
	}
	msg := cmd()
	switch v := msg.(type) {
	case tea.BatchMsg:
		var out []string
		for _, c := range v {
			out = append(out, printed(c))
		}
		return strings.Join(out, "\n")
	}
	return fmt.Sprintf("%+v", msg)
}

func TestStreamingTextIsLiveThenPrinted(t *testing.T) {
	m := newTestModel(t)
	partial := agent.NewAssistantMessage("m")
	partial.Content = []agent.Content{&agent.TextContent{Text: "Hello wor"}}
	m.handleEvent(&agent.MessageUpdateEvent{Message: partial, AssistantMessageEvent: &agent.TextDelta{Delta: "wor", Partial: partial}})
	if v := m.View().Content; !strings.Contains(v, "Hello wor") {
		t.Fatalf("live area missing partial text:\n%s", v)
	}

	final := partial.Clone()
	final.Content = []agent.Content{&agent.TextContent{Text: "Hello world"}}
	final.Usage = agent.Usage{Input: 10, Output: 5, TotalTokens: 15}
	out := printed(m.handleEvent(&agent.MessageEndEvent{Message: final}))
	if !strings.Contains(out, "world") {
		t.Fatalf("final text not printed: %s", out)
	}
	if strings.Contains(m.View().Content, "Hello wor") {
		t.Fatal("completed message must leave the live area")
	}
	if !strings.Contains(m.statusLine(), "↑10 ↓5") {
		t.Fatalf("status: %s", m.statusLine())
	}
}

func TestToolLifecycle(t *testing.T) {
	m := newTestModel(t)
	args := map[string]any{"command": "go test ./..."}
	m.handleEvent(&agent.ToolExecutionStartEvent{ToolCallID: "c", ToolName: "bash", Args: args})
	m.handleEvent(&agent.ToolExecutionUpdateEvent{ToolCallID: "c", ToolName: "bash", PartialResult: agent.TextResult("ok pkg/a")})
	v := m.View().Content
	if !strings.Contains(v, "$ go test ./...") || !strings.Contains(v, "ok pkg/a") {
		t.Fatalf("running tool not shown:\n%s", v)
	}
	out := printed(m.handleEvent(&agent.ToolExecutionEndEvent{ToolCallID: "c", ToolName: "bash", Result: agent.TextResult("ok pkg/a\nok pkg/b")}))
	if !strings.Contains(out, "💻") || !strings.Contains(out, "ok pkg/b") {
		t.Fatalf("result not printed: %s", out)
	}
	if strings.Contains(m.View().Content, "$ go test") || m.last == nil {
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
	if out := printed(m.command("/help")); !strings.Contains(out, "/resume") {
		t.Fatalf("help: %s", out)
	}
	if out := printed(m.command("/bogus")); !strings.Contains(out, "unknown command") {
		t.Fatalf("unknown: %s", out)
	}
	if out := printed(m.command("/thinking high")); !strings.Contains(out, "high") || m.s.ThinkingLevel() != "high" {
		t.Fatalf("thinking: %s", out)
	}
}

func TestBashStatusMarks(t *testing.T) {
	m := newTestModel(t)
	for _, tc := range []struct {
		details map[string]any
		want    []string
	}{
		{map[string]any{"exit_code": 0}, []string{"💻"}},
		{map[string]any{"exit_code": float64(0)}, []string{"💻"}}, // resumed from JSON
		{map[string]any{"exit_code": 2}, []string{"💻", "✗ exit 2"}},
		{map[string]any{"cancelled": true, "exit_code": -1}, []string{"🛑", "cancelled"}},
		{map[string]any{"timed_out": true, "exit_code": -1}, []string{"⌛", "timed out"}},
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
	for tool, icon := range map[string]string{"read": "📖", "edit": "📝", "write": "📄", "bash": "💻", "grep": "🔧"} {
		if out := m.r.toolResult(tool, map[string]any{}, agent.TextResult(""), false); !strings.Contains(out, icon) {
			t.Errorf("%s: want %s in %q", tool, icon, out)
		}
	}
	if out := m.r.toolResult("read", map[string]any{"path": "x"}, agent.TextResult("File not found"), true); !strings.Contains(out, "❌") {
		t.Errorf("error icon missing: %q", out)
	}
	a := agent.NewAssistantMessage("m")
	a.Content = []agent.Content{&agent.ThinkingContent{Thinking: "hmm"}, &agent.TextContent{Text: "Done."}}
	out := m.r.assistantMessage(a)
	if !strings.Contains(out, "💭") || !strings.Contains(out, "💬") {
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
	if !strings.HasPrefix(lines[0], "💬 First paragraph.") {
		t.Fatalf("first line: %q", lines[0])
	}
	for _, l := range lines[1:] {
		if l != "" && !strings.HasPrefix(l, "   ") {
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
	if !strings.HasPrefix(out, "💬 • one") {
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

func TestDotsIconSet(t *testing.T) {
	r := newRenderer(80, true, "dots", "")
	out := ansi.Strip(r.toolResult("bash", map[string]any{"command": "ls"}, agent.TextResult(""), false))
	if !strings.Contains(out, "⏺ $ ls") {
		t.Fatalf("dots: %q", out)
	}
	if newRenderer(80, true, "bogus", "").icons[iconReply] != "💬" {
		t.Fatal("unknown icon set should fall back to emoji")
	}
}
