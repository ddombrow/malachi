package coding

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/ai/fake"
	"github.com/ddombrow/malachi/mcp"
	"github.com/ddombrow/malachi/sandbox"
)

func TestMCPToolNames(t *testing.T) {
	long := strings.Repeat("x", 80)
	got := mcpToolNames("srv", []mcp.Tool{{Name: "read_file"}, {Name: "files.list"}, {Name: "files_list"}, {Name: long}})
	if got[0] != "mcp__srv__read_file" || got[1] != "mcp__srv__files_list" {
		t.Errorf("names: %q", got)
	}
	if got[2] == got[1] || !strings.HasPrefix(got[2], "mcp__srv__files_list_") {
		t.Errorf("a colliding name kept its twin's: %q", got[2])
	}
	if len(got[3]) != maxToolName {
		t.Errorf("long name is %d bytes: %q", len(got[3]), got[3])
	}
	if s, ok := mcpServerOf(got[0]); !ok || s != "srv" {
		t.Errorf("server of %s: %q %v", got[0], s, ok)
	}
	if _, ok := mcpServerOf("read"); ok {
		t.Error("read is not an MCP tool")
	}
}

// A server that failed to start is not tried again on every use, but only
// once its backoff has passed.
func TestMCPFailedServerBacksOff(t *testing.T) {
	if mcpBackoff(1) != mcpRetryMin || mcpBackoff(2) != 2*mcpRetryMin || mcpBackoff(100) != mcpRetryMax {
		t.Errorf("backoff: %s %s %s", mcpBackoff(1), mcpBackoff(2), mcpBackoff(100))
	}
	cwd := t.TempDir()
	starts := filepath.Join(cwd, "starts")
	script := filepath.Join(cwd, "fail.sh")
	os.WriteFile(script, []byte("echo x >> "+starts+"; echo 'no token' >&2; exit 2\n"), 0o755)
	m := newTestManager(t, cwd, map[string]MCPServerConfig{"bad": {Command: "/bin/sh", Args: []string{script}}},
		func() sandbox.Policy { return sandbox.Policy{} }, ToolOptions{})
	for range 3 {
		if _, err := m.Call(context.Background(), "bad", "x", nil, nil); err == nil {
			t.Fatal("a failing server's call succeeded")
		}
	}
	_, err := m.Call(context.Background(), "bad", "x", nil, nil)
	if !strings.Contains(err.Error(), "no token") || !strings.Contains(err.Error(), "retrying in") {
		t.Errorf("error: %v", err)
	}
	if b, _ := os.ReadFile(starts); strings.Count(string(b), "x") != 1 {
		t.Errorf("started %d times", strings.Count(string(b), "x"))
	}
	m.servers[0].mu.Lock()
	m.servers[0].failedAt = time.Now().Add(-time.Hour)
	m.servers[0].mu.Unlock()
	m.Call(context.Background(), "bad", "x", nil, nil)
	if b, _ := os.ReadFile(starts); strings.Count(string(b), "x") != 2 {
		t.Errorf("after the backoff: started %d times", strings.Count(string(b), "x"))
	}
}

// mcpSession opens a session whose home configures a deferred server
// ("lazy") and a direct one ("direct"), both the fake.
func mcpSession(t *testing.T, codeMode bool, provider *fake.Provider) *Session {
	t.Helper()
	home, cwd := t.TempDir(), t.TempDir()
	server := func(extra string) string {
		return `{"command": "` + os.Args[0] + `", "env": {"` + fakeMCPEnv + `": "1"}` + extra + `}`
	}
	cfg := `{"mcpServers": {
		"lazy": ` + server(`, "description": "Reports\n what its sandbox allows."`) + `,
		"direct": ` + server(`, "deferred": false`) + `,
		"off": ` + server(`, "disabled": true`) + `}}`
	if err := os.WriteFile(filepath.Join(home, MCPConfigFile), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	off := false
	settings := &Settings{Sandbox: SandboxSettings{Enabled: &off}, CodeMode: &codeMode}
	s, err := Open(Options{Cwd: cwd, Home: home, Settings: settings, Provider: provider, NoSession: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func toolNames(tools []*agent.Tool) []string {
	var out []string
	for _, t := range tools {
		out = append(out, t.Name)
	}
	return out
}

// Deferred servers get a line in the system prompt and are reached from
// scripts; direct ones' tools are offered to the model, in time for the
// first run.
func TestSessionMCPTools(t *testing.T) {
	code := `
		const found = searchTools("mcp__lazy").map(t => t.name);
		const pid = await tools.mcp__lazy__pid({});
		const direct = await tools.mcp__direct__pid({});
		const off = await tools.mcp__off__pid({});
		return {found, pid: pid.isError ? pid.text : "ok", direct: direct.text.split(" Available")[0], off: off.text.split(" Available")[0]};`
	provider := fake.New(
		fake.ToolCalls("", fake.Call{ID: "c1", Name: "script", Args: map[string]any{"code": code}}),
		fake.Text("done"),
	)
	s := mcpSession(t, true, provider)
	if err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if len(provider.Requests) != 2 {
		t.Fatalf("%d requests", len(provider.Requests))
	}
	req := provider.Requests[0]
	for _, want := range []string{
		"MCP servers the user configured:\n- direct\n- lazy (script): Reports what its sandbox allows.\n",
		`searchTools("mcp__<server>")`,
	} {
		if !strings.Contains(req.System, want) {
			t.Errorf("system prompt lacks %q:\n%s", want, req.System)
		}
	}
	if strings.Contains(req.System, "- off") {
		t.Error("a disabled server is in the prompt")
	}
	names := toolNames(req.Tools)
	if !slices.Contains(names, "mcp__direct__pid") || slices.ContainsFunc(names, func(n string) bool { return strings.HasPrefix(n, "mcp__lazy") }) {
		t.Errorf("tools offered: %v", names)
	}

	var result *agent.ToolResultMessage
	for _, m := range s.Harness.Messages() {
		if r, ok := m.(*agent.ToolResultMessage); ok {
			result = r
		}
	}
	if result == nil || result.IsError {
		t.Fatalf("script result: %+v", result)
	}
	text := agent.ToolResult{Content: result.Content}.Text()
	for _, want := range []string{
		`"mcp__lazy__big"`, `"pid": "ok"`,
		`"direct": "Tool mcp__direct__pid not found. MCP server direct's tools are offered directly, not to scripts."`,
		`"off": "Tool mcp__off__pid not found. MCP server off is disabled."`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("script output lacks %s:\n%s", want, text)
		}
	}
}

// Without Code Mode a deferred server's tools could not be reached, so they
// are offered directly.
func TestSessionMCPWithoutCodeMode(t *testing.T) {
	provider := fake.New(fake.Text("done"))
	s := mcpSession(t, false, provider)
	if err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	names := toolNames(provider.Requests[0].Tools)
	if !slices.Contains(names, "mcp__lazy__pid") || !slices.Contains(names, "mcp__direct__pid") || slices.Contains(names, "script") {
		t.Errorf("tools offered: %v", names)
	}
	if strings.Contains(provider.Requests[0].System, "(script)") {
		t.Error("prompt sends tools to scripts without Code Mode")
	}
}

// A tool's output is kept to the limits the built-in tools keep, the rest
// in a spill file.
func TestMCPToolOutputIsCut(t *testing.T) {
	spill := t.TempDir()
	m := newTestManager(t, t.TempDir(), map[string]MCPServerConfig{"fake": fakeServerConfig(nil)},
		func() sandbox.Policy { return sandbox.Policy{} }, ToolOptions{SpillDir: spill})
	m.bind(func() sandbox.Policy { return sandbox.Policy{} }, nil, false)
	m.await(context.Background(), false)
	var big *agent.Tool
	for _, tl := range m.AgentTools(false) {
		if tl.Name == "mcp__fake__big" {
			big = tl
		}
	}
	if big == nil {
		t.Fatalf("tools: %v", toolNames(m.AgentTools(false)))
	}
	res, isErr := agent.ExecuteTool(context.Background(), big, &agent.ToolCall{ID: "1", Name: big.Name, Arguments: map[string]any{}}, func(agent.ToolResult) {})
	text := res.Text()
	if isErr || !strings.Contains(text, "[Showing lines 1-") || strings.Count(text, "\n") > MaxOutputLines+5 {
		t.Fatalf("result (%d lines): %.200q", strings.Count(text, "\n"), text)
	}
	if files, _ := filepath.Glob(filepath.Join(spill, "malachi-mcp-*.log")); len(files) != 1 {
		t.Errorf("spill files: %v", files)
	}
	if d, _ := res.Details.(map[string]any); d["server"] != "fake" || d["tool"] != "big" {
		t.Errorf("details: %v", res.Details)
	}
}

// Frontends hear each server change; a restart the user asks for happens
// now, whatever the backoff.
func TestSessionMCPEventsAndRestart(t *testing.T) {
	s := mcpSession(t, true, fake.New())
	events := make(chan MCPState, 64)
	s.Subscribe(func(e any) {
		if ev, ok := e.(MCPChangedEvent); ok {
			events <- ev.State
		}
	})
	ready := func(st MCPState, name string) (MCPServerState, bool) {
		for _, srv := range st.Servers {
			if srv.Name == name && srv.Status == MCPReady {
				return srv, true
			}
		}
		return MCPServerState{}, false
	}
	waitFor := func(what string, ok func(MCPState) bool) MCPState {
		t.Helper()
		if st := s.MCPState(); ok(st) {
			return st
		}
		timeout := time.After(time.Minute)
		for {
			select {
			case st := <-events:
				if ok(st) {
					return st
				}
			case <-timeout:
				t.Fatalf("no event: %s (now %+v)", what, s.MCPState())
			}
		}
	}
	waitFor("lazy ready", func(st MCPState) bool { _, ok := ready(st, "lazy"); return ok })
	if st := s.MCPState(); !strings.HasSuffix(st.Config, MCPConfigFile) || len(st.Servers) != 3 {
		t.Fatalf("state: %+v", st)
	}
	before, _ := callText(t, s.MCP(), "lazy", "pid", nil)
	if err := s.RestartMCP(context.Background(), "lazy"); err != nil {
		t.Fatal(err)
	}
	st := waitFor("restarted", func(st MCPState) bool { srv, ok := ready(st, "lazy"); return ok && srv.Restarts == 1 })
	if after, _ := callText(t, s.MCP(), "lazy", "pid", nil); after == before {
		t.Errorf("same process after a restart: %s (%+v)", after, st)
	}
	if err := s.RestartMCP(context.Background(), "off"); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Errorf("restarting a disabled server: %v", err)
	}
	if err := s.RestartMCP(context.Background(), "nope"); err == nil {
		t.Error("restarted a server that does not exist")
	}
}
