package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/ddombrow/malachi/coding"
)

// The sidebar is a panel beside the transcript showing the session's
// surroundings, one section after another: the sandbox, project trust,
// Code Mode, the diff pane and MCP servers. Each section draws from session state cached in
// sidebarState and refreshed from the session's events; the UI keeps no
// rules of its own.
const (
	// sidebarWidth is the columns the sidebar takes, its border included.
	sidebarWidth = 32
	// sidebarMinWidth is the narrowest terminal that shows it: below this
	// the transcript would be too cramped to give columns away.
	sidebarMinWidth = 100
	// sidebarBorder separates the sidebar from the transcript.
	sidebarBorder = "│ "
	// sidebarInner is the columns a section's rows get: all but the border.
	sidebarInner = sidebarWidth - 2
)

const (
	keySidebar  = "ctrl+s"
	keyNetwork  = "ctrl+o"
	keyCodeMode = "ctrl+x"
)

// sidebarState is what the sections draw, computed when its source changes
// rather than every frame (resolving the sandbox's default roots touches
// the disk).
type sidebarState struct {
	sandbox coding.SandboxState
	grants  coding.SandboxGrants
	mcp     coding.MCPState
	jobs    []coding.JobState
}

// sideRow is one row of the sidebar. A row with click set does something
// when clicked.
type sideRow struct {
	text  string
	click func() tea.Cmd
}

func (m *model) syncSandbox(st coding.SandboxState) {
	m.side.sandbox, m.side.grants = st, st.Grants()
}

// sidebarShown reports whether the sidebar is on screen: wanted, and the
// terminal roomy enough for it.
func (m *model) sidebarShown() bool {
	return !m.sidebarHidden && !m.dp.open && m.width >= sidebarMinWidth && m.height >= chromeRows+2
}

// contentWidth is the columns the transcript gets. A diff pane that covers
// the transcript leaves it its width, for when the pane closes.
func (m *model) contentWidth() int {
	if m.dp.open && m.diffSplit() {
		return m.width - m.diffPaneWidth()
	}
	if m.sidebarShown() {
		return m.width - sidebarWidth
	}
	return m.width
}

// toggleSidebar shows or hides the sidebar. The transcript re-wraps to its
// new width, so a selection's positions go stale.
func (m *model) toggleSidebar() {
	m.sidebarHidden = !m.sidebarHidden
	m.r = newRenderer(m.contentWidth(), m.isDark, m.s.Settings().Icons, m.s.Cwd())
	m.sel = selection{}
}

// toggleNetwork switches commands' network access, as /network does.
func (m *model) toggleNetwork() tea.Cmd {
	if m.s.Sandbox().Network {
		return m.networkCommand("off")
	}
	return m.networkCommand("on")
}

// cycleCodeMode moves to the next code mode, off → on → explore, and says
// what the model is now offered.
func (m *model) cycleCodeMode() tea.Cmd {
	mode, err := m.s.CycleCodeMode()
	if err != nil {
		return m.printErr(err)
	}
	return m.printDim(codeModeLine(mode))
}

// setCodeMode switches to mode, as /codemode does, and says what the
// model is now offered.
func (m *model) setCodeMode(mode string) tea.Cmd {
	if err := m.s.SetCodeMode(mode); err != nil {
		return m.printErr(err)
	}
	return m.printDim(codeModeLine(mode))
}

func codeModeLine(mode string) string {
	return "code mode " + mode + ": " + codeModeMeaning[mode] + " (from the next request)"
}

// codeModeCommand sets the code mode, or with no argument opens a picker
// of the modes.
func (m *model) codeModeCommand(arg string) tea.Cmd {
	if arg != "" {
		return m.setCodeMode(strings.ToLower(arg))
	}
	var items []pickerItem
	for _, mode := range coding.CodeModes() {
		items = append(items, pickerItem{label: mode, detail: codeModeMeaning[mode], value: mode})
	}
	m.picker = newPicker("code mode", m.s.CodeMode().Mode, items, m.setCodeMode)
	return nil
}

var codeModeMeaning = map[string]string{
	coding.CodeModeOff:     "no script tool",
	coding.CodeModeOn:      "the script tool beside the other tools",
	coding.CodeModeExplore: "read, grep and glob only through scripts",
}

// sidebarSections are the sidebar's sections, top to bottom.
func (m *model) sidebarSections() [][]sideRow {
	return [][]sideRow{m.sandboxSection(), m.trustSection(), m.codeModeSection(), m.changesSection(), m.mcpSection(), m.jobsSection()}
}

// changesSection points at the diff pane, which takes the sidebar's place
// while it is open; clicking it opens the pane.
func (m *model) changesSection() []sideRow {
	s := m.r.st
	return []sideRow{{
		text:  sidePair(s.accent.Render("changes"), s.dim.Render("^g ")+s.toolOK.Render("show")),
		click: m.toggleDiff,
	}}
}

// sidePair puts label on the left of a row and value on the right.
func sidePair(label, value string) string {
	gap := max(1, sidebarInner-lipgloss.Width(label)-lipgloss.Width(value))
	return label + strings.Repeat(" ", gap) + value
}

// sidePath is a path listed under a heading, its end kept when too long.
func sidePath(p string) string {
	return "  " + truncateLeft(shortenHome(p), sidebarInner-2)
}

// sandboxSection shows what the sandbox lets model-run tools touch.
func (m *model) sandboxSection() []sideRow {
	st, g, s := m.side.sandbox, m.side.grants, m.r.st
	var rows []sideRow
	add := func(text string) { rows = append(rows, sideRow{text: text}) }

	switch {
	case !st.Enabled:
		add(sidePair(s.accent.Render("sandbox"), s.toolErr.Render("off")))
		add(s.dim.Render("tools reach everything"))
		add(s.dim.Render("you can"))
		return rows
	case st.Unavailable != nil:
		add(sidePair(s.accent.Render("sandbox"), s.toolErr.Render("unavailable")))
		add(s.toolErr.Render("commands will fail"))
	default:
		add(sidePair(s.accent.Render("sandbox"), s.toolOK.Render("on")))
	}
	network := s.toolOK.Render("off")
	if st.Network {
		network = s.toolRun.Render("on")
	}
	rows = append(rows, sideRow{text: sidePair("network", s.dim.Render("^o ")+network), click: m.toggleNetwork})
	scope := s.dim.Render(" anywhere")
	if len(st.Private) > 0 {
		scope = s.dim.Render(" in ~: only")
	}
	add("read" + scope)
	for _, r := range g.Read {
		add(sidePath(r))
	}
	if g.BuiltinRead > 0 {
		add(s.dim.Render(fmt.Sprintf("  + %d toolchain dirs", g.BuiltinRead)))
	}
	add("write")
	for _, w := range g.Write {
		add(sidePath(w))
	}
	if g.BuiltinWrite > 0 {
		add(s.dim.Render(fmt.Sprintf("  + %d cache & temp dirs", g.BuiltinWrite)))
	}
	add(sidePair("hidden", s.dim.Render(fmt.Sprintf("%d paths", len(st.Hidden)))))
	return rows
}

// trustSection shows whether the project's instruction files are in the
// prompt, and what decided it. It reads the session's trust state directly:
// that is held in memory, and only this frontend's /trust changes it. It
// offers no click: trusting a project is a decision to make by typing.
func (m *model) trustSection() []sideRow {
	t, s := m.s.TrustState(), m.r.st
	var rows []sideRow
	add := func(text string) { rows = append(rows, sideRow{text: text}) }
	files := fmt.Sprintf("  %d instruction file", t.Resources.Total)
	if t.Resources.Total != 1 {
		files += "s"
	}
	switch {
	case t.Resources.Empty():
		add(sidePair(s.accent.Render("trust"), s.dim.Render("no project files")))
		return rows
	case t.Trusted():
		add(sidePair(s.accent.Render("trust"), s.toolOK.Render("yes")))
		add(s.dim.Render(files + " loaded"))
	case t.Pending:
		add(sidePair(s.accent.Render("trust"), s.toolRun.Render("undecided")))
		add(s.dim.Render(files + " withheld"))
		add(s.dim.Render("  /trust to decide"))
		return rows
	default:
		add(sidePair(s.accent.Render("trust"), s.toolErr.Render("no")))
		add(s.dim.Render(files + " withheld"))
	}
	add(s.dim.Render("  " + trustSourceText(t)))
	return rows
}

// codeModeSection shows whether the script tool is offered and what scripts
// have done in this session.
func (m *model) codeModeSection() []sideRow {
	cm, s := m.s.CodeMode(), m.r.st
	var rows []sideRow
	add := func(text string) { rows = append(rows, sideRow{text: text}) }
	value := s.toolOK.Render(cm.Mode)
	if !cm.Enabled {
		value = s.dim.Render(cm.Mode)
	}
	rows = append(rows, sideRow{text: sidePair(s.accent.Render("code mode"), s.dim.Render("^x ")+value), click: m.cycleCodeMode})
	if !cm.Enabled {
		return rows
	}
	if cm.Mode == coding.CodeModeExplore {
		add(s.dim.Render("  read, grep, glob in scripts"))
	}
	if cm.Scripts == 0 {
		add(s.dim.Render("  no scripts yet"))
		return rows
	}
	add(s.dim.Render("  " + plural(cm.Scripts, "script") + " · " + plural(cm.Calls, "call")))
	var failed []string
	if cm.Failed > 0 {
		failed = append(failed, plural(cm.Failed, "script"))
	}
	if cm.CallErrors > 0 {
		failed = append(failed, plural(cm.CallErrors, "call"))
	}
	if len(failed) > 0 {
		add(s.toolErr.Render("  failed: " + strings.Join(failed, ", ")))
	}
	return rows
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// trustSourceText says where a trust decision came from.
func trustSourceText(t coding.TrustState) string {
	switch t.Source {
	case "run":
		return "for this run (-trust)"
	case "session":
		return "for this session"
	case "saved":
		return "saved for this directory"
	case "inherited":
		return "saved for " + shortenHome(t.InheritedFrom)
	case "always", "never":
		return "projectTrust: " + t.Source
	}
	return t.Source
}

// sidebarRows lays the sidebar out as exactly height rows: a top row with
// malachi's version and the close button, a divider, then the sections with
// a blank row after each.
func (m *model) sidebarRows(height int) []sideRow {
	closeBtn := m.r.st.dim.Render("^s ") + m.r.st.accent.Render("×")
	version := ansi.Truncate("malachi "+coding.Version, sidebarInner-lipgloss.Width(closeBtn)-1, "…")
	closeRow := sideRow{
		text:  sidePair(m.r.st.dim.Render(version), closeBtn),
		click: func() tea.Cmd { m.toggleSidebar(); return nil },
	}
	divider := sideRow{text: m.r.st.dim.Render(strings.Repeat("─", sidebarInner))}
	rows := []sideRow{closeRow, divider}
	for _, sec := range m.sidebarSections() {
		rows = append(rows, sec...)
		rows = append(rows, sideRow{})
	}
	if len(rows) > height {
		rows = rows[:max(0, height)]
		if height > 0 {
			rows[height-1] = sideRow{text: m.r.st.dim.Render("  …")}
		}
	}
	for len(rows) < height {
		rows = append(rows, sideRow{})
	}
	return rows
}

// sidebarLines renders the sidebar as exactly height lines, each
// sidebarWidth columns wide.
func (m *model) sidebarLines(height int) []string {
	border := m.r.st.dim.Render(sidebarBorder)
	var lines []string
	for _, r := range m.sidebarRows(height) {
		text := ansi.Truncate(r.text, sidebarInner, "…")
		lines = append(lines, border+text+strings.Repeat(" ", max(0, sidebarInner-lipgloss.Width(text))))
	}
	return lines
}

// sidebarClick runs whatever the clicked sidebar row does.
func (m *model) sidebarClick(y int) tea.Cmd {
	rows := m.sidebarRows(m.vp.Height())
	if y < 0 || y >= len(rows) || rows[y].click == nil {
		return nil
	}
	return rows[y].click()
}

// withSidebar puts the sidebar, or the diff pane, to the right of the
// transcript's lines; a pane on a narrow terminal takes their place.
func (m *model) withSidebar(transcript string) string {
	if m.dp.open {
		lines := strings.Split(transcript, "\n")
		pane := m.diffLines(len(lines))
		if m.diffCovers() {
			return strings.Join(pane, "\n")
		}
		for i, l := range lines {
			lines[i] = l + pane[i]
		}
		return strings.Join(lines, "\n")
	}
	if !m.sidebarShown() {
		return transcript
	}
	// The scroller's rows are each exactly the content width already.
	lines := strings.Split(transcript, "\n")
	side := m.sidebarLines(len(lines))
	for i, l := range lines {
		lines[i] = l + side[i]
	}
	return strings.Join(lines, "\n")
}

// sidebarHit reports whether a mouse position falls on the sidebar.
func (m *model) sidebarHit(x, y int) bool {
	return m.sidebarShown() && x >= m.contentWidth() && y >= 0 && y < m.vp.Height()
}
