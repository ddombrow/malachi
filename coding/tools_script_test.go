package coding

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/ai/fake"
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

// A script gets far more of a command's output than the model is shown, so
// it can filter a test run's log itself: whole up to 1 MiB, then the start
// and end around a marker. A direct call carries none of it.
func TestScriptsGetACommandsWholeOutput(t *testing.T) {
	opts := ToolOptions{SpillDir: t.TempDir()}
	bash := NewBashTool(t.TempDir(), opts)
	tool := NewScriptTool([]*agent.Tool{bash}, opts, nil, nil)
	res, isError := runScriptTool(t, tool, `
		const show = (r) => {
			const o = r.details.output;
			return {cut: r.details.output_truncated, len: o.length, first: o.slice(0, 2), last: o.trimEnd().split("\n").pop(),
				textLen: r.text.length, marker: o.includes("of output omitted")};
		};
		return [
			show(await tools.bash({command: "echo hi"})),
			show(await tools.bash({command: "seq 1 20000"})),
			show(await tools.bash({command: "seq 1 300000"})),
		];`)
	if isError {
		t.Fatalf("script failed: %s", res.Text())
	}
	var got []struct {
		Cut     bool
		Len     int
		First   string
		Last    string
		TextLen int
		Marker  bool
	}
	if err := json.Unmarshal([]byte(res.Text()), &got); err != nil {
		t.Fatalf("%v: %s", err, res.Text())
	}
	small, whole, long := got[0], got[1], got[2]
	if small.Cut || small.Last != "hi" {
		t.Errorf("small output: %+v", small)
	}
	// 108 kB: cut for the model, whole for the script.
	if whole.Cut || whole.Marker || whole.Last != "20000" || whole.First != "1\n" || whole.Len <= whole.TextLen {
		t.Errorf("output under 1 MiB must reach the script whole: %+v", whole)
	}
	// 2 MB: the start and the end, the middle left out and said so.
	if !long.Cut || !long.Marker || long.First != "1\n" || long.Last != "300000" || long.Len > MaxScriptOutputBytes+200 || long.Len < MaxScriptOutputBytes-200 {
		t.Errorf("output over 1 MiB must keep its start and end: %+v", long)
	}

	r, err := run(t, bash, map[string]any{"command": "echo hi"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Details.(map[string]any)["output"]; ok {
		t.Error("a direct call carried the script's copy of the output")
	}
}

// Code mode decides what the provider is sent. On, the script tool is
// offered, every other tool says how a script calls it, and the prompt's
// advice allows for scripts; off, none of that is sent, and read's advice
// is to read files in parallel. Scripts see the tools as they are either
// way, without the note about calling them from a script.
func TestCodeModeChangesWhatIsSent(t *testing.T) {
	sent := func(on bool) agent.Request {
		t.Helper()
		p := fake.New(fake.Text("ok"))
		s, err := Open(Options{Cwd: t.TempDir(), Home: t.TempDir(), Settings: &Settings{CodeMode: &on}, Provider: p, NoSession: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(s.Close)
		if err := s.Prompt(context.Background(), "hi"); err != nil {
			t.Fatal(err)
		}
		if len(p.Requests) != 1 {
			t.Fatalf("%d requests", len(p.Requests))
		}
		return p.Requests[0]
	}
	tool := func(req agent.Request, name string) *agent.Tool {
		for _, tl := range req.Tools {
			if tl.Name == name {
				return tl
			}
		}
		return nil
	}
	const note = "In a script: `await tools."

	off := sent(false)
	if tool(off, codemode.ToolName) != nil {
		t.Error("code mode off: the script tool was sent")
	}
	for _, tl := range off.Tools {
		if strings.Contains(tl.Description, note) {
			t.Errorf("code mode off: %s says how a script calls it", tl.Name)
		}
	}
	if !strings.Contains(off.System, readSeveral) || strings.Contains(off.System, readSeveralWithScripts) ||
		strings.Contains(off.System, "Use script") || strings.Contains(off.System, "scripts") || strings.Contains(off.System, "\n- script:") {
		t.Errorf("code mode off: the prompt mentions scripts or lacks read's advice:\n%s", off.System)
	}

	on := sent(true)
	script := tool(on, codemode.ToolName)
	if script == nil {
		t.Fatal("code mode on: no script tool was sent")
	}
	for _, tl := range on.Tools {
		if tl.Name == codemode.ToolName {
			continue
		}
		if !strings.Contains(tl.Description, note+tl.Name+"({...})`") {
			t.Errorf("code mode on: %s does not say how a script calls it:\n%s", tl.Name, tl.Description)
		}
	}
	if !strings.Contains(tool(on, "edit").Description, "details {edits, first_changed_line, patch, path}") {
		t.Errorf("edit's note lacks its details fields:\n%s", tool(on, "edit").Description)
	}
	if strings.Contains(on.System, readSeveral) || !strings.Contains(on.System, readSeveralWithScripts) ||
		!strings.Contains(on.System, "Use script when you would otherwise take in more than you need") {
		t.Errorf("code mode on: the prompt's advice does not allow for scripts:\n%s", on.System)
	}
	// The fields are said once, on each tool, not again in the script tool.
	if strings.Contains(script.Description, "Each result's details, by tool") {
		t.Errorf("the script tool repeats every tool's details:\n%s", script.Description)
	}

	res, isError := runScriptTool(t, script, `return [describeTool("read").description.includes("In a script"), ALL_TOOLS.length > 0]`)
	if isError || res.Text() != "[\n  false,\n  true\n]" {
		t.Errorf("scripts must see the tools without the note: %s", res.Text())
	}
}

// Exploring in scripts takes read, grep and glob out of what the model is
// sent: it learns of them, arguments and all, only from the script tool,
// and is told not to explore with bash instead. Scripts still call them,
// and view shows it images, which scripts cannot.
func TestExploringInScripts(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "a.txt"), []byte("alpha\nbeta\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var img bytes.Buffer
	if err := png.Encode(&img, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "dot.png"), img.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	p := fake.New(fake.Text("ok"))
	s, err := Open(Options{Cwd: cwd, Home: t.TempDir(), Settings: &Settings{ExploreInScripts: true}, Provider: p, NoSession: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if err := s.Prompt(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	req := p.Requests[0]
	sent := map[string]*agent.Tool{}
	for _, tl := range req.Tools {
		sent[tl.Name] = tl
	}
	for _, name := range []string{"read", "grep", "glob"} {
		if sent[name] != nil {
			t.Errorf("%s was offered directly", name)
		}
	}
	for _, name := range []string{"bash", "edit", "write", "view", codemode.ToolName} {
		if sent[name] == nil {
			t.Errorf("%s was not offered", name)
		}
	}
	script := sent[codemode.ToolName]
	for _, want := range []string{
		"### tools.read(args)", "### tools.grep(args)", "### tools.glob(args)",
		"literal (boolean, optional): Match pattern as plain text",
		"output (string, optional) one of lines, files, count",
		"Details: {files, hit_limit, matches",
		"Use grep's context option",
		"to look at an image, call view",
	} {
		if !strings.Contains(script.Description, want) {
			t.Errorf("the script tool's description lacks %q:\n%s", want, script.Description)
		}
	}
	if strings.Contains(script.Description, "### tools.bash") || strings.Contains(script.Description, readSeveral) {
		t.Errorf("the script tool describes a direct tool, or advises parallel reads:\n%s", script.Description)
	}
	if !strings.Contains(req.System, "Do not read or search files with bash") || strings.Contains(req.System, "call read once per file") {
		t.Errorf("the prompt's advice does not fit exploring in scripts:\n%s", req.System)
	}

	res, isError := runScriptTool(t, script, `
		const [a, hits] = await Promise.all([tools.read({path: "a.txt"}), tools.grep({pattern: "beta"})]);
		return [a.text.trim().split("\n")[0], hits.details.results[0].line];`)
	if isError || res.Text() != "[\n  \"alpha\",\n  2\n]" {
		t.Errorf("scripts must still read and search: %s", res.Text())
	}
	view := sent["view"]
	shown, err := view.Execute(context.Background(), "v", map[string]any{"path": "dot.png"}, nil)
	if err != nil || !slices.ContainsFunc(shown.Content, func(c agent.Content) bool { _, ok := c.(*agent.ImageContent); return ok }) {
		t.Errorf("view must show an image: %v %v", shown, err)
	}
	if _, err := view.Execute(context.Background(), "v", map[string]any{"path": "a.txt"}, nil); err == nil || !strings.Contains(err.Error(), "in a script") {
		t.Errorf("view must send a text file to a script: %v", err)
	}
}
