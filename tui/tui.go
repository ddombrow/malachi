// Package tui is malachi's interactive terminal frontend.
//
// It runs inline rather than full-screen: completed messages and tool results
// are printed once into the terminal's normal scrollback (tea.Println), and
// Bubble Tea only redraws the live area at the bottom — the streaming
// message, running tools, the input box, and a status bar. That keeps native
// scrollback, search, and copy, and long sessions never re-render history.
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
	tea "charm.land/bubbletea/v2"

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
	id, summary string
	output      string
	started     time.Time
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
		width:    80,
		height:   24,
		isDark:   true,
		toolArgs: map[string]map[string]any{},
		initial:  initialPrompt,
	}
	m.r = newRenderer(m.width, m.isDark)
	m.applyInputStyles()
	return m
}

func (m *model) Init() tea.Cmd {
	history := m.s.Harness.Messages()
	cmds := []tea.Cmd{tea.RequestBackgroundColor, m.bridge.next(), m.spin.Tick}
	print := []string{m.r.banner(m.s, len(history))}
	print = append(print, m.r.history(history)...)
	cmds = append(cmds, tea.Println(strings.Join(print, "\n")))
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
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.r = newRenderer(m.width, m.isDark)
		m.applyInputStyles()
		return m, nil

	case tea.BackgroundColorMsg:
		m.isDark = msg.IsDark()
		m.r = newRenderer(m.width, m.isDark)
		m.applyInputStyles()
		return m, nil

	case agentEventMsg:
		cmd := m.handleEvent(msg.e)
		return m, tea.Batch(cmd, m.bridge.next())

	case runDoneMsg:
		m.running, m.cancelRun = false, nil
		m.partial, m.tools = nil, nil
		var cmds []tea.Cmd
		if msg.err != nil {
			cmds = append(cmds, tea.Println(m.r.st.errorText.Render("✗ "+msg.err.Error())))
		}
		if err := m.s.PersistError(); err != nil {
			cmds = append(cmds, tea.Println(m.r.st.errorText.Render("✗ "+err.Error())))
		}
		if m.quitting {
			cmds = append(cmds, tea.Quit)
		}
		return m, tea.Sequence(cmds...)

	case submitMsg:
		return m, m.submit(msg.text)

	case printMsg:
		return m, tea.Println(msg.text)

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd

	case tea.KeyPressMsg:
		switch msg.String() {
		case "ctrl+c":
			if m.input.Value() != "" {
				m.input.Reset()
				return m, nil
			}
			if m.running {
				m.quitting = true
				m.s.Harness.Cancel()
				return m, nil
			}
			return m, tea.Quit
		case "ctrl+d":
			if m.input.Value() == "" && !m.running {
				return m, tea.Quit
			}
		case "esc":
			if m.running {
				m.s.Harness.Cancel()
				m.s.Harness.ClearQueues()
				return m, nil
			}
		case "enter":
			text := strings.TrimSpace(m.input.Value())
			if text == "" {
				return m, nil
			}
			m.input.Reset()
			return m, m.submit(text)
		}
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// submit handles a line of input: a slash command, a steering message while
// the agent runs, or a new prompt.
func (m *model) submit(text string) tea.Cmd {
	if strings.HasPrefix(text, "/") {
		return m.command(text)
	}
	if m.running {
		m.s.Harness.Steer(agent.NewUserText(text))
		return tea.Println(m.r.st.dim.Render("  (queued — will be sent after the current step)"))
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
			return tea.Println(m.r.userMessage(msg.Content.String()))
		case *agent.AssistantMessage:
			m.partial = nil
			m.usage = m.usage.Add(msg.Usage)
			if t := msg.Usage.Input + msg.Usage.CacheRead + msg.Usage.CacheWrite + msg.Usage.Output; t > 0 {
				m.context = t
			}
			for _, c := range msg.ToolCalls() {
				m.toolArgs[c.ID] = c.Arguments
			}
			if s := m.r.assistantMessage(msg); s != "" {
				return tea.Println(s)
			}
		case *agent.ToolResultMessage:
			// Synthetic interruption results (no execution events) still
			// deserve a line in scrollback.
			if msg.IsError && msg.Text() == agent.InterruptedToolResult {
				return tea.Println(m.r.st.dim.Render("  ⏹ " + coding.SummarizeToolCall(msg.ToolName, m.toolArgs[msg.ToolCallID]) + " (interrupted)"))
			}
		}
	case *agent.ToolExecutionStartEvent:
		m.toolArgs[ev.ToolCallID] = ev.Args
		m.tools = append(m.tools, &runningTool{
			id:      ev.ToolCallID,
			summary: coding.SummarizeToolCall(ev.ToolName, ev.Args),
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
		return tea.Println(m.r.toolResult(ev.ToolName, args, ev.Result, ev.IsError))
	}
	return nil
}

func (m *model) View() tea.View {
	if m.quitting && !m.running {
		return tea.NewView("")
	}
	var b strings.Builder
	maxLive := max(3, m.height-m.input.Height()-6)

	if m.partial != nil {
		var live []string
		if th := strings.TrimSpace(m.partial.ThinkingText()); th != "" && m.partial.Text() == "" {
			for _, l := range lastN(wrapLines(th, m.width-4), 3) {
				live = append(live, m.r.st.thinking.Render("  "+l))
			}
		}
		if text := m.partial.Text(); text != "" {
			live = append(live, wrapLines(text, m.width-1)...)
		}
		for _, c := range m.partial.ToolCalls() {
			live = append(live, m.r.st.toolRun.Render("  ⋯ preparing "+c.Name))
		}
		if len(live) > 0 {
			b.WriteString("\n" + strings.Join(lastN(live, maxLive), "\n") + "\n")
		}
	}
	for _, t := range m.tools {
		elapsed := time.Since(t.started).Truncate(time.Second)
		fmt.Fprintf(&b, "%s %s %s\n", m.r.st.toolRun.Render(m.spin.View()), t.summary, m.r.st.dim.Render(elapsed.String()))
		if out := strings.TrimSpace(t.output); out != "" {
			for _, l := range lastN(strings.Split(out, "\n"), 4) {
				b.WriteString(m.r.st.dim.Render("    "+truncateWidth(l, m.width-6)) + "\n")
			}
		}
	}
	if m.running && m.partial == nil && len(m.tools) == 0 {
		b.WriteString(m.r.st.toolRun.Render(m.spin.View()) + m.r.st.dim.Render(" working…") + "\n")
	}

	b.WriteString("\n" + m.input.View() + "\n")
	b.WriteString(m.statusLine())
	return tea.NewView(b.String())
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
	left := strings.Join(parts, " · ")
	pad := max(1, m.width-len([]rune(left))-len(right)-1)
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
