package coding

import (
	"strings"
	"testing"

	"github.com/ddombrow/malachi/agent"
)

// assistant builds a finished assistant message with the given usage.
func assistant(input int64) *agent.AssistantMessage {
	m := agent.NewAssistantMessage("m")
	m.Usage.Input = input
	return m
}

// The ratio between tokens and bytes of tool output is measured from the
// deltas between consecutive requests, not assumed: the whole point is that
// code and JSON are denser than prose, and the rate differs per model.
func TestContextSamplerMeasuresToolOutputRatio(t *testing.T) {
	sm := newCtxSampler()

	// A first request with little tool output cannot produce a ratio.
	sm.observe(assistant(1000), 500, 0)
	if got := sm.get(); got.Ratio != 0 || got.Ratios != 0 {
		t.Fatalf("ratio measured without enough signal: %+v", got)
	}
	if got := sm.get(); got.ToolShare <= 0 {
		t.Error("share should still be shown, flagged as unmeasured")
	}

	// Each request adds 32 kB of tool output and 8k tokens of context, i.e.
	// 0.25 tokens per byte — four bytes per token.
	for i := 2; i <= 5; i++ {
		sm.observe(assistant(int64(i)*8000), i*32*1024, 0)
	}
	got := sm.get()
	if got.Ratios != 4 {
		t.Fatalf("want 4 usable deltas, got %d", got.Ratios)
	}
	if got.Ratio < 0.24 || got.Ratio > 0.26 {
		t.Errorf("ratio = %.3f, want ~0.25 tokens per byte", got.Ratio)
	}
	if got.InputTokens != 40_000 || got.PeakInput != 40_000 {
		t.Errorf("last/peak input = %d/%d, want 40000", got.InputTokens, got.PeakInput)
	}
	if tt := got.ToolTokens(); tt < 40_000 || tt > 41_000 {
		t.Errorf("tool tokens = %d, want ~40960", tt)
	}
	if got.ToolShare < 0.99 || got.ToolShare > 1.01 {
		t.Errorf("tool share = %.2f, want ~1.0 (all context is tool output here)", got.ToolShare)
	}
	if sm.get().Samples != 5 {
		t.Errorf("samples = %d, want 5", sm.get().Samples)
	}
}

// A request with no usage gives nothing to measure and must not corrupt the
// series or fake progress.
func TestContextSamplerIgnoresMissingUsage(t *testing.T) {
	sm := newCtxSampler()
	sm.observe(assistant(1000), 500, 0)
	sm.observe(agent.NewAssistantMessage("m"), 5000, 0)
	if got := sm.get(); got.Samples != 1 || got.InputTokens != 1000 {
		t.Fatalf("a usage-less message changed the measurement: %+v", got)
	}
}

// The median keeps one odd request from moving the ratio.
func TestContextSamplerUsesMedian(t *testing.T) {
	sm := newCtxSampler()
	sm.observe(assistant(1000), 0, 0)
	sm.observe(assistant(9000), 32*1024, 0)  // 0.25 tokens/byte
	sm.observe(assistant(17000), 64*1024, 0) // 0.25
	sm.observe(assistant(70000), 96*1024, 0) // one wild request
	if got := sm.get().Ratio; got < 0.24 || got > 0.26 {
		t.Errorf("median ratio = %.3f, want ~0.25 despite one outlier", got)
	}
}

// Compaction and limit errors are counted as they pass through.
func TestContextSamplerTracksCompactionAndLimitErrors(t *testing.T) {
	sm := newCtxSampler()
	sm.observe(assistant(1000), 10, 0)
	sm.observe(assistant(2000), 20, 3) // a pass fired before this request
	if got := sm.get(); !got.Compacted || got.Compactions != 3 {
		t.Errorf("compaction not recorded: %+v", got)
	}

	rejected := assistant(3000)
	rejected.StopReason = agent.StopError
	rejected.ErrorMessage = "400 Bad Request"
	rejected.Diagnostics = []agent.Diagnostic{{
		Type: "http_error",
		Error: &agent.DiagnosticError{
			Name: "BadRequestError", Message: "This model's maximum context length is 8192 tokens", Code: 400,
		},
	}}
	sm.observe(rejected, 30, 3)
	if sm.get().LimitErrors != 1 {
		t.Errorf("limit error not counted: %+v", sm.get())
	}
	// The rejected request must not pollute the ratio series.
	if sm.get().Ratios != 0 {
		t.Errorf("a rejected request entered the ratio series: %+v", sm.get())
	}
}

// A 500 mentioning "long" is an outage, not a size complaint.
func TestIsContextLimitIgnoresServerErrors(t *testing.T) {
	m := agent.NewAssistantMessage("m")
	m.StopReason = agent.StopError
	m.Diagnostics = []agent.Diagnostic{{
		Type:  "http_error",
		Error: &agent.DiagnosticError{Message: "upstream request took too long", Code: 503},
	}}
	if isContextLimit(m) {
		t.Error("a 503 is not a context-limit rejection")
	}
	m.Diagnostics[0].Error.Code = 400
	m.Diagnostics[0].Error.Message = "prompt is too long: 300000 tokens > 200000"
	if !isContextLimit(m) {
		t.Error("a 400 about prompt length is a context-limit rejection")
	}
	if isContextLimit(assistant(10)) {
		t.Error("a successful message is not a rejection")
	}
}

func TestContextLineRendersBothStates(t *testing.T) {
	if got := ContextLine("m", ContextStats{}); !strings.Contains(got, "no usage reported") {
		t.Errorf("unmeasured session should say so: %q", got)
	}
	line := ContextLine("kimi-k2.7-code", ContextStats{
		Samples: 5, InputTokens: 40_000, PeakInput: 41_000, ToolBytes: 163_840,
		Ratio: 0.25, Ratios: 4, ToolShare: 1, Compactions: 3, LimitErrors: 0,
	})
	for _, want := range []string{"kimi-k2.7-code", "40.0k", "peak 41.0k", "160.0 kB", "40.0k", "0.25 tokens per byte", "3 passes", "limit errors   0"} {
		if !strings.Contains(line, want) {
			t.Errorf("line missing %q:\n%s", want, line)
		}
	}
	// Without a ratio the line must not imply one was measured.
	line = ContextLine("m", ContextStats{Samples: 1, InputTokens: 1000, ToolBytes: 500})
	if !strings.Contains(line, "not yet") {
		t.Errorf("unmeasured ratio should be stated plainly:\n%s", line)
	}
}
