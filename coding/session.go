package coding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/agent/session"
)

// Options configure Open.
type Options struct {
	Cwd      string
	Home     string    // default Home()
	Settings *Settings // default LoadSettings(Home)
	// Model is a "provider/model" or "model" reference; empty uses the
	// resumed session's model, then settings defaults.
	Model         string
	ThinkingLevel string
	Continue      bool   // resume the newest session for Cwd
	Resume        string // resume a session by path or name prefix
	NoSession     bool   // keep the session in memory only
	// Provider, when set, is used instead of constructing one from the
	// resolved provider config (tests and embedding).
	Provider agent.Provider
}

// Session is the coding-agent environment around a Harness: tools rooted at
// a cwd, the assembled system prompt, provider/model selection, and push-based
// persistence to an append-only session file.
//
// Port of the core of tau_coding/session.py (CodingSession).
type Session struct {
	Harness *agent.Harness

	mu         sync.Mutex
	cwd        string
	home       string
	settings   *Settings
	provider   ProviderConfig
	model      string
	thinking   string
	file       *session.File // nil when NoSession
	header     []*session.Entry
	persistErr error
	unsub      func()
}

// SessionsDir returns the directory holding cwd's sessions, named like tau's
// project session directories: a readable slug plus a short path digest.
func SessionsDir(home, cwd string) string {
	abs, _ := filepath.Abs(cwd)
	sum := sha256.Sum256([]byte(abs))
	return filepath.Join(home, "sessions", slugify(abs)+"-"+hex.EncodeToString(sum[:])[:6])
}

var slugUnsafe = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func slugify(path string) string {
	parts := strings.Split(path, string(filepath.Separator))
	if h, err := os.UserHomeDir(); err == nil {
		if rel, err := filepath.Rel(h, path); err == nil && !strings.HasPrefix(rel, "..") {
			parts = append([]string{"home"}, strings.Split(rel, string(filepath.Separator))...)
		}
	}
	var out []string
	for _, p := range parts {
		if p = strings.ToLower(strings.Trim(slugUnsafe.ReplaceAllString(p, "-"), ".-_")); p != "" {
			out = append(out, p)
		}
	}
	slug := strings.Join(out, "-")
	if len(slug) > 72 {
		slug = strings.TrimLeft(slug[len(slug)-72:], "-")
	}
	if slug == "" {
		return "project"
	}
	return slug
}

func newSessionPath(dir string) string {
	return filepath.Join(dir, time.Now().UTC().Format("2006-01-02T15-04-05")+"_"+session.NewID()[:8]+".jsonl")
}

// Open builds a coding session, resuming one if requested.
func Open(opts Options) (*Session, error) {
	if opts.Cwd == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		opts.Cwd = wd
	}
	cwd, err := filepath.Abs(opts.Cwd)
	if err != nil {
		return nil, err
	}
	if opts.Home == "" {
		opts.Home = Home()
	}
	if opts.Settings == nil {
		if opts.Settings, err = LoadSettings(opts.Home); err != nil {
			return nil, err
		}
	}
	s := &Session{cwd: cwd, home: opts.Home, settings: opts.Settings}

	// Pick the session file and replay it.
	var state session.State
	dir := SessionsDir(opts.Home, cwd)
	switch {
	case opts.NoSession:
	case opts.Resume != "":
		path, err := session.Find(dir, opts.Resume)
		if err != nil {
			return nil, err
		}
		if s.file, state, err = loadSession(path); err != nil {
			return nil, err
		}
	case opts.Continue:
		path, err := session.Latest(dir)
		if errors.Is(err, fs.ErrNotExist) {
			s.file = session.NewFile(newSessionPath(dir))
			break
		}
		if err != nil {
			return nil, err
		}
		if s.file, state, err = loadSession(path); err != nil {
			return nil, err
		}
	default:
		s.file = session.NewFile(newSessionPath(dir))
	}

	// Explicit flags win over the resumed session, which wins over settings.
	ref := opts.Model
	if ref == "" && state.Model != "" {
		ref = state.Provider + "/" + state.Model
		if state.Provider == "" {
			ref = state.Model
		}
	}
	pc, model, err := opts.Settings.ResolveModel(ref)
	if err != nil {
		return nil, err
	}
	provider := opts.Provider
	if provider == nil {
		if provider, err = pc.NewProvider(model); err != nil {
			return nil, err
		}
	}
	level := opts.ThinkingLevel
	if level == "" {
		level = state.ThinkingLevel
	}
	if level == "" {
		level = opts.Settings.ThinkingLevel
	}
	s.provider, s.model, s.thinking = pc, model, pc.ValidThinking(level)

	tools := CodingTools(cwd)
	sessionID := ""
	if s.file != nil {
		sessionID = strings.TrimSuffix(filepath.Base(s.file.Path()), ".jsonl")
	}
	s.Harness = agent.NewHarness(agent.HarnessConfig{
		Provider:      provider,
		Model:         model,
		System:        s.buildPrompt(tools),
		Tools:         tools,
		ThinkingLevel: s.thinking,
		SessionID:     sessionID,
	}, state.Messages)

	if s.file != nil {
		if !s.file.Exists() {
			s.header = []*session.Entry{session.NewSessionInfo(cwd, "")}
		}
		// Record the effective model and level whenever they differ from
		// what the file last said.
		if state.Model != model || state.Provider != pc.Name {
			s.header = append(s.header, session.NewModelChange(pc.Name, model))
		}
		if state.ThinkingLevel != s.thinking {
			s.header = append(s.header, session.NewThinkingLevelChange(s.thinking))
		}
		s.unsub = s.Harness.Subscribe(s.persist)
	}
	return s, nil
}

func loadSession(path string) (*session.File, session.State, error) {
	f, err := session.Load(path)
	if err != nil {
		return nil, session.State{}, err
	}
	return f, session.Replay(session.BranchPath(f.Entries(), f.TipID())), nil
}

func (s *Session) buildPrompt(tools []*agent.Tool) string {
	return BuildSystemPrompt(PromptOptions{
		Cwd:          s.cwd,
		Tools:        tools,
		ContextFiles: LoadContextFiles(s.home, s.cwd),
		Append:       s.settings.AppendSystemPrompt,
	})
}

// persist is the push-based persistence listener: each completed message is
// written before frontends see the event. Header entries are written lazily
// so an abandoned session leaves no file.
func (s *Session) persist(e agent.Event) {
	me, ok := e.(*agent.MessageEndEvent)
	if !ok {
		return
	}
	s.append(session.NewMessageEntry(me.Message))
}

func (s *Session) append(e *session.Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return
	}
	pending := append(s.header, e)
	s.header = nil
	for _, entry := range pending {
		if err := s.file.Append(entry); err != nil && s.persistErr == nil {
			s.persistErr = fmt.Errorf("persist session: %w", err)
		}
	}
}

// recordSetting writes a settings-change entry now if the file exists,
// otherwise queues it with the header.
func (s *Session) recordSetting(e *session.Entry) {
	s.mu.Lock()
	if s.file == nil {
		s.mu.Unlock()
		return
	}
	if !s.file.Exists() {
		s.header = append(s.header, e)
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	s.append(e)
}

// Prompt sends a user message and runs the agent to completion.
func (s *Session) Prompt(ctx context.Context, text string) error {
	return s.Harness.Prompt(ctx, agent.NewUserText(text))
}

// SetModel switches provider/model for subsequent turns.
func (s *Session) SetModel(ref string) error {
	pc, model, err := s.settings.ResolveModel(ref)
	if err != nil {
		return err
	}
	provider, err := pc.NewProvider(model)
	if err != nil {
		return err
	}
	level := pc.ValidThinking(s.ThinkingLevel())
	s.Harness.Update(func(c *agent.HarnessConfig) {
		c.Provider, c.Model, c.ThinkingLevel = provider, model, level
	})
	s.mu.Lock()
	changedLevel := level != s.thinking
	s.provider, s.model, s.thinking = pc, model, level
	s.mu.Unlock()
	s.recordSetting(session.NewModelChange(pc.Name, model))
	if changedLevel {
		s.recordSetting(session.NewThinkingLevelChange(level))
	}
	return nil
}

// SetThinkingLevel changes the reasoning level for subsequent turns.
func (s *Session) SetThinkingLevel(level string) error {
	s.mu.Lock()
	pc := s.provider
	s.mu.Unlock()
	if len(pc.ThinkingLevels) > 0 && !slices.Contains(pc.ThinkingLevels, level) {
		return fmt.Errorf("provider %s supports thinking levels: %s", pc.Name, strings.Join(pc.ThinkingLevels, ", "))
	}
	s.Harness.Update(func(c *agent.HarnessConfig) { c.ThinkingLevel = level })
	s.mu.Lock()
	s.thinking = level
	s.mu.Unlock()
	s.recordSetting(session.NewThinkingLevelChange(level))
	return nil
}

// Cwd, ProviderName, Model, ThinkingLevel report the current selection.
func (s *Session) Cwd() string { return s.cwd }

func (s *Session) Provider() ProviderConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.provider
}

func (s *Session) Model() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.model
}

func (s *Session) ThinkingLevel() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.thinking
}

// Path returns the session file path, or "" for in-memory sessions.
func (s *Session) Path() string {
	if s.file == nil {
		return ""
	}
	return s.file.Path()
}

// SessionsDir returns this cwd's session directory.
func (s *Session) SessionsDir() string { return SessionsDir(s.home, s.cwd) }

// PersistError returns the first persistence failure, if any.
func (s *Session) PersistError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.persistErr
}

// Close detaches persistence.
func (s *Session) Close() {
	if s.unsub != nil {
		s.unsub()
	}
}

// Reopen replaces this session's state with a fresh or resumed one, keeping
// the current model unless the resumed session says otherwise. It is used by
// /new and /resume in interactive frontends.
//
// The receiver keeps working if Reopen fails; on success it is closed and
// must no longer be used.
func (s *Session) Reopen(resume string) (*Session, error) {
	opts := Options{Cwd: s.cwd, Home: s.home, Settings: s.settings, Resume: resume}
	if resume == "" {
		opts.Model = s.Provider().Name + "/" + s.Model()
		opts.ThinkingLevel = s.ThinkingLevel()
	}
	next, err := Open(opts)
	if err != nil {
		return nil, err
	}
	s.Close()
	return next, nil
}
