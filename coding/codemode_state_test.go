package coding

import (
	"context"
	"testing"

	"github.com/ddombrow/malachi/ai/fake"
	"github.com/ddombrow/malachi/codemode"
)

// The session counts what scripts did: scripts, failures, calls and failed
// calls.
func TestSessionCountsScripts(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	write(t, cwd, "a.txt", "alpha")
	p := fake.New(
		fake.ToolCalls("", fake.Call{ID: "s1", Name: codemode.ToolName, Args: map[string]any{
			"code": `await Promise.all([tools.read({path: "a.txt"}), tools.read({path: "missing.txt"})])`}}),
		fake.ToolCalls("", fake.Call{ID: "s2", Name: codemode.ToolName, Args: map[string]any{
			"code": `throw new Error("boom")`}}),
		fake.Text("done"),
	)
	opts := testOpts(t, home, cwd, p)
	opts.NoSession = true
	s, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if got := s.CodeMode(); !got.Enabled || got.Scripts != 0 {
		t.Fatalf("before: %+v", got)
	}
	if err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	want := CodeModeState{Enabled: true, Scripts: 2, Failed: 1, Calls: 2, CallErrors: 1}
	if got := s.CodeMode(); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestCodeModeOffLeavesTheToolOut(t *testing.T) {
	off := false
	opts := testOpts(t, t.TempDir(), t.TempDir(), fake.New())
	opts.NoSession = true
	opts.Settings = &Settings{CodeMode: &off}
	s, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if s.CodeMode().Enabled {
		t.Error("code mode reported on")
	}
	for _, tool := range s.Harness.Config().Tools {
		if tool.Name == codemode.ToolName {
			t.Error("script tool offered with codeMode: false")
		}
	}
}
