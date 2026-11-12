package coding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/mcp"
)

// mcpToolPrefix starts the name of every MCP tool: mcp__<server>__<tool>.
const mcpToolPrefix = "mcp__"

// maxToolName is the longest tool name every provider accepts.
const maxToolName = 64

// mcpToolNames names a server's tools as the model sees them,
// mcp__<server>__<tool>, in the characters providers accept. A name that
// had to change, or would collide, gets a short hash of the original.
func mcpToolNames(server string, tools []mcp.Tool) []string {
	names := make([]string, len(tools))
	seen := map[string]bool{}
	for i, t := range tools {
		safe := strings.Map(func(r rune) rune {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
				return r
			}
			return '_'
		}, t.Name)
		name := mcpToolPrefix + server + "__" + safe
		if len(name) > maxToolName || seen[name] {
			sum := sha256.Sum256([]byte(t.Name))
			tag := "_" + hex.EncodeToString(sum[:4])
			name = name[:min(len(name), maxToolName-len(tag))] + tag
		}
		seen[name] = true
		names[i] = name
	}
	return names
}

// mcpServerOf returns the server a tool name belongs to, if it is an MCP
// tool's name.
func mcpServerOf(name string) (string, bool) {
	rest, ok := strings.CutPrefix(name, mcpToolPrefix)
	if !ok {
		return "", false
	}
	server, _, ok := strings.Cut(rest, "__")
	return server, ok
}

// deferred reports whether srv's tools go through Code Mode rather than to
// the model directly. Without Code Mode there is no other way to reach
// them, so they go directly.
func (m *MCPManager) deferred(srv *mcpServer) bool {
	m.mu.Lock()
	codeMode := m.codeMode
	m.mu.Unlock()
	return codeMode && (srv.cfg.Deferred == nil || *srv.cfg.Deferred)
}

// await waits, bounded by ctx, for the servers whose deferral is deferred
// to be running under the current policy: starting ones finish starting,
// stale ones restart, and failed ones retry once their backoff has passed.
// Failures are left in the servers' states. It reports how many servers
// it waited for.
func (m *MCPManager) await(ctx context.Context, deferred bool) int {
	var wg sync.WaitGroup
	n := 0
	for _, srv := range m.servers {
		if srv.runnable() && m.deferred(srv) == deferred {
			n++
			wg.Go(func() { _, _ = m.ensure(ctx, srv) })
		}
	}
	wg.Wait()
	return n
}

// AgentTools returns the tools of the running servers whose deferral is
// deferred, as agent tools, from their last listing: it never waits.
func (m *MCPManager) AgentTools(deferred bool) []*agent.Tool {
	var out []*agent.Tool
	for _, srv := range m.servers {
		if !srv.runnable() || m.deferred(srv) != deferred {
			continue
		}
		srv.mu.Lock()
		tools := srv.tools
		running := srv.proc != nil
		srv.mu.Unlock()
		if !running {
			continue
		}
		for i, name := range mcpToolNames(srv.name, tools) {
			out = append(out, m.agentTool(srv.name, name, tools[i]))
		}
	}
	return out
}

// agentTool wraps one server tool for the agent: a call goes through Call,
// so it starts or restarts the server as needed, and its output is kept to
// the limits the built-in tools keep.
func (m *MCPManager) agentTool(server, name string, t mcp.Tool) *agent.Tool {
	label := t.Title
	if label == "" && t.Annotations != nil {
		label = t.Annotations.Title
	}
	if label == "" {
		label = t.Name
	}
	params := t.InputSchema
	if params == nil {
		params = map[string]any{"type": "object"}
	}
	return &agent.Tool{
		Name:        name,
		Label:       server + ": " + label,
		Description: t.Description,
		Parameters:  params,
		// A server's read-only hint is a claim, but a false one costs only
		// ordering: Parallel decides what runs together, not what may run.
		Parallel: t.ReadOnly(),
		Execute: func(ctx context.Context, _ string, args map[string]any, onUpdate func(agent.ToolResult)) (agent.ToolResult, error) {
			progress := func(p mcp.Progress) {
				if onUpdate == nil {
					return
				}
				text := p.Message
				if p.Total > 0 {
					text = strings.TrimSpace(fmt.Sprintf("%g/%g %s", p.Progress, p.Total, p.Message))
				}
				onUpdate(agent.TextResult(text))
			}
			res, err := m.Call(ctx, server, t.Name, args, progress)
			var failed *agent.ResultError
			switch {
			case err == nil:
				return m.fitOutput(res), nil
			case errors.As(err, &failed):
				return agent.ToolResult{}, agent.FailWith(m.fitOutput(failed.Result))
			}
			return res, err
		},
	}
}

// fitOutput keeps a result's text within the display limits, the rest in a
// spill file; images and other content stay as they are.
func (m *MCPManager) fitOutput(r agent.ToolResult) agent.ToolResult {
	shown, cut := fitText(r.Text(), m.toolOpts.SpillDir, "malachi-mcp-*.log")
	if !cut {
		return r
	}
	content := []agent.Content{&agent.TextContent{Text: shown}}
	for _, c := range r.Content {
		if _, isText := c.(*agent.TextContent); !isText {
			content = append(content, c)
		}
	}
	r.Content = content
	return r
}

// missing explains why a script's call to an MCP tool found no such tool:
// its server is not running, or does not offer it.
func (m *MCPManager) missing(name string) string {
	server, ok := mcpServerOf(name)
	if !ok {
		return ""
	}
	srv := m.server(server)
	if srv == nil {
		return fmt.Sprintf("There is no MCP server named %s.", server)
	}
	if !m.deferred(srv) {
		return fmt.Sprintf("MCP server %s's tools are offered directly, not to scripts.", server)
	}
	st := srv.state(m)
	switch {
	case st.Error != "":
		return fmt.Sprintf("MCP server %s is %s: %s", server, st.Status, st.Error)
	case st.Status != MCPReady:
		return fmt.Sprintf("MCP server %s is %s.", server, st.Status)
	}
	return fmt.Sprintf("MCP server %s offers no such tool; searchTools(%q) lists its tools.", server, mcpToolPrefix+server)
}

// PromptServer is an MCP server as the system prompt describes it.
type PromptServer struct {
	Name, Description string
	// Deferred servers' tools are reached from scripts, not offered
	// directly.
	Deferred bool
}

// promptServers lists the servers that can run, for the system prompt. It
// depends only on the configuration, so the prompt stays the same as
// servers start, stop and restart.
func (m *MCPManager) promptServers() []PromptServer {
	var out []PromptServer
	for _, srv := range m.servers {
		if srv.runnable() {
			out = append(out, PromptServer{Name: srv.name, Description: srv.cfg.Description, Deferred: m.deferred(srv)})
		}
	}
	return out
}
