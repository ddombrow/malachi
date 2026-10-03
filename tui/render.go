package tui

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/coding"
)

// styles holds every color decision so light/dark switching is one place.
type styles struct {
	user, dim, thinking, toolOK, toolErr, toolRun, errorText, add, del, hunk, status, accent lipgloss.Style
}

func newStyles(isDark bool) styles {
	ld := lipgloss.LightDark(isDark)
	return styles{
		user:      lipgloss.NewStyle().Bold(true).Foreground(ld(lipgloss.Color("25"), lipgloss.Color("117"))),
		dim:       lipgloss.NewStyle().Foreground(ld(lipgloss.Color("245"), lipgloss.Color("243"))),
		thinking:  lipgloss.NewStyle().Italic(true).Foreground(ld(lipgloss.Color("246"), lipgloss.Color("242"))),
		toolOK:    lipgloss.NewStyle().Foreground(ld(lipgloss.Color("28"), lipgloss.Color("114"))),
		toolErr:   lipgloss.NewStyle().Foreground(ld(lipgloss.Color("160"), lipgloss.Color("203"))),
		toolRun:   lipgloss.NewStyle().Foreground(ld(lipgloss.Color("130"), lipgloss.Color("221"))),
		errorText: lipgloss.NewStyle().Foreground(ld(lipgloss.Color("160"), lipgloss.Color("203"))),
		add:       lipgloss.NewStyle().Foreground(ld(lipgloss.Color("28"), lipgloss.Color("114"))),
		del:       lipgloss.NewStyle().Foreground(ld(lipgloss.Color("160"), lipgloss.Color("203"))),
		hunk:      lipgloss.NewStyle().Foreground(ld(lipgloss.Color("31"), lipgloss.Color("80"))),
		status:    lipgloss.NewStyle().Foreground(ld(lipgloss.Color("244"), lipgloss.Color("244"))),
		accent:    lipgloss.NewStyle().Foreground(ld(lipgloss.Color("97"), lipgloss.Color("141"))),
	}
}

// renderer turns transcript items into scrollback text.
type renderer struct {
	st     styles
	md     *glamour.TermRenderer
	width  int
	isDark bool
}

func newRenderer(width int, isDark bool) *renderer {
	r := &renderer{st: newStyles(isDark), width: width, isDark: isDark}
	style := "light"
	if isDark {
		style = "dark"
	}
	wrap := max(20, width-2)
	if md, err := glamour.NewTermRenderer(glamour.WithStandardStyle(style), glamour.WithWordWrap(wrap)); err == nil {
		r.md = md
	}
	return r
}

func (r *renderer) markdown(text string) string {
	if r.md == nil || strings.TrimSpace(text) == "" {
		return text
	}
	out, err := r.md.Render(text)
	if err != nil {
		return text
	}
	// Glamour pads every line to the wrap width; trailing blanks make
	// copying from scrollback ugly, so strip them (ANSI-aware).
	lines := strings.Split(strings.Trim(out, "\n"), "\n")
	for i, l := range lines {
		lines[i] = trailingBlank.ReplaceAllString(l, "") + "\x1b[0m"
	}
	return strings.Join(lines, "\n")
}

// trailingBlank matches trailing spaces interleaved with SGR sequences.
var trailingBlank = regexp.MustCompile(`(?:\x1b\[[0-9;]*m| )+$`)

func (r *renderer) userMessage(text string) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for i, l := range lines {
		prefix := "  "
		if i == 0 {
			prefix = "› "
		}
		lines[i] = r.st.user.Render(prefix + l)
	}
	return "\n" + strings.Join(lines, "\n")
}

// thinkingBlock shows at most a few lines of reasoning, dimmed.
func (r *renderer) thinkingBlock(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	lines := wrapLines(text, r.width-4)
	const keep = 3
	more := ""
	if len(lines) > keep {
		more = fmt.Sprintf(" … (+%d lines)", len(lines)-keep)
		lines = lines[:keep]
	}
	for i, l := range lines {
		lines[i] = r.st.thinking.Render("  " + l)
	}
	return strings.Join(lines, "\n") + r.st.dim.Render(more)
}

// assistantMessage renders a completed assistant message (without its tool
// calls, which are printed as they execute).
func (r *renderer) assistantMessage(m *agent.AssistantMessage) string {
	var parts []string
	if t := r.thinkingBlock(m.ThinkingText()); t != "" {
		parts = append(parts, t)
	}
	if text := m.Text(); strings.TrimSpace(text) != "" {
		parts = append(parts, r.markdown(text))
	}
	switch m.StopReason {
	case agent.StopError:
		parts = append(parts, r.st.errorText.Render("✗ "+m.ErrorMessage))
	case agent.StopAborted:
		parts = append(parts, r.st.dim.Render("⏹ cancelled"))
	case agent.StopLength:
		parts = append(parts, r.st.dim.Render("(response hit the output token limit)"))
	}
	if len(parts) == 0 {
		return ""
	}
	return "\n" + strings.Join(parts, "\n")
}

// toolResult renders a finished tool call: a status line plus a short,
// tool-specific preview of the output.
func (r *renderer) toolResult(name string, args map[string]any, res agent.ToolResult, isError bool) string {
	summary := coding.SummarizeToolCall(name, args)
	if isError {
		head := r.st.toolErr.Render("✗ " + summary)
		return head + "\n" + r.indent(r.st.errorText, firstLines(res.Text(), 4))
	}
	details, _ := res.Details.(map[string]any)
	head := r.st.toolOK.Render("✓ ") + summary
	switch {
	case details["cancelled"] == true:
		head = r.st.toolErr.Render("⏹ ") + summary
	case details["timed_out"] == true:
		head = r.st.toolErr.Render("⏱ ") + summary
	case name == "bash" && nonZero(details["exit_code"]):
		head = r.st.toolErr.Render("✗ ") + summary
	}
	switch name {
	case "edit":
		if patch, _ := details["patch"].(string); patch != "" {
			return head + "\n" + r.diff(patch, 30)
		}
	case "read", "write":
		return head
	case "bash":
		out := strings.TrimSpace(res.Text())
		if out == "" || out == "(no output)" {
			return head
		}
		return head + "\n" + r.indent(r.st.dim, lastLines(out, 6))
	}
	if out := strings.TrimSpace(res.Text()); out != "" {
		return head + "\n" + r.indent(r.st.dim, firstLines(out, 4))
	}
	return head
}

func (r *renderer) diff(patch string, maxLines int) string {
	lines := strings.Split(strings.TrimRight(patch, "\n"), "\n")
	var out []string
	for _, l := range lines {
		if strings.HasPrefix(l, "---") || strings.HasPrefix(l, "+++") {
			continue
		}
		out = append(out, l)
	}
	more := 0
	if len(out) > maxLines {
		more = len(out) - maxLines
		out = out[:maxLines]
	}
	for i, l := range out {
		switch {
		case strings.HasPrefix(l, "@@"):
			out[i] = r.st.hunk.Render("    " + l)
		case strings.HasPrefix(l, "+"):
			out[i] = r.st.add.Render("    " + l)
		case strings.HasPrefix(l, "-"):
			out[i] = r.st.del.Render("    " + l)
		default:
			out[i] = r.st.dim.Render("    " + l)
		}
	}
	s := strings.Join(out, "\n")
	if more > 0 {
		s += "\n" + r.st.dim.Render(fmt.Sprintf("    … %d more diff lines (/last to see all)", more))
	}
	return s
}

func (r *renderer) indent(style lipgloss.Style, text string) string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = style.Render("    " + truncateWidth(l, r.width-6))
	}
	return strings.Join(lines, "\n")
}

// history renders a resumed transcript compactly.
func (r *renderer) history(msgs []agent.Message) []string {
	calls := map[string]*agent.ToolCall{}
	var out []string
	for _, m := range msgs {
		switch v := m.(type) {
		case *agent.UserMessage:
			out = append(out, r.userMessage(v.Content.String()))
		case *agent.AssistantMessage:
			if s := r.assistantMessage(v); s != "" {
				out = append(out, s)
			}
			for _, c := range v.ToolCalls() {
				calls[c.ID] = c
			}
		case *agent.ToolResultMessage:
			args := map[string]any{}
			if c := calls[v.ToolCallID]; c != nil {
				args = c.Arguments
			}
			res := agent.ToolResult{Content: v.Content, Details: v.Details}
			out = append(out, r.toolResult(v.ToolName, args, res, v.IsError))
		case *agent.CompactionSummaryMessage:
			out = append(out, r.st.dim.Render("── earlier conversation compacted ──"))
		default:
			if t := agent.MessageText(m); t != "" {
				out = append(out, r.st.dim.Render(firstLines(t, 3)))
			}
		}
	}
	return out
}

func (r *renderer) banner(s *coding.Session, resumed int) string {
	title := r.st.accent.Bold(true).Render("malachi")
	info := r.st.dim.Render(fmt.Sprintf(" %s/%s · %s", s.Provider().Name, s.Model(), shortenHome(s.Cwd())))
	help := r.st.dim.Render("enter send · alt+enter newline · esc cancel · /help commands · ctrl+c quit")
	b := title + info + "\n" + help
	if resumed > 0 {
		b += "\n" + r.st.dim.Render(fmt.Sprintf("resumed %s (%d messages)", filepath.Base(s.Path()), resumed))
	}
	return b
}

// nonZero reports whether v is a number other than zero. Details decoded
// from a session file hold float64; fresh ones hold int.
func nonZero(v any) bool {
	switch n := v.(type) {
	case int:
		return n != 0
	case float64:
		return n != 0
	}
	return false
}

// ---- text helpers ----

func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[:n], "\n") + fmt.Sprintf("\n… (+%d lines)", len(lines)-n)
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return fmt.Sprintf("… (%d earlier lines)\n", len(lines)-n) + strings.Join(lines[len(lines)-n:], "\n")
}

func truncateWidth(s string, w int) string {
	if w <= 1 || lipgloss.Width(s) <= w {
		return s
	}
	runes := []rune(s)
	for len(runes) > 0 && lipgloss.Width(string(runes)) > w-1 {
		runes = runes[:len(runes)-1]
	}
	return string(runes) + "…"
}

// wrapLines hard-wraps text to width for the live area.
func wrapLines(s string, width int) []string {
	if width < 10 {
		width = 10
	}
	var out []string
	for _, line := range strings.Split(s, "\n") {
		runes := []rune(line)
		for len(runes) > width {
			out = append(out, string(runes[:width]))
			runes = runes[width:]
		}
		out = append(out, string(runes))
	}
	return out
}

func shortenHome(p string) string {
	if home, err := filepath.Abs(homeDir()); err == nil && home != "" && strings.HasPrefix(p, home) {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}
