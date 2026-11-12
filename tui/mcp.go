package tui

import (
	"context"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/ddombrow/malachi/coding"
)

// syncMCP caches the servers' state for the sidebar: computing it resolves
// each server's policy, which touches the disk.
func (m *model) syncMCP(st coding.MCPState) {
	m.side.mcp = st
}

// mcpStatus is a server's status as one short styled word or count.
func mcpStatus(st styles, srv coding.MCPServerState) string {
	switch srv.Status {
	case coding.MCPReady:
		if srv.Stale {
			return st.toolRun.Render("restarts on use")
		}
		return st.toolOK.Render(plural(len(srv.Tools), "tool"))
	case coding.MCPStarting:
		return st.toolRun.Render("starting")
	case coding.MCPFailed, coding.MCPInvalid:
		return st.toolErr.Render(srv.Status)
	}
	return st.dim.Render(srv.Status)
}

// mcpSection shows each configured server's status; clicking one prints its
// details. It is left out when no servers are configured.
func (m *model) mcpSection() []sideRow {
	mc, s := m.side.mcp, m.r.st
	if len(mc.Servers) == 0 && mc.LoadError == "" {
		return nil
	}
	var rows []sideRow
	add := func(text string) { rows = append(rows, sideRow{text: text}) }
	ready := 0
	for _, srv := range mc.Servers {
		if srv.Status == coding.MCPReady {
			ready++
		}
	}
	if mc.LoadError != "" {
		add(sidePair(s.accent.Render("mcp"), s.toolErr.Render("mcp.json unreadable")))
		add(s.toolErr.Render("  " + truncateLeft(mc.LoadError, sidebarInner-2)))
		return rows
	}
	add(sidePair(s.accent.Render("mcp"), s.dim.Render(fmt.Sprintf("%d/%d ready", ready, len(mc.Servers)))))
	for _, srv := range mc.Servers {
		status := mcpStatus(s, srv)
		name := truncateEnd("  "+srv.Name, sidebarInner-lipgloss.Width(status)-1)
		rows = append(rows, sideRow{text: sidePair(name, status), click: m.mcpDetailsCmd(srv.Name)})
		if srv.Error != "" && (srv.Status == coding.MCPFailed || srv.Status == coding.MCPInvalid) {
			add(s.dim.Render(truncateEnd("    "+errorGist(srv.Error), sidebarInner)))
		}
	}
	add(s.dim.Render("  /mcp for details"))
	return rows
}

func (m *model) mcpDetailsCmd(name string) func() tea.Cmd {
	return func() tea.Cmd { return m.mcpCommand(name) }
}

// mcpCommand is /mcp: every server's details, one server's, or restarting
// one.
func (m *model) mcpCommand(arg string) tea.Cmd {
	verb, name, _ := strings.Cut(arg, " ")
	if verb == "restart" {
		name = strings.TrimSpace(name)
		if name == "" {
			return m.printErr(fmt.Errorf("usage: /mcp restart <server>"))
		}
		s := m.s
		return tea.Batch(m.printDim("restarting "+name+"…"), func() tea.Msg {
			if err := s.RestartMCP(context.Background(), name); err != nil {
				return printMsg{render: func(r *renderer) string {
					return item(r.gutter(iconError, r.st.errorText, r.st.errorText.Render(name+": "+err.Error())))
				}}
			}
			return printMsg{render: func(r *renderer) string { return item(r.st.dim.Render(name + " restarted")) }}
		})
	}
	st := m.s.MCPState()
	m.syncMCP(st)
	if st.LoadError != "" {
		return m.printErr(fmt.Errorf("%s: %s", shortenHome(st.Config), st.LoadError))
	}
	if len(st.Servers) == 0 {
		return m.printDim("no MCP servers: configure them in " + shortenHome(st.Config))
	}
	servers := st.Servers
	if arg != "" {
		servers = nil
		for _, srv := range st.Servers {
			if srv.Name == arg {
				servers = append(servers, srv)
			}
		}
		if servers == nil {
			return m.printErr(fmt.Errorf("no MCP server named %s in %s", arg, shortenHome(st.Config)))
		}
	}
	return m.print(func(r *renderer) string {
		var blocks []string
		for _, srv := range servers {
			blocks = append(blocks, mcpDetails(r, srv))
		}
		if arg == "" {
			blocks = append(blocks, r.st.dim.Render("configured in "+shortenHome(st.Config)+" · /mcp restart <server>"))
		}
		return item(strings.Join(blocks, "\n\n"))
	})
}

// mcpDetails is everything about one server, for /mcp.
func mcpDetails(r *renderer, srv coding.MCPServerState) string {
	st := r.st
	lines := []string{st.accent.Render(srv.Name) + "  " + mcpStatus(st, srv)}
	line := func(label, value string) {
		if value != "" {
			lines = append(lines, st.dim.Render(fmt.Sprintf("  %-9s", label))+value)
		}
	}
	if srv.Error != "" {
		lines = append(lines, "  "+st.toolErr.Render(srv.Error))
	}
	if srv.Server != "" {
		line("server", srv.Server+" · protocol "+srv.Version)
	}
	if srv.Command != "" {
		line("command", shortenHome(strings.Join(append([]string{srv.Command}, srv.Args...), " ")))
	}
	line("reads", shortenHomes(srv.Readable))
	line("writes", shortenHomes(srv.Writable))
	switch {
	case srv.Status == coding.MCPDisabled || srv.Status == coding.MCPInvalid:
	case srv.Deferred:
		line("offered", "from scripts, as tools.mcp__"+srv.Name+"__<tool>")
	default:
		line("offered", "directly, as mcp__"+srv.Name+"__<tool>")
	}
	line("tools", strings.Join(srv.Tools, ", "))
	if srv.Restarts > 0 {
		line("restarts", fmt.Sprint(srv.Restarts))
	}
	return strings.Join(lines, "\n")
}

func shortenHomes(paths []string) string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = shortenHome(p)
	}
	return strings.Join(out, ", ")
}

// errorGist is the line of a server's error worth a sidebar row: the last
// thing it wrote to stderr, which is usually why it stopped, else the
// error's first line.
func errorGist(s string) string {
	if _, stderr, ok := strings.Cut(s, "server stderr:\n"); ok {
		lines := strings.Split(strings.TrimSpace(stderr), "\n")
		if last := strings.TrimSpace(lines[len(lines)-1]); last != "" {
			return last
		}
	}
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

// truncateEnd keeps the start of s within w columns.
func truncateEnd(s string, w int) string { return ansi.Truncate(s, max(w, 1), "…") }
