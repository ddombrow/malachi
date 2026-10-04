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
  /ctx               measured context: tokens, tool output share, compactions
  /new                start a fresh session
  /resume [n|name]    list recent sessions, or resume one
  /copy               copy the latest assistant response
  /last               print the full output of the last tool call
  /session            show the session file path
  /quit               exit

Keys: enter send · alt+enter newline · esc cancel run · ctrl+c clear/quit
Scroll: mouse wheel · pgup/pgdown · shift+↑/↓ · ctrl+home/ctrl+end
Select text: hold shift (option in iTerm2/Terminal) while dragging.
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
		if m.running {
			m.quitting = true
			m.s.Harness.Cancel()
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
		return tea.Batch(tea.SetClipboard(m.lastReply), m.printDim("copied latest assistant response"))
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
func (m *model) compactCommand(arg string) tea.Cmd {
	if m.running {
		return m.printErr(errors.New("compact cannot run while the agent is working"))
	}
	if m.phase != "" {
		return m.printErr(errors.New("a compaction is already running"))
	}
	m.pendingPrompt = ""
	return tea.Batch(
		m.summarize("compact", func(ctx context.Context, phases func(string)) (*coding.SummarizeResult, error) {
			return m.s.Summarize(ctx, arg, phases)
		}),
		waitForPhase(m.phaseCh), compactTick())
}

// summarize runs a compaction, arming the phase channel and the ticker that
// keeps the progress line moving. head is shown until the model produces
// bytes of its own, so the first moments of a long wait are not a blank line.
// run is the compaction itself: the manual command and the automatic one
// summarize differently, and both show progress the same way.
func (m *model) summarize(head string, run func(context.Context, func(string)) (*coding.SummarizeResult, error)) tea.Cmd {
	phases := make(chan string, 8)
	// The run context belongs to the harness; a summarisation gets its own so esc can
	// stop it without touching an agent run.
	ctx, cancel := context.WithCancel(context.Background())
	m.compacting = true
	m.phase, m.phaseStart = head, time.Now()
	m.phaseCh, m.cancelPhase = phases, cancel
	return func() tea.Msg {
		defer close(phases)
		res, err := run(ctx, func(phase string) {
			select {
			case phases <- phase:
			case <-ctx.Done():
			}
		})
		if ctx.Err() != nil {
			return compactDoneMsg{err: context.Canceled}
		}
		return compactDoneMsg{result: res, err: err}
	}
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

// waitForPhase re-reads the summarisation's progress channel. Each return is
// re-armed by the handler, so one command reports every step.
func waitForPhase(ch <-chan string) tea.Cmd {
	return func() tea.Msg {
		phase, ok := <-ch
		if !ok {
			return nil
		}
		return compactPhaseMsg(phase)
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
	if !m.s.Compact(budget) {
		c := m.s.Compaction()
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
	if m.running {
		return m.printErr(fmt.Errorf("wait for the current run to finish (or esc) before switching models"))
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
	if m.running {
		return m.printErr(fmt.Errorf("wait for the current run to finish (or esc) first"))
	}
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
