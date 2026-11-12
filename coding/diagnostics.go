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
	"github.com/ddombrow/malachi/codemode"
	"github.com/ddombrow/malachi/sandbox"
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

	// targets holds what each running tool call was aimed at, from its start
	// event, so a denial can be logged with the path or command refused.
	targetsMu sync.Mutex
	targets   map[string]string
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

// maxLogBytes is the size at which the log is rotated. A variable so tests
// can rotate without writing megabytes.
var maxLogBytes int64 = 8 << 20

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
	switch e := e.(type) {
	case *agent.MessageEndEvent:
		if a, ok := e.Message.(*agent.AssistantMessage); ok {
			d.LogAssistantError(a)
		}
	case *agent.ToolExecutionStartEvent:
		d.targetsMu.Lock()
		if d.targets == nil {
			d.targets = map[string]string{}
		}
		d.targets[e.ToolCallID] = toolTarget(e.Args)
		d.targetsMu.Unlock()
	case *agent.ToolExecutionEndEvent:
		d.targetsMu.Lock()
		target := d.targets[e.ToolCallID]
		delete(d.targets, e.ToolCallID)
		d.targetsMu.Unlock()
		if source, reason := sandboxDenialOf(e.Result, e.IsError); source != "" {
			d.LogSandboxDenied(e.ToolName, source, reason, target)
		}
	}
}

// sandboxDenialOf reports whether a tool result is the sandbox refusing it.
// source is "policy" when a file tool's check refused, which is certain, and
// "command" when a command's output looked like a refusal, which is a guess.
func sandboxDenialOf(r agent.ToolResult, isError bool) (source, reason string) {
	if isError {
		text := r.Text()
		if strings.Contains(text, sandbox.ErrDenied.Error()) {
			return "policy", text
		}
		return "", ""
	}
	if details, ok := r.Details.(map[string]any); ok {
		if line, _ := details["sandbox_denial"].(string); line != "" {
			return "command", line
		}
	}
	return "", ""
}

// toolTarget is what a tool call acts on, for the log: its path, command
// or URL.
func toolTarget(args map[string]any) string {
	for _, key := range []string{"path", "command", "url"} {
		if s, ok := args[key].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// LogSandboxDenied records the sandbox refusing a tool call, so whether
// models run into it, and what they do next, can be answered from the log
// rather than by searching transcripts.
func (d *Diagnostics) LogSandboxDenied(tool, source, reason, target string) {
	d.record("sandbox_denied", map[string]any{
		"tool":   tool,
		"source": source,
		"reason": truncate(reason, 1000),
		"target": truncate(target, 1000),
	})
}

// LogScript records how a script ended: how many calls it made of which
// tools, and whether it failed. It is the measure of whether Code Mode
// pays its way.
func (d *Diagnostics) LogScript(det codemode.Details) {
	tools := map[string]int{}
	errs := 0
	for _, c := range det.Calls {
		tools[c.Name]++
		if c.Status == codemode.StatusError {
			errs++
		}
	}
	fields := map[string]any{
		"calls":      len(det.Calls),
		"callErrors": errs,
		"batches":    det.Batches,
		"tools":      tools,
		"durationMs": det.DurationMs,
	}
	if det.Error != "" {
		fields["error"] = truncate(det.Error, 1000)
	}
	d.record("script", fields)
}

// LogCompaction records a summary replacing part of the conversation. The
// summary itself is recorded only as its length: it is long, and the point is
// that it happened and what it cost.
func (d *Diagnostics) LogCompaction(summary string, replaced, kept int, u agent.Usage) {
	if d == nil {
		return
	}
	d.record("compaction", map[string]any{
		"summaryBytes": len(summary),
		"replaced":     replaced,
		"kept":         kept,
		"usage":        u.TotalTokens,
	})
}

// LogOverflowRecovery records a compaction made to rescue a request the
// provider rejected for exceeding the context window.
func (d *Diagnostics) LogOverflowRecovery(replaced, kept int, u agent.Usage) {
	if d == nil {
		return
	}
	d.record("overflow_recovery", map[string]any{
		"replaced": replaced,
		"kept":     kept,
		"usage":    u.TotalTokens,
	})
}

// LogOverflowRecoveryFailed records a context-window rejection that could not
// be rescued by compacting. It is its own kind so it is not mistaken for a
// session-file failure, which is a different problem with a different fix.
func (d *Diagnostics) LogOverflowRecoveryFailed(err error) {
	d.record("overflow_recovery_failed", map[string]any{
		"error": truncate(err.Error(), 2000),
	})
}

// LogTrust records a project-trust decision, by path and outcome only. Which
// directories were trusted is the thing worth being able to audit later.
func (d *Diagnostics) LogTrust(t TrustState) {
	if d == nil {
		return
	}
	d.record("project_trust", map[string]any{
		"path":     t.Path,
		"decision": string(t.Decision),
		"policy":   string(t.Policy),
		"source":   t.Source,
		"files":    t.Resources.Total,
	})
}

// LogContextSample records what one request cost: the provider's token count
// beside the tool output it carried. Numbers only, no content, so a week of
// sessions is a usable distribution rather than one session's snapshot.
func (d *Diagnostics) LogContextSample(model string, c ContextStats) {
	if d == nil || c.Samples == 0 {
		return
	}
	fields := map[string]any{
		"promptTokens": c.PromptTokens,
		"toolBytes":    c.ToolBytes,
		"samples":      c.Samples,
		"trimmed":      c.Trimmed,
	}
	if c.Ratio > 0 {
		fields["tokensPerByte"] = c.Ratio
		fields["toolShare"] = c.ToolShare
	}
	if c.PeakPrompt > 0 {
		fields["peakPrompt"] = c.PeakPrompt
	}
	if c.LimitErrors > 0 {
		fields["limitErrors"] = c.LimitErrors
	}
	d.record("ctx_sample", fields)
}

// LogContextEstimate records a locally sized request, so a gauge that was
// showing an estimate can be told apart from one showing a measurement.
func (d *Diagnostics) LogContextEstimate(promptTokens, toolBytes int64) {
	d.record("ctx_estimate", map[string]any{
		"promptTokens": promptTokens,
		"toolBytes":    toolBytes,
	})
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
	// The log is shared by every session and records a sample per reply, so
	// it is capped: past maxLogBytes the file becomes the single backup and a
	// new one starts. Disk use stays under twice the cap, and the most recent
	// history is always kept.
	if info, err := os.Stat(d.path); err == nil && info.Size() >= maxLogBytes {
		_ = os.Rename(d.path, d.path+".1")
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
