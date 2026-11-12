package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/ddombrow/malachi/coding"
)

func mcpState() coding.MCPState {
	return coding.MCPState{Config: "/home/u/.malachi/mcp.json", Servers: []coding.MCPServerState{
		{Name: "time", Status: coding.MCPReady, Tools: []string{"get_current_time", "convert_time"}, Deferred: true,
			Server: "mcp-time", Version: "2025-06-18", Command: "/opt/bin/mcp-server-time"},
		{Name: "memory", Status: coding.MCPStarting, Writable: []string{"/data/mem"}},
		{Name: "github", Status: coding.MCPFailed, Error: "missing GITHUB_TOKEN\nmore"},
		{Name: "stale", Status: coding.MCPReady, Stale: true, Tools: []string{"x"}},
		{Name: "fs", Status: coding.MCPFailed, Error: "mcp: server exited: exit status 1\nserver stderr:\nWarning: skipping\nError: no directories\n"},
	}}
}

func TestSidebarShowsMCPServers(t *testing.T) {
	m := newTestModel(t)
	if strings.Contains(sidebarText(m), "mcp") {
		t.Fatalf("an MCP section without servers:\n%s", sidebarText(m))
	}
	m.handleEvent(coding.MCPChangedEvent{State: mcpState()})
	text := ansi.Strip(strings.Join(m.sidebarLines(60), "\n"))
	for _, want := range []string{"2/5 ready", "Error: no directories", "time", "2 tools", "memory", "starting", "github", "failed",
		"missing GITHUB_TOKEN", "restarts on use", "/mcp for details"} {
		if !strings.Contains(text, want) {
			t.Errorf("sidebar lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "more") {
		t.Errorf("an error's later lines are in the sidebar:\n%s", text)
	}
}

func TestMCPDetails(t *testing.T) {
	m := newTestModel(t)
	got := ansi.Strip(mcpDetails(m.r, mcpState().Servers[0]))
	for _, want := range []string{"time  2 tools", "mcp-time · protocol 2025-06-18", "/opt/bin/mcp-server-time",
		"from scripts, as tools.mcp__time__<tool>", "get_current_time, convert_time"} {
		if !strings.Contains(got, want) {
			t.Errorf("details lack %q:\n%s", want, got)
		}
	}
	got = ansi.Strip(mcpDetails(m.r, mcpState().Servers[1]))
	if !strings.Contains(got, "/data/mem") || !strings.Contains(got, "directly, as mcp__memory__<tool>") {
		t.Errorf("details:\n%s", got)
	}
}

func TestMCPCommandWithoutServers(t *testing.T) {
	m := newTestModel(t)
	out := shown(m, func() tea.Cmd { return m.command("/mcp") })
	if !strings.Contains(out, "no MCP servers: configure them in") || !strings.Contains(out, "mcp.json") {
		t.Errorf("/mcp: %q", out)
	}
	out = shown(m, func() tea.Cmd { return m.command("/mcp restart") })
	if !strings.Contains(out, "usage: /mcp restart <server>") {
		t.Errorf("/mcp restart: %q", out)
	}
}
