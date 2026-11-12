package coding

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/mcp"
	"github.com/ddombrow/malachi/sandbox"
)

// mcpStartTimeout bounds starting a server: launching it, negotiating, and
// listing its tools.
const mcpStartTimeout = 30 * time.Second

// A server that failed to start is not retried until a backoff has passed,
// doubling with each consecutive failure, so a broken server does not cost
// every script and run a start attempt.
const (
	mcpRetryMin = 5 * time.Second
	mcpRetryMax = 5 * time.Minute
)

func mcpBackoff(failures int) time.Duration {
	d := mcpRetryMin
	for i := 1; i < failures && d < mcpRetryMax; i++ {
		d *= 2
	}
	return min(d, mcpRetryMax)
}

// MCP server statuses.
const (
	MCPDisabled = "disabled" // "disabled": true in mcp.json
	MCPInvalid  = "invalid"  // the configuration is refused; Error says why
	MCPStarting = "starting"
	MCPReady    = "ready"
	MCPFailed   = "failed" // it failed to start or died; the next use retries
	MCPStopped  = "stopped"
)

// MCPServerState is what one server is doing.
type MCPServerState struct {
	Name   string
	Status string
	Error  string
	// Command is the resolved program, the one that actually runs.
	Command string
	Args    []string
	// Server and Version are how it identified itself and the protocol
	// version negotiated.
	Server, Version string
	Tools           []string
	// Readable and Writable are its own roots, beyond the session's.
	Readable, Writable []string
	Deferred           bool
	// Restarts counts restarts for policy changes and crashes.
	Restarts int
	// Stale is set when the sandbox policy changed since it started: it
	// restarts on its next use.
	Stale bool
}

// MCPManager runs the configured servers, each a long-running command under
// the session's sandbox plus its own roots. Servers start in the background
// and restart on their next use when the policy they started under changes
// (an OS sandbox is fixed when a process starts) or when they died.
type MCPManager struct {
	cwd      string
	toolOpts ToolOptions
	diag     *Diagnostics
	life     context.Context
	stop     context.CancelFunc

	mu       sync.Mutex
	policy   func() sandbox.Policy
	onChange func()
	codeMode bool // whether deferred servers' tools can go through scripts
	servers  []*mcpServer
	loadErr  error
}

type mcpServer struct {
	name string
	cfg  MCPServerConfig
	path string // resolved command
	err  error  // why the configuration is refused

	startMu sync.Mutex // held while starting, so callers wait for one start

	mu       sync.Mutex
	status   string
	lastErr  error
	proc     *mcp.Process
	started  sandbox.Policy // the policy proc runs under
	tools    []mcp.Tool
	restarts int
	// failures counts consecutive failed starts, failedAt the last one.
	failures int
	failedAt time.Time
}

func newMCPManager(cfg MCPConfig, loadErr error, cwd string, toolOpts ToolOptions, policy func() sandbox.Policy, diag *Diagnostics) *MCPManager {
	life, stop := context.WithCancel(context.Background())
	m := &MCPManager{cwd: cwd, toolOpts: toolOpts, diag: diag, life: life, stop: stop, policy: policy, loadErr: loadErr}
	for _, name := range cfg.Names() {
		c := cfg.Servers[name]
		srv := &mcpServer{name: name, cfg: c, status: MCPStarting}
		switch path, err := checkServer(name, c, cwd); {
		case c.Disabled:
			srv.status = MCPDisabled
		case err != nil:
			srv.status, srv.err = MCPInvalid, err
		default:
			srv.path = path
		}
		m.servers = append(m.servers, srv)
	}
	return m
}

// startAll starts every runnable server in the background.
func (m *MCPManager) startAll() {
	for _, srv := range m.servers {
		if srv.runnable() {
			go func() { _, _ = m.ensure(m.life, srv) }()
		}
	}
}

func (srv *mcpServer) runnable() bool { return srv.err == nil && !srv.cfg.Disabled }

// bind points the manager at a session: its policy, its listener for
// changes, and whether it has Code Mode. A session handing its servers to
// the one that replaces it rebinds them.
func (m *MCPManager) bind(policy func() sandbox.Policy, onChange func(), codeMode bool) {
	m.mu.Lock()
	m.policy, m.onChange, m.codeMode = policy, onChange, codeMode
	m.mu.Unlock()
}

func (m *MCPManager) changed() {
	m.mu.Lock()
	fn := m.onChange
	m.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// LoadError is why mcp.json could not be read, if it could not.
func (m *MCPManager) LoadError() error { return m.loadErr }

// policyFor is the sandbox a server runs under: the session's, plus its own
// roots and its program's directories. Its roots never reach bash or the
// file tools; hidden paths stay hidden whatever the roots say.
func (m *MCPManager) policyFor(srv *mcpServer) sandbox.Policy {
	m.mu.Lock()
	p := m.policy()
	m.mu.Unlock()
	p.Readable = slices.Clone(p.Readable)
	p.Writable = slices.Clone(p.Writable)
	if !p.Enabled {
		return p
	}
	for _, r := range srv.cfg.Readable {
		p.Readable = append(p.Readable, sandbox.Canonical(resolvePath(m.cwd, r)))
	}
	for _, w := range srv.cfg.Writable {
		p.Writable = append(p.Writable, sandbox.Canonical(resolvePath(m.cwd, w)))
	}
	p.Readable = append(p.Readable, commandRoots(srv.path)...)
	return p
}

// commandRoots are the directories a server's program needs to read to run:
// its own directory and, through a symlink, the installed package it points
// into (an npm package's node_modules, a Python virtualenv).
func commandRoots(path string) []string {
	roots := []string{sandbox.Canonical(filepath.Dir(path))}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return roots
	}
	roots = append(roots, filepath.Dir(real))
	sep := string(filepath.Separator)
	if i := strings.Index(real, sep+"node_modules"+sep); i >= 0 {
		roots = append(roots, real[:i+len(sep+"node_modules")])
	}
	for dir, n := filepath.Dir(real), 0; n < 4 && dir != filepath.Dir(dir); dir, n = filepath.Dir(dir), n+1 {
		if _, err := os.Stat(filepath.Join(dir, "pyvenv.cfg")); err == nil {
			roots = append(roots, dir)
			break
		}
	}
	return roots
}

// ensure returns srv's process, started under the current policy: it
// starts a server that is not running, and restarts one that died or whose
// policy changed.
func (m *MCPManager) ensure(ctx context.Context, srv *mcpServer) (*mcp.Process, error) {
	if !srv.runnable() {
		if srv.err != nil {
			return nil, srv.err
		}
		return nil, fmt.Errorf("server %s is disabled in %s", srv.name, MCPConfigFile)
	}
	srv.startMu.Lock()
	defer srv.startMu.Unlock()
	pol := m.policyFor(srv)

	srv.mu.Lock()
	proc, started := srv.proc, srv.started
	if proc == nil && srv.failures > 0 {
		if wait := mcpBackoff(srv.failures) - time.Since(srv.failedAt); wait > 0 {
			err := srv.lastErr
			srv.mu.Unlock()
			return nil, fmt.Errorf("%w (retrying in %s)", err, wait.Round(time.Second))
		}
	}
	srv.mu.Unlock()
	if proc != nil {
		select {
		case <-proc.Done():
			m.diag.LogMCPServer(srv.name, "died", proc.Err())
		default:
			if reflect.DeepEqual(started, pol) {
				return proc, nil
			}
			m.diag.LogMCPServer(srv.name, "restart: sandbox policy changed", nil)
		}
		proc.Close()
		srv.mu.Lock()
		srv.proc = nil
		srv.restarts++
		srv.mu.Unlock()
	}

	srv.setStatus(MCPStarting, nil)
	m.changed()
	proc, tools, err := m.start(ctx, srv, pol)
	if err != nil {
		srv.mu.Lock()
		// A start cut short by its caller says nothing about the server.
		if ctx.Err() == nil {
			srv.failures++
			srv.failedAt = time.Now()
		}
		srv.mu.Unlock()
		srv.setStatus(MCPFailed, err)
		m.diag.LogMCPServer(srv.name, "start failed", err)
		m.changed()
		return nil, err
	}
	srv.mu.Lock()
	srv.proc, srv.started, srv.tools = proc, pol, tools
	srv.status, srv.lastErr, srv.failures = MCPReady, nil, 0
	srv.mu.Unlock()
	m.changed()
	return proc, nil
}

// start launches srv under pol and lists its tools.
func (m *MCPManager) start(ctx context.Context, srv *mcpServer, pol sandbox.Policy) (*mcp.Process, []mcp.Tool, error) {
	env, err := serverEnv(srv.cfg, m.toolOpts)
	if err != nil {
		return nil, nil, err
	}
	// The process lives as long as the manager, not the call that started
	// it.
	cmd, err := sandbox.Command(m.life, pol, srv.path, srv.cfg.Args...)
	if err != nil {
		return nil, nil, err
	}
	cmd.Dir, cmd.Env = m.cwd, env
	sctx, cancel := context.WithTimeout(ctx, mcpStartTimeout)
	defer cancel()
	proc, err := mcp.Start(sctx, cmd, mcp.Options{
		Client:         mcp.Implementation{Name: "malachi", Version: Version},
		OnToolsChanged: func() { go m.refresh(srv) },
	})
	if err != nil {
		return nil, nil, err
	}
	tools, err := m.listTools(sctx, srv, proc)
	if err != nil {
		proc.Close()
		return nil, nil, err
	}
	return proc, tools, nil
}

// listTools lists a server's tools, keeping only its allowlist if it has
// one.
func (m *MCPManager) listTools(ctx context.Context, srv *mcpServer, proc *mcp.Process) ([]mcp.Tool, error) {
	tools, err := proc.ListTools(ctx)
	if err != nil {
		return nil, err
	}
	if len(srv.cfg.Tools) > 0 {
		tools = slices.DeleteFunc(tools, func(t mcp.Tool) bool { return !slices.Contains(srv.cfg.Tools, t.Name) })
	}
	return tools, nil
}

// refresh relists a server's tools after it said they changed.
func (m *MCPManager) refresh(srv *mcpServer) {
	srv.mu.Lock()
	proc := srv.proc
	srv.mu.Unlock()
	if proc == nil {
		return
	}
	ctx, cancel := context.WithTimeout(m.life, mcpStartTimeout)
	defer cancel()
	tools, err := m.listTools(ctx, srv, proc)
	if err != nil {
		return
	}
	srv.mu.Lock()
	if srv.proc == proc {
		srv.tools = tools
	}
	srv.mu.Unlock()
	m.changed()
}

func (srv *mcpServer) setStatus(status string, err error) {
	srv.mu.Lock()
	srv.status, srv.lastErr = status, err
	srv.mu.Unlock()
}

func (m *MCPManager) server(name string) *mcpServer {
	for _, srv := range m.servers {
		if srv.name == name {
			return srv
		}
	}
	return nil
}

// Tools returns a server's tools, waiting for it to start if it is
// starting.
func (m *MCPManager) Tools(ctx context.Context, name string) ([]mcp.Tool, error) {
	srv := m.server(name)
	if srv == nil {
		return nil, fmt.Errorf("no MCP server named %s", name)
	}
	if _, err := m.ensure(ctx, srv); err != nil {
		return nil, err
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	return slices.Clone(srv.tools), nil
}

// Call calls a tool on a server, starting or restarting the server first if
// it needs it. A tool error is returned as a failed result
// (agent.ResultError), with the server and tool in its details.
func (m *MCPManager) Call(ctx context.Context, name, tool string, args map[string]any, onProgress func(mcp.Progress)) (agent.ToolResult, error) {
	srv := m.server(name)
	if srv == nil {
		return agent.ToolResult{}, fmt.Errorf("no MCP server named %s", name)
	}
	proc, err := m.ensure(ctx, srv)
	if err != nil {
		return agent.ToolResult{}, fmt.Errorf("MCP server %s: %w", name, err)
	}
	srv.mu.Lock()
	offered := slices.ContainsFunc(srv.tools, func(t mcp.Tool) bool { return t.Name == tool })
	srv.mu.Unlock()
	if !offered {
		return agent.ToolResult{}, fmt.Errorf("MCP server %s offers no tool %s", name, tool)
	}
	res, err := proc.CallTool(ctx, tool, args, onProgress)
	if err != nil {
		if errors.Is(err, mcp.ErrClosed) {
			srv.setStatus(MCPFailed, err)
			m.changed()
		}
		return agent.ToolResult{}, fmt.Errorf("MCP server %s: %w", name, err)
	}
	out := res.Result()
	details, _ := out.Details.(map[string]any)
	if details == nil {
		details = map[string]any{}
	}
	details["server"], details["tool"] = name, tool
	out.Details = details
	if res.IsError {
		return agent.ToolResult{}, agent.FailWith(out)
	}
	return out, nil
}

// State reports every configured server.
func (m *MCPManager) State() []MCPServerState {
	out := make([]MCPServerState, 0, len(m.servers))
	for _, srv := range m.servers {
		out = append(out, srv.state(m))
	}
	return out
}

func (srv *mcpServer) state(m *MCPManager) MCPServerState {
	var now sandbox.Policy
	deferred := false
	if srv.runnable() {
		now = m.policyFor(srv)
		deferred = m.deferred(srv)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	st := MCPServerState{
		Name: srv.name, Status: srv.status, Command: srv.path, Args: srv.cfg.Args,
		Readable: srv.cfg.Readable, Writable: srv.cfg.Writable, Restarts: srv.restarts,
		Deferred: deferred,
	}
	switch {
	case srv.err != nil:
		st.Error = srv.err.Error()
	case srv.lastErr != nil:
		st.Error = srv.lastErr.Error()
	}
	if srv.proc != nil {
		st.Server, st.Version = srv.proc.Server().Name, srv.proc.Version()
		st.Stale = !reflect.DeepEqual(srv.started, now)
	}
	for _, t := range srv.tools {
		st.Tools = append(st.Tools, t.Name)
	}
	return st
}

// Restart stops a server and starts it again under the current policy,
// whatever its backoff: the user asking is reason enough to try.
func (m *MCPManager) Restart(ctx context.Context, name string) error {
	srv := m.server(name)
	if srv == nil {
		return fmt.Errorf("no MCP server named %s", name)
	}
	if !srv.runnable() {
		_, err := m.ensure(ctx, srv) // says why it cannot run
		return err
	}
	srv.startMu.Lock()
	srv.mu.Lock()
	proc := srv.proc
	srv.proc, srv.failures = nil, 0
	if proc != nil {
		srv.restarts++
	}
	srv.mu.Unlock()
	srv.startMu.Unlock()
	if proc != nil {
		m.diag.LogMCPServer(srv.name, "restart: asked by the user", nil)
		proc.Close()
	}
	_, err := m.ensure(ctx, srv)
	return err
}

// Close stops every server, each by closing its input first, as the spec's
// graceful shutdown asks; then the manager's context ends, killing any
// that remain.
func (m *MCPManager) Close() {
	defer m.stop()
	var wg sync.WaitGroup
	for _, srv := range m.servers {
		srv.mu.Lock()
		proc := srv.proc
		srv.proc = nil
		if srv.runnable() {
			srv.status = MCPStopped
		}
		srv.mu.Unlock()
		if proc != nil {
			wg.Go(proc.Close)
		}
	}
	wg.Wait()
}
