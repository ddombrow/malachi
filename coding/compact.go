package coding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/agent/session"
)

// Summarisation replaces the conversation prefix with a summary written by the
// model, and persists that summary so a resumed session does not replay the
// whole thing again.
//
// Two things make this different from the automatic tool-output trim in
// context.go. The trim is deterministic, cheap, and keeps everything; this
// costs a model call and cannot be undone by replay, which is why the
// validation gate and the full-replay escape hatch are not optional.
//
// Port of tau's /compact: CompactionPlan, _generate_compaction_summary and
// the CompactionStart/CompactionEnd events.

// compactProgressInterval throttles the received-bytes readout. Fast enough to
// look alive, slow enough that a chatty stream cannot flood the TUI's one-slot
// phase channel. It is a variable only so a test can watch the unthrottled
// stream of updates.
var compactProgressInterval = 500 * time.Millisecond

const (
	// compactKeepMessages is how much of the tail is retained verbatim. Enough
	// for the current exchange to be coherent, since a summary describes the
	// past and the tail is what the agent is in the middle of.
	compactKeepMessages = 20
	// compactMinKeepMessages is the least a turn boundary may leave of that
	// tail. A boundary that would keep fewer is not used: see compactBoundary.
	compactMinKeepMessages = compactKeepMessages / 2

	// minSummaryBytes rejects an empty or truncated summary. A summary shorter
	// than this has almost certainly lost the conversation.
	minSummaryBytes = 240
	// minSummarySections is how many of the requested headings must appear.
	// Below this the model ignored the structure and wrote prose instead.
	minSummarySections = 2
	// minKeptIdentifierSurvival is the fraction of the dropped span's
	// distinctive identifiers that must appear in the summary. Paths, symbol
	// names and error strings are what a summariser loses first, and they are
	// what an agent needs to keep working.
	minKeptIdentifierSurvival = 0.25

	// maxToolActivityBytes bounds one tool line in the summariser's input. A
	// heredoc or a long pipeline is cut; the line is for orientation, not
	// replay.
	maxToolActivityBytes = 240
	// maxFilesChanged bounds the files-changed record carried in a summary.
	// The most recently changed are kept, since they are the likeliest to
	// matter to the work in progress.
	maxFilesChanged = 60
)

// ErrNothingToSummarize is returned when the conversation is too short or too
// recent to be worth replacing.
var ErrNothingToSummarize = errors.New("not enough conversation to summarize")

// SummarizeResult reports what a summarisation did. Its JSON is the shape
// tau's RPC compact returns, plus malachi's own counts.
type SummarizeResult struct {
	Summary      string `json:"summary"`      // the summary the model wrote, as it will be replayed
	TokensBefore int    `json:"tokensBefore"` // estimated prompt size before the replacement
	// TokensAfter is the estimated prompt size once the summary replaced the
	// prefix.
	TokensAfter int         `json:"estimatedTokensAfter"`
	Replaced    int         `json:"replaced"` // messages the summary stands in for
	Kept        int         `json:"kept"`     // messages retained verbatim
	Usage       agent.Usage `json:"usage"`
	FirstKeptID string      `json:"firstKeptEntryId"` // entry id of the retained tail's first message
	Warnings    []string    `json:"warnings,omitempty"`
}

// MarshalJSON adds the empty details object tau's wire shape carries.
func (r SummarizeResult) MarshalJSON() ([]byte, error) {
	type plain SummarizeResult
	return json.Marshal(struct {
		plain
		Details map[string]any `json:"details"`
	}{plain(r), map[string]any{}})
}

// summarizationSystem asks for a structured handover rather than a reply. The
// instruction against continuing the conversation matters: the same model that
// wrote the code is being asked to describe it, and without it some providers
// simply answer.
const summarizationSystem = `You are compacting a coding session's history so that work can continue after it is replaced.

Do not continue the conversation, do not answer any request in it, and do not
add commentary. Output only the handover document, using these headings:

## Goal
What the user is ultimately trying to achieve.

## Constraints & Preferences
Rules the user stated or that were inferred: libraries, styles, things to
avoid, commands to run, interfaces that must not change.

## Progress
Done, in progress, and blocked, with enough detail to pick the work up.

## Key Decisions
Choices that were made and why, including options that were rejected. These
are the easiest thing to lose and the most expensive to rediscover.

## Next Steps
The concrete next actions, in order.

## Critical Context
Exact file paths, function and type names, error messages and commands.
Reproduce these literally; never paraphrase an identifier or a path.

Tool calls appear in the history as "tool" entries: what was run and how it
ended, without its output. Use them to ground Progress, and keep the commands
and paths that still matter in Critical Context. A list of the files that were
changed is appended to this document automatically; do not write one.`

// summarizationBody frames the conversation for the summariser.
const summarizationBody = `Below is a coding session's history, oldest first, as JSON. Write the handover document described in your instructions.

Tool calls are included as one-line "tool" entries; their output is not.

%s

%sWrite the handover document now.`

// Summarize replaces everything but the last compactKeepMessages of the
// conversation with a summary written by the model, and persists it.
//
// phases, when non-nil, is called as the work moves between steps so the caller
// can show progress: "reading", "summarizing", "validating", "writing".
func (s *Session) Summarize(ctx context.Context, instructions string, phases func(string)) (*SummarizeResult, error) {
	report := func(phase string) {
		if phases != nil {
			phases(phase)
		}
	}
	messages := s.Harness.Messages()
	keepFrom, err := compactBoundary(messages)
	if err != nil {
		return nil, err
	}
	prefix, tail := messages[:keepFrom], messages[keepFrom:]
	// The one phase with a real denominator: the work is known before it
	// starts, so say what is being folded rather than just naming the step.
	report(fmt.Sprintf("reading %d messages", len(prefix)))
	previous := ""
	if len(messages) > 0 {
		// A second compaction folds the previous summary in rather than
		// starting from scratch, so summaries compound instead of regressing.
		if c, ok := messages[0].(*agent.CompactionSummaryMessage); ok {
			previous = c.Summary
		}
	}
	// The files-changed record is exact and is carried forward by merging, not
	// by handing it to the model to rewrite.
	previous, previousFiles := splitFilesChanged(previous)
	files := mergeFiles(previousFiles, filesChanged(prefix, s.cwd))

	stats := s.ctxSampler.get()
	body := fmt.Sprintf(summarizationBody, previousSection(previous), transcriptJSON(prefix))

	// A summary of a long conversation takes minutes, and the phases either
	// side of it are instantaneous. Reporting the bytes received so far gives
	// the only honest answer to "is it stuck?": a number that stops moving is
	// a model that stopped talking, and a number that moves is not. It is not
	// a percentage, because a summary has no length it must reach.
	var received, lastReport int
	var reportedAt time.Time
	progress := func(n int) {
		received = n
		now := time.Now()
		if n != lastReport && (lastReport == 0 || now.Sub(reportedAt) >= compactProgressInterval) {
			lastReport, reportedAt = n, now
			report(fmt.Sprintf("summarizing %s", byteCount(received)))
		}
	}

	// One consistent view of what the session talks to: a /model switch
	// mid-summary must not pair one model's name with another's endpoint.
	s.mu.Lock()
	runtime, model, providerName := s.runtime, s.model, s.provider.Name
	s.mu.Unlock()

	report("summarizing")
	summary, usage, stop, err := s.summarizeOnce(ctx, runtime, model, body, instructions, previous != "", progress)
	if err != nil {
		return nil, err
	}

	report("validating")
	warnings := validateSummary(summary, prefix)
	if summary == "" {
		return nil, errors.New("compact: the model returned an empty summary; nothing was changed")
	}
	if stop == agent.StopLength {
		// The headings are written in order, so what a cut-off summary lacks
		// is its end: the next steps and the exact names.
		warnings = append(warnings, "the summary hit the output token limit and is cut off; its last sections may be missing")
	}
	summary += renderFilesChanged(files)

	// Last point at which cancelling leaves the session untouched: past
	// here the entry is written and the transcript replaced.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	report("writing")
	result := &SummarizeResult{
		Summary:      summary,
		TokensBefore: int(stats.EffectivePrompt()),
		Replaced:     len(prefix),
		Kept:         len(tail),
		Usage:        usage,
		Warnings:     warnings,
	}
	// The entry goes down before the in-memory transcript is replaced, so a
	// crash between the two leaves a session that replays the full history
	// rather than one whose tail has vanished.
	if s.file != nil {
		result.FirstKeptID = s.entryIDFor(tail[0])
		// Replay keeps only what follows the compaction unless the entry names
		// where the tail starts, so writing one without that would silently
		// drop the tail on the next resume. Refuse instead; nothing has changed.
		if result.FirstKeptID == "" {
			return nil, errors.New("compact: the retained messages are not in the session file yet; nothing was changed")
		}
		if !s.append(session.NewCompactionEntry(summary, result.FirstKeptID,
			result.TokensBefore, usage, providerName, model)) {
			return nil, errors.New("compact: could not write the compaction to the session file; nothing was changed")
		}
	}
	note := &agent.CompactionSummaryMessage{
		Summary:      summary,
		TokensBefore: int64(result.TokensBefore),
		Timestamp:    time.Now().UnixMilli(),
	}
	s.Harness.ReplaceMessages(append([]agent.Message{note}, tail...))
	// The transcript the preparer measured is gone, so its caches and its
	// reported figures no longer describe anything.
	s.preparer.reset()
	s.trims.reset()
	// The gauge would otherwise keep showing the size of a conversation that no
	// longer exists, right up until the next reply.
	s.ctxSampler.compacted()
	s.estimateContext()
	result.TokensAfter = int(s.ctxSampler.get().EffectivePrompt())
	s.diag.LogCompaction(summary, result.Replaced, result.Kept, usage)
	return result, nil
}

// summarizeOnce runs the summarisation request and returns the text and why
// the model stopped writing it. progress, when non-nil, is called with the
// running count of characters the model has produced so far.
func (s *Session) summarizeOnce(ctx context.Context, provider agent.Provider, model, body, instructions string, folding bool, progress func(int)) (string, agent.Usage, agent.StopReason, error) {
	if instructions != "" {
		body = "Pay particular attention to: " + instructions + "\n\n" + body
	}
	if folding {
		body = "A previous handover document is included above. Carry it forward: keep what still holds, correct what has changed, and do not simply repeat it.\n\n" + body
	}
	req := agent.Request{
		Model:  model,
		System: summarizationSystem,
		// The same routing and prompt-cache hint the loop sends. A gateway
		// that requires the session header rejects the request without it,
		// and the summariser would be the only call in the agent that omits it.
		SessionID:     s.sessionID,
		Messages:      []agent.Message{agent.NewUserText(body)},
		ThinkingLevel: "off", // a handover document is not a reasoning task
	}
	var out strings.Builder
	var usage agent.Usage
	var stop agent.StopReason
	for ev := range provider.Stream(ctx, req) {
		switch e := ev.(type) {
		case *agent.TextDelta:
			out.WriteString(e.Delta)
			if progress != nil {
				progress(out.Len())
			}
		case *agent.AssistantDone:
			usage, stop = e.Message.Usage, e.Message.StopReason
		case *agent.AssistantError:
			msg := "the provider failed"
			if e.Error != nil && e.Error.ErrorMessage != "" {
				msg = e.Error.ErrorMessage
			}
			return "", usage, stop, fmt.Errorf("compact: %s", msg)
		}
	}
	return strings.TrimSpace(out.String()), usage, stop, nil
}

// compactBoundary picks where the retained tail begins: far enough back that
// there is something to summarize, at the earliest user message inside the
// last compactKeepMessages so as much of the recent conversation as possible
// stays verbatim from a turn boundary, and never on a tool result, whose call
// would be summarized away and leave the result orphaned.
//
// A single long agentic turn (one user message, then many tool calls) has no
// user message in the window; the tail then starts at the nearest assistant
// message, which opens with the calls its results belong to. The user's
// request for that turn is in the summarized prefix.
//
// The same goes for a user message that arrives late in the window, such as
// a prompt that overflowed the context or a steer near the end of a run.
// Starting the tail there would keep little but that message, and what it
// answers ("yes, do the second one") would survive only as summary. A turn
// boundary is used only if it keeps at least compactMinKeepMessages.
func compactBoundary(messages []agent.Message) (int, error) {
	if len(messages) <= compactKeepMessages {
		return 0, ErrNothingToSummarize
	}
	keepFrom := len(messages) - compactKeepMessages
	for i := keepFrom; i <= len(messages)-compactMinKeepMessages; i++ {
		if _, ok := messages[i].(*agent.UserMessage); ok {
			keepFrom = i
			break
		}
	}
	// Step back over tool results so the tail opens with their call.
	for keepFrom > 0 {
		if _, ok := messages[keepFrom].(*agent.ToolResultMessage); !ok {
			break
		}
		keepFrom--
	}
	if keepFrom <= 1 {
		return 0, ErrNothingToSummarize
	}
	return keepFrom, nil
}

// transcriptJSON renders the conversation for the summariser: what the user
// asked, what the assistant said, and one line per tool call saying what was
// run and how it ended. Tool output is omitted: it is bulky, and the outcome is
// what a handover needs. Without the tool lines the summariser cannot tell what
// was actually done from what was only discussed.
func transcriptJSON(messages []agent.Message) string {
	var b strings.Builder
	b.WriteString("```json\n[")
	for i, e := range summarizerView(messages) {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, "\n{\"role\": %s, \"text\": %s}", quoteJSON(e.role), quoteJSON(e.text))
	}
	b.WriteString("\n]\n```")
	return b.String()
}

// summarizerEntry is one message as the summariser is shown it.
type summarizerEntry struct{ role, text string }

// summarizerView is the conversation as the summariser sees it. It is also
// what a summary is checked against: a summary cannot be faulted for losing
// something its author was never shown.
func summarizerView(messages []agent.Message) []summarizerEntry {
	var out []summarizerEntry
	calls := map[string]*agent.ToolCall{}
	for _, m := range messages {
		var e summarizerEntry
		switch m := m.(type) {
		case *agent.UserMessage:
			e = summarizerEntry{"user", agent.MessageText(m)}
		case *agent.AssistantMessage:
			e = summarizerEntry{"assistant", agent.MessageText(m)}
			for _, call := range m.ToolCalls() {
				calls[call.ID] = call
			}
		case *agent.ToolResultMessage:
			e = summarizerEntry{"tool", toolActivity(m, calls[m.ToolCallID])}
		default:
			// A previous summary, which is passed on separately as the
			// previous handover.
			continue
		}
		if strings.TrimSpace(e.text) != "" {
			out = append(out, e)
		}
	}
	return out
}

// quoteJSON encodes s as a JSON string. HTML escaping is off: the reader is a
// model, and code is full of <, > and & that \u003c would only obscure.
func quoteJSON(s string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return `""` // a string always encodes; this is unreachable
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// toolActivity is one tool call as the summariser sees it: the call in its
// short form, and how it ended.
func toolActivity(result *agent.ToolResultMessage, call *agent.ToolCall) string {
	line := result.ToolName
	if call != nil {
		line = SummarizeToolCall(call.Name, call.Arguments)
	}
	if fact := trimFact(result); fact != "" {
		line += " → " + fact
	} else if result.IsError {
		line += " → error"
	}
	return truncateUTF8(line, maxToolActivityBytes)
}

// filesChangedHeading opens the record of changed files appended to a summary.
// It is matched exactly when a later compaction carries the record forward.
const filesChangedHeading = "\n\n## Files changed\n\nRecorded from tool calls, not written by the summariser.\n"

// filesChanged lists the files that successful edit and write calls changed,
// relative to cwd where they are inside it, in the order they were last
// changed.
func filesChanged(messages []agent.Message, cwd string) []string {
	calls := map[string]*agent.ToolCall{}
	var files []string
	for _, m := range messages {
		switch m := m.(type) {
		case *agent.AssistantMessage:
			for _, call := range m.ToolCalls() {
				calls[call.ID] = call
			}
		case *agent.ToolResultMessage:
			call := calls[m.ToolCallID]
			if m.IsError || call == nil || (call.Name != "edit" && call.Name != "write") {
				continue
			}
			path, _ := call.Arguments["path"].(string)
			if path == "" {
				continue
			}
			files = mergeFiles(files, []string{relativeTo(cwd, path)})
		}
	}
	return files
}

func relativeTo(cwd, path string) string {
	if cwd == "" {
		return path
	}
	abs := resolvePath(cwd, path)
	if rel, err := filepath.Rel(filepath.Clean(cwd), abs); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return rel
	}
	return abs
}

// mergeFiles appends newer to older, moving a file that appears again to the
// end so the order stays most-recently-changed last, and keeps the newest
// maxFilesChanged.
func mergeFiles(older, newer []string) []string {
	out := make([]string, 0, len(older)+len(newer))
	seen := map[string]bool{}
	all := append(append([]string(nil), older...), newer...)
	for i := len(all) - 1; i >= 0; i-- {
		if f := all[i]; !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	if len(out) > maxFilesChanged {
		out = out[:maxFilesChanged]
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func renderFilesChanged(files []string) string {
	if len(files) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(filesChangedHeading)
	for _, f := range files {
		b.WriteString("\n- " + f)
	}
	return b.String()
}

// splitFilesChanged separates a summary written by Summarize into the model's
// handover and the files-changed record appended to it.
func splitFilesChanged(summary string) (string, []string) {
	head, record, ok := strings.Cut(summary, filesChangedHeading)
	if !ok {
		return summary, nil
	}
	var files []string
	for _, line := range strings.Split(record, "\n") {
		if f, ok := strings.CutPrefix(line, "- "); ok && f != "" {
			files = append(files, f)
		}
	}
	return head, files
}

func previousSection(summary string) string {
	if summary == "" {
		return ""
	}
	return "## Previous handover\n\n" + summary + "\n\n"
}

// summarySections are the headings the system prompt asks for.
var summarySections = []string{"goal", "constraints", "progress", "decision", "next steps", "critical context"}

// identifierish finds the tokens a summariser loses first: paths, qualified
// names, and quoted strings that look like errors.
var identifierish = regexp.MustCompile(`[\w./-]+\.(?:go|ts|tsx|js|py|md|json|ya?ml|mod|sum|lock|toml)|` + "`[^`]{2,}`" + `|\b[A-Za-z_][A-Za-z0-9_]*\.[A-Z][A-Za-z0-9_]*\b`)

// validateSummary checks that a summary is plausibly a handover document and
// reports what it dropped. It does not rewrite or reject on identifiers alone:
// the warning is surfaced so the user can see what the model left out, which
// is more useful than a silent refusal.
func validateSummary(summary string, dropped []agent.Message) []string {
	var warnings []string
	body := strings.ToLower(summary)
	if len(summary) < minSummaryBytes {
		warnings = append(warnings, fmt.Sprintf("summary is only %d bytes; it may have lost the conversation", len(summary)))
	}
	found := 0
	for _, h := range summarySections {
		if strings.Contains(body, h) {
			found++
		}
	}
	if found < minSummarySections {
		warnings = append(warnings, fmt.Sprintf("only %d of %d expected sections present; the structure may be missing", found, len(summarySections)))
	}
	identifiers := distinctiveIdentifiers(dropped)
	if len(identifiers) > 0 {
		kept := 0
		for _, id := range identifiers {
			if strings.Contains(summary, id) {
				kept++
			}
		}
		if float64(kept)/float64(len(identifiers)) < minKeptIdentifierSurvival {
			warnings = append(warnings, fmt.Sprintf("only %d of %d distinctive identifiers survived the summary", kept, len(identifiers)))
		}
	}
	return warnings
}

// distinctiveIdentifiers collects the tokens worth checking for, capped so a
// long conversation does not turn this into a scan of everything. They come
// from what the summariser was shown, plus the previous handover it was asked
// to carry forward. Tool output is not shown to it, so its identifiers are not
// expected back: counting them made one file read fill the sample with names
// no summary could contain, and nearly every compaction warn.
func distinctiveIdentifiers(messages []agent.Message) []string {
	var texts []string
	if len(messages) > 0 {
		if c, ok := messages[0].(*agent.CompactionSummaryMessage); ok {
			previous, _ := splitFilesChanged(c.Summary)
			texts = append(texts, previous)
		}
	}
	for _, e := range summarizerView(messages) {
		texts = append(texts, e.text)
	}
	seen := map[string]bool{}
	var out []string
	for _, text := range texts {
		for _, match := range identifierish.FindAllString(text, -1) {
			id := strings.Trim(match, "`")
			if len(id) < 4 || seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, id)
			if len(out) >= 60 {
				return out
			}
		}
	}
	return out
}
