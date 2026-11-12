package coding

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/codemode"
	"github.com/ddombrow/malachi/sandbox"
)

func runScriptTool(t *testing.T, tool *agent.Tool, code string) (agent.ToolResult, bool) {
	t.Helper()
	call := &agent.ToolCall{ID: "s", Name: codemode.ToolName, Arguments: map[string]any{"code": code}}
	return agent.ExecuteTool(context.Background(), tool, call, func(agent.ToolResult) {})
}

// Scripts call the real tools, under the same sandbox: a hidden file is as
// hidden from a script as from the model, and the refusal is logged.
func TestScriptToolUsesRealToolsUnderTheSandbox(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	secret := filepath.Join(home, "settings.json")
	for path, body := range map[string]string{
		secret:                       `{"apiKey": "sk-secret"}`,
		filepath.Join(cwd, "a.txt"):  "alpha\n",
		filepath.Join(cwd, "b.txt"):  "beta\n",
		filepath.Join(cwd, "go.mod"): "module x\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pol := sandbox.Build(sandbox.Config{Cwd: cwd, Home: home})
	opts := ToolOptions{Sandbox: func() sandbox.Policy { return pol }, SpillDir: t.TempDir()}
	d := NewDiagnostics(t.TempDir())
	tool := NewScriptTool(CodingTools(cwd, opts), opts, d, nil)

	res, isError := runScriptTool(t, tool, `
		const files = (await tools.glob({pattern: "*.txt"})).text.split("\n").filter(l => l.endsWith(".txt"));
		const bodies = await Promise.all(files.map(path => tools.read({path})));
		const secret = await tools.read({path: `+"`"+secret+"`"+`});
		print(secret.isError ? "refused" : secret.text);
		return bodies.map(b => b.text.trim()).join(",");`)
	if isError {
		t.Fatalf("script failed: %s", res.Text())
	}
	if res.Text() != "refused\nalpha,beta" {
		t.Errorf("result %q", res.Text())
	}
	if strings.Contains(res.Text(), "sk-secret") {
		t.Fatal("the hidden file reached the script")
	}

	var kinds []string
	var denial, script map[string]any
	for _, r := range readLog(t, d.Path()) {
		kinds = append(kinds, r["kind"].(string))
		switch r["kind"] {
		case "sandbox_denied":
			denial = r
		case "script":
			script = r
		}
	}
	if denial == nil || denial["tool"] != "script:read" || denial["target"] != secret {
		t.Errorf("denial %v (log %v)", denial, kinds)
	}
	if script == nil || script["calls"] != 4.0 || script["callErrors"] != 1.0 || script["batches"] != 3.0 {
		t.Errorf("script record %v", script)
	}
}

// Output past the display limits is cut, and the whole of it spilled.
func TestScriptToolSpillsLongOutput(t *testing.T) {
	spill := t.TempDir()
	tool := NewScriptTool(nil, ToolOptions{SpillDir: spill}, nil, nil)
	res, isError := runScriptTool(t, tool, `for (let i = 1; i <= 5000; i++) print("line " + i)`)
	if isError {
		t.Fatal(res.Text())
	}
	text := res.Text()
	if !strings.HasPrefix(text, "line 1\nline 2\n") || !strings.Contains(text, "[Showing lines 1-2000 of 5000. Full output: ") {
		t.Fatalf("text ends %q", text[max(0, len(text)-200):])
	}
	path := text[strings.Index(text, "Full output: ")+len("Full output: ") : len(text)-1]
	full, err := os.ReadFile(path)
	if err != nil || !strings.HasSuffix(string(full), "line 5000") || filepath.Dir(path) != spill {
		t.Fatalf("spill %s: %v", path, err)
	}

	// A failing script's output is cut the same way, and it still fails.
	res, isError = runScriptTool(t, tool, `for (let i = 1; i <= 5000; i++) print("line " + i); throw new Error("late")`)
	if !isError || !strings.Contains(res.Text(), "[Showing lines 1-2000 of 500") ||
		!strings.Contains(res.Text(), "]\n\nScript failed: Error: late") {
		t.Fatalf("%v %q", isError, res.Text()[max(0, len(res.Text())-200):])
	}
	if res.Details.(codemode.Details).Error == "" {
		t.Error("failure lost its details")
	}
}

func TestCodeModeSetting(t *testing.T) {
	var s Settings
	if !s.CodeModeEnabled() {
		t.Error("code mode should default to on")
	}
	off := false
	s.CodeMode = &off
	if s.CodeModeEnabled() {
		t.Error("codeMode: false should turn it off")
	}
}

// Searches made by a script return data as well as text, with paths a
// script can pass straight to read; the model's own calls do not carry it.
func TestScriptSearchResultsAreData(t *testing.T) {
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{
		"pkg/a_test.go": "package pkg\nfunc TestA(t *testing.T) {}\nfunc TestB(t *testing.T) {}\n",
		"pkg/b_test.go": "package pkg\nfunc TestC(t *testing.T) {}\n",
		"main.go":       "package main\n",
	} {
		if err := os.WriteFile(filepath.Join(cwd, path), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tools := CodingTools(cwd, ToolOptions{})
	tool := NewScriptTool(tools, ToolOptions{}, nil, nil)
	res, isError := runScriptTool(t, tool, `
		const [lines, files, counts, glob] = await Promise.all([
			tools.grep({path: "pkg", pattern: "^func Test"}),
			tools.grep({path: "pkg", pattern: "^func Test", output: "files"}),
			tools.grep({path: "pkg", pattern: "^func Test", output: "count"}),
			tools.glob({path: "pkg", pattern: "*.go"}),
		]);
		const first = await tools.read({path: lines.details.results[0].path});
		return {
			lines: lines.details.results.map(r => r.path + ":" + r.line + ":" + r.text),
			files: files.details.results.map(r => r.path),
			counts: counts.details.results.map(r => r.path + "=" + r.count),
			glob: glob.details.paths,
			readable: !first.isError,
		};`)
	if isError {
		t.Fatal(res.Text())
	}
	want := `{
  "lines": [
    "pkg/a_test.go:2:func TestA(t *testing.T) {}",
    "pkg/a_test.go:3:func TestB(t *testing.T) {}",
    "pkg/b_test.go:2:func TestC(t *testing.T) {}"
  ],
  "files": [
    "pkg/a_test.go",
    "pkg/b_test.go"
  ],
  "counts": [
    "pkg/a_test.go=2",
    "pkg/b_test.go=1"
  ],
  "glob": [
    "pkg/a_test.go",
    "pkg/b_test.go"
  ],
  "readable": true
}`
	if res.Text() != want {
		t.Errorf("got\n%s\nwant\n%s", res.Text(), want)
	}

	// Called directly, as the model calls it, grep keeps its details small.
	for _, tl := range tools {
		if tl.Name != "grep" {
			continue
		}
		r, err := tl.Execute(context.Background(), "g", map[string]any{"pattern": "Test"}, func(agent.ToolResult) {})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := r.Details.(map[string]any)["results"]; ok {
			t.Error("a direct grep carried script results")
		}
	}
}
