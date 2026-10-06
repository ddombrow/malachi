package tui

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/agent/session"
	"github.com/ddombrow/malachi/coding"
)

const helpText = `Commands:
  /model [ref]        show models, or switch (e.g. /model glm-5.2, /model openai/gpt-5.1)
  /thinking [level]   show or set the reasoning level
  /compact [note]    summarize the conversation with the agent
  /trim [bytes]      trim old tool output before the next request
  /trust [yes|no]    load this directory's AGENTS.md into the prompt
  /trust parent    do the same for every directory beneath the parent
  /sandbox            show what commands and file tools may touch
  /ctx               measured context: tokens, tool output share, trim passes
  /new                start a fresh session
  /resume [n|name]    list recent sessions, or resume one
  /copy               copy the latest assistant response
  /last               print the full output of the last tool call
  /session            show the session file path
  /quit               exit

Keys: enter send · alt+enter newline · esc cancel run · ctrl+c clear/quit
Scroll: mouse wheel · pgup/pgdown · shift+↑/↓ · ctrl+home/ctrl+end
Copy: drag over the transcript to select; releasing copies it.
Hold shift while dragging to use the terminal's own selection instead.
While the agent runs, enter queues a steering message.`

func (m *model) printDim(s string) tea.Cmd {
	return m.print(func(r *renderer) string { return item(perLine(r.st.dim, s)) })
}

func (m *model) printErr(err error) tea.Cmd {
	msg := err.Error()
	return m.print(func(r *renderer) string {
		return item(r.gutter(iconError, r.st.errorText, r.st.errorText.Render(msg)))
	})
}

func (m *model) command(line string) tea.Cmd {
	name, arg, _ := strings.Cut(strings.TrimPrefix(line, "/"), " ")
	arg = strings.TrimSpace(arg)
	switch name {
	case "help", "?":
		return m.printDim(helpText)
	case "quit", "exit", "q":
		if m.busy() {
			m.quitting = true
			m.s.Abort()
			return nil
		}
		return tea.Quit
	case "session":
		if m.s.Path() == "" {
			return m.printDim("in-memory session (--no-session)")
		}
		return m.printDim("session: " + m.s.Path() + "\nlog:     " + m.s.Diagnostics().Path())
	case "copy":
		if strings.TrimSpace(m.lastReply) == "" {
			return m.printDim("no assistant response to copy yet")
		}
		return tea.Batch(m.copyText(m.lastReply, "copied latest response"), m.printDim("copied latest assistant response"))
	case "last":
		if m.last == nil {
			return m.printDim("no tool has run yet")
		}
		last := m.last
		return m.print(func(r *renderer) string {
			out := last.result.Text()
			if d, ok := last.result.Details.(map[string]any); ok {
				if patch, _ := d["patch"].(string); patch != "" {
					out = r.diff(patch, 1<<30)
				}
			}
			return item(r.st.accent.Render("── "+r.summary(last.name, last.args)+" ──") + "\n" + out)
		})
	case "model":
		return m.modelCommand(arg)
	case "thinking":
		if arg == "" {
			levels := strings.Join(m.s.Provider().ThinkingLevels, ", ")
			return m.printDim(fmt.Sprintf("thinking: %s (available: %s)", m.s.ThinkingLevel(), levels))
		}
		if err := m.s.SetThinkingLevel(arg); err != nil {
			return m.printErr(err)
		}
		return m.printDim("thinking level set to " + arg)
	case "trim":
		return m.trimCommand(arg)
	case "trust":
		return m.trustCommand(arg)
	case "sandbox":
		return m.printDim(m.s.Sandbox().Describe())
	case "compact":
		return m.compactCommand(arg)
	case "ctx":
		return m.printDim(coding.ContextLine(m.s.Model(), m.s.ContextStats()))
	case "new":
		return m.reopen("")
	case "resume":
		return m.resumeCommand(arg)
	}
	return m.printErr(fmt.Errorf("unknown command /%s (try /help)", name))
}

// compactCommand replaces the conversation prefix with a summary written by
// the model and persists it, so a resumed session does not replay the whole
// thing. The summary is printed into the transcript afterwards: seeing what
// the model now believes is the point of doing this by hand rather than by
// threshold.
// trustCommand decides whether this directory's instruction files are folded
// into the system prompt. With no argument it explains the current state, so
// the decision is never a guess about what /trust would do.
func (m *model) trustCommand(arg string) tea.Cmd {
	state := m.s.TrustState()
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "yes", "no":
		remember := true
		if state.Pending {
			// Nobody has vouched for this directory yet, so a bare /trust yes
			// is an answer to a question that was just asked: it applies to
			// this session. Saving it is an explicit /trust yes --save.
			remember = false
		}
		if err := m.s.Trust(trustDecisionOf(arg), remember); err != nil {
			return m.printErr(err)
		}
		note := "for this session"
		if remember {
			note = "remembered for this directory"
		}
		return m.printDim(trustOutcome(m.s.TrustState()) + " (" + note + ")")
	case "parent":
		if err := m.s.TrustParent(true); err != nil {
			return m.printErr(err)
		}
		return m.printDim(trustOutcome(m.s.TrustState()) + " (remembered for the parent directory)")
	case "":
		if state.Trusted() {
			return m.printDim(trustOutcome(state))
		}
		return m.printDim(state.TrustNotice() + "\n\n  /trust yes to load them · /trust parent if this is a package in a repo · /trust yes --save to remember · /trust no to decline")
	default:
		return m.printErr(fmt.Errorf("usage: /trust [yes [--save] | parent | no]"))
	}
}

func trustDecisionOf(arg string) coding.TrustDecision {
	if strings.EqualFold(strings.TrimSpace(arg), "yes") {
		return coding.TrustTrusted
	}
	return coding.TrustUntrusted
}

// trustOutcome is one line saying what the prompt now contains.
func trustOutcome(state coding.TrustState) string {
	inherited := ""
	if state.InheritedFrom != "" {
		inherited = " (inherited from " + shortenHome(state.InheritedFrom) + ")"
	}
	if state.Trusted() {
		if state.Resources.Empty() {
			return "no project instruction files here; nothing to load"
		}
		return fmt.Sprintf("loading %d project instruction file(s) for %s%s",
			state.Resources.Total, shortenHome(state.Path), inherited)
	}
	return fmt.Sprintf("not loading %d project instruction file(s) for %s%s",
		state.Resources.Total, shortenHome(state.Path), inherited)
}

// showTrustNotice announces withheld project instructions once, at startup.
// Silently ignoring a repository's AGENTS.md reads as a bug in the agent
// rather than a decision the user has not made yet.
func (m *model) showTrustNotice() {
	if notice := m.s.TrustState().TrustNotice(); notice != "" {
		m.tr.add(func(r *renderer) string {
			return item(r.st.toolErr.Render(notice))
		})
	}
}

// showSandboxNotice says so at startup when the sandbox protects nothing:
// turned off, or unable to confine commands here.
func (m *model) showSandboxNotice() {
	if notice := m.s.Sandbox().Notice(); notice != "" {
		m.tr.add(func(r *renderer) string {
			return item(r.st.toolErr.Render(notice + " (/sandbox for details)"))
		})
	}
}

func (m *model) compactCommand(arg string) tea.Cmd {
	// Refuse here rather than only in the goroutine, so the message names the
	// situation in the TUI's own words.
	if st := m.s.State(); st.Running {
		return m.printErr(errors.New("compact cannot run while the agent is working"))
	} else if st.Compacting {
		return m.printErr(errors.New("a compaction is already running"))
	}
	// Progress, the summary and failures all arrive as session events; only a
	// refusal that raced the check above is left to report from here.
	s := m.s
	return func() tea.Msg {
		if _, err := s.Compact(context.Background(), arg); errors.Is(err, coding.ErrBusy) {
			return compactErrMsg{err}
		}
		return nil
	}
}

// compactErrMsg reports a compaction that could not start.
type compactErrMsg struct{ err error }

// compactionHead is the progress line shown before the model has produced
// anything, by why the compaction is happening.
func compactionHead(reason coding.CompactionReason) string {
	switch reason {
	case coding.CompactionThreshold:
		return "auto · compacting to fit the context window"
	case coding.CompactionOverflow:
		return "auto · compacting after the provider rejected the request for size"
	}
	return "compact"
}

// compactionEnded reports how a compaction finished. An aborted one hands
// back any prompt that was waiting for it, rather than sending or dropping it.
func (m *model) compactionEnded(ev coding.CompactionEndEvent) tea.Cmd {
	m.clearPhase()
	// The transcript the gauge measured has been replaced.
	if st := m.s.ContextStats(); st.PromptEstimated() {
		m.context, m.contextEstimated = st.EffectivePrompt(), true
	}
	var cmds []tea.Cmd
	switch {
	case ev.Aborted:
		if ev.HeldPrompt != "" && strings.TrimSpace(m.input.Value()) == "" {
			m.input.SetValue(ev.HeldPrompt)
			cmds = append(cmds, m.printDim("compaction cancelled — your prompt is back in the input"))
		} else {
			cmds = append(cmds, m.printDim("compaction cancelled"))
		}
	case ev.ErrorMessage != "":
		cmds = append(cmds, m.printErr(errors.New(ev.ErrorMessage)))
	case ev.Result != nil:
		m.compactSummary(ev.Result)
		switch ev.Reason {
		case coding.CompactionThreshold:
			cmds = append(cmds, m.printDim("compacted automatically to fit the context window"))
		case coding.CompactionOverflow:
			cmds = append(cmds, m.printDim("compacted after the provider rejected the request for size; retrying"))
		}
	}
	if m.quitting && !m.busy() {
		cmds = append(cmds, tea.Quit)
	}
	return tea.Sequence(cmds...)
}

// compactTickMsg repaints the compaction line while a summarisation runs.
type compactTickMsg struct{}

// compactTick is a single self-rearming ticker, started with the compaction and
// stopped by the next tick after it ends. It exists so the line moves even when
// the model is silent: a counter frozen for a minute and a frozen frame look
// identical without it.
func compactTick() tea.Cmd {
	return func() tea.Msg {
		time.Sleep(250 * time.Millisecond)
		return compactTickMsg{}
	}
}

// compactCommand writes the summary into the transcript.
func (m *model) compactSummary(res *coding.SummarizeResult) {
	line := fmt.Sprintf("compacted %d messages into a summary · kept %d · %s",
		res.Replaced, res.Kept, tokens(int64(res.Usage.PromptTokens())))
	m.tr.add(func(r *renderer) string { return r.gutter(iconCompacted, r.st.dim, r.st.dim.Render(line)) })
	m.tr.add(func(r *renderer) string {
		return r.gutter(iconCompacted, r.st.dim, r.markdown(strings.TrimSpace(res.Summary)))
	})
	for _, w := range res.Warnings {
		m.tr.add(func(r *renderer) string { return r.gutter(iconError, r.st.toolErr, r.st.toolErr.Render(w)) })
	}
}

// trimCommand trims tool output ahead of the next provider request. With no
// argument it trims as far as it can; a byte budget trims to that ceiling
// instead. The transcript is never rewritten, so the marker line is the only
// record of what the model stopped seeing.
//
// /compact is reserved for the agent-written summary, which is what the word
// means everywhere else; this is the mechanical half of it.
func (m *model) trimCommand(arg string) tea.Cmd {
	budget := 0
	if arg != "" {
		n, err := strconv.Atoi(arg)
		if err != nil || n < 0 {
			return m.printErr(fmt.Errorf("usage: /trim [bytes]"))
		}
		budget = n
	}
	if !m.s.ForceTrim(budget) {
		c := m.s.Trim()
		return m.printDim(fmt.Sprintf("nothing to trim: %s of tool output, already within the limit",
			bytesHuman(c.Before)))
	}
	if budget == 0 {
		return m.printDim("trimming tool output before the next request (as far as possible)")
	}
	return m.printDim(fmt.Sprintf("trimming tool output above %s before the next request", bytesHuman(budget)))
}

func (m *model) modelCommand(arg string) tea.Cmd {
	if arg == "" {
		// Fetch off the UI goroutine; the result is printed when it arrives.
		s := m.s
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			ids, err := s.Models(ctx)
			var b strings.Builder
			fmt.Fprintf(&b, "current: %s/%s\n", s.Provider().Name, s.Model())
			if err != nil {
				fmt.Fprintf(&b, "(could not fetch live model list: %v; showing built-in list)\n", err)
			}
			text := strings.TrimRight(b.String(), "\n")
			return printMsg{func(r *renderer) string {
				t := text
				if len(ids) > 0 {
					t += fmt.Sprintf("\n%s models (%d):\n%s", s.Provider().Name, len(ids), columns(ids, r.width-4))
				}
				return item(perLine(r.st.dim, t))
			}}
		}
	}
	if err := m.s.SetModel(arg); err != nil {
		return m.printErr(err)
	}
	return m.printDim(fmt.Sprintf("model set to %s/%s (thinking %s)", m.s.Provider().Name, m.s.Model(), m.s.ThinkingLevel()))
}

func (m *model) resumeCommand(arg string) tea.Cmd {
	infos, err := session.List(m.s.SessionsDir())
	if err != nil {
		return m.printErr(err)
	}
	if arg == "" {
		if len(infos) == 0 {
			return m.printDim("no saved sessions for this directory")
		}
		var b strings.Builder
		b.WriteString("Recent sessions (resume with /resume <n>):\n")
		for i, in := range infos[:min(len(infos), 15)] {
			preview := in.Title
			if preview == "" {
				preview = strings.ReplaceAll(in.Preview, "\n", " ")
			}
			current := ""
			if in.Path == m.s.Path() {
				current = " (current)"
			}
			fmt.Fprintf(&b, "  %2d. %s  %3d msgs  %s%s\n", i+1, in.Modified.Format("Jan 02 15:04"), in.Messages, truncateWidth(preview, 60), current)
		}
		return m.printDim(strings.TrimRight(b.String(), "\n"))
	}
	ref := arg
	if n, err := strconv.Atoi(arg); err == nil {
		if n < 1 || n > len(infos) {
			return m.printErr(fmt.Errorf("no session #%d", n))
		}
		ref = infos[n-1].Path
	}
	return m.reopen(ref)
}

// reopen swaps in a fresh (resume == "") or resumed session.
func (m *model) reopen(resume string) tea.Cmd {
	next, err := m.s.Reopen(resume)
	if err != nil {
		return m.printErr(err)
	}
	m.bridge.close()
	m.s = next
	m.bridge = newBridge(next)
	m.usage, m.context, m.last = agent.Usage{}, 0, nil
	if st := next.ContextStats(); st.PromptEstimated() {
		m.context, m.contextEstimated = st.EffectivePrompt(), true
	} else {
		m.contextEstimated = false
	}
	m.toolArgs = map[string]map[string]any{}

	m.showSession(next)
	m.vp.GotoBottom()
	return m.bridge.next()
}

// printMsg asks the UI loop to append a block to the transcript; background
// commands return it because only the UI goroutine may touch the model.
type printMsg struct{ render func(r *renderer) string }

// columns lays out ids in aligned columns within width.
func columns(ids []string, width int) string {
	colW := 0
	for _, id := range ids {
		colW = max(colW, len(id)+2)
	}
	perRow := max(1, width/max(colW, 1))
	var b strings.Builder
	for i, id := range ids {
		b.WriteString("  ")
		b.WriteString(id)
		if (i+1)%perRow == 0 || i == len(ids)-1 {
			b.WriteByte('\n')
		} else {
			b.WriteString(strings.Repeat(" ", colW-len(id)))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
