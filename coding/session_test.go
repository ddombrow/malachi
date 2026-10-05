package coding

import (
	"context"
	"os"
	"path/filepath"
	"slices"
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
	files := LoadContextFiles(home, sub, true)
	var got []string
	for _, f := range files {
		got = append(got, f.Content)
	}
	if strings.Join(got, ",") != "global,root rules,a rules" {
		t.Fatalf("got %v", got)
	}
	prompt := BuildSystemPrompt(PromptOptions{Cwd: sub, Tools: CodingTools(sub), ContextFiles: files})
	for _, want := range []string{
		"- read: Read file contents",
		"<project_context>",
		"a rules",
		"Use bash for builds, tests, version control",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

// The search guidance must not contradict itself. Telling the model to use
// rg and ls in one line and grep and glob in the next leaves the choice to
// it, and it resolves that by shelling out, which is the expensive path the
// tools exist to avoid.
func TestSystemPromptHasNoConflictingSearchGuidance(t *testing.T) {
	sub := t.TempDir()
	prompt := BuildSystemPrompt(PromptOptions{Cwd: sub, Tools: CodingTools(sub)})
	if strings.Contains(prompt, "Use bash for file operations") {
		t.Error("prompt tells the model to use bash to find things")
	}
	// Each tool should be recommended once. The tool list always names every
	// tool; only the guidelines are at risk of saying the same thing twice.
	var guidelines []string
	inGuidelines := false
	for _, line := range strings.Split(prompt, "\n") {
		if line == "Guidelines:" {
			inGuidelines = true
			continue
		}
		if inGuidelines && strings.HasPrefix(line, "- ") {
			guidelines = append(guidelines, strings.TrimPrefix(line, "- "))
		}
	}
	counts := map[string]int{}
	for _, g := range guidelines {
		if rest, ok := strings.CutPrefix(g, "Use "); ok {
			tool, _, _ := strings.Cut(rest, " ")
			counts[tool]++
		}
	}
	for _, tool := range []string{"grep", "glob", "read", "bash"} {
		if counts[tool] > 1 {
			t.Errorf("guidelines recommend %s %d times:\n  %s", tool, counts[tool],
				strings.Join(guidelines, "\n  "))
		}
	}
}

func TestUntrustedProjectInstructionsAreWithheld(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	sub := filepath.Join(root, "a")
	_ = os.MkdirAll(sub, 0o755)
	write(t, home, "AGENTS.md", "global")
	write(t, sub, "AGENTS.md", "obey me")

	files := LoadContextFiles(home, sub, false)
	for _, f := range files {
		if f.Content == "obey me" {
			t.Fatal("an untrusted project's instructions reached the prompt")
		}
	}
	// The user's own instructions are not project input and stay.
	if len(files) != 1 || files[0].Content != "global" {
		t.Fatalf("want only the home file, got %v", files)
	}
}

func TestSessionIDIsStableAcrossResume(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	p := fake.New(fake.Text("a"))
	s, err := Open(testOpts(t, home, cwd, p))
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Prompt(context.Background(), "x")
	p2 := fake.New(fake.Text("b"))
	r, err := Open(Options{Cwd: cwd, Home: home, Settings: &Settings{}, Provider: p2, Continue: true})
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Prompt(context.Background(), "y")
	if id := p.Requests[0].SessionID; id == "" || id != p2.Requests[0].SessionID {
		t.Fatalf("session ids: %q vs %q", id, p2.Requests[0].SessionID)
	}

	o := testOpts(t, home, cwd, fake.New(fake.Text("c")))
	o.NoSession = true
	e, _ := Open(o)
	if e.Harness.Config().SessionID == "" {
		t.Fatal("in-memory sessions need a session id too")
	}
}

// A message whose entry failed to write must not be remembered as persisted:
// a later compaction would name it as the start of its retained tail.
func TestFailedWriteIsNotRecordedAsPersisted(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	s, err := Open(testOpts(t, home, cwd, fake.New(fake.Text("a"), fake.Text("b"))))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Prompt(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	// The file exists; now make it unwritable, so the next failure is the
	// first one and lands on a message entry, not a header.
	if err := os.Chmod(s.Path(), 0o400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(s.Path(), 0o600) })
	if err := s.Prompt(context.Background(), "second"); err != nil {
		t.Fatal(err)
	}
	if s.PersistError() == nil {
		t.Fatal("the failed write was not reported")
	}
	msgs := s.Harness.Messages()
	for _, m := range msgs[len(msgs)-2:] {
		if id := s.entryIDFor(m); id != "" {
			t.Fatalf("%s message recorded as entry %s, which never reached the disk", m.Role(), id)
		}
	}
	if s.entryIDFor(msgs[0]) == "" {
		t.Fatal("messages written before the failure should still be recorded")
	}
}

// OpenCode Go serves its GPT, Grok and Muse models only over /responses; the
// preset says so, and settings can change the list.
func TestResponsesModelsPresetAndOverride(t *testing.T) {
	pcs := (&Settings{}).ProviderConfigs()
	go_ := pcs["opencode-go"]
	for _, m := range []string{"gpt-6-luna", "grok-4.7", "muse-spark-1.3-contributor"} {
		if !slices.Contains(go_.ResponsesModels, m) {
			t.Errorf("opencode-go preset does not route %s to /responses", m)
		}
	}
	if slices.Contains(go_.ResponsesModels, "kimi-k2.7-code") {
		t.Error("chat models must stay on /chat/completions")
	}
	over := (&Settings{Providers: map[string]ProviderConfig{
		"opencode-go": {ResponsesModels: []string{"gpt-7"}},
	}}).ProviderConfigs()["opencode-go"]
	if !slices.Equal(over.ResponsesModels, []string{"gpt-7"}) || over.BaseURL == "" {
		t.Fatalf("override: %+v", over.ResponsesModels)
	}
}
