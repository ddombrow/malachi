package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ddombrow/malachi/agent/session"
	"github.com/ddombrow/malachi/coding"
)

// Options configure a server.
type Options struct {
	// Session is the template every session is opened from: Home, Model,
	// ThinkingLevel, Trust, Sandbox and Network come from malachi's flags.
	// Cwd, Resume and ExtraMCPServers are set per session.
	Session coding.Options
	// Open opens a session; nil means coding.Open. Tests inject providers
	// through it.
	Open func(coding.Options) (*coding.Session, error)
	// Log receives one line per notable event (ignored editor MCP servers).
	// nil means os.Stderr, which editors keep in their agent logs.
	Log io.Writer
}

// Server serves ACP sessions on one connection.
type Server struct {
	c    *conn
	opts Options

	mu       sync.Mutex
	client   clientCapabilities
	sessions map[string]*acpSession
}

// New returns a server reading the client's messages from in and writing
// to out.
func New(opts Options, in io.Reader, out io.Writer) *Server {
	if opts.Open == nil {
		opts.Open = coding.Open
	}
	if opts.Log == nil {
		opts.Log = os.Stderr
	}
	if opts.Session.Home == "" {
		opts.Session.Home = coding.Home()
	}
	return &Server{c: newConn(in, out), opts: opts, sessions: map[string]*acpSession{}}
}

// Run serves the client until in reaches EOF, reading fails, or ctx ends.
// However it ends, it stops every session's work, answers the prompts still
// waiting, and closes the sessions before returning. It returns nil at EOF.
func (sv *Server) Run(ctx context.Context) error {
	err := sv.c.serve(ctx, sv)
	sv.mu.Lock()
	all := make([]*acpSession, 0, len(sv.sessions))
	for _, as := range sv.sessions {
		all = append(all, as)
	}
	sv.mu.Unlock()
	for _, as := range all {
		as.s.Abort()
	}
	sv.c.wait()
	for _, as := range all {
		as.close()
	}
	return err
}

func (sv *Server) logf(format string, args ...any) {
	fmt.Fprintf(sv.opts.Log, "malachi acp: "+format+"\n", args...)
}

func (sv *Server) clientCaps() clientCapabilities {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	return sv.client
}

func decode(params json.RawMessage, v any) error {
	if len(params) == 0 {
		params = []byte("{}")
	}
	if err := json.Unmarshal(params, v); err != nil {
		return invalidParams(err)
	}
	return nil
}

func (sv *Server) request(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case "initialize":
		var p initializeParams
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		sv.mu.Lock()
		sv.client = p.ClientCapabilities
		sv.mu.Unlock()
		return initializeResult{
			ProtocolVersion: protocolVersion,
			AgentCapabilities: agentCapabilities{
				LoadSession:         true,
				PromptCapabilities:  promptCapabilities{EmbeddedContext: true},
				SessionCapabilities: sessionCapabilities{List: &struct{}{}, AdditionalDirectories: &struct{}{}},
			},
			AgentInfo:   implementation{Name: "malachi", Title: "malachi", Version: coding.Version},
			AuthMethods: []any{},
		}, nil
	case "authenticate":
		return struct{}{}, nil // no methods are offered; keys live in ~/.malachi
	case "session/new":
		var p newSessionParams
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		as, err := sv.open(p.Cwd, "", p.MCPServers, p.AdditionalDirectories)
		if err != nil {
			return nil, err
		}
		return followed{newSessionResult{SessionID: as.id, ConfigOptions: as.configOptions()}, as.opened}, nil
	case "session/load":
		var p loadSessionParams
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		if !validSessionID(p.SessionID) {
			return nil, invalidParams(fmt.Errorf("unknown session id %q", p.SessionID))
		}
		as, err := sv.open(p.Cwd, p.SessionID, p.MCPServers, p.AdditionalDirectories)
		if err != nil {
			return nil, err
		}
		as.replay()
		return followed{loadSessionResult{ConfigOptions: as.configOptions()}, as.opened}, nil
	case "session/list":
		var p listSessionsParams
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		return sv.list(p.Cwd)
	case "session/prompt":
		var p promptParams
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		as, err := sv.session(p.SessionID)
		if err != nil {
			return nil, err
		}
		if name, arg, ok := slashCommand(p.Prompt); ok {
			as.command(name, arg)
			return promptResult{StopReason: stopEndTurn}, nil
		}
		reason, err := as.prompt(ctx, promptText(p.Prompt))
		if err != nil {
			return nil, err
		}
		return promptResult{StopReason: reason}, nil
	case "session/set_config_option":
		var p setConfigOptionParams
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		as, err := sv.session(p.SessionID)
		if err != nil {
			return nil, err
		}
		var value string
		if err := json.Unmarshal(p.Value, &value); err != nil {
			return nil, invalidParams(errors.New("value must be a string"))
		}
		if err := as.setConfig(p.ConfigID, value); err != nil {
			return nil, err
		}
		return setConfigOptionResult{ConfigOptions: as.configOptions()}, nil
	}
	return nil, &Error{Code: codeMethodNotFound, Message: "method not found: " + method}
}

func (sv *Server) notify(method string, params json.RawMessage) {
	if method != "session/cancel" {
		return // unknown notifications are ignored
	}
	var p cancelParams
	if decode(params, &p) != nil {
		return
	}
	if as, err := sv.session(p.SessionID); err == nil {
		as.cancel()
	}
}

func (sv *Server) session(id string) (*acpSession, error) {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	if as := sv.sessions[id]; as != nil {
		return as, nil
	}
	return nil, invalidParams(fmt.Errorf("unknown session %q", id))
}

// validSessionID accepts what Session.ID returns: a session file's name,
// never a path. Resuming takes paths too, which a client must not reach.
func validSessionID(id string) bool {
	return id != "" && id == filepath.Base(id) && !strings.HasPrefix(id, ".") && !strings.ContainsAny(id, `/\`)
}

// open opens a new session in cwd, or resumes the one called resume.
func (sv *Server) open(cwd, resume string, servers []mcpServer, dirs []string) (*acpSession, error) {
	if !filepath.IsAbs(cwd) {
		return nil, invalidParams(fmt.Errorf("cwd must be an absolute path, not %q", cwd))
	}
	for _, d := range dirs {
		if !filepath.IsAbs(d) {
			return nil, invalidParams(fmt.Errorf("additional directories must be absolute paths, not %q", d))
		}
	}
	opts := sv.opts.Session
	opts.Cwd, opts.Resume = cwd, resume
	settings := opts.Settings
	if settings == nil {
		var err error
		if settings, err = coding.LoadSettings(opts.Home); err != nil {
			return nil, err
		}
		opts.Settings = settings
	}
	opts.ExtraMCPServers = sv.editorServers(settings, servers)

	s, err := sv.opts.Open(opts)
	if err != nil {
		return nil, err
	}
	if resume != "" && s.ID() != resume {
		s.Close()
		return nil, invalidParams(fmt.Errorf("unknown session id %q", resume))
	}
	as := newSession(sv, s)
	as.addDirectories(dirs)
	sv.mu.Lock()
	old := sv.sessions[as.id]
	sv.sessions[as.id] = as
	sv.mu.Unlock()
	if old != nil {
		// Loading a session already open here replaces it.
		old.s.Abort()
		old.close()
	}
	as.attach()
	go as.fetchModels()
	return as, nil
}

// unsafeName matches what an MCP server name may not contain (coding's
// rule); an editor's names are made to fit.
var unsafeName = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

// editorServers turns the MCP servers a client passed into mcp.json
// entries, when settings allow them. Only names are ever logged: env
// values are secrets.
func (sv *Server) editorServers(settings *coding.Settings, servers []mcpServer) map[string]coding.MCPServerConfig {
	if len(servers) == 0 {
		return nil
	}
	if !settings.ACP.EditorMCPServers {
		names := make([]string, len(servers))
		for i, m := range servers {
			names[i] = m.Name
		}
		sv.logf("ignoring the editor's MCP servers (%s); set \"acp\": {\"editorMcpServers\": true} in settings.json to run them", strings.Join(names, ", "))
		return nil
	}
	out := map[string]coding.MCPServerConfig{}
	for _, m := range servers {
		name := strings.Trim(unsafeName.ReplaceAllString(m.Name, "-"), "-")
		if len(name) > 32 {
			name = name[:32]
		}
		if name == "" {
			name = "editor"
		}
		c := coding.MCPServerConfig{Command: m.Command, Args: m.Args, Type: m.Type, URL: m.URL}
		if len(m.Env) > 0 {
			c.Env = map[string]string{}
			for _, e := range m.Env {
				c.Env[e.Name] = e.Value
			}
		}
		out[name] = c // a remote entry is refused by coding's rules, and shows as failed
	}
	return out
}

// list answers session/list for cwd, the process's directory when empty.
func (sv *Server) list(cwd string) (listSessionsResult, error) {
	if cwd == "" {
		wd, err := os.Getwd()
		if err != nil {
			return listSessionsResult{}, err
		}
		cwd = wd
	}
	if !filepath.IsAbs(cwd) {
		return listSessionsResult{}, invalidParams(fmt.Errorf("cwd must be an absolute path, not %q", cwd))
	}
	infos, err := session.List(coding.SessionsDir(sv.opts.Session.Home, cwd))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return listSessionsResult{}, err
	}
	out := listSessionsResult{Sessions: []sessionInfo{}}
	sort.SliceStable(infos, func(i, j int) bool { return infos[i].Modified.After(infos[j].Modified) })
	for _, in := range infos {
		if in.Messages == 0 {
			continue
		}
		title := in.Title
		if title == "" {
			title = firstLine(in.Preview, 80)
		}
		out.Sessions = append(out.Sessions, sessionInfo{
			SessionID: strings.TrimSuffix(filepath.Base(in.Path), ".jsonl"),
			Cwd:       cwd,
			Title:     title,
			UpdatedAt: in.Modified.UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}

// promptText flattens a prompt into the text malachi sends. A resource link
// becomes its path, for the model to read; an embedded resource is quoted
// with its uri.
func promptText(blocks []contentBlock) string {
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		case "resource_link":
			parts = append(parts, "@"+uriPath(b.URI))
		case "resource":
			if b.Resource == nil || b.Resource.Text == "" {
				if b.Resource != nil {
					parts = append(parts, "@"+uriPath(b.Resource.URI))
				}
				continue
			}
			fence := "```"
			for strings.Contains(b.Resource.Text, fence) {
				fence += "`"
			}
			parts = append(parts, fmt.Sprintf("%s\n%s\n%s\n%s", uriPath(b.Resource.URI), fence, strings.TrimRight(b.Resource.Text, "\n"), fence))
		}
	}
	return strings.Join(parts, "\n\n")
}

// uriPath is a file URI's path, or the URI itself.
func uriPath(uri string) string {
	if p, ok := strings.CutPrefix(uri, "file://"); ok {
		return p
	}
	return uri
}

func firstLine(s string, n int) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
