package coding

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"

	"github.com/ddombrow/malachi/agent"
)

const (
	// defaultToolResultBudget bounds tool-result payloads in the provider
	// request only. It is not a model context window, and it does not shrink
	// the saved transcript. It is the ceiling until a provider has reported
	// enough usage to derive a better one.
	defaultToolResultBudget = 64 * 1024
	// maxTrimFactFieldBytes bounds each quoted value (a path, a command, an
	// error line) in a trim marker, so a marker stays a fixed small cost.
	maxTrimFactFieldBytes = 240
	// trimLowWaterDivisor sets how far below the ceiling an automatic trim
	// goes. Trims are sticky, and every newly trimmed result changes the
	// request from that point on, invalidating the provider's prompt cache for
	// everything after it. Trimming just enough to fit made that happen on
	// nearly every request; trimming to half the ceiling makes it rare.
	trimLowWaterDivisor = 2
)

// TrimEvent is the state of the request view. Results is how many tool
// results are currently kept out of it; NewResults is how many the latest
// pass newly trimmed. Seq moves only when that grows, so a UI can tell a fresh
// trim from a pass that merely re-applied the same markers.
type TrimEvent struct {
	Results int
	// NewResults is what a pass newly trimmed. The transcript is never
	// rewritten, so every pass re-applies the same markers; only this counts
	// as an event worth reporting.
	NewResults int
	Before     int
	After      int
	Seq        uint64
}

type trimLog struct {
	mu sync.Mutex
	TrimEvent
}

// reset clears the reported state, for when the transcript it described has
// been replaced.
func (l *trimLog) reset() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.TrimEvent = TrimEvent{}
}

func (l *trimLog) note(results, newResults, before, after int) {
	if l == nil || results == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	// The view's size is state: it moves as the transcript grows, and the
	// status bar wants the current figure. Only newly trimmed results are an
	// event, and only those may advance Seq.
	l.Results, l.Before, l.After = results, before, after
	if newResults == 0 {
		l.NewResults = 0
		return
	}
	l.NewResults = newResults
	l.Seq++
}

func (l *trimLog) get() TrimEvent {
	if l == nil {
		return TrimEvent{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.TrimEvent
}

type toolResultSize struct {
	index int
	bytes int
	// fact is what the trim marker says about how the call ended, so the
	// model keeps the outcome after losing the output.
	fact string
}

// codingContextPreparer is used serially by Harness.Run. The mutex also keeps
// it safe when request preparation is invoked directly from another goroutine,
// as /compact does.
type codingContextPreparer struct {
	mu sync.Mutex

	log       *trimLog
	prefix    []agent.Message
	results   []toolResultSize
	totalSize int
	// trimmed records which result indexes have been replaced with a marker.
	// It is sticky: once trimmed, a result stays trimmed in every later
	// request, so the request prefix only changes when more is trimmed.
	trimmed  map[int]bool
	budget   int  // derived ceiling in bytes, set from measured usage
	override int  // ceiling for one request, set by forceBudget
	force    bool // the override is pending
}

// newCodingContextPreparer builds a bounded, deterministic view of the coding
// history for each provider request. It caches facts as the transcript grows
// and uses copy-on-write for compacted results. It is used serially by
// Harness.Run; the mutex also keeps it safe when request preparation is invoked
// directly from another goroutine, as /compact does.
func newCodingContextPreparer(log *trimLog) *codingContextPreparer {
	return &codingContextPreparer{log: log, trimmed: map[int]bool{}}
}

func (c *codingContextPreparer) prepare(req agent.Request) agent.Request {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.update(req.Messages)
	budget, low := c.budgetLocked()
	trimmed, results, fresh, before, after := trimToolResultsWithSizes(req.Messages, c.results, c.trimmed, c.totalSize, budget, low)
	if results > 0 {
		req.Messages = trimmed
		c.log.note(results, fresh, before, after)
	}
	return req
}

// prime teaches the preparer the transcript before any request has been made,
// so a forced pass can tell the caller whether there is anything to compact.
func (c *codingContextPreparer) prime(messages []agent.Message) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.update(messages)
}

// toolBytes is the context-visible tool output in the request as last
// prepared. Read from the harness dispatch, it describes the request whose
// usage is arriving.
func (c *codingContextPreparer) toolBytes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.totalSize
}

// forceBudget asks for the next request to compact tool output harder than
// usual: everything above budget bytes is replaced by a marker. It reports
// whether there is anything left to compact. The override applies to one
// request; later ones go back to the default ceiling, though what it trimmed
// stays trimmed.
func (c *codingContextPreparer) forceBudget(budget int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.visibleLocked() <= budget {
		return false
	}
	c.override, c.force = budget, true
	return true
}

// visibleLocked is the tool output a request would carry with the sticky
// trims applied. Callers must hold c.mu.
func (c *codingContextPreparer) visibleLocked() int {
	visible := c.totalSize
	for _, size := range c.results {
		if c.trimmed[size.index] {
			visible -= size.bytes - len(trimMarker(c.prefix[size.index].(*agent.ToolResultMessage), size))
		}
	}
	return visible
}

// budgetLocked returns the ceiling for the next request and the level a trim
// goes down to once it is exceeded: a forced override trims to exactly its
// budget, the automatic ceiling well below itself. Callers must hold c.mu.
func (c *codingContextPreparer) budgetLocked() (budget, low int) {
	if c.force {
		c.force = false
		return c.override, c.override
	}
	budget = defaultToolResultBudget
	if c.budget > 0 {
		budget = c.budget
	}
	return budget, budget / trimLowWaterDivisor
}

// reset forgets everything measured about the old transcript. It is called
// when the conversation is replaced wholesale, which the append-only prefix
// cache would otherwise treat as a divergence on the next request.
func (c *codingContextPreparer) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prefix, c.results = nil, nil
	c.trimmed = map[int]bool{}
	c.totalSize = 0
}

// setBudget records the derived ceiling. The session recomputes it from each
// provider response, so one writer keeps this off the request path.
func (c *codingContextPreparer) setBudget(bytes int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if bytes > 0 {
		c.budget = bytes
	}
}

func (c *codingContextPreparer) update(messages []agent.Message) {
	common := 0
	for common < len(c.prefix) && common < len(messages) && sameMessage(c.prefix[common], messages[common]) {
		common++
	}
	if common < len(c.prefix) {
		c.prefix = nil
		c.results = nil
		c.trimmed = map[int]bool{}
		c.totalSize = 0
	}
	for i := len(c.prefix); i < len(messages); i++ {
		if m, ok := messages[i].(*agent.ToolResultMessage); ok {
			n := toolResultPayloadBytes(m)
			c.totalSize += n
			c.results = append(c.results, toolResultSize{index: i, bytes: n, fact: trimFact(m)})
		}
		c.prefix = append(c.prefix, messages[i])
	}
}

func sameMessage(a, b agent.Message) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	aValue, bValue := reflect.ValueOf(a), reflect.ValueOf(b)
	return aValue.Type() == bValue.Type() && aValue.Kind() == reflect.Pointer && aValue.Pointer() == bValue.Pointer()
}

// trimToolResults keeps tool-call/result pairs intact while replacing old
// result payloads until their combined context-visible content fits the byte
// budget. The most recent result is retained even when it alone exceeds it.
func trimToolResults(messages []agent.Message, budget int) ([]agent.Message, int, int, int, int) {
	var results []toolResultSize
	total := 0
	for i, m := range messages {
		if m, ok := m.(*agent.ToolResultMessage); ok {
			n := toolResultPayloadBytes(m)
			results = append(results, toolResultSize{index: i, bytes: n, fact: trimFact(m)})
			total += n
		}
	}
	return trimToolResultsWithSizes(messages, results, map[int]bool{}, total, budget, budget)
}

// trimToolResultsWithSizes replaces old result payloads with markers, keeping
// tool-call/result pairs intact and the most recent result whole.
//
// Trims are sticky: every result already in trimmed is replaced again, so a
// request differs from the previous one only where something new is trimmed.
// Re-deciding from scratch on each pass trimmed one more old result per
// request, which rewrote the prompt after it and defeated the provider's
// prompt cache. Only when the visible bytes exceed budget does it trim more,
// oldest first, down to low; a low well below budget means that happens
// rarely.
//
// It returns the trimmed view, how many results are trimmed out of it, how
// many were newly trimmed, and the byte totals either side.
func trimToolResultsWithSizes(messages []agent.Message, results []toolResultSize, trimmed map[int]bool, total, budget, low int) (out []agent.Message, count, fresh int, before, after int) {
	budget, low = max(budget, 0), max(min(low, budget), 0)
	before = total

	visible := total
	for _, size := range results {
		if trimmed[size.index] {
			visible -= size.bytes - len(trimMarker(messages[size.index].(*agent.ToolResultMessage), size))
		}
	}
	if visible > budget {
		for i, size := range results {
			if visible <= low || i == len(results)-1 {
				break
			}
			if trimmed[size.index] || size.bytes == 0 {
				continue
			}
			marker := trimMarker(messages[size.index].(*agent.ToolResultMessage), size)
			if len(marker) >= size.bytes {
				continue
			}
			trimmed[size.index] = true
			visible -= size.bytes - len(marker)
			fresh++
		}
	}

	for _, size := range results {
		if !trimmed[size.index] {
			continue
		}
		if out == nil {
			out = append([]agent.Message(nil), messages...)
		}
		result := messages[size.index].(*agent.ToolResultMessage)
		copy := *result
		copy.Content = []agent.Content{&agent.TextContent{Text: trimMarker(result, size)}}
		out[size.index] = &copy
		count++
	}
	if count == 0 {
		return messages, 0, 0, before, before
	}
	return out, count, fresh, before, visible
}

// trimMarker is what replaces a trimmed result. This is a trim, not a
// compaction: nothing was summarized, the output was removed after it stopped
// being useful. Saying "compacted" here would describe the lossy conversation
// handover that /compact writes.
//
// The marker carries the call's facts, so what the model can no longer see is
// exactly what it is told about. It sits where the output was, and is as
// sticky as the trim, so it never moves the request prefix on its own.
func trimMarker(result *agent.ToolResultMessage, size toolResultSize) string {
	kind := "output"
	if result.IsError {
		kind = "error output"
	}
	if size.fact == "" {
		return fmt.Sprintf("[trimmed %s tool %s: %d bytes]", result.ToolName, kind, size.bytes)
	}
	return fmt.Sprintf("[trimmed %s tool %s: %d bytes; %s]", result.ToolName, kind, size.bytes, size.fact)
}

func toolResultPayloadBytes(result *agent.ToolResultMessage) int {
	n := 0
	for _, content := range result.Content {
		switch block := content.(type) {
		case *agent.TextContent:
			n += len(block.Text) + len(block.TextSignature)
		case *agent.ThinkingContent:
			n += len(block.Thinking) + len(block.ThinkingSignature)
		case *agent.ImageContent:
			n += len(block.Data) + len(block.MimeType)
		}
	}
	return n
}

// trimFact is what a trim marker keeps about a call: how it ended, read from
// the result alone. The call's own arguments (its path, its command) are not
// repeated: trimming keeps call/result pairs intact, so they are already in
// the request beside the marker, and repeating them made every marker long
// enough that the markers themselves crowded the ceiling. It is empty when the
// result's outcome says nothing the call does not. Error lines are quoted, so
// nothing from a tool reads as anything but data.
func trimFact(result *agent.ToolResultMessage) string {
	details, _ := result.Details.(map[string]any)
	var fact string
	switch {
	case details["timed_out"] == true:
		fact = "timed out"
	case details["cancelled"] == true:
		fact = "cancelled"
	case details["exit_code"] != nil:
		fact = fmt.Sprintf("exit %v", details["exit_code"])
	case details["first_changed_line"] != nil && !result.IsError:
		fact = fmt.Sprintf("first changed line %v", details["first_changed_line"])
	}
	if result.IsError {
		if text := firstLine(result.Text()); text != "" {
			if fact != "" {
				fact += ": "
			}
			fact += quoteTrimFact(text)
		}
	}
	return fact
}

func quoteTrimFact(value string) string {
	if len(value) > maxTrimFactFieldBytes {
		value = truncateUTF8(value, maxTrimFactFieldBytes)
	}
	return strconv.Quote(value)
}

func truncateUTF8(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	var b strings.Builder
	for _, r := range value {
		if b.Len()+len(string(r))+len("…") > maxBytes {
			break
		}
		b.WriteRune(r)
	}
	b.WriteString("…")
	return b.String()
}

func firstLine(value string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(value), "\n")
	return line
}
