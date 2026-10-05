package coding

import (
	"context"
	"errors"
	"fmt"
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
)

// ErrNothingToSummarize is returned when the conversation is too short or too
// recent to be worth replacing.
var ErrNothingToSummarize = errors.New("not enough conversation to summarize")

// SummarizeResult reports what a summarisation did.
type SummarizeResult struct {
	Summary      string // the summary the model wrote, as it will be replayed
	TokensBefore int    // estimated prompt size before the replacement
	Replaced     int    // messages the summary stands in for
	Kept         int    // messages retained verbatim
	Usage        agent.Usage
	FirstKeptID  string // entry id of the retained tail's first message
	Warnings     []string
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

Factual tool activity (commands run, files touched, exit codes) is recorded
separately and is not part of this document. Do not repeat it.`

// summarizationBody frames the conversation for the summariser.
const summarizationBody = `Below is a coding session's history, oldest first, as JSON. Write the handover document described in your instructions.

Facts about tool calls are recorded elsewhere and omitted here; only the user
and assistant text is included.

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

	report("summarizing")
	summary, usage, err := s.summarizeOnce(ctx, s.runtime, body, instructions, previous != "", progress)
	if err != nil {
		return nil, err
	}

	report("validating")
	warnings := validateSummary(summary, prefix)
	if summary == "" {
		return nil, errors.New("compact: the model returned an empty summary; nothing was changed")
	}

	report("writing")
	result := &SummarizeResult{
		Summary:      summary,
		TokensBefore: int(stats.PromptTokens),
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
			result.TokensBefore, usage, s.provider.Name, s.model)) {
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
	s.estimateContext()
	s.diag.LogCompaction(summary, result.Replaced, result.Kept, usage)
	return result, nil
}

// summarizeOnce runs the summarisation request and returns the text.
// progress, when non-nil, is called with the running count of characters the
// model has produced so far.
func (s *Session) summarizeOnce(ctx context.Context, provider agent.Provider, body, instructions string, folding bool, progress func(int)) (string, agent.Usage, error) {
	if instructions != "" {
		body = "Pay particular attention to: " + instructions + "\n\n" + body
	}
	if folding {
		body = "A previous handover document is included above. Carry it forward: keep what still holds, correct what has changed, and do not simply repeat it.\n\n" + body
	}
	req := agent.Request{
		Model:  s.model,
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
	for ev := range provider.Stream(ctx, req) {
		switch e := ev.(type) {
		case *agent.TextDelta:
			out.WriteString(e.Delta)
			if progress != nil {
				progress(out.Len())
			}
		case *agent.AssistantDone:
			usage = e.Message.Usage
		case *agent.AssistantError:
			msg := "the provider failed"
			if e.Error != nil && e.Error.ErrorMessage != "" {
				msg = e.Error.ErrorMessage
			}
			return "", usage, fmt.Errorf("compact: %s", msg)
		}
	}
	return strings.TrimSpace(out.String()), usage, nil
}

// compactBoundary picks where the retained tail begins: far enough back that
// there is something to summarize, and at a user message so the tail does not
// open with an orphaned tool result or half a turn.
func compactBoundary(messages []agent.Message) (int, error) {
	if len(messages) <= compactKeepMessages {
		return 0, ErrNothingToSummarize
	}
	keepFrom := len(messages) - compactKeepMessages
	for i := len(messages) - 1; i > 0; i-- {
		if _, ok := messages[i].(*agent.UserMessage); ok {
			if i <= keepFrom {
				break
			}
			keepFrom = i
			break
		}
	}
	if keepFrom <= 1 {
		return 0, ErrNothingToSummarize
	}
	return keepFrom, nil
}

// transcriptJSON renders the conversation for the summariser: what the user
// asked and what the assistant said. Tool calls and tool results are omitted:
// the ledger already carries that, and sending it again invites the model to
// restate it. An assistant turn that called tools still contributes its text,
// which is usually where the reasoning about those calls is.
func transcriptJSON(messages []agent.Message) string {
	var b strings.Builder
	b.WriteString("```json\n[")
	first := true
	for _, m := range messages {
		var role string
		switch m.(type) {
		case *agent.UserMessage:
			role = "user"
		case *agent.AssistantMessage:
			role = "assistant"
		default:
			// Tool results, and a previous summary, which is passed on
			// separately as the previous handover.
			continue
		}
		text := agent.MessageText(m)
		if strings.TrimSpace(text) == "" {
			continue
		}
		if !first {
			b.WriteString(",")
		}
		first = false
		fmt.Fprintf(&b, "\n{\"role\": %s, \"text\": %s}", quoteJSON(role), quoteJSON(text))
	}
	b.WriteString("\n]\n```")
	return b.String()
}

func quoteJSON(s string) string {
	return strings.ReplaceAll(`"`+strings.ReplaceAll(s, `"`, `\"`)+`"`, "\n", `\n`)
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
// long conversation does not turn this into a scan of everything.
func distinctiveIdentifiers(messages []agent.Message) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range messages {
		for _, match := range identifierish.FindAllString(agent.MessageText(m), -1) {
			id := strings.Trim(strings.Trim(match, "`"), "")
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
