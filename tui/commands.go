package tui

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/agent/session"
	"github.com/ddombrow/malachi/coding"
)

const helpText = `Commands:
  /model [ref]        pick a model, or switch (e.g. /model glm-5.2, /model openai/gpt-5.1)
  /thinking [level]   pick or set the reasoning level
  /compact [note]    summarize the conversation with the agent
  /trim [bytes]      trim old tool output before the next request
  /trust [yes|no] [--save] load this directory's AGENTS.md into the prompt
  /trust parent    do the same for every directory beneath the parent
  /sandbox            show what commands and file tools may touch
  /allow-read <dir>   allow reading a directory in your home for this session
  /allow-write <dir>  allow writes to a directory for this session
  /network [on|off]   show, or turn commands' network access on or off (ctrl+o)
  /mcp [server]       show MCP servers: status, roots, tools
  /mcp restart <name> restart an MCP server now
  /sidebar            show or hide the sidebar (ctrl+s)
  /ctx               measured context: tokens, tool output share, trim passes
  /codemode [mode]    pick or set the code mode: off, on, explore (ctrl+x cycles)
  /new, /clear        start a fresh session with an empty context
  /resume [name]      pick a recent session, or resume one
  /copy               copy the latest assistant response
  /last               print the full output of the last tool call
  /session            show the session file path
  /quit               exit

Keys: enter send · alt+enter newline · esc cancel run · ctrl+c clear/quit
ctrl+x cycle code mode: off → on → explore (read, grep and glob only in scripts)
Scroll: mouse wheel · pgup/pgdown · shift+↑/↓ · ctrl+home/ctrl+end
Copy: drag over the transcript to select; releasing copies it.
Hold shift while dragging to use the terminal's own selection instead.
While the agent runs, enter queues a steering message.
In a picker: type to filter · ↑↓ move · enter choose · esc cancel.`

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
					out, _ = r.diffView(patch, diffOpts{width: r.width, files: true, wrap: true})
				}
			}
			// A script shows what it ran and every call it made, then its
			// output.
			if d, ok := scriptDetails(last.result.Details); ok && last.name == "script" {
				code, _ := last.args["code"].(string)
				parts := []string{r.st.dim.Render(strings.TrimRight(code, "\n"))}
				if len(d.Calls) > 0 {
					parts = append(parts, r.st.dim.Render(scriptTally(d)), r.scriptCalls(d, "…", len(d.Calls)))
				}
				out = strings.Join(append(parts, out), "\n\n")
			}
			return item(r.st.accent.Render("── "+r.summary(last.name, last.args)+" ──") + "\n" + out)
		})
	case "model":
		return m.modelCommand(arg)
	case "mcp":
		return m.mcpCommand(arg)
	case "thinking":
		return m.thinkingCommand(arg)
	case "trim":
		return m.trimCommand(arg)
	case "trust":
		return m.trustCommand(arg)
	case "sandbox":
		return m.printDim(m.s.Sandbox().Describe())
	case "allow-read":
		root, err := m.s.AddReadableRoot(arg)
		if err != nil {
			return m.printErr(err)
		}
		m.syncSandbox(m.s.Sandbox())
		return m.printDim(fmt.Sprintf("Added readable root %s for this session. Whatever model-run tools read there is sent to your model provider.", root))
	case "allow-write":
		root, err := m.s.AddWritableRoot(arg)
		if err != nil {
			return m.printErr(err)
		}
		m.syncSandbox(m.s.Sandbox())
		return m.printDim(fmt.Sprintf("Added writable root %s for this session. Model-run tools can modify or delete its contents.", root))
	case "network":
		return m.networkCommand(arg)
	case "sidebar":
		m.toggleSidebar()
		if !m.sidebarHidden && !m.sidebarShown() {
			return m.printDim(fmt.Sprintf("the sidebar needs a terminal at least %d columns wide", sidebarMinWidth))
		}
		return nil
	case "compact":
		return m.compactCommand(arg)
	case "codemode":
		return m.codeModeCommand(arg)
	case "ctx":
		return m.printDim(coding.ContextLine(m.s.Model(), m.s.ContextStats()))
	case "new", "clear":
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
	args := strings.Fields(strings.ToLower(strings.TrimSpace(arg)))
	if len(args) > 0 && (args[0] == "yes" || args[0] == "no") && (len(args) == 1 || len(args) == 2 && args[1] == "--save") {
		remember := len(args) == 2
		if err := m.s.Trust(trustDecisionOf(args[0]), remember); err != nil {
			return m.printErr(err)
		}
		note := "for this session (saved for resume)"
		if remember {
			note = "remembered for this directory"
		}
		return m.printDim(trustOutcome(m.s.TrustState()) + " (" + note + ")")
	}
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "parent":
		if err := m.s.TrustParent(true); err != nil {
			return m.printErr(err)
		}
		return m.printDim(trustOutcome(m.s.TrustState()) + " (remembered for the parent directory)")
	case "":
		return m.trustPicker(state)
	default:
		return m.printErr(fmt.Errorf("usage: /trust [yes|no] [--save] | parent"))
	}
}

// trustPicker shows what the decision covers and asks yes or no for this
// session. Settings that fix the answer, or a directory with nothing to
// load, leave nothing to choose, so those only say how things stand.
func (m *model) trustPicker(state coding.TrustState) tea.Cmd {
	if state.Resources.Empty() || state.Source == "always" || state.Source == "never" {
		if state.Trusted() {
			return m.printDim(trustOutcome(state))
		}
		return m.printDim(state.TrustNotice())
	}
	current := ""
	if !state.Pending {
		current = "no"
		if state.Trusted() {
			current = "yes"
		}
	}
	items := []pickerItem{
		{label: "yes", detail: "load them for this session", value: "yes"},
		{label: "no", detail: "withhold them for this session", value: "no"},
	}
	// Trusting the parent outlives the session and reaches past this
	// directory, so the option says how far.
	if parent := filepath.Dir(state.Path); parent != state.Path {
		items = append(items, pickerItem{label: "parent", detail: "remember yes for everything under " + shortenHome(parent), value: "parent"})
	}
	m.picker = newPicker("load project instructions?", current, items, m.trustChoice)
	about := trustOutcome(state)
	if !state.Trusted() {
		// The notice ends by saying which command to type; the picker below
		// is that answer.
		about = state.TrustNotice()
		if i := strings.LastIndex(about, "\n"); i >= 0 {
			about = about[:i]
		}
	}
	return m.printDim(about + "\n\n  /trust yes --save to remember yes for this directory alone")
}

// trustChoice applies a picker answer. Choosing parent is itself a yes, so
// it is given for this directory before being shared with the parent, which
// only ever shares a yes.
func (m *model) trustChoice(answer string) tea.Cmd {
	if answer == "parent" && !m.s.TrustState().Trusted() {
		if err := m.s.Trust(coding.TrustTrusted, false); err != nil {
			return m.printErr(err)
		}
	}
	return m.trustCommand(answer)
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
	// The checks behind these are heuristics; the compaction went through.
	for _, w := range res.Warnings {
		m.tr.add(func(r *renderer) string { return r.gutter(iconWarning, r.st.toolRun, r.st.toolRun.Render(w)) })
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

// thinkingCommand sets the reasoning level, or with no argument opens a
// picker of the levels the provider offers.
func (m *model) thinkingCommand(arg string) tea.Cmd {
	if arg != "" {
		return m.setThinking(arg)
	}
	levels := m.s.Provider().ThinkingLevels
	if len(levels) == 0 {
		return m.printDim(fmt.Sprintf("thinking: %s (%s lists no levels; set one with /thinking <level>)", m.s.ThinkingLevel(), m.s.Provider().Name))
	}
	items := make([]pickerItem, len(levels))
	for i, l := range levels {
		items[i] = pickerItem{label: l, value: l}
	}
	m.picker = newPicker("thinking", m.s.ThinkingLevel(), items, m.setThinking)
	return nil
}

func (m *model) setThinking(level string) tea.Cmd {
	if err := m.s.SetThinkingLevel(level); err != nil {
		return m.printErr(err)
	}
	return m.printDim("thinking level set to " + level)
}

// modelsMsg carries a live model list back to the picker that asked for it.
type modelsMsg struct {
	p   *picker
	ids []string
	err error
}

// modelCommand switches model, or with no argument opens a picker filled
// with the provider's live list once it arrives.
func (m *model) modelCommand(arg string) tea.Cmd {
	if arg != "" {
		return m.setModel(arg)
	}
	s := m.s
	p := newPicker(s.Provider().Name+" model", s.Model(), nil, m.setModel)
	p.loading = true
	m.picker = p
	// Fetch off the UI goroutine; the picker spins until the list arrives.
	fetch := func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		ids, err := s.Models(ctx)
		return modelsMsg{p: p, ids: ids, err: err}
	}
	return tea.Batch(fetch, m.spin.Tick)
}

// fillModels hands a fetched list to its picker, unless that picker has
// already been closed or replaced.
func (m *model) fillModels(msg modelsMsg) tea.Cmd {
	if m.picker != msg.p {
		return nil
	}
	if len(msg.ids) == 0 {
		m.picker = nil
		if msg.err != nil {
			return m.printErr(fmt.Errorf("could not fetch the model list: %w", msg.err))
		}
		return m.printDim("the provider lists no models; set one with /model <ref>")
	}
	items := make([]pickerItem, len(msg.ids))
	for i, id := range msg.ids {
		items[i] = pickerItem{label: id, value: id}
	}
	if msg.err != nil {
		msg.p.note = "live list unavailable; showing the built-in list"
	}
	msg.p.setItems(items)
	return nil
}

func (m *model) setModel(ref string) tea.Cmd {
	if err := m.s.SetModel(ref); err != nil {
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
		items := make([]pickerItem, len(infos))
		for i, in := range infos {
			preview := in.Title
			if preview == "" {
				preview = strings.Join(strings.Fields(in.Preview), " ")
			}
			items[i] = pickerItem{
				label: fmt.Sprintf("%s  %3d msgs  %s", in.Modified.Format("Jan 02 15:04"), in.Messages, truncateWidth(preview, 60)),
				value: in.Path,
			}
		}
		current := m.s.Path()
		m.picker = newPicker("resume", current, items, func(path string) tea.Cmd {
			if path == current {
				return m.printDim("already in that session")
			}
			return m.reopen(path)
		})
		return nil
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
	m.syncSandbox(next.Sandbox())
	m.syncMCP(next.MCPState())
	m.syncJobs(next.Jobs())

	m.showSession(next)
	m.vp.GotoBottom()
	return m.bridge.next()
}

// printMsg asks the UI loop to append a block to the transcript; background
// commands return it because only the UI goroutine may touch the model.
type printMsg struct{ render func(r *renderer) string }

// networkCommand shows or switches sandboxed commands' network access for
// this session.
func (m *model) networkCommand(arg string) tea.Cmd {
	state := func() string {
		if m.s.Sandbox().Network {
			return "network on: commands can reach the internet"
		}
		return "network off: commands cannot open network connections"
	}
	switch strings.ToLower(arg) {
	case "":
		if !m.s.Sandbox().Enabled {
			return m.printDim("the sandbox is off, so commands have the network")
		}
		return m.printDim(state() + " (/network on|off to switch)")
	case "on", "off":
		if err := m.s.SetNetwork(strings.EqualFold(arg, "on")); err != nil {
			return m.printErr(err)
		}
		m.syncSandbox(m.s.Sandbox()) // at once; the session's event follows
		return m.printDim(state() + ", from the next command, for this session")
	}
	return m.printErr(fmt.Errorf("usage: /network [on|off]"))
}
