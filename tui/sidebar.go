package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/ddombrow/malachi/coding"
)

// The sidebar shows what the sandbox lets model-run tools touch, beside the
// transcript. It is drawn from the session's state and redrawn when the
// session reports a change (SandboxChangedEvent); the UI keeps no sandbox
// rules of its own.
const (
	// sidebarWidth is the columns the sidebar takes, its border included.
	sidebarWidth = 32
	// sidebarMinWidth is the narrowest terminal that shows it: below this
	// the transcript would be too cramped to give columns away.
	sidebarMinWidth = 100
	// sidebarBorder separates the sidebar from the transcript.
	sidebarBorder = "│ "
)

const (
	keySidebar = "ctrl+s"
	keyNetwork = "ctrl+o"
)

// sidebarState is what the sidebar draws, computed when the sandbox changes
// rather than every frame (resolving the default roots touches the disk).
type sidebarState struct {
	st     coding.SandboxState
	grants coding.SandboxGrants
}

func (m *model) syncSandbox(st coding.SandboxState) {
	m.side = sidebarState{st: st, grants: st.Grants()}
}

// sidebarShown reports whether the sidebar is on screen: wanted, and the
// terminal roomy enough for it.
func (m *model) sidebarShown() bool {
	return !m.sidebarHidden && m.width >= sidebarMinWidth && m.height >= chromeRows+2
}

// contentWidth is the columns the transcript gets.
func (m *model) contentWidth() int {
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

// sidebarNetworkRow is the sidebar row showing the network, which a click
// toggles.
const sidebarNetworkRow = 1

// sidebarLines renders the sidebar as exactly height lines, each
// sidebarWidth columns wide.
func (m *model) sidebarLines(height int) []string {
	st, g := m.side.st, m.side.grants
	inner := sidebarWidth - lipgloss.Width(sidebarBorder)
	s := m.r.st
	var rows []string
	add := func(line string) { rows = append(rows, line) }
	// pair puts label on the left and value on the right of a row.
	pair := func(label, value string) string {
		gap := max(1, inner-lipgloss.Width(label)-lipgloss.Width(value))
		return label + strings.Repeat(" ", gap) + value
	}
	path := func(p string) string {
		return "  " + truncateLeft(shortenHome(p), inner-2)
	}

	switch {
	case !st.Enabled:
		add(pair(s.accent.Render("sandbox"), s.toolErr.Render("off")))
	case st.Unavailable != nil:
		add(pair(s.accent.Render("sandbox"), s.toolErr.Render("unavailable")))
	default:
		add(pair(s.accent.Render("sandbox"), s.toolOK.Render("on")))
	}
	if !st.Enabled {
		add(s.dim.Render("tools reach everything"))
		add(s.dim.Render("you can"))
	} else {
		network := s.toolOK.Render("off")
		if st.Network {
			network = s.toolRun.Render("on")
		}
		add(pair("network", network+s.dim.Render(" ^o"))) // sidebarNetworkRow
		if st.Unavailable != nil {
			add(s.toolErr.Render("commands will fail"))
		}
		add("")
		scope := s.dim.Render(" anywhere")
		if len(st.Private) > 0 {
			scope = s.dim.Render(" in ~: only")
		}
		add("read" + scope)
		for _, r := range g.Read {
			add(path(r))
		}
		if g.BuiltinRead > 0 {
			add(s.dim.Render(fmt.Sprintf("  + %d toolchain dirs", g.BuiltinRead)))
		}
		add("")
		add("write")
		for _, w := range g.Write {
			add(path(w))
		}
		if g.BuiltinWrite > 0 {
			add(s.dim.Render(fmt.Sprintf("  + %d cache & temp dirs", g.BuiltinWrite)))
		}
		add("")
		add(pair("hidden", s.dim.Render(fmt.Sprintf("%d paths", len(st.Hidden)))))
	}
	add("")
	add(s.dim.Render("/sandbox · ^s hide"))

	if len(rows) > height {
		rows = rows[:max(0, height)]
		if height > 0 {
			rows[height-1] = s.dim.Render("  …")
		}
	}
	for len(rows) < height {
		rows = append(rows, "")
	}
	border := s.dim.Render(sidebarBorder)
	for i, r := range rows {
		r = ansi.Truncate(r, inner, "…")
		rows[i] = border + r + strings.Repeat(" ", max(0, inner-lipgloss.Width(r)))
	}
	return rows
}

// withSidebar puts the sidebar to the right of the transcript's lines.
func (m *model) withSidebar(transcript string) string {
	if !m.sidebarShown() {
		return transcript
	}
	lines := strings.Split(transcript, "\n")
	side := m.sidebarLines(len(lines))
	w := m.contentWidth()
	for i, l := range lines {
		l = ansi.Truncate(l, w, "")
		lines[i] = l + strings.Repeat(" ", max(0, w-lipgloss.Width(l))) + side[i]
	}
	return strings.Join(lines, "\n")
}

// sidebarHit reports whether a mouse position falls on the sidebar.
func (m *model) sidebarHit(x, y int) bool {
	return m.sidebarShown() && x >= m.contentWidth() && y >= 0 && y < m.vp.Height()
}
