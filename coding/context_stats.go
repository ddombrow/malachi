package coding

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/ddombrow/malachi/agent"
)

// Context measurement.
//
// Compaction's ceiling used to be a fixed byte count, on the assumption that
// bytes convert to tokens at some fixed rate. They do not: JSON and source code
// are denser than prose, and the rate differs per model. Rather than guess,
// this samples what the provider actually reported for each request and
// derives the conversion factor from the deltas between consecutive requests,
// where the change is dominated by the tool output that was just added.
//
// The numbers exist to answer two questions: how much of the context window
// tool output occupies, and whether the ceiling is ever the binding
// constraint. They are reported by /ctx and appended to the diagnostics log.

const (
	// ctxSampleRing is how many requests are kept for the delta measurement.
	// Old samples only matter for the ratio, and the ratio converges fast.
	ctxSampleRing = 64
	// minRatioDeltaBytes is the smallest change in tool output worth using
	// for a ratio sample. Below it the delta in tokens is dominated by the
	// user's prompt and the assistant's own reply.
	minRatioDeltaBytes = 8 * 1024
	// fallbackCharsPerToken is only used to show a share before anything has
	// been measured, and is never used to make a decision.
	fallbackCharsPerToken = 3.5
)

// ctxSample is one request as the provider saw it and as we measured it.
type ctxSample struct {
	InputTokens int64
	ToolBytes   int
	Compacted   bool
}

// ContextStats reports the measured shape of a session's context. Every field
// is observed; nothing here is estimated from a character count except
// ToolShare, and only until a real ratio exists.
type ContextStats struct {
	Samples     int     // requests measured
	InputTokens int64   // input tokens the provider reported for the last one
	PeakInput   int64   // highest input tokens seen
	ToolBytes   int     // tool-result bytes carried by the last request
	Compacted   bool    // whether the last request was compacted
	Compactions uint64  // passes so far
	Ratio       float64 // measured tokens per byte of tool output; 0 until known
	Ratios      int     // how many deltas the ratio is based on
	ToolShare   float64 // tool output as a fraction of the last request
	LimitErrors int     // requests rejected for exceeding the context window
}

// ToolTokens is the tool output in the last request, converted with the
// measured ratio when one exists.
func (c ContextStats) ToolTokens() int64 {
	if c.Ratio > 0 {
		return int64(float64(c.ToolBytes) * c.Ratio)
	}
	if c.RatioBytes() > 0 {
		return int64(float64(c.ToolBytes) / c.RatioBytes())
	}
	return 0
}

// RatioBytes is the inverse of Ratio, kept for readability at call sites.
func (c ContextStats) RatioBytes() float64 {
	if c.Ratio > 0 {
		return 1 / c.Ratio
	}
	return 0
}

type ctxSampler struct {
	mu      sync.Mutex
	ring    []ctxSample
	stats   ContextStats
	lastSeq uint64 // compaction Seq already accounted for
}

func newCtxSampler() *ctxSampler { return &ctxSampler{ring: make([]ctxSample, 0, ctxSampleRing)} }

// observe folds one finished assistant message into the measurement. It runs
// synchronously from the harness dispatch, before the next request is
// prepared, so the preparer's tool bytes belong to the request the usage
// describes.
func (sm *ctxSampler) observe(m *agent.AssistantMessage, toolBytes int, compactionSeq uint64) {
	if m.Usage.Input <= 0 {
		return // providers that report no usage give us nothing to measure
	}
	compacted := compactionSeq > sm.lastSeq
	sm.lastSeq = compactionSeq

	sm.mu.Lock()
	defer sm.mu.Unlock()

	sm.ring = append(sm.ring, ctxSample{InputTokens: m.Usage.Input, ToolBytes: toolBytes, Compacted: compacted})
	if len(sm.ring) > ctxSampleRing {
		sm.ring = sm.ring[len(sm.ring)-ctxSampleRing:]
	}

	sm.stats.Samples++
	sm.stats.InputTokens = m.Usage.Input
	sm.stats.ToolBytes = toolBytes
	sm.stats.Compacted = compacted
	sm.stats.Compactions = compactionSeq
	if m.Usage.Input > sm.stats.PeakInput {
		sm.stats.PeakInput = m.Usage.Input
	}
	if isContextLimit(m) {
		sm.stats.LimitErrors++
	}

	ratios := make([]float64, 0, len(sm.ring))
	for i := 1; i < len(sm.ring); i++ {
		dBytes := sm.ring[i].ToolBytes - sm.ring[i-1].ToolBytes
		dTokens := sm.ring[i].InputTokens - sm.ring[i-1].InputTokens
		if dBytes < minRatioDeltaBytes || dTokens <= 0 {
			continue
		}
		ratios = append(ratios, float64(dTokens)/float64(dBytes))
	}
	if len(ratios) > 0 {
		sort.Float64s(ratios)
		sm.stats.Ratio = ratios[len(ratios)/2] // median: one odd request should not move it
		sm.stats.Ratios = len(ratios)
		sm.stats.ToolShare = float64(sm.ring[len(sm.ring)-1].ToolBytes*int(sm.stats.Ratio*1000)) /
			float64(sm.ring[len(sm.ring)-1].InputTokens*1000)
	} else {
		// Unmeasured: show a share from a documented guess, clearly flagged,
		// rather than implying a precision that does not exist.
		sm.stats.ToolShare = float64(toolBytes) / fallbackCharsPerToken / float64(m.Usage.Input)
	}
}

func (sm *ctxSampler) get() ContextStats {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.stats
}

// contextLimitPhrases are how providers say "your request was too big". This is
// a heuristic used for counting, and later for deciding to retry; it is not a
// substitute for a per-provider error type.
var contextLimitPhrases = []string{
	"context length", "context_length", "maximum context", "too many tokens",
	"too long", "prompt is too long", "reduce the length",
}

// isContextLimit reports whether a failed assistant message looks like the
// provider rejecting the request for size.
func isContextLimit(m *agent.AssistantMessage) bool {
	if m == nil || m.StopReason != agent.StopError {
		return false
	}
	for _, d := range m.Diagnostics {
		if d.Type != "http_error" || d.Error == nil {
			continue
		}
		text := strings.ToLower(d.Error.Message + " " + d.Error.Name)
		if !mentionsSize(text) {
			continue
		}
		// Only 400-class rejections are size complaints; a 503 whose body
		// happens to say "too long" is an outage.
		switch statusCode(d.Error.Code) {
		case 400, 413:
			return true
		case 0: // no code recorded: the phrase is all we have
			return true
		}
	}
	// No classified diagnostic to go on, so fall back to the message text.
	if len(m.Diagnostics) == 0 {
		return mentionsSize(strings.ToLower(m.ErrorMessage))
	}
	return false
}

func mentionsSize(text string) bool {
	for _, phrase := range contextLimitPhrases {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

// statusCode reads an HTTP status from a decoded diagnostic, which may be a
// float64 from JSON or an int set in this process.
func statusCode(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int32:
		return int(n)
	case int64:
		return int(n)
	case float64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}

// contextReserveTokens is headroom kept for the next response and for the
// provider's own framing; tau reserves the same amount.
const contextReserveTokens = 16_000

// minToolBudgetTokens is the floor on how much tool output is always left
// visible. Below this, trimming costs more detail than the space saves.
const minToolBudgetTokens = 8_000

// toolOutputBudgetBytes derives the ceiling on context-visible tool output from
// what the provider actually reports, rather than from a fixed byte count.
//
// Everything except tool output — system prompt, tool definitions, the
// conversation, the ledger — is already known as tokens, because the provider
// counted it. What is left of the window after that, and after a reserve for
// the reply, is what tool output may have. It is converted to bytes with the
// measured ratio so the ceiling tracks the model's real density.
//
// It returns defaultToolResultBudget when there is nothing to measure yet: an
// unmeasured session should keep the old behaviour rather than a guess.
func toolOutputBudgetBytes(window int, inputTokens int, toolTokens int, ratio float64) int {
	if window <= 0 || inputTokens <= 0 || ratio <= 0 {
		return defaultToolResultBudget
	}
	other := inputTokens - toolTokens
	free := window - contextReserveTokens - other
	if free < minToolBudgetTokens {
		return defaultToolResultBudget
	}
	budget := int(float64(free) / ratio)
	if budget < 1 {
		budget = 1
	}
	return budget
}

// ContextLine renders the measurement for /ctx in the TUI's plain style.
func ContextLine(model string, c ContextStats) string {
	if c.Samples == 0 {
		return "no usage reported yet — this provider does not send token counts"
	}
	var b strings.Builder
	peak := ""
	if c.PeakInput > c.InputTokens {
		peak = fmt.Sprintf(" (peak %s)", tokenCount(c.PeakInput))
	}
	fmt.Fprintf(&b, "context: %s\n  last request   %s%s", model, tokenCount(c.InputTokens), peak)
	fmt.Fprintf(&b, "\n  tool output    %s ≈ %s (%.0f%% of the request)",
		byteCount(c.ToolBytes), tokenCount(c.ToolTokens()), c.ToolShare*100)
	if c.Ratio > 0 {
		fmt.Fprintf(&b, "\n  measured       %.2f tokens per byte of tool output (%d deltas)",
			c.Ratio, c.Ratios)
	} else {
		fmt.Fprintf(&b, "\n  measured       not yet — need %s of tool output between two requests",
			byteCount(minRatioDeltaBytes))
	}
	compactions := "none"
	if c.Compactions > 0 {
		compactions = fmt.Sprintf("%d passes", c.Compactions)
		if c.Compacted {
			compactions += " (the last request was one)"
		}
	}
	fmt.Fprintf(&b, "\n  compaction     %s", compactions)
	fmt.Fprintf(&b, "\n  limit errors   %d", c.LimitErrors)
	fmt.Fprintf(&b, "\n  requests       %d measured", c.Samples)
	return b.String()
}

func tokenCount(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

func byteCount(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f kB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
