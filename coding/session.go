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
	// Trust overrides project trust for this run only: "yes" to load the
	// working directory's instruction files, "no" to withhold them. Empty
	// defers to the saved decision and then to the configured policy.
	Trust string
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
	diag       *Diagnostics
	unsub      func()
	liveModels map[string][]string // provider name -> fetched model ids
	trims      *trimLog
	preparer   *codingContextPreparer
	ctxSampler *ctxSampler
	// trust records whether the working directory's instruction files were
	// folded into the system prompt.
	trust TrustState
	// TrustOverride is a decision made for this run only, carried across a
	// reopen so /new does not silently ask again.
	TrustOverride string
	// system and tools are kept so a request can be sized locally, without
	// sending one, before the provider has reported any usage.
	system string
	tools  []*agent.Tool
	// persisted maps messages to the session entries they were written as, so
	// a compaction can record where its retained tail begins.
	persisted []persistedMessage
	// runtime is the provider the session actually talks to, which may have
	// been injected rather than built from configuration. A summarisation must
	// use the same one, or an embedded session would summarise through a
	// different provider than it converses with.
	runtime agent.Provider
	// sessionID is the routing and prompt-cache hint the loop sends. Anything
	// else calling the provider directly has to send it too: a gateway that
	// requires the header answers 400 when it is missing.
	sessionID string
}

// persistedMessage is one message and the entry id it was written as.
type persistedMessage struct {
	message agent.Message
	entryID string
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
	s := &Session{cwd: cwd, home: opts.Home, settings: opts.Settings, diag: NewDiagnostics(opts.Home)}
	// Resolved before the prompt is built: an untrusted project's instruction
	// files must never reach the system prompt, not even briefly.
	s.trust = ResolveTrust(opts.Home, cwd, opts.Settings.TrustPolicyOrDefault(), opts.Trust)

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
	// A stable per-conversation id: the session file name, or a random id
	// for in-memory sessions. Providers use it for routing/prompt caching.
	sessionID := session.NewID()
	if s.file != nil {
		sessionID = strings.TrimSuffix(filepath.Base(s.file.Path()), ".jsonl")
	}
	s.sessionID = sessionID
	s.trims = &trimLog{}
	s.preparer = newCodingContextPreparer(cwd, s.trims)
	s.runtime = provider
	s.system, s.tools = s.buildPrompt(tools), tools
	s.Harness = agent.NewHarness(agent.HarnessConfig{
		Provider:        provider,
		Model:           model,
		System:          s.system,
		Tools:           s.tools,
		ThinkingLevel:   s.thinking,
		SessionID:       sessionID,
		PrepareRequest:  s.preparer.prepare,
		RecoverOverflow: s.recoverOverflow,
	}, state.Messages)
	// Messages replayed from disk are already persisted. Recording their entry
	// ids lets a compaction of a resumed session name where its retained tail
	// begins; without them it would look unpersisted and replay would drop it.
	for i, m := range state.Messages {
		if id := state.EntryIDs[i]; id != "" {
			s.persisted = append(s.persisted, persistedMessage{message: m, entryID: id})
		}
	}

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
	sessionPath := ""
	if s.file != nil {
		sessionPath = s.file.Path()
	}
	s.diag.Bind(sessionPath, pc.Name, model)
	// Diagnostics follow every run, including in-memory sessions that have
	// no file to record a failure in.
	s.Harness.Subscribe(s.diag.observe)
	// Measurement follows the same stream, in the same dispatch, so the
	// preparer's byte count belongs to the request the usage describes.
	s.ctxSampler = newCtxSampler()
	s.Harness.Subscribe(s.observeContext)
	// A resumed session is already large, and the gauge that exists to say so
	// used to stay blank until the first reply arrived. Size it locally now,
	// once the sampler it feeds exists.
	s.estimateContext()
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
		ContextFiles: LoadContextFiles(s.home, s.cwd, s.trust.Trusted()),
		Append:       s.settings.AppendSystemPrompt,
	})
}

// TrustParent extends a trust decision to this directory's parent, so it
// covers every sibling beneath it. Declining is refused on purpose: a refusal
// saved against a parent would silently withhold instructions from unrelated
// repositories that merely share an ancestor.
func (s *Session) TrustParent(remember bool) error {
	s.mu.Lock()
	decision := s.trust.Decision
	parent := filepath.Dir(s.cwd)
	if parent == s.cwd {
		s.mu.Unlock()
		return fmt.Errorf("cannot trust the parent of %s", s.cwd)
	}
	if decision != TrustTrusted {
		s.mu.Unlock()
		return fmt.Errorf("only trust can be shared with a parent; use /trust no for this directory")
	}
	if remember {
		if err := SetTrust(s.home, parent, TrustTrusted); err != nil {
			s.mu.Unlock()
			return err
		}
	}
	s.mu.Unlock()
	// Resolve without a run override so the parent decision just saved is what
	// now applies: the user asked for something that outlives the session, and
	// the session's own decision would otherwise mask it.
	return s.trustNow(TrustTrusted, false, "")
}

// TrustState reports why project instructions were or were not loaded.
func (s *Session) TrustState() TrustState { return s.trust }

// Trust applies a decision about this directory's instruction files. With
// remember set it is saved so the next session starts the same way; without
// it the decision lasts only as long as the session does.
//
// The system prompt is rebuilt either way. Trust is the difference between a
// project's instructions being part of the prompt and not, so a decision that
// did not change the prompt would be a decision that did nothing.
func (s *Session) Trust(decision TrustDecision, remember bool) error {
	if decision != TrustTrusted && decision != TrustUntrusted {
		return fmt.Errorf("unknown trust decision %q", decision)
	}
	override := ""
	if !remember {
		// A decision that is not saved lasts only as long as the session, so
		// it is held as an override rather than left to whatever is on disk.
		override = "yes"
		if decision == TrustUntrusted {
			override = "no"
		}
	}
	return s.trustNow(decision, remember, override)
}

// trustNow applies a decision and rebuilds everything derived from it.
// override is a decision scoped to this run; it is empty when the decision
// should come from the saved store, which is what lets a newly saved parent
// decision take effect instead of being masked by the session's own.
func (s *Session) trustNow(decision TrustDecision, remember bool, override string) error {
	s.mu.Lock()
	if remember {
		if err := SetTrust(s.home, s.cwd, decision); err != nil {
			s.mu.Unlock()
			return err
		}
	}
	s.TrustOverride = override
	s.trust = ResolveTrust(s.home, s.cwd, s.settings.TrustPolicyOrDefault(), s.TrustOverride)
	resolved := s.trust
	system := s.buildPrompt(s.tools)
	s.mu.Unlock()

	s.Harness.Update(func(c *agent.HarnessConfig) { c.System = system })
	// The prompt grew or shrank, so every figure derived from it is stale.
	s.estimateContext()
	s.diag.LogTrust(resolved)
	return nil
}

// persist is the push-based persistence listener: each completed message is
// written before frontends see the event. Header entries are written lazily
// so an abandoned session leaves no file.
func (s *Session) persist(e agent.Event) {
	me, ok := e.(*agent.MessageEndEvent)
	if !ok {
		return
	}
	entry := session.NewMessageEntry(me.Message)
	if s.append(entry) {
		s.rememberEntry(me.Message, entry.ID)
	}
}

// rememberEntry records which session entry a message was written as, so a
// compaction can name the entry its retained tail begins at. Without that,
// replay could not find a tail that was already on disk when the compaction
// entry was appended.
func (s *Session) rememberEntry(m agent.Message, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.persisted = append(s.persisted, persistedMessage{message: m, entryID: id})
}

// entryIDFor names the entry a message was persisted as, or "" if it has not
// been written yet.
func (s *Session) entryIDFor(m agent.Message) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.persisted {
		if p.message == m {
			return p.entryID
		}
	}
	return ""
}

// append writes an entry, reporting whether it reached the disk.
func (s *Session) append(e *session.Entry) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return false
	}
	pending := append(s.header, e)
	s.header = nil
	for _, entry := range pending {
		if err := s.file.Append(entry); err != nil && s.persistErr == nil {
			s.persistErr = fmt.Errorf("persist session: %w", err)
			// The session file is the one place that cannot report its own
			// failure, so send it somewhere that can.
			s.diag.LogPersistError(s.persistErr)
		} else if err != nil {
			return false
		}
	}
	return true
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
	s.diag.NewRun()
	return s.Harness.Prompt(ctx, agent.NewUserText(text))
}

// Diagnostics is the failure log for this session's home.
func (s *Session) Diagnostics() *Diagnostics { return s.diag }

// Trim reports the latest request-view tool-output reduction. Seq is 0 until
// trimming has fired. The saved transcript is not rewritten.
func (s *Session) Trim() TrimEvent { return s.trims.get() }

// ContextStats reports what the provider actually charged for each request,
// so the cost of tool output can be derived instead of guessed.
func (s *Session) ContextStats() ContextStats {
	stats := s.ctxSampler.get()
	stats.Window = s.ContextWindow()
	return stats
}

// observeContext records one request's measured shape.
func (s *Session) observeContext(e agent.Event) {
	end, ok := e.(*agent.MessageEndEvent)
	if !ok {
		return
	}
	m, ok := end.Message.(*agent.AssistantMessage)
	if !ok {
		return
	}
	seq := s.trims.get().Seq
	s.ctxSampler.observe(m, s.preparer.toolBytes(), seq)
	stats := s.ctxSampler.get()
	s.diag.LogContextSample(m.Model, stats)
	// The ceiling follows the measurement: what the provider actually charged
	// for, minus what was tool output, is what is left for tool output.
	s.preparer.setBudget(s.toolOutputBudget(stats))
}

// estimateContext sizes the conversation without asking the provider. The
// measurement replaces it as soon as there is one.
func (s *Session) estimateContext() {
	prompt, toolBytes := estimatePromptTokens(s.system, s.tools, s.Harness.Messages(), s.ctxSampler.get().Ratio)
	s.ctxSampler.estimate(prompt, toolBytes)
	s.diag.LogContextEstimate(prompt, toolBytes)
}

// ContextWindow is the provider's context window in tokens, defaulting
// conservatively when the configuration does not say.
func (s *Session) ContextWindow() int { return s.provider.ContextWindowTokens() }

// toolOutputBudget derives the tool-output ceiling from the latest
// measurement and the provider's window.
func (s *Session) toolOutputBudget(c ContextStats) int {
	return toolOutputBudgetBytes(s.ContextWindow(), int(c.PromptTokens), int(c.ToolTokens()), c.Ratio)
}

// Compact asks for the next provider request to trim tool output harder than
// the default ceiling: everything above budget bytes is replaced by a marker
// and the ledger is refreshed. A budget of 0 trims as much as possible. It
// reports whether there was anything left to trim, so a caller can say so
// instead of silently doing nothing.
func (s *Session) ForceTrim(budget int) bool {
	if budget < 0 {
		budget = 0
	}
	// The preparer learns sizes as requests go by; prime it so the answer is
	// honest even before the first request of a session.
	s.preparer.prime(s.Harness.Messages())
	return s.preparer.forceBudget(budget)
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

// Models returns the current provider's model ids, fetched live from the
// endpoint once per session and cached. If the fetch fails, the preset list
// is returned along with the error so callers can mention it.
func (s *Session) Models(ctx context.Context) ([]string, error) {
	s.mu.Lock()
	pc := s.provider
	cached, ok := s.liveModels[pc.Name]
	s.mu.Unlock()
	if ok {
		return cached, nil
	}
	ids, err := pc.FetchModels(ctx)
	if err != nil || len(ids) == 0 {
		return pc.Models, err
	}
	s.mu.Lock()
	if s.liveModels == nil {
		s.liveModels = map[string][]string{}
	}
	s.liveModels[pc.Name] = ids
	s.mu.Unlock()
	return ids, nil
}

// Settings returns the settings the session was opened with.
func (s *Session) Settings() *Settings { return s.settings }

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
	opts.Trust = s.TrustOverride
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
