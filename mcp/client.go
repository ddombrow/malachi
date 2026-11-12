// Package mcp is a client for the Model Context Protocol over a stream:
// newline-delimited JSON-RPC to a server malachi started itself. It speaks
// the stateless protocol (2026-07-28, every request carrying its version
// and capabilities) and falls back to the initialize handshake of the
// 2025 versions for servers that predate it.
//
// It asks for tools only. malachi declares no client capabilities, so
// servers have no way to ask it for anything: no sampling, elicitation or
// roots. It imports agent only.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"
)

// Protocol versions this client speaks, newest first. Modern versions carry
// their metadata on each request; legacy ones need the initialize
// handshake.
var (
	ModernVersions = []string{"2026-07-28"}
	LegacyVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}
)

// Request metadata keys of the stateless protocol.
const (
	metaVersion      = "io.modelcontextprotocol/protocolVersion"
	metaCapabilities = "io.modelcontextprotocol/clientCapabilities"
	metaClientInfo   = "io.modelcontextprotocol/clientInfo"
	metaServerInfo   = "io.modelcontextprotocol/serverInfo"
)

// DefaultProbeTimeout is how long Connect waits for an answer to its
// server/discover probe before treating the server as one that predates it:
// some never answer a request that arrives before initialize.
const DefaultProbeTimeout = 5 * time.Second

// Implementation names a client or server.
type Implementation struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

// Options configure Connect.
type Options struct {
	// Client identifies malachi to the server.
	Client Implementation
	// ProbeTimeout overrides DefaultProbeTimeout.
	ProbeTimeout time.Duration
	// OnToolsChanged is called when the server says its tools changed; call
	// ListTools again to see them. It runs on the connection's reader, so
	// it must not block.
	OnToolsChanged func()

	// eof, if set, is why the connection ended when the server's output
	// does: Start sets it to the exit status and stderr.
	eof func() error
	// legacyOnly skips the probe, for tests of the initialize path against
	// servers that speak both.
	legacyOnly bool
}

// Client is a connection to one server.
type Client struct {
	c    *conn
	opts Options

	version      string
	modern       bool
	server       Implementation
	instructions string
	listChanged  bool

	mu       sync.Mutex
	progress map[string]func(Progress)
	nextTok  int
	listen   context.CancelFunc
}

// Progress is a progress notification for a tool call.
type Progress struct {
	Progress float64 `json:"progress"`
	Total    float64 `json:"total,omitempty"`
	Message  string  `json:"message,omitempty"`
}

// Connect negotiates a protocol version over r and w and returns the client.
// It probes with server/discover; a server that answers is modern, one that
// refuses the version offers its own, and anything else (an error, or
// silence until the probe times out) means a server that needs initialize.
func Connect(ctx context.Context, r io.Reader, w io.Writer, opts Options) (*Client, error) {
	if opts.ProbeTimeout <= 0 {
		opts.ProbeTimeout = DefaultProbeTimeout
	}
	cl := &Client{c: newConn(r, w, opts.eof), opts: opts, progress: map[string]func(Progress){}}
	cl.c.onNotify = cl.notified
	cl.c.onRequest = cl.requested
	if err := cl.negotiate(ctx); err != nil {
		cl.c.shut(err)
		return nil, err
	}
	if cl.modern && cl.listChanged && opts.OnToolsChanged != nil {
		cl.subscribe()
	}
	return cl, nil
}

func (cl *Client) negotiate(ctx context.Context) error {
	if cl.opts.legacyOnly {
		return cl.initialize(ctx, LegacyVersions[0])
	}
	return cl.discover(ctx, ModernVersions[0], true)
}

// discover probes with version. retry allows one more probe with a modern
// version the server names instead.
func (cl *Client) discover(ctx context.Context, version string, retry bool) error {
	pctx, cancel := context.WithTimeout(ctx, cl.opts.ProbeTimeout)
	raw, err := cl.c.request(pctx, "server/discover", map[string]any{"_meta": cl.meta(version)})
	cancel()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var rpcErr *Error
	switch {
	case err == nil:
		var res struct {
			SupportedVersions []string `json:"supportedVersions"`
			Capabilities      struct {
				Tools *struct {
					ListChanged bool `json:"listChanged"`
				} `json:"tools"`
			} `json:"capabilities"`
			Instructions string                     `json:"instructions"`
			Meta         map[string]json.RawMessage `json:"_meta"`
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			return fmt.Errorf("mcp: server/discover: %w", err)
		}
		v, ok := pick(ModernVersions, res.SupportedVersions)
		if !ok {
			// A modern server whose versions are all legacy, or none we know.
			if lv, ok := pick(LegacyVersions, res.SupportedVersions); ok {
				return cl.initialize(ctx, lv)
			}
			return fmt.Errorf("mcp: server supports protocol versions %v; malachi speaks %v", res.SupportedVersions, append(ModernVersions, LegacyVersions...))
		}
		if res.Capabilities.Tools == nil {
			return errors.New("mcp: server offers no tools")
		}
		cl.version, cl.modern = v, true
		cl.instructions = res.Instructions
		cl.listChanged = res.Capabilities.Tools.ListChanged
		if info, ok := res.Meta[metaServerInfo]; ok {
			_ = json.Unmarshal(info, &cl.server)
		}
		return nil
	case errors.As(err, &rpcErr) && rpcErr.Code == CodeUnsupportedVersion:
		var data struct {
			Supported []string `json:"supported"`
		}
		_ = json.Unmarshal(rpcErr.Data, &data)
		if v, ok := pick(ModernVersions, data.Supported); ok && v != version && retry {
			return cl.discover(ctx, v, false)
		}
		if v, ok := pick(LegacyVersions, data.Supported); ok {
			return cl.initialize(ctx, v)
		}
		return fmt.Errorf("mcp: server supports protocol versions %v; malachi speaks %v", data.Supported, append(ModernVersions, LegacyVersions...))
	case errors.As(err, new(closedError)):
		return err // already says why: how the server exited, and its stderr
	case errors.Is(err, ErrClosed) || cl.c.isClosed():
		return fmt.Errorf("mcp: server closed the connection before answering: %w", err)
	}
	// Any other error, or no answer: a server from before server/discover.
	return cl.initialize(ctx, LegacyVersions[0])
}

// initialize runs the legacy handshake, offering version.
func (cl *Client) initialize(ctx context.Context, version string) error {
	raw, err := cl.c.request(ctx, "initialize", map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{},
		"clientInfo":      cl.opts.Client,
	})
	if err != nil {
		return fmt.Errorf("mcp: initialize: %w", err)
	}
	var res struct {
		ProtocolVersion string `json:"protocolVersion"`
		Capabilities    struct {
			Tools *struct {
				ListChanged bool `json:"listChanged"`
			} `json:"tools"`
		} `json:"capabilities"`
		ServerInfo   Implementation `json:"serverInfo"`
		Instructions string         `json:"instructions"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("mcp: initialize: %w", err)
	}
	if !slices.Contains(LegacyVersions, res.ProtocolVersion) {
		return fmt.Errorf("mcp: server answered with protocol version %q; malachi speaks %v", res.ProtocolVersion, append(ModernVersions, LegacyVersions...))
	}
	if res.Capabilities.Tools == nil {
		return errors.New("mcp: server offers no tools")
	}
	cl.version, cl.server, cl.instructions = res.ProtocolVersion, res.ServerInfo, res.Instructions
	cl.listChanged = res.Capabilities.Tools.ListChanged
	return cl.c.notify("notifications/initialized", nil)
}

// pick returns the first of ours (newest first) that theirs includes.
func pick(ours, theirs []string) (string, bool) {
	for _, v := range ours {
		if slices.Contains(theirs, v) {
			return v, true
		}
	}
	return "", false
}

// meta is the metadata every modern request carries.
func (cl *Client) meta(version string) map[string]any {
	return map[string]any{
		metaVersion:      version,
		metaCapabilities: map[string]any{},
		metaClientInfo:   cl.opts.Client,
	}
}

// params adds the request metadata to p for a modern server.
func (cl *Client) params(p map[string]any) map[string]any {
	if p == nil {
		p = map[string]any{}
	}
	if cl.modern {
		meta, _ := p["_meta"].(map[string]any)
		if meta == nil {
			meta = map[string]any{}
		}
		for k, v := range cl.meta(cl.version) {
			meta[k] = v
		}
		p["_meta"] = meta
	}
	return p
}

// subscribe opens the long-lived subscriptions/listen request through which
// a modern server reports tool list changes.
func (cl *Client) subscribe() {
	ctx, cancel := context.WithCancel(context.Background())
	cl.mu.Lock()
	cl.listen = cancel
	cl.mu.Unlock()
	go func() {
		_, _ = cl.c.request(ctx, "subscriptions/listen", cl.params(map[string]any{
			"notifications": map[string]any{"toolsListChanged": true},
		}))
	}()
}

func (cl *Client) notified(method string, params json.RawMessage) {
	switch method {
	case "notifications/tools/list_changed":
		if cl.opts.OnToolsChanged != nil {
			cl.opts.OnToolsChanged()
		}
	case "notifications/progress":
		var p struct {
			ProgressToken json.RawMessage `json:"progressToken"`
			Progress
		}
		if json.Unmarshal(params, &p) != nil {
			return
		}
		cl.mu.Lock()
		fn := cl.progress[string(p.ProgressToken)]
		cl.mu.Unlock()
		if fn != nil {
			fn(p.Progress)
		}
	}
	// Log messages, resource updates and the rest are not asked for.
}

// requested answers a legacy server's requests. malachi declares no
// capabilities, so the only one it expects is ping.
func (cl *Client) requested(method string, _ json.RawMessage) (any, *Error) {
	if method == "ping" {
		return map[string]any{}, nil
	}
	return nil, &Error{Code: CodeMethodNotFound, Message: "malachi does not support " + method}
}

// Version is the negotiated protocol version.
func (cl *Client) Version() string { return cl.version }

// Server is how the server identified itself.
func (cl *Client) Server() Implementation { return cl.server }

// Instructions are the server's instructions for using it, if any.
func (cl *Client) Instructions() string { return cl.instructions }

// Done is closed when the connection ends; Err then says why.
func (cl *Client) Done() <-chan struct{} { return cl.c.done }

// Err is why the connection ended, or nil while it is open.
func (cl *Client) Err() error {
	cl.c.mu.Lock()
	defer cl.c.mu.Unlock()
	return cl.c.err
}

// Close ends the connection. It does not stop the server process; see
// Server.Close for that.
func (cl *Client) Close() {
	cl.mu.Lock()
	if cl.listen != nil {
		cl.listen()
	}
	cl.mu.Unlock()
	cl.c.shut(ErrClosed)
}

func (c *conn) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}
