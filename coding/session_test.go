package coding

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/agent/session"
	"github.com/ddombrow/malachi/ai/fake"
)

func testOpts(t *testing.T, home, cwd string, p agent.Provider) Options {
	t.Helper()
	return Options{Cwd: cwd, Home: home, Settings: &Settings{}, Provider: p}
}

func TestSessionPersistsAndResumes(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	write(t, cwd, "hello.txt", "hi from file")
	p := fake.New(
		fake.ToolCalls("Reading.", fake.Call{ID: "c1", Name: "read", Args: map[string]any{"path": "hello.txt"}}),
		fake.Text("It says hi."),
	)
	s, err := Open(testOpts(t, home, cwd, p))
	if err != nil {
		t.Fatal(err)
	}
	if s.file.Exists() {
		t.Fatal("session file must be created lazily")
	}
	if err := s.Prompt(context.Background(), "what's in hello.txt?"); err != nil {
		t.Fatal(err)
	}
	if err := s.PersistError(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Requests[1].Messages[2].(*agent.ToolResultMessage).Text(), "hi from file") {
		t.Fatal("tool did not run against cwd")
	}
	if !strings.Contains(p.Requests[0].System, "Current working directory: "+s.Cwd()) {
		t.Fatal("system prompt missing cwd")
	}

	f, err := session.Load(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, e := range f.Entries() {
		types = append(types, e.Type)
	}
	want := "session_info model_change thinking_level_change message message message message"
	if strings.Join(types, " ") != want {
		t.Fatalf("entries: %v", types)
	}

	// --continue picks the same file and replays the transcript and model.
	p2 := fake.New(fake.Text("again"))
	r, err := Open(Options{Cwd: cwd, Home: home, Settings: &Settings{}, Provider: p2, Continue: true})
	if err != nil {
		t.Fatal(err)
	}
	if r.Path() != s.Path() || len(r.Harness.Messages()) != 4 || r.Model() != "kimi-k2.7-code" {
		t.Fatalf("resume: path=%s msgs=%d model=%s", r.Path(), len(r.Harness.Messages()), r.Model())
	}
	_ = r.Prompt(context.Background(), "and now?")
	if len(p2.Requests[0].Messages) != 5 {
		t.Fatalf("resumed request should include history, got %d", len(p2.Requests[0].Messages))
	}
	f, _ = session.Load(s.Path())
	if n := len(f.Entries()); n != 9 {
		t.Fatalf("resume must not rewrite headers; entries=%d", n)
	}
}

func TestSessionModelSwitchIsRecorded(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	s, err := Open(testOpts(t, home, cwd, fake.New(fake.Text("a"))))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENCODE_API_KEY", "test")
	if err := s.SetModel("glm-5.2"); err != nil {
		t.Fatal(err)
	}
	if s.Provider().Name != "opencode-go" || s.Model() != "glm-5.2" {
		t.Fatalf("got %s/%s", s.Provider().Name, s.Model())
	}
	if err := s.SetThinkingLevel("bogus"); err == nil {
		t.Fatal("invalid level accepted")
	}
}

func TestNoSessionWritesNothing(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	o := testOpts(t, home, cwd, fake.New(fake.Text("a")))
	o.NoSession = true
	s, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Prompt(context.Background(), "x")
	if _, err := os.Stat(filepath.Join(home, "sessions")); !os.IsNotExist(err) {
		t.Fatal("no-session mode wrote to disk")
	}
}

func TestResolveModel(t *testing.T) {
	s := &Settings{}
	cases := map[string][2]string{
		"":                   {"opencode-go", "kimi-k2.7-code"},
		"glm-5.2":            {"opencode-go", "glm-5.2"},
		"openai/gpt-5.1":     {"openai", "gpt-5.1"},
		"openrouter/":        {"openrouter", ""},
		"anthropic/claude-x": {"opencode-go", "anthropic/claude-x"},
	}
	for ref, want := range cases {
		pc, model, err := s.ResolveModel(ref)
		if ref == "openrouter/" {
			if err == nil {
				t.Errorf("%q: want error for provider without default model", ref)
			}
			continue
		}
		if err != nil || pc.Name != want[0] || model != want[1] {
			t.Errorf("%q: got %s/%s (%v), want %s/%s", ref, pc.Name, model, err, want[0], want[1])
		}
	}
}

func TestContextFiles(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	sub := filepath.Join(root, "a", "b")
	_ = os.MkdirAll(sub, 0o755)
	write(t, home, "AGENTS.md", "global")
	write(t, root, "AGENTS.md", "root rules")
	write(t, filepath.Join(root, "a"), "CLAUDE.md", "a rules")
	files := LoadContextFiles(home, sub)
	var got []string
	for _, f := range files {
		got = append(got, f.Content)
	}
	if strings.Join(got, ",") != "global,root rules,a rules" {
		t.Fatalf("got %v", got)
	}
	prompt := BuildSystemPrompt(PromptOptions{Cwd: sub, Tools: CodingTools(sub), ContextFiles: files})
	for _, want := range []string{"- read: Read file contents", "<project_context>", "a rules", "Use bash for file operations"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}
