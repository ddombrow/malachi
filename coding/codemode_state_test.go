package coding

import (
	"context"
	"strings"
	"testing"

	"github.com/ddombrow/malachi/agent"
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
	want := CodeModeState{Enabled: true, Mode: CodeModeOn, Scripts: 2, Failed: 1, Calls: 2, CallErrors: 1}
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

// The code mode can change while a session runs, and each mode's tools and
// advice are what the next request carries; a run in progress refuses it.
func TestCodeModeCyclesWhileTheSessionRuns(t *testing.T) {
	release := make(chan struct{})
	p := fake.New(fake.Text("one"), fake.Text("two"), fake.Text("three"), fake.Text("four"), fake.Text("five"))
	off := false
	s, err := Open(Options{Cwd: t.TempDir(), Home: t.TempDir(), Settings: &Settings{CodeMode: &off}, Provider: p, NoSession: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	var events []string
	s.Subscribe(func(e any) {
		if c, ok := e.(CodeModeChangedEvent); ok {
			events = append(events, c.State.Mode)
		}
	})
	names := func(req agent.Request) string {
		var n []string
		for _, tl := range req.Tools {
			n = append(n, tl.Name)
		}
		return strings.Join(n, " ")
	}
	if got := s.CodeMode().Mode; got != CodeModeOff {
		t.Fatalf("starts in %q", got)
	}
	for _, want := range []struct{ mode, tools, advice string }{
		{CodeModeOn, "read grep glob bash fetch edit write jobs script", "Use script when you would otherwise"},
		{CodeModeExplore, "bash fetch edit write jobs view script", "Do not read or search files with bash"},
		{CodeModeOff, "read grep glob bash fetch edit write jobs", "call read once per file in the same response"},
	} {
		mode, err := s.CycleCodeMode()
		if err != nil || mode != want.mode {
			t.Fatalf("cycled to %q, %v; want %q", mode, err, want.mode)
		}
		if err := s.Prompt(context.Background(), "go"); err != nil {
			t.Fatal(err)
		}
		req := p.Requests[len(p.Requests)-1]
		if got := names(req); got != want.tools {
			t.Errorf("%s: tools %q, want %q", mode, got, want.tools)
		}
		if !strings.Contains(req.System, want.advice) {
			t.Errorf("%s: the prompt lacks %q", mode, want.advice)
		}
	}
	s.Flush()
	if strings.Join(events, " ") != "on explore off" {
		t.Errorf("events %v", events)
	}
	if err := s.SetCodeMode("sideways"); err == nil {
		t.Error("an unknown mode was accepted")
	}

	// A run in progress keeps its tools.
	s2 := openRunSession(t, fake.New(blockingReply(release, "done")))
	if err := s2.Submit(context.Background(), "go", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.CycleCodeMode(); err == nil {
		t.Error("the code mode changed during a run")
	}
	if s2.CodeMode().Mode != CodeModeOn {
		t.Errorf("mode during the run: %q", s2.CodeMode().Mode)
	}
	close(release)
}

// Options.CodeMode opens a session in a mode whatever the settings say,
// and the mode in effect carries over to the session that replaces it, as
// a frontend's /new or switch would otherwise lose it.
func TestCodeModeOptionAndCarryOver(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	t.Setenv("OPENCODE_API_KEY", "test") // Reopen builds a real provider; it is never called
	s, err := Open(Options{Cwd: cwd, Home: home, Settings: &Settings{}, Provider: fake.New(), NoSession: true, CodeMode: CodeModeExplore})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.CodeMode().Mode; got != CodeModeExplore {
		t.Fatalf("opened in %q", got)
	}
	next, err := s.Reopen("")
	if err != nil {
		t.Fatal(err)
	}
	if got := next.CodeMode().Mode; got != CodeModeExplore {
		t.Errorf("a new session went to %q", got)
	}
	if err := next.SetCodeMode(CodeModeOff); err != nil {
		t.Fatal(err)
	}
	last, err := next.Reopen("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(last.Close)
	if got := last.CodeMode().Mode; got != CodeModeOff {
		t.Errorf("a switch made since did not carry over: %q", got)
	}
	if _, err := Open(Options{Cwd: cwd, Home: home, Settings: &Settings{}, Provider: fake.New(), NoSession: true, CodeMode: "sideways"}); err == nil {
		t.Error("an unknown code mode was accepted")
	}
}
