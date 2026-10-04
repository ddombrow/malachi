// Package tui is malachi's interactive terminal frontend.
//
// It runs full-screen (alternate screen): a scrollable transcript viewport
// fills the terminal above a pinned input box and status bar. Completed items
// are kept as render functions in a transcript, so everything re-wraps on
// resize; the in-progress message and running tools are appended live below
// them. The view follows new output unless the user has scrolled up.
package tui

import (
	"context"
	"fmt"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/coding"
)

// Messages delivered to the Bubble Tea loop.
type (
	agentEventMsg   struct{ e agent.Event }
	runDoneMsg      struct{ err error }
	bridgeClosedMsg struct{}
)

// bridge forwards harness events into the UI. Events are delivered through a
// channel read by a tea.Cmd, so the harness goroutine never touches UI state.
type bridge struct {
	ch    chan tea.Msg
	done  chan struct{}
	unsub func()
}

func newBridge(s *coding.Session) *bridge {
	b := &bridge{ch: make(chan tea.Msg, 4096), done: make(chan struct{})}
	b.unsub = s.Harness.Subscribe(func(e agent.Event) {
		select {
		case b.ch <- agentEventMsg{e}:
		case <-b.done:
		}
	})
	return b
}

func (b *bridge) close() {
	if b.unsub != nil {
		b.unsub()
		b.unsub = nil
	}
	select {
	case <-b.done:
	default:
		close(b.done)
	}
}

func (b *bridge) next() tea.Cmd {
	return func() tea.Msg {
		select {
		case <-b.done:
			return bridgeClosedMsg{}
		case msg := <-b.ch:
			select {
			case <-b.done:
				return bridgeClosedMsg{}
			default:
				return msg
			}
		}
	}
}

type runningTool struct {
	id, name string
	summary  string
	output   string
	started  time.Time
}

type lastTool struct {
	name    string
	args    map[string]any
	result  agent.ToolResult
	isError bool
}

type model struct {
	s      *coding.Session
	bridge *bridge
	r      *renderer
	input  textarea.Model
	spin   spinner.Model
	vp     viewport.Model
	tr     transcript

	width, height int
	chrome        int // rows the block under the transcript currently uses
	isDark        bool

	running   bool
	cancelRun context.CancelFunc
	partial   *agent.AssistantMessage
	lastReply string
	tools     []*runningTool
	toolArgs  map[string]map[string]any
	last      *lastTool
	usage     agent.Usage
	context   int64 // tokens in the most recent request
	// compactionSeq is the last compaction counter rendered as a transcript
	// marker; a newer one means a request pass replaced tool output.
	compactionSeq uint64
	quitting      bool
	initial       string
}

// Run starts the interactive UI. initialPrompt, if non-empty, is sent first.
// A panic is recorded with its stack and reported as an error rather than
// taking the process down without a trace.
func Run(s *coding.Session, initialPrompt string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("internal error (logged to %s): %v", s.Diagnostics().LogPanic("tui.Run", r, debug.Stack()), r)
		}
	}()
	m := newModel(s, initialPrompt)
	p := tea.NewProgram(m)
	_, err = p.Run()
	m.bridge.close()
	if m.cancelRun != nil {
		m.cancelRun()
	}
	if path := m.s.Path(); path != "" && fileExists(path) {
		fmt.Printf("\nSession saved: %s\nResume with: malachi -c\n", shortenHome(path))
	}
	return err
}

// inputMaxHeight is how tall the input box may grow on a roomy terminal.
const inputMaxHeight = 10

func newModel(s *coding.Session, initialPrompt string) *model {
	ta := textarea.New()
	ta.Placeholder = "Ask malachi to do something…"
	ta.ShowLineNumbers = false
	ta.Prompt = "› " // replaced with the icon set's user glyph in applyInputStyles
	ta.DynamicHeight = true
	ta.MinHeight = 1
	ta.MaxHeight = inputMaxHeight
	ta.CharLimit = 0
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("alt+enter", "shift+enter", "ctrl+j"))
	ta.Focus()

	m := &model{
		s:        s,
		bridge:   newBridge(s),
		input:    ta,
		spin:     spinner.New(spinner.WithSpinner(spinner.MiniDot)),
		vp:       viewport.New(),
		width:    80,
		height:   24,
		chrome:   chromeRows,
		isDark:   true,
		toolArgs: map[string]map[string]any{},
		initial:  initialPrompt,
	}
	m.r = newRenderer(m.width, m.isDark, m.s.Settings().Icons, m.s.Cwd())
	m.applyInputStyles()
	m.vp.MouseWheelEnabled = true
	m.vp.MouseWheelDelta = 3
	m.showSession(s)
	return m
}

// showSession starts the transcript with the banner and any resumed history.
func (m *model) showSession(s *coding.Session) {
	history := s.Harness.Messages()
	m.tr.reset()
	m.lastReply = ""
	for i := len(history) - 1; i >= 0; i-- {
		if a, ok := history[i].(*agent.AssistantMessage); ok && strings.TrimSpace(a.Text()) != "" {
			m.lastReply = a.Text()
			break
		}
	}
	m.tr.add(func(r *renderer) string { return r.banner(s, len(history)) })
	if len(history) > 0 {
		// One block: history() pairs tool results with their calls.
		m.tr.add(func(r *renderer) string { return strings.Join(r.history(history), "") })
	}
}

// print appends a block to the transcript.
func (m *model) print(f func(r *renderer) string) tea.Cmd {
	m.tr.add(f)
	return nil
}

func (m *model) Init() tea.Cmd {
	cmds := []tea.Cmd{tea.RequestBackgroundColor, m.bridge.next()}
	if m.initial != "" {
		text := m.initial
		m.initial = ""
		cmds = append(cmds, func() tea.Msg { return submitMsg{text} })
	}
	return tea.Sequence(cmds...)
}

type submitMsg struct{ text string }

func (m *model) applyInputStyles() {
	st := textarea.DefaultStyles(m.isDark)
	st.Focused.CursorLine = st.Focused.Text
	st.Focused.Prompt = m.r.st.user
	// The input's prompt is the same glyph, and the same width, as the
	// gutter on a user message in the transcript above it.
	m.input.Prompt = m.r.icons[iconUser] + " "
	m.input.SetStyles(st)
	m.input.SetWidth(max(20, m.width-1))
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := m.update(msg)
	m.noteCompaction()
	m.refresh()
	return m, cmd
}

// noteCompaction appends a marker line when a request pass has replaced tool
// output since the last time the UI looked. Compaction is otherwise invisible:
// the transcript and the session file are left alone on purpose.
func (m *model) noteCompaction() {
	c := m.s.Compaction()
	if c.Seq == m.compactionSeq {
		return
	}
	m.compactionSeq = c.Seq
	word := "results"
	if c.NewResults == 1 {
		word = "result"
	}
	line := fmt.Sprintf("compacted %d tool %s · %s → %s",
		c.NewResults, word, bytesHuman(c.Before), bytesHuman(c.After))
	if total := c.Results - c.NewResults; total > 0 {
		line += fmt.Sprintf(" · %d already trimmed", total)
	}
	if c.LedgerEntries > 0 {
		line += fmt.Sprintf(" · ledger %d entries", c.LedgerEntries)
	}
	m.tr.add(func(r *renderer) string { return r.gutter(iconCompacted, r.st.dim, r.st.dim.Render(line)) })
}

// bytesHuman formats a byte count for the compaction marker. These are context
// bytes, not tokens, so they must not read like the ctx figure beside them.
func bytesHuman(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f kB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// refresh lays out the viewport and fills it with the transcript plus the
// live area, keeping the view pinned to the bottom if it already was.
func (m *model) refresh() {
	follow := m.vp.AtBottom() || m.vp.TotalLineCount() == 0
	m.vp.SetWidth(m.width)
	// The input box yields rows to the transcript on a short terminal, and
	// below four spare rows the rules are dropped, so the view can never grow
	// past the screen and scroll the transcript off the top.
	m.chrome = chromeRows
	if m.height < chromeRows+2 {
		m.chrome = bareRows
	}
	avail := m.height - m.chrome - 1 // always leave the transcript a row
	m.input.MaxHeight = max(1, min(inputMaxHeight, avail))
	// Let the box hold more input than it can show, so a small terminal
	// scrolls the input instead of refusing what is typed into it.
	m.input.MaxContentHeight = inputMaxHeight
	m.vp.SetHeight(max(1, m.height-m.input.Height()-m.chrome))
	content := m.tr.text(m.r)
	if live := m.live(); live != "" {
		content += "\n" + live
	}
	m.vp.SetContent(strings.TrimLeft(content, "\n"))
	if follow {
		m.vp.GotoBottom()
	}
}

func (m *model) update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.r = newRenderer(m.width, m.isDark, m.s.Settings().Icons, m.s.Cwd())
		m.applyInputStyles()
		return nil

	case tea.BackgroundColorMsg:
		m.isDark = msg.IsDark()
		m.r = newRenderer(m.width, m.isDark, m.s.Settings().Icons, m.s.Cwd())
		m.applyInputStyles()
		return nil

	case agentEventMsg:
		cmd := m.handleEvent(msg.e)
		return tea.Batch(cmd, m.bridge.next())

	case runDoneMsg:
		m.running, m.cancelRun = false, nil
		m.partial, m.tools = nil, nil
		var cmds []tea.Cmd
		if msg.err != nil {
			cmds = append(cmds, m.printErr(msg.err))
		}
		if err := m.s.PersistError(); err != nil {
			cmds = append(cmds, m.printErr(err))
		}
		if m.quitting {
			cmds = append(cmds, tea.Quit)
		}
		return tea.Sequence(cmds...)

	case submitMsg:
		return m.submit(msg.text)

	case printMsg:
		return m.print(msg.render)

	case tea.MouseWheelMsg:
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return cmd

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		if !m.running {
			return nil
		}
		return cmd

	case bridgeClosedMsg:
		return nil

	case tea.KeyPressMsg:
		switch msg.String() {
		case "ctrl+c":
			if m.input.Value() != "" {
				m.input.Reset()
				return nil
			}
			if m.running {
				m.quitting = true
				m.s.Harness.Cancel()
				return nil
			}
			return tea.Quit
		case "ctrl+d":
			if m.input.Value() == "" && !m.running {
				return tea.Quit
			}
		case "esc":
			if m.running {
				m.s.Harness.Cancel()
				m.s.Harness.ClearQueues()
				return nil
			}
		case "pgup":
			m.vp.PageUp()
			return nil
		case "pgdown":
			m.vp.PageDown()
			return nil
		case "shift+up":
			m.vp.ScrollUp(3)
			return nil
		case "shift+down":
			m.vp.ScrollDown(3)
			return nil
		case "ctrl+home":
			m.vp.GotoTop()
			return nil
		case "ctrl+end":
			m.vp.GotoBottom()
			return nil
		case "enter":
			text := strings.TrimSpace(m.input.Value())
			if text == "" {
				return nil
			}
			m.input.Reset()
			return m.submit(text)
		}
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return cmd
}

// submit handles a line of input: a slash command, a steering message while
// the agent runs, or a new prompt.
func (m *model) submit(text string) tea.Cmd {
	if strings.HasPrefix(text, "/") {
		return m.command(text)
	}
	if m.running {
		m.s.Harness.Steer(agent.NewUserText(text))
		return m.printDim("queued — will be sent after the current step")
	}
	return m.startRun(text)
}

func (m *model) startRun(text string) tea.Cmd {
	ctx, cancel := context.WithCancel(context.Background())
	m.running, m.cancelRun = true, cancel
	s := m.s
	run := func() tea.Msg {
		err := s.Prompt(ctx, text)
		cancel()
		return runDoneMsg{err}
	}
	return tea.Batch(run, m.spin.Tick)
}

// handleEvent updates the live area and prints completed items.
func (m *model) handleEvent(e agent.Event) tea.Cmd {
	switch ev := e.(type) {
	case *agent.MessageStartEvent:
		if a, ok := ev.Message.(*agent.AssistantMessage); ok {
			m.partial = a
		}
	case *agent.MessageUpdateEvent:
		if a, ok := ev.Message.(*agent.AssistantMessage); ok {
			m.partial = a
		}
	case *agent.MessageEndEvent:
		switch msg := ev.Message.(type) {
		case *agent.UserMessage:
			text := msg.Content.String()
			return m.print(func(r *renderer) string { return r.userMessage(text) })
		case *agent.AssistantMessage:
			m.partial = nil
			if strings.TrimSpace(msg.Text()) != "" {
				m.lastReply = msg.Text()
			}
			m.usage = m.usage.Add(msg.Usage)
			if t := msg.Usage.Input + msg.Usage.CacheRead + msg.Usage.CacheWrite + msg.Usage.Output; t > 0 {
				m.context = t
			}
			for _, c := range msg.ToolCalls() {
				m.toolArgs[c.ID] = c.Arguments
			}
			return m.print(func(r *renderer) string { return r.assistantMessage(msg) })
		case *agent.ToolResultMessage:
			// Synthetic interruption results (no execution events) still
			// deserve a line in the transcript.
			if msg.IsError && msg.Text() == agent.InterruptedToolResult {
				args := m.toolArgs[msg.ToolCallID]
				return m.print(func(r *renderer) string {
					return item(r.gutter(iconInterrupted, r.st.dim, r.st.dim.Render(r.summary(msg.ToolName, args)+" · interrupted")))
				})
			}
		}
	case *agent.ToolExecutionStartEvent:
		m.toolArgs[ev.ToolCallID] = ev.Args
		m.tools = append(m.tools, &runningTool{
			id:      ev.ToolCallID,
			name:    ev.ToolName,
			summary: m.r.summary(ev.ToolName, ev.Args),
			started: time.Now(),
		})
	case *agent.ToolExecutionUpdateEvent:
		for _, t := range m.tools {
			if t.id == ev.ToolCallID {
				t.output = ev.PartialResult.Text()
			}
		}
	case *agent.ToolExecutionEndEvent:
		for i, t := range m.tools {
			if t.id == ev.ToolCallID {
				m.tools = append(m.tools[:i], m.tools[i+1:]...)
				break
			}
		}
		args := m.toolArgs[ev.ToolCallID]
		m.last = &lastTool{name: ev.ToolName, args: args, result: ev.Result, isError: ev.IsError}
		return m.print(func(r *renderer) string { return r.toolResult(ev.ToolName, args, ev.Result, ev.IsError) })
	}
	return nil
}

// statusPad is one cell of breathing room at each end of the status bar, so
// the text doesn't sit flush against the edges.
const statusPad = " "

// Layout of the rows under the transcript viewport: a blank pad row, then
// the two rules that bracket the input box, then the status bar. refresh()
// gives the viewport whatever is left, so the view fills the terminal
// exactly at any size, dropping the rules when the terminal is too short to
// spare the rows.
const (
	chromeRows = 4 // pad + rule + rule + status bar
	bareRows   = 1 // status bar only, for very short terminals
)

// Rules that bracket the input box. Box-drawing ─ draws at mid-cell-height,
// which reads as a line floating between the transcript and the input;
// highRule and lowRule sit at the top and bottom of their cells instead, so
// each hugs the edge it belongs to.
const (
	highRule = "‾" // overline, at the top of the cell
	lowRule  = "▁" // lower one-eighth block, at the bottom of the cell
)

func (m *model) View() tea.View {
	v := tea.NewView("")
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	if m.quitting && !m.running {
		return v
	}
	top := m.r.st.dim.Render(strings.Repeat(highRule, max(1, m.width)))
	// The underline hugs the bottom of the input's row; the bar's own
	// background separates it from the status text.
	lower := m.r.st.dim.Render(strings.Repeat(lowRule, max(1, m.width)))
	input := m.input.View()
	body := m.vp.View() + "\n" + input
	if m.chrome >= chromeRows {
		body = m.vp.View() + "\n\n" + top + "\n" + input + "\n" + lower
	}
	v.Content = body + "\n" + m.statusLine()
	return v
}

// live renders the in-progress parts of the current turn: the streaming
// message, tools that are running, or a spinner while waiting.
func (m *model) live() string {
	var b strings.Builder
	if m.partial != nil {
		if th := strings.TrimSpace(m.partial.ThinkingText()); th != "" && m.partial.Text() == "" {
			lines := lastN(wrapLines(th, m.width-m.r.gutterWidth(iconThinking)-1), 3)
			b.WriteString(item(m.r.gutter(iconThinking, m.r.st.thinking, m.r.st.thinking.Render(strings.Join(lines, "\n")))))
		}
		if text := strings.TrimLeft(m.partial.Text(), "\n"); text != "" {
			lines := wrapLines(text, m.width-m.r.gutterWidth(iconReply)-1)
			b.WriteString(item(m.r.gutter(iconReply, lipgloss.NewStyle(), strings.Join(lines, "\n"))))
		}
		for _, c := range m.partial.ToolCalls() {
			b.WriteString(item(m.r.gutter(toolIcon(c.Name), m.r.st.toolRun, m.r.st.dim.Render("preparing "+c.Name+"…"))))
		}
	}
	for _, t := range m.tools {
		elapsed := time.Since(t.started).Truncate(time.Second)
		head := t.summary + " " + m.r.st.toolRun.Render(m.spin.View()) + m.r.st.dim.Render(" "+elapsed.String())
		if out := strings.TrimSpace(t.output); out != "" {
			head += "\n" + m.r.preview(m.r.st.dim, strings.Join(lastN(strings.Split(out, "\n"), 8), "\n"))
		}
		b.WriteString(item(m.r.gutter(toolIcon(t.name), m.r.st.toolRun, head)))
	}
	if m.running && m.partial == nil && len(m.tools) == 0 {
		b.WriteString(item(m.r.st.toolRun.Render(m.spin.View()) + m.r.st.dim.Render("  working…")))
	}
	return b.String()
}

func (m *model) statusLine() string {
	parts := []string{m.s.Provider().Name + "/" + m.s.Model()}
	if lvl := m.s.ThinkingLevel(); lvl != "" && lvl != "off" {
		parts = append(parts, "thinking "+lvl)
	}
	if m.usage.TotalTokens > 0 {
		parts = append(parts, fmt.Sprintf("↑%s ↓%s", tokens(m.usage.Input+m.usage.CacheRead+m.usage.CacheWrite), tokens(m.usage.Output)))
		if m.usage.CacheRead > 0 {
			parts = append(parts, "cache "+tokens(m.usage.CacheRead))
		}
	}
	if m.context > 0 {
		parts = append(parts, "ctx "+tokens(m.context))
	}
	// Compaction trims what the provider sees without touching the transcript,
	// so the bar is the only always-visible trace of it.
	if c := m.s.Compaction(); c.Seq > 0 {
		parts = append(parts, "cmp "+tokens(int64(c.After)))
	}
	steer, follow := m.s.Harness.Queued()
	if n := len(steer) + len(follow); n > 0 {
		parts = append(parts, fmt.Sprintf("%d queued", n))
	}
	right := "/help"
	if m.running {
		right = "esc to cancel"
	}
	if !m.vp.AtBottom() {
		right = "↓ more · ctrl+end"
	}
	lead, trail := statusPad, statusPad
	if m.width < 2*lipgloss.Width(statusPad) {
		trail = "" // no room for a pad on each side of a one-column bar
	}
	caps := lipgloss.Width(lead) + lipgloss.Width(trail)
	right = truncateWidth(right, max(0, m.width-caps))
	avail := m.width - caps - lipgloss.Width(right)
	if avail <= 0 {
		return m.r.st.status.Render(lead + right + trail)
	}
	for len(parts) > 0 && lipgloss.Width(strings.Join(parts, " · "))+1 > avail {
		parts = parts[:len(parts)-1]
	}
	left := strings.Join(parts, " · ")
	if left != "" {
		left = truncateWidth(left, avail-1)
	}
	pad := max(0, avail-lipgloss.Width(left))
	return m.r.st.status.Render(lead + left + strings.Repeat(" ", pad) + right + trail)
}

func tokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

func lastN[T any](s []T, n int) []T {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func homeDir() string {
	h, _ := os.UserHomeDir()
	return h
}
