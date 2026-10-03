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
	agentEventMsg struct{ e agent.Event }
	runDoneMsg    struct{ err error }
)

// bridge forwards harness events into the UI. Events are delivered through a
// channel read by a tea.Cmd, so the harness goroutine never touches UI state.
type bridge struct {
	ch    chan tea.Msg
	unsub func()
}

func newBridge(s *coding.Session) *bridge {
	b := &bridge{ch: make(chan tea.Msg, 4096)}
	b.unsub = s.Harness.Subscribe(func(e agent.Event) { b.ch <- agentEventMsg{e} })
	return b
}

func (b *bridge) next() tea.Cmd {
	return func() tea.Msg { return <-b.ch }
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
	isDark        bool

	running   bool
	cancelRun context.CancelFunc
	partial   *agent.AssistantMessage
	tools     []*runningTool
	toolArgs  map[string]map[string]any
	last      *lastTool
	usage     agent.Usage
	context   int64 // tokens in the most recent request
	quitting  bool
	initial   string
}

// Run starts the interactive UI. initialPrompt, if non-empty, is sent first.
func Run(s *coding.Session, initialPrompt string) error {
	m := newModel(s, initialPrompt)
	p := tea.NewProgram(m)
	_, err := p.Run()
	m.bridge.unsub()
	if m.cancelRun != nil {
		m.cancelRun()
	}
	if path := m.s.Path(); path != "" && fileExists(path) {
		fmt.Printf("\nSession saved: %s\nResume with: malachi -c\n", shortenHome(path))
	}
	return err
}

func newModel(s *coding.Session, initialPrompt string) *model {
	ta := textarea.New()
	ta.Placeholder = "Ask malachi to do something…"
	ta.ShowLineNumbers = false
	ta.Prompt = "› "
	ta.DynamicHeight = true
	ta.MinHeight = 1
	ta.MaxHeight = 10
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
	cmds := []tea.Cmd{tea.RequestBackgroundColor, m.bridge.next(), m.spin.Tick}
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
	m.input.SetStyles(st)
	m.input.SetWidth(max(20, m.width-1))
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := m.update(msg)
	m.refresh()
	return m, cmd
}

// refresh lays out the viewport and fills it with the transcript plus the
// live area, keeping the view pinned to the bottom if it already was.
func (m *model) refresh() {
	follow := m.vp.AtBottom() || m.vp.TotalLineCount() == 0
	m.vp.SetWidth(m.width)
	m.vp.SetHeight(max(1, m.height-m.input.Height()-2))
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
		return cmd

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

func (m *model) View() tea.View {
	v := tea.NewView("")
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	if m.quitting && !m.running {
		return v
	}
	rule := m.r.st.dim.Render(strings.Repeat("─", max(1, m.width)))
	v.Content = m.vp.View() + "\n" + rule + "\n" + m.input.View() + "\n" + m.statusLine()
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
		b.WriteString(item(m.r.st.toolRun.Render(m.spin.View()) + m.r.st.dim.Render(" working…")))
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
	left := strings.Join(parts, " · ")
	// The right-hand hint is the actionable part; trim the stats first.
	left = truncateWidth(left, m.width-lipgloss.Width(right)-2)
	pad := max(1, m.width-lipgloss.Width(left)-lipgloss.Width(right)-1)
	return m.r.st.status.Render(left + strings.Repeat(" ", pad) + right)
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
