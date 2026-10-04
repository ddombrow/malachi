package coding

import (
	"strings"
	"testing"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/ai/fake"
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
	if got := sm.get(); got.ToolShare() <= 0 {
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
	if got.PromptTokens != 40_000 || got.PeakPrompt != 40_000 {
		t.Errorf("last/peak input = %d/%d, want 40000", got.PromptTokens, got.PeakPrompt)
	}
	if tt := got.ToolTokens(); tt < 40_000 || tt > 41_000 {
		t.Errorf("tool tokens = %d, want ~40960", tt)
	}
	if share := got.ToolShare(); share < 0.99 || share > 1.01 {
		t.Errorf("tool share = %.2f, want ~1.0 (all context is tool output here)", share)
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
	if got := sm.get(); got.Samples != 1 || got.PromptTokens != 1000 {
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

// The ceiling on tool output comes from what the provider actually charged
// for: the window, minus the reserve, minus everything that is not tool output.
// Before there is a measurement, the fixed default stands.
func budgetAt(tokens int) int { return int(float64(tokens) / 0.25) }

func TestToolOutputBudgetIsDerived(t *testing.T) {
	const window = 128_000
	const ratio = 0.25 // four bytes per token, typical for prose

	// budgetAt is the token allowance in bytes, so the arithmetic in these
	// tests reads like the formula instead of a precomputed constant.

	// Nothing measured yet: the old fixed ceiling stands.
	if got := toolOutputBudgetBytes(window, 0, 0, 0); got != defaultToolResultBudget {
		t.Errorf("unmeasured budget = %d, want the default %d", got, defaultToolResultBudget)
	}

	// 20k input tokens of which 5k is tool output leaves 15k of conversation,
	// so 128k - 16k reserve - 15k = 97k tokens for tool output.
	if got, want := toolOutputBudgetBytes(window, 20_000, 5_000, ratio), budgetAt(97_000); got != want {
		t.Errorf("light budget = %d, want %d", got, want)
	}

	// A fuller context allows less: 100k input with 80k of tool output leaves
	// only 92k tokens against 97k.
	heavy, light := toolOutputBudgetBytes(window, 100_000, 80_000, ratio), toolOutputBudgetBytes(window, 20_000, 5_000, ratio)
	if heavy >= light {
		t.Errorf("a fuller context should allow less tool output: %d vs %d", heavy, light)
	}

	// A session nearly full of conversation has no headroom, and the floor
	// applies rather than a derived budget.
	if got := toolOutputBudgetBytes(window, 127_000, 20_000, ratio); got != defaultToolResultBudget {
		t.Errorf("no headroom should fall back to the default, got %d", got)
	}
	// Just above the floor the derived value is used: 9k tokens left.
	if got, want := toolOutputBudgetBytes(window, 127_000, 24_000, ratio), budgetAt(9_000); got != want {
		t.Errorf("just above the floor should use the derived value: %d, want %d", got, want)
	}

	// A larger window allows proportionally more tool output.
	if big, light := toolOutputBudgetBytes(400_000, 20_000, 5_000, ratio), toolOutputBudgetBytes(window, 20_000, 5_000, ratio); big <= light {
		t.Errorf("a larger window should allow more tool output: %d vs %d", big, light)
	}

	// Denser output buys fewer bytes for the same token budget.
	dense, sparse := toolOutputBudgetBytes(window, 20_000, 5_000, 0.5), toolOutputBudgetBytes(window, 20_000, 5_000, 0.15)
	if dense >= sparse {
		t.Errorf("dense tool output should get fewer bytes: %d vs %d", dense, sparse)
	}
}

// The session reports the provider's window, and derives a ceiling from the
// measurement while keeping the fixed default until there is something to
// derive from.
func TestSessionDerivesCeilingFromUsage(t *testing.T) {
	s, err := Open(Options{
		Cwd: t.TempDir(), Home: t.TempDir(), Settings: &Settings{},
		Provider: fake.New(fake.Text("ok")), NoSession: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.ContextWindow() != defaultContextWindow {
		t.Errorf("unset window = %d, want the default %d", s.ContextWindow(), defaultContextWindow)
	}
	if got := s.toolOutputBudget(s.ContextStats()); got != defaultToolResultBudget {
		t.Errorf("budget before any measurement = %d, want the default %d", got, defaultToolResultBudget)
	}
	// 120 kB of tool output measured at a quarter token per byte is 30k tokens,
	// which is all of the 30k the request was charged for, so nothing else is
	// in the way and all 128k-16k is available.
	stats := ContextStats{PromptTokens: 30_000, ToolBytes: 120_000, Ratio: 0.25}
	if got, want := s.toolOutputBudget(stats), budgetAt(112_000); got != want {
		t.Errorf("derived budget = %d, want %d", got, want)
	}
}

func TestContextLineRendersBothStates(t *testing.T) {
	if got := ContextLine("m", ContextStats{}); !strings.Contains(got, "no usage reported") {
		t.Errorf("unmeasured session should say so: %q", got)
	}
	line := ContextLine("kimi-k2.7-code", ContextStats{
		Samples: 5, PromptTokens: 40_000, PeakPrompt: 41_000, ToolBytes: 163_840,
		Ratio: 0.25, Ratios: 4, Compactions: 3, LimitErrors: 0,
	})
	for _, want := range []string{"kimi-k2.7-code", "40.0k", "peak 41.0k", "160.0 kB", "40.0k", "0.25 tokens per byte", "3 passes", "limit errors   0"} {
		if !strings.Contains(line, want) {
			t.Errorf("line missing %q:\n%s", want, line)
		}
	}
	// Without a ratio the line must not imply one was measured.
	line = ContextLine("m", ContextStats{Samples: 1, PromptTokens: 1000, ToolBytes: 500})
	if !strings.Contains(line, "not yet") {
		t.Errorf("unmeasured ratio should be stated plainly:\n%s", line)
	}
}

// The window can be wrong, and the only way to know is to compare what the
// provider accepted against what the configuration claims. This is the failure
// the user hits: a model that takes far more than the assumed window, so the
// gauge reads full and the derived budget is nonsense.
func TestContextLineWarnsWhenTheWindowIsWrong(t *testing.T) {
	stats := ContextStats{
		Samples: 3, PromptTokens: 255_200, PeakPrompt: 255_200, Window: 128_000,
		ToolBytes: 40_000, Ratio: 0.25, Ratios: 2,
	}
	if !exceedsConfiguredWindow(stats) {
		t.Fatal("a request larger than the configured window should be detected")
	}
	if line := ContextLine("m", stats); !strings.Contains(line, "CONFIGURED AT 128.0k BUT 255.2k WAS ACCEPTED") {
		t.Errorf("the readout should name the mismatch:\n%s", line)
	}
	// At or under the configured window there is nothing to warn about.
	stats.PromptTokens, stats.PeakPrompt = 100_000, 100_000
	if exceedsConfiguredWindow(stats) {
		t.Error("a request inside the window should not warn")
	}
	// Nothing measured yet cannot disagree with anything.
	if exceedsConfiguredWindow(ContextStats{Window: 128_000}) {
		t.Error("an unmeasured session should not warn")
	}
}

// Resuming a long session must not leave the context blank until the first
// reply: the size is knowable locally, and the gauge that exists to warn about
// it is the thing that was missing.
func TestResumedSessionIsSizedBeforeAnyRequest(t *testing.T) {
	s := summarizeFixture(t, fake.Text("unused"))
	s.estimateContext()

	stats := s.ContextStats()
	if stats.Samples != 0 {
		t.Fatalf("no request has been sent, but %d were measured", stats.Samples)
	}
	if !stats.PromptEstimated() {
		t.Fatal("a resumed session should report an estimate before any measurement")
	}
	if stats.EffectivePrompt() != stats.EstimatedPrompt {
		t.Errorf("effective prompt %d should be the estimate %d while nothing is measured",
			stats.EffectivePrompt(), stats.EstimatedPrompt)
	}
	if stats.EstimatedPrompt <= 0 {
		t.Fatal("the estimate is empty; a conversation and a system prompt were loaded")
	}
}

// The first real usage replaces the estimate. Averaging them, or letting the
// estimate stand, would misreport what the provider charged for.
func TestMeasurementSupersedesTheEstimate(t *testing.T) {
	sm := newCtxSampler()
	sm.estimate(90_000, 0)

	sm.observe(assistant(12_000), 0, 0)

	st := sm.get()
	if st.Samples != 1 {
		t.Fatalf("samples = %d, want 1", st.Samples)
	}
	if st.PromptEstimated() {
		t.Error("the estimate should not be presented as the context once usage is known")
	}
	if st.EffectivePrompt() != 12_000 {
		t.Errorf("effective prompt = %d, want the measured 12000", st.EffectivePrompt())
	}
}

// Both figures are estimates, so the readout must say which, on every line that
// carries a number.
func TestContextLineBeforeAnyRequestSaysItIsAnEstimate(t *testing.T) {
	line := ContextLine("test/model", ContextStats{Window: 128_000, EstimatedPrompt: 90_000})
	if strings.Contains(line, "no usage reported") {
		t.Errorf("an estimated session has a figure to report: %q", line)
	}
	if !strings.Contains(line, "estimated") || !strings.Contains(line, "≈") {
		t.Errorf("the readout should mark the figure as an estimate: %q", line)
	}
	if !strings.Contains(line, "0 measured") {
		t.Errorf("the readout should be clear nothing was measured: %q", line)
	}
}

// An estimate past the window is worth warning about before the request that
// would be rejected.
func TestContextLineWarnsWhenEstimatedPastWindow(t *testing.T) {
	line := ContextLine("test/model", ContextStats{Window: 128_000, EstimatedPrompt: 200_000})
	if !strings.Contains(line, "compact before sending") {
		t.Errorf("an over-window estimate should advise compacting: %q", line)
	}
}

// The estimate has to track the conversation: doubling the messages cannot
// leave the figure unchanged.
func TestEstimateGrowsWithTheConversation(t *testing.T) {
	s := summarizeFixture(t, fake.Text("unused"))
	before, _ := estimatePromptTokens(s.system, s.tools, s.Harness.Messages(), 0)

	doubled := append(s.Harness.Messages(), s.Harness.Messages()...)
	after, _ := estimatePromptTokens(s.system, s.tools, doubled, 0)

	if after <= before {
		t.Errorf("doubling the conversation did not grow the estimate: %d then %d", before, after)
	}
}
