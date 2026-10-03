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
		// The status bar is a full-width tinted row, so it reads as its own
		// surface without needing a blank line above it.
		status: lipgloss.NewStyle().
			Foreground(ld(lipgloss.Color("240"), lipgloss.Color("250"))).
			Background(ld(lipgloss.Color("252"), lipgloss.Color("236"))),
		accent: lipgloss.NewStyle().Foreground(ld(lipgloss.Color("97"), lipgloss.Color("141"))),
	}
}

// renderer turns transcript items into styled text.
type renderer struct {
	st     styles
	md     *glamour.TermRenderer
	width  int
	isDark bool
	icons  map[string]string
	cwd    string // for shortening paths in tool summaries
}

func newRenderer(width int, isDark bool, icons, cwd string) *renderer {
	r := &renderer{st: newStyles(isDark), width: width, isDark: isDark, icons: iconSet(icons), cwd: cwd}
	style := "light"
	if isDark {
		style = "dark"
	}
	// Leave room for the icon gutter; glamour's own 2-space margin is
	// stripped in markdown().
	wrap := max(20, width-r.gutterWidth(iconReply)+glamourMargin-1)
	if md, err := glamour.NewTermRenderer(glamour.WithStandardStyle(style), glamour.WithWordWrap(wrap)); err == nil {
		r.md = md
	}
	return r
}

// glamourMargin is the plain-space left margin glamour's standard styles
// put on every line.
const glamourMargin = 2

func (r *renderer) markdown(text string) string {
	if r.md == nil || strings.TrimSpace(text) == "" {
		return text
	}
	out, err := r.md.Render(text)
	if err != nil {
		return text
	}
	// Glamour pads every line to the wrap width; trailing blanks make
	// copying text ugly, so strip them (ANSI-aware). Also drop
	// its left margin so the text sits right after the icon gutter.
	lines := strings.Split(strings.Trim(out, "\n"), "\n")
	for i, l := range lines {
		l = strings.TrimPrefix(l, strings.Repeat(" ", glamourMargin))
		l = trailingBlank.ReplaceAllString(l, "")
		if l != "" {
			l += "\x1b[0m"
		}
		lines[i] = l
	}
	for len(lines) > 0 && lines[0] == "" {
		lines = lines[1:]
	}
	return strings.Join(lines, "\n")
}

// trailingBlank matches trailing spaces interleaved with SGR sequences.
var trailingBlank = regexp.MustCompile(`(?:\x1b\[[0-9;]*m| )+$`)

// Every transcript item starts on a new line; the transcript adds the blank
// line between items.
func item(s string) string { return "\n" + s }

// summary is coding.SummarizeToolCall with paths shown relative to the
// working directory (or ~) to keep tool lines short.
func (r *renderer) summary(name string, args map[string]any) string {
	s := coding.SummarizeToolCall(name, args)
	if r.cwd != "" {
		s = strings.ReplaceAll(s, r.cwd+string(filepath.Separator), "")
	}
	if home := homeDir(); home != "" {
		s = strings.ReplaceAll(s, home+string(filepath.Separator), "~"+string(filepath.Separator))
	}
	return s
}

func (r *renderer) userMessage(text string) string {
	lines := wordWrap(strings.TrimRight(text, "\n"), r.width-r.gutterWidth(iconUser)-1)
	for i, l := range lines {
		lines[i] = r.st.user.Render(l)
	}
	return item(r.gutter(iconUser, r.st.user, strings.Join(lines, "\n")))
}

// thinkingBlock shows at most a few lines of reasoning, dimmed.
func (r *renderer) thinkingBlock(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	lines := wrapLines(text, r.width-r.gutterWidth(iconThinking)-1)
	const keep = 3
	more := ""
	if len(lines) > keep {
		more = fmt.Sprintf(" … (+%d lines)", len(lines)-keep)
		lines = lines[:keep]
	}
	for i, l := range lines {
		lines[i] = r.st.thinking.Render(l)
	}
	if more != "" {
		lines[len(lines)-1] += r.st.dim.Render(more)
	}
	return r.gutter(iconThinking, r.st.thinking, strings.Join(lines, "\n"))
}

// assistantMessage renders a completed assistant message (without its tool
// calls, which are printed as they execute).
func (r *renderer) assistantMessage(m *agent.AssistantMessage) string {
	var parts []string
	if t := r.thinkingBlock(m.ThinkingText()); t != "" {
		parts = append(parts, item(t))
	}
	if text := m.Text(); strings.TrimSpace(text) != "" {
		parts = append(parts, item(r.gutter(iconReply, lipgloss.NewStyle(), r.markdown(text))))
	}
	switch m.StopReason {
	case agent.StopError:
		parts = append(parts, item(r.gutter(iconError, r.st.errorText, r.st.errorText.Render(m.ErrorMessage))))
	case agent.StopAborted:
		parts = append(parts, item(r.gutter(iconCancelled, r.st.dim, r.st.dim.Render("cancelled"))))
	case agent.StopLength:
		parts = append(parts, item(r.st.dim.Render("(response hit the output token limit)")))
	}
	return strings.Join(parts, "")
}

// toolResult renders a finished tool call: an icon and summary line plus a
// short, tool-specific preview of the output. Failure shows as a red
// summary with a reason, since emoji icons cannot be recolored.
func (r *renderer) toolResult(name string, args map[string]any, res agent.ToolResult, isError bool) string {
	summary := r.summary(name, args)
	if isError {
		body := r.st.toolErr.Render(summary) + "\n" + r.preview(r.st.errorText, firstLines(res.Text(), 4))
		return item(r.gutter(iconError, r.st.toolErr, body))
	}
	details, _ := res.Details.(map[string]any)
	kind, iconStyle, head := toolIcon(name), r.st.toolOK, summary
	switch {
	case details["cancelled"] == true:
		kind, iconStyle, head = iconCancelled, r.st.toolErr, r.st.toolErr.Render(summary)+r.st.dim.Render(" · cancelled")
	case details["timed_out"] == true:
		kind, iconStyle, head = iconTimeout, r.st.toolErr, r.st.toolErr.Render(summary)+r.st.dim.Render(" · timed out")
	case name == "bash" && nonZero(details["exit_code"]):
		iconStyle = r.st.toolErr
		head = r.st.toolErr.Render(summary) + r.st.toolErr.Render(fmt.Sprintf(" ✗ exit %v", details["exit_code"]))
	}

	var body string
	switch name {
	case "edit":
		if patch, _ := details["patch"].(string); patch != "" {
			body = r.diff(patch, 30)
		}
	case "read", "write":
	case "bash":
		if out := strings.TrimSpace(res.Text()); out != "" && out != "(no output)" {
			body = r.preview(r.st.dim, lastLines(out, 6))
		}
	default:
		if out := strings.TrimSpace(res.Text()); out != "" {
			body = r.preview(r.st.dim, firstLines(out, 4))
		}
	}
	if body != "" {
		head += "\n" + body
	}
	return item(r.gutter(kind, iconStyle, head))
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
		l = truncateWidth(l, r.width-4)
		switch {
		case strings.HasPrefix(l, "@@"):
			out[i] = r.st.hunk.Render(l)
		case strings.HasPrefix(l, "+"):
			out[i] = r.st.add.Render(l)
		case strings.HasPrefix(l, "-"):
			out[i] = r.st.del.Render(l)
		default:
			out[i] = r.st.dim.Render(l)
		}
	}
	s := strings.Join(out, "\n")
	if more > 0 {
		s += "\n" + r.st.dim.Render(fmt.Sprintf("… %d more diff lines (/last to see all)", more))
	}
	return s
}

// preview styles tool output lines for display under a tool's summary
// (the gutter supplies the indentation).
func (r *renderer) preview(style lipgloss.Style, text string) string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = style.Render(truncateWidth(l, r.width-4))
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
			out = append(out, item(r.gutter(iconCompacted, r.st.dim, r.st.dim.Render("earlier conversation compacted"))))
		default:
			if t := agent.MessageText(m); t != "" {
				out = append(out, item(r.st.dim.Render(firstLines(t, 3))))
			}
		}
	}
	return out
}

func (r *renderer) banner(s *coding.Session, resumed int) string {
	const name = "malachi"
	title := r.st.accent.Bold(true).Render(name)
	// Every line is truncated to the terminal before styling: truncation is
	// cell-based and must not have to reason about SGR sequences.
	info := fmt.Sprintf(" %s/%s · %s", s.Provider().Name, s.Model(), shortenHome(s.Cwd()))
	info = truncateLeft(info, r.width-lipgloss.Width(name)-1)
	help := "enter send · alt+enter newline · esc cancel · /help commands · ctrl+c quit"
	b := title + r.st.dim.Render(info) + "\n" + r.st.dim.Render(truncateWidth(help, r.width))
	if resumed > 0 {
		line := fmt.Sprintf("resumed %s (%d messages)", filepath.Base(s.Path()), resumed)
		b += "\n" + r.st.dim.Render(truncateWidth(line, r.width))
	}
	return b
}

// perLine styles each line separately; rendering a multi-line block in one
// call pads every line to the widest one, leaving trailing blanks in
// the transcript.
func perLine(style lipgloss.Style, text string) string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = style.Render(l)
	}
	return strings.Join(lines, "\n")
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
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	if w == 1 {
		return "…"
	}
	runes := []rune(s)
	for len(runes) > 0 && lipgloss.Width(string(runes)) > w-1 {
		runes = runes[:len(runes)-1]
	}
	return string(runes) + "…"
}

// truncateLeft keeps the end of s (the informative part of a path) within w.
func truncateLeft(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	if w == 1 {
		return "…"
	}
	runes := []rune(s)
	for len(runes) > 0 && lipgloss.Width(string(runes)) > w-1 {
		runes = runes[1:]
	}
	return "…" + string(runes)
}

// wordWrap wraps text at spaces to width, hard-breaking words that are
// longer than a line.
func wordWrap(s string, width int) []string {
	width = max(width, 10)
	var out []string
	for _, para := range strings.Split(s, "\n") {
		line := ""
		for _, word := range strings.Fields(para) {
			for lipgloss.Width(word) > width {
				if line != "" {
					out = append(out, line)
					line = ""
				}
				out = append(out, string([]rune(word)[:width]))
				word = string([]rune(word)[width:])
			}
			switch {
			case line == "":
				line = word
			case lipgloss.Width(line)+1+lipgloss.Width(word) <= width:
				line += " " + word
			default:
				out = append(out, line)
				line = word
			}
		}
		out = append(out, line)
	}
	return out
}

// wrapLines hard-wraps text to width for the live area, counting display
// cells so CJK and emoji don't overflow.
func wrapLines(s string, width int) []string {
	if width < 10 {
		width = 10
	}
	var out []string
	for _, line := range strings.Split(s, "\n") {
		var cur strings.Builder
		curW := 0
		for _, r := range line {
			w := lipgloss.Width(string(r))
			if curW > 0 && curW+w > width {
				out = append(out, cur.String())
				cur.Reset()
				curW = 0
			}
			cur.WriteRune(r)
			curW += w
		}
		out = append(out, cur.String())
	}
	return out
}

func shortenHome(p string) string {
	if home, err := filepath.Abs(homeDir()); err == nil && home != "" && strings.HasPrefix(p, home) {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}
