package coding

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ddombrow/malachi/agent"
)

// Diagnostics is an append-only JSONL log for failures that have nowhere else
// to go. Provider errors and tool errors are already durable because they are
// messages in the session file; panics and persistence failures are not, and
// a trace that scrolls past an alternate screen is gone. This is where those
// land instead.
//
// Every record carries the session and run it belongs to, so one file per
// home is enough to reconstruct what happened. Writes never fail loudly: a
// logger that can take down the agent is worse than no logger.
//
// Port of tau_coding/diagnostics.py.
type Diagnostics struct {
	mu    sync.Mutex
	path  string
	ctx   map[string]any // session path, provider, model
	runID string
}

// LogsDir is where diagnostics live under a malachi home.
func LogsDir(home string) string { return filepath.Join(home, "logs") }

// NewDiagnostics returns a logger writing to <home>/logs/agent.jsonl.
func NewDiagnostics(home string) *Diagnostics {
	return &Diagnostics{
		path: filepath.Join(LogsDir(home), "agent.jsonl"),
		ctx:  map[string]any{},
	}
}

// Path is the log file, whether or not it exists yet.
func (d *Diagnostics) Path() string { return d.path }

// Bind records the session context stamped onto every later entry. Call it
// once the provider and model are known.
func (d *Diagnostics) Bind(sessionPath, provider, model string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if sessionPath != "" {
		d.ctx["session"] = sessionPath
	}
	d.ctx["provider"] = provider
	d.ctx["model"] = model
}

// runSeq keeps run ids distinct within a millisecond.
var runSeq atomic.Uint64

// NewRun starts a new run id, grouping everything that follows until the
// next call.
func (d *Diagnostics) NewRun() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.runID = fmt.Sprintf("run-%d-%d", time.Now().UnixMilli(), runSeq.Add(1))
	return d.runID
}

// LogPanic records a recovered panic with its stack and returns the log path
// so the caller can show the user where to look.
func (d *Diagnostics) LogPanic(where string, v any, stack []byte) string {
	d.record("panic", map[string]any{
		"where": where,
		"value": truncate(fmt.Sprint(v), 2000),
		"stack": truncate(string(stack), 16000),
	})
	return d.path
}

// LogAssistantError records a provider or loop failure. The message's own
// diagnostics (HTTP status, provider body) travel with it, so the log carries
// the parts the transcript never shows.
func (d *Diagnostics) LogAssistantError(m *agent.AssistantMessage) {
	if m.StopReason != agent.StopError && m.StopReason != agent.StopAborted {
		return
	}
	fields := map[string]any{
		"stopReason":   string(m.StopReason),
		"model":        m.Model,
		"errorMessage": truncate(m.ErrorMessage, 4000),
	}
	if m.ResponseID != "" {
		fields["responseId"] = m.ResponseID
	}
	if m.ResponseProvider != "" {
		fields["responseProvider"] = m.ResponseProvider
	}
	if len(m.Diagnostics) > 0 {
		fields["diagnostics"] = m.Diagnostics
	}
	d.record("assistant_error", fields)
}

// LogPersistError records a failure to append to the session file. This is
// the failure that cannot report itself anywhere else, so it is logged even
// though logging may also fail.
func (d *Diagnostics) LogPersistError(err error) {
	d.record("persist_error", map[string]any{
		"error": truncate(err.Error(), 2000),
	})
}

// observe follows the harness and records failures. It is subscribed even for
// in-memory sessions, which have no file to record them in.
func (d *Diagnostics) observe(e agent.Event) {
	if end, ok := e.(*agent.MessageEndEvent); ok {
		if a, ok := end.Message.(*agent.AssistantMessage); ok {
			d.LogAssistantError(a)
		}
	}
}

// LogContextSample records what one request cost: the provider's token count
// beside the tool output it carried. Numbers only, no content, so a week of
// sessions is a usable distribution rather than one session's snapshot.
func (d *Diagnostics) LogContextSample(model string, c ContextStats) {
	if d == nil || c.Samples == 0 {
		return
	}
	fields := map[string]any{
		"inputTokens": c.InputTokens,
		"toolBytes":   c.ToolBytes,
		"samples":     c.Samples,
		"compacted":   c.Compacted,
	}
	if c.Ratio > 0 {
		fields["tokensPerByte"] = c.Ratio
		fields["toolShare"] = c.ToolShare
	}
	if c.PeakInput > 0 {
		fields["peakInput"] = c.PeakInput
	}
	if c.LimitErrors > 0 {
		fields["limitErrors"] = c.LimitErrors
	}
	d.record("ctx_sample", fields)
}

func (d *Diagnostics) record(kind string, fields map[string]any) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	rec := map[string]any{
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		"kind":      kind,
	}
	if d.runID != "" {
		rec["runId"] = d.runID
	}
	for k, v := range d.ctx {
		rec[k] = v
	}
	for k, v := range fields {
		rec[k] = v
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(d.path), 0o700); err != nil {
		return
	}
	fh, err := os.OpenFile(d.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer fh.Close()
	_, _ = fh.Write(append(line, '\n'))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.TrimSpace(s[:n]) + fmt.Sprintf("… (%d bytes total)", len(s))
}
