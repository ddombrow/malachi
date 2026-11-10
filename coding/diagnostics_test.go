package coding

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/ai/fake"
	"github.com/ddombrow/malachi/sandbox"
)

// readLog returns the parsed records from a diagnostics log.
func readLog(t *testing.T, path string) []map[string]any {
	t.Helper()
	fh, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	var out []map[string]any
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("log line is not JSON: %v (%q)", err, sc.Text())
		}
		out = append(out, rec)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDiagnosticsRecordsFailures(t *testing.T) {
	home := t.TempDir()
	d := NewDiagnostics(home)
	d.Bind("/tmp/s.jsonl", "openai", "gpt-5.1")
	d.NewRun()

	d.LogPanic("tui.Run", "boom", []byte("goroutine 1 [running]:\nmain.main()"))
	d.LogPersistError(os.ErrPermission)
	m := agent.NewAssistantMessage("gpt-5.1")
	m.StopReason = agent.StopError
	m.ErrorMessage = "500 from provider"
	m.ResponseID = "resp_1"
	m.Diagnostics = []agent.Diagnostic{{
		Type: "http_error",
		Error: &agent.DiagnosticError{
			Name: "HTTPError", Message: "upstream exploded", Code: 500,
		},
	}}
	d.LogAssistantError(m)

	recs := readLog(t, d.Path())
	if len(recs) != 3 {
		t.Fatalf("want 3 records, got %d: %v", len(recs), recs)
	}
	kinds := []string{"panic", "persist_error", "assistant_error"}
	for i, want := range kinds {
		if recs[i]["kind"] != want {
			t.Errorf("record %d: kind = %v, want %s", i, recs[i]["kind"], want)
		}
		if recs[i]["session"] != "/tmp/s.jsonl" || recs[i]["provider"] != "openai" {
			t.Errorf("record %d: missing session context: %v", i, recs[i])
		}
		if recs[i]["runId"] == nil || recs[i]["timestamp"] == nil {
			t.Errorf("record %d: missing run or timestamp: %v", i, recs[i])
		}
	}
	if !strings.Contains(recs[0]["stack"].(string), "goroutine 1") {
		t.Errorf("panic record lost its stack: %v", recs[0])
	}
	if recs[1]["error"] != os.ErrPermission.Error() {
		t.Errorf("persist error: %v", recs[1])
	}
	if recs[2]["errorMessage"] != "500 from provider" || recs[2]["responseId"] != "resp_1" {
		t.Errorf("assistant error fields: %v", recs[2])
	}
	// The message's own diagnostics carry the HTTP status the transcript
	// never shows.
	diags, _ := recs[2]["diagnostics"].([]any)
	if len(diags) != 1 {
		t.Fatalf("diagnostics not recorded: %v", recs[2])
	}
	inner, _ := diags[0].(map[string]any)
	errObj, _ := inner["error"].(map[string]any)
	if errObj["code"] != float64(500) || errObj["name"] != "HTTPError" {
		t.Errorf("http detail lost: %v", diags[0])
	}
}

// Only failures are recorded: a normal assistant message is not an event
// worth a log line.
func TestDiagnosticsSkipsSuccessfulMessages(t *testing.T) {
	d := NewDiagnostics(t.TempDir())
	m := agent.NewAssistantMessage("m")
	m.Content = []agent.Content{&agent.TextContent{Text: "done"}}
	d.LogAssistantError(m)
	if _, err := os.Stat(d.Path()); !os.IsNotExist(err) {
		t.Fatalf("a successful turn wrote %s", d.Path())
	}
}

func TestDiagnosticsTruncatesAndSurvivesBadWrites(t *testing.T) {
	d := NewDiagnostics(t.TempDir())
	long := strings.Repeat("x", 50000)
	d.LogPanic("t", long, []byte(long))
	recs := readLog(t, d.Path())
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	if v := recs[0]["stack"].(string); !strings.Contains(v, "bytes total") || len(v) > 17000 {
		t.Errorf("stack not truncated: %d bytes", len(v))
	}
	// A home that cannot hold the log must not take the agent down.
	root := t.TempDir()
	blocker := filepath.Join(root, "home")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	blocked := NewDiagnostics(blocker)
	blocked.LogPanic("t", "x", nil) // must not panic
	if _, err := os.Stat(blocked.Path()); err == nil {
		t.Error("log file exists despite an unusable home")
	}
}

// A run id groups everything from one prompt until the next.
func TestDiagnosticsRunsAreDistinct(t *testing.T) {
	d := NewDiagnostics(t.TempDir())
	first, second := d.NewRun(), d.NewRun()
	if first == second {
		t.Fatalf("run ids repeated: %q", first)
	}
	d.LogPersistError(os.ErrClosed)
	recs := readLog(t, d.Path())
	if recs[0]["runId"] != second {
		t.Errorf("record tagged %v, want the latest run %q", recs[0]["runId"], second)
	}
}

// After /model, failures are attributed to the model now in use.
func TestDiagnosticsFollowModelSwitch(t *testing.T) {
	settings := &Settings{DefaultProvider: "a", Providers: map[string]ProviderConfig{
		"a": {BaseURL: "http://127.0.0.1:1", DefaultModel: "model-a"},
		"b": {BaseURL: "http://127.0.0.1:1", DefaultModel: "model-b"},
	}}
	s, err := Open(Options{Cwd: t.TempDir(), Home: t.TempDir(), Settings: settings, NoSession: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetModel("b/model-b"); err != nil {
		t.Fatal(err)
	}
	s.Diagnostics().LogPersistError(errors.New("disk full"))
	recs := readLog(t, s.Diagnostics().Path())
	last := recs[len(recs)-1]
	if last["provider"] != "b" || last["model"] != "model-b" {
		t.Fatalf("record attributed to %v/%v", last["provider"], last["model"])
	}
}

func TestDiagnosticsLogRotates(t *testing.T) {
	old := maxLogBytes
	maxLogBytes = 2048
	t.Cleanup(func() { maxLogBytes = old })

	d := NewDiagnostics(t.TempDir())
	for i := 0; i < 100; i++ {
		d.LogPersistError(fmt.Errorf("failure %d %s", i, strings.Repeat("x", 100)))
	}
	cur, err := os.Stat(d.Path())
	if err != nil {
		t.Fatal(err)
	}
	backup, err := os.Stat(d.Path() + ".1")
	if err != nil {
		t.Fatal("no backup after the log passed its cap")
	}
	if cur.Size() > maxLogBytes+512 || backup.Size() > maxLogBytes+512 {
		t.Fatalf("log %d bytes, backup %d bytes; cap %d", cur.Size(), backup.Size(), maxLogBytes)
	}
	recs := readLog(t, d.Path())
	if last := recs[len(recs)-1]["error"].(string); !strings.HasPrefix(last, "failure 99 ") {
		t.Fatalf("the newest record was lost: %q", last)
	}
}

// A real denial from a file tool, run through the loop, is logged with the
// tool, the path refused and the policy's reason; an ordinary tool error is
// not.
func TestDiagnosticsRecordsSandboxDenials(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	secret := filepath.Join(home, "settings.json")
	if err := os.WriteFile(secret, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	pol := sandbox.Build(sandbox.Config{Cwd: cwd, Home: home})
	tools := CodingTools(cwd, ToolOptions{Sandbox: func() sandbox.Policy { return pol }})
	p := fake.New(
		fake.ToolCalls("", fake.Call{ID: "c1", Name: "read", Args: map[string]any{"path": secret}}),
		fake.ToolCalls("", fake.Call{ID: "c2", Name: "read", Args: map[string]any{"path": filepath.Join(cwd, "missing.go")}}),
		fake.Text("done"),
	)
	d := NewDiagnostics(t.TempDir())
	agent.Run(context.Background(), agent.LoopConfig{Provider: p, Model: "m", Tools: tools}, nil,
		[]agent.Message{agent.NewUserText("go")}, d.observe)

	var denials []map[string]any
	for _, r := range readLog(t, d.Path()) {
		if r["kind"] == "sandbox_denied" {
			denials = append(denials, r)
		}
	}
	if len(denials) != 1 {
		t.Fatalf("want 1 denial, got %v", denials)
	}
	r := denials[0]
	if r["tool"] != "read" || r["source"] != "policy" || r["target"] != secret ||
		!strings.Contains(r["reason"].(string), "hidden") {
		t.Fatalf("record %v", r)
	}
}

func TestSandboxDenialFromCommandOutput(t *testing.T) {
	on := sandbox.Policy{Enabled: true}
	cases := []struct {
		name   string
		pol    sandbox.Policy
		output string
		want   string
	}{
		// gh prints Go's lowercase error; this used to go unnoticed.
		{"lowercase EPERM", on, "warning: x\nopen /Users/u/.config/gh/config.yml: operation not permitted\n",
			"open /Users/u/.config/gh/config.yml: operation not permitted"},
		{"DNS with the network off", on, "fatal: unable to access 'https://github.com/x.git/': Could not resolve host: github.com\n",
			"fatal: unable to access 'https://github.com/x.git/': Could not resolve host: github.com"},
		{"DNS with the network on is not the sandbox", sandbox.Policy{Enabled: true, Network: true},
			"Could not resolve host: github.com\n", ""},
		{"sandbox off", sandbox.Policy{}, "operation not permitted\n", ""},
		{"ordinary failure", on, "FAIL\tgithub.com/x\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sandboxDenial(tc.pol, tc.output); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}

	// A command denial reaches the log through the result's details.
	d := NewDiagnostics(t.TempDir())
	d.observe(&agent.ToolExecutionStartEvent{ToolCallID: "b", ToolName: "bash", Args: map[string]any{"command": "git push"}})
	d.observe(&agent.ToolExecutionEndEvent{ToolCallID: "b", ToolName: "bash", Result: agent.ToolResult{
		Details: map[string]any{"sandbox_denial": "Could not resolve host: github.com"}}})
	recs := readLog(t, d.Path())
	if len(recs) != 1 || recs[0]["source"] != "command" || recs[0]["target"] != "git push" || recs[0]["tool"] != "bash" {
		t.Fatalf("records %v", recs)
	}
}
