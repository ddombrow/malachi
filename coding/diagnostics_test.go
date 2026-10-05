package coding

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ddombrow/malachi/agent"
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
