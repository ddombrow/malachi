package tui

import (
	"encoding/json"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/codemode"
)

func scriptRun() codemode.Details {
	return codemode.Details{Batches: 2, Calls: []codemode.CallRecord{
		{ID: 1, Name: "read", Args: map[string]any{"path": "go.mod"}, Status: codemode.StatusDone},
		{ID: 2, Name: "grep", Args: map[string]any{"pattern": "TODO"}, Status: codemode.StatusError},
		{ID: 3, Name: "read", Args: map[string]any{"path": "main.go"}, Status: codemode.StatusRunning},
	}}
}

func TestScriptTally(t *testing.T) {
	if got := scriptTally(scriptRun()); got != "3 calls: read ×2, grep · 1 failed" {
		t.Errorf("got %q", got)
	}
}

// Details read back from a session file are a map; they render the same.
func TestScriptDetailsFromDisk(t *testing.T) {
	b, _ := json.Marshal(scriptRun())
	var m map[string]any
	json.Unmarshal(b, &m)
	d, ok := scriptDetails(m)
	if !ok || len(d.Calls) != 3 || d.Calls[1].Status != codemode.StatusError {
		t.Fatalf("%v %+v", ok, d)
	}
	if _, ok := scriptDetails(map[string]any{"exit_code": 1}); ok {
		t.Error("bash details read as a script's")
	}
}

// A running script lists its calls as they go; a finished one shows a
// tally and its output.
func TestScriptProgressAndResult(t *testing.T) {
	m := newTestModel(t)
	args := map[string]any{"code": "// look around\nconst r = await tools.read({path: \"go.mod\"});\nreturn r.text"}
	m.Update(sessionEventMsg{&agent.ToolExecutionStartEvent{ToolCallID: "s", ToolName: "script", Args: args}})
	m.Update(sessionEventMsg{&agent.ToolExecutionUpdateEvent{ToolCallID: "s", ToolName: "script", Args: args,
		PartialResult: agent.ToolResult{Content: []agent.Content{&agent.TextContent{Text: "3 calls, 1 running"}}, Details: scriptRun()}}})
	live := ansi.Strip(m.live())
	for _, want := range []string{"script const r = await tools.read({path: \"go.mod\"}); …", "✓ read go.mod", "✗ grep TODO", "read main.go"} {
		if !strings.Contains(live, want) {
			t.Errorf("live view lacks %q:\n%s", want, live)
		}
	}

	done := scriptRun()
	done.Calls[2].Status = codemode.StatusDone
	res := agent.ToolResult{Content: []agent.Content{&agent.TextContent{Text: "module x"}}, Details: done}
	out := shown(m, func() tea.Cmd {
		return m.handleEvent(&agent.ToolExecutionEndEvent{ToolCallID: "s", ToolName: "script", Result: res})
	})
	if !strings.Contains(out, "3 calls: read ×2, grep · 1 failed") || !strings.Contains(out, "module x") {
		t.Errorf("result:\n%s", out)
	}
	last := shown(m, func() tea.Cmd { return m.command("/last") })
	for _, want := range []string{"// look around", "✓ read main.go", "✗ grep TODO", "module x"} {
		if !strings.Contains(last, want) {
			t.Errorf("/last lacks %q:\n%s", want, last)
		}
	}
}

func TestSidebarShowsCodeMode(t *testing.T) {
	m := newTestModel(t)
	if text := sidebarText(m); !strings.Contains(text, "code mode") || !strings.Contains(text, "no scripts yet") {
		t.Fatalf("sidebar:\n%s", text)
	}
}
