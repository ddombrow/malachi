package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/agent/session"
)

const helpText = `Commands:
  /model [ref]        show models, or switch (e.g. /model glm-5.2, /model openai/gpt-5.1)
  /thinking [level]   show or set the reasoning level
  /new                start a fresh session
  /resume [n|name]    list recent sessions, or resume one
  /last               print the full output of the last tool call
  /session            show the session file path
  /quit               exit

Keys: enter send · alt+enter newline · esc cancel run · ctrl+c clear/quit
While the agent runs, enter queues a steering message.`

func (m *model) printDim(s string) tea.Cmd { return tea.Println(perLine(m.r.st.dim, s)) }

func (m *model) printErr(err error) tea.Cmd {
	return tea.Println(m.r.st.errorText.Render("✗ " + err.Error()))
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
		return m.printDim(m.s.Path())
	case "last":
		if m.last == nil {
			return m.printDim("no tool has run yet")
		}
		out := m.last.result.Text()
		if d, ok := m.last.result.Details.(map[string]any); ok {
			if patch, _ := d["patch"].(string); patch != "" {
				out = m.r.diff(patch, 1<<30)
			}
		}
		head := m.r.summary(m.last.name, m.last.args)
		return tea.Println(m.r.st.accent.Render("── "+head+" ──") + "\n" + out)
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
	case "new":
		return m.reopen("")
	case "resume":
		return m.resumeCommand(arg)
	}
	return m.printErr(fmt.Errorf("unknown command /%s (try /help)", name))
}

func (m *model) modelCommand(arg string) tea.Cmd {
	if arg == "" {
		// Fetch off the UI goroutine; the result is printed when it arrives.
		s, r := m.s, m.r
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			ids, err := s.Models(ctx)
			var b strings.Builder
			fmt.Fprintf(&b, "current: %s/%s\n", s.Provider().Name, s.Model())
			if err != nil {
				fmt.Fprintf(&b, "(could not fetch live model list: %v; showing built-in list)\n", err)
			}
			if len(ids) > 0 {
				fmt.Fprintf(&b, "%s models (%d):\n%s", s.Provider().Name, len(ids), columns(ids, r.width-4))
			}
			return printMsg{perLine(r.st.dim, strings.TrimRight(b.String(), "\n"))}
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
	m.bridge.unsub()
	m.s = next
	m.bridge = newBridge(next)
	m.usage, m.context, m.last = agent.Usage{}, 0, nil
	m.toolArgs = map[string]map[string]any{}

	history := next.Harness.Messages()
	lines := []string{"", m.r.banner(next, len(history))}
	lines = append(lines, m.r.history(history)...)
	return tea.Batch(tea.Println(strings.Join(lines, "\n")), m.bridge.next())
}

// printMsg asks the UI loop to print text to scrollback; background commands
// return it instead of calling tea.Println themselves.
type printMsg struct{ text string }

// columns lays out ids in aligned columns within width.
func columns(ids []string, width int) string {
	colW := 0
	for _, id := range ids {
		colW = max(colW, len(id)+2)
	}
	perRow := max(1, width/max(colW, 1))
	var b strings.Builder
	for i, id := range ids {
		b.WriteString("  " + id)
		if (i+1)%perRow == 0 || i == len(ids)-1 {
			b.WriteByte('\n')
		} else {
			b.WriteString(strings.Repeat(" ", colW-len(id)))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
