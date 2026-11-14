package acp

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/coding"
)

// modelListTimeout bounds the background fetch of the provider's models.
const modelListTimeout = 20 * time.Second

// acpSession is one coding session a client opened on this connection.
type acpSession struct {
	sv  *Server
	id  string
	s   *coding.Session
	cwd string

	mu     sync.Mutex
	unsub  func()
	closed bool
	// turn is the prompt being answered; asking cancels a trust question
	// still waiting for the user.
	turn   *turn
	asking context.CancelFunc
	// models is the provider's live model list, nil until fetched.
	models      []string
	compactions int
}

// turn is one session/prompt waiting for its run to settle.
type turn struct {
	done      chan struct{}
	stop      string
	cancelled bool
	last      *agent.AssistantMessage
}

func newSession(sv *Server, s *coding.Session) *acpSession {
	return &acpSession{sv: sv, id: s.ID(), s: s, cwd: s.Cwd()}
}

func (as *acpSession) attach() {
	unsub := as.s.Subscribe(as.event)
	as.mu.Lock()
	as.unsub = unsub
	as.mu.Unlock()
}

func (as *acpSession) close() {
	as.mu.Lock()
	if as.closed {
		as.mu.Unlock()
		return
	}
	as.closed = true
	unsub := as.unsub
	as.mu.Unlock()
	if unsub != nil {
		unsub()
	}
	as.s.Close()
}

func (as *acpSession) update(u any) {
	_ = as.sv.c.notifyClient("session/update", sessionNotification{SessionID: as.id, Update: u})
}

// errBusy answers a prompt sent while another is being answered: a client
// cancels the running one first.
var errBusy = &Error{Code: codeInvalidRequest, Message: "a prompt is already running in this session; cancel it first"}

// prompt runs text and waits for all the work it starts to settle,
// returning the stop reason.
func (as *acpSession) prompt(ctx context.Context, text string) (string, error) {
	as.mu.Lock()
	if as.turn != nil || as.asking != nil {
		as.mu.Unlock()
		return "", errBusy
	}
	askCtx, cancelAsk := context.WithCancel(ctx)
	as.asking = cancelAsk
	as.mu.Unlock()
	decided, err := as.settleTrust(askCtx)
	cancelAsk()
	as.mu.Lock()
	as.asking = nil
	as.mu.Unlock()
	if err != nil {
		return "", err
	}
	if !decided {
		return stopCancelled, nil
	}

	// Events from earlier work are delivered first, so a late
	// AgentSettledEvent cannot end this turn.
	as.s.Flush()
	t := &turn{done: make(chan struct{})}
	as.mu.Lock()
	as.turn = t
	as.mu.Unlock()
	if err := as.s.Submit(ctx, text, ""); err != nil {
		as.mu.Lock()
		as.turn = nil
		as.mu.Unlock()
		return "", err
	}
	select {
	case <-t.done:
		return t.stop, nil
	case <-ctx.Done():
		// The connection is ending: stop, and answer once the run has.
		as.cancel()
		select {
		case <-t.done:
		case <-time.After(30 * time.Second):
		}
		return stopCancelled, nil
	}
}

// cancel stops the session's work for session/cancel. The prompt it ends
// answers "cancelled", never an error.
func (as *acpSession) cancel() {
	as.mu.Lock()
	if as.turn != nil {
		as.turn.cancelled = true
	}
	ask := as.asking
	as.mu.Unlock()
	if ask != nil {
		ask()
	}
	as.s.Abort()
}

// finish ends the current turn, if any.
func (as *acpSession) finish() {
	as.mu.Lock()
	t := as.turn
	as.turn = nil
	as.mu.Unlock()
	if t == nil {
		return
	}
	t.stop = stopEndTurn
	switch {
	case t.cancelled:
		t.stop = stopCancelled
	case t.last != nil && t.last.StopReason == agent.StopAborted:
		t.stop = stopCancelled
	case t.last != nil && t.last.StopReason == agent.StopLength:
		t.stop = stopMaxTokens
	case t.last != nil && t.last.StopReason == agent.StopError:
		// The client shows the turn's text; an error it never sees looks
		// like the agent ignoring the prompt.
		msg := t.last.ErrorMessage
		if msg == "" {
			msg = "the provider returned an error"
		}
		as.update(chunkUpdate{SessionUpdate: "agent_message_chunk", Content: textBlock("\n\n**Error:** " + msg)})
	}
	as.usage()
	close(t.done)
}

func (as *acpSession) usage() {
	st, size := as.s.ContextStats(), as.s.ContextWindow()
	if st.Samples == 0 || size <= 0 {
		return
	}
	as.update(usageUpdate{SessionUpdate: "usage_update", Used: st.PromptTokens, Size: int64(size)})
}

// Trust option ids.
const (
	trustOnce     = "trust-once"
	trustAlways   = "trust-always"
	declineOnce   = "decline-once"
	declineAlways = "decline-always"
	trustCallID   = "malachi-trust"
)

// settleTrust asks the user, through the client's permission dialog,
// whether to load the project's instruction files, when nobody has decided
// yet. It reports false when the question was cancelled: the prompt is not
// sent with the answer still open.
func (as *acpSession) settleTrust(ctx context.Context) (bool, error) {
	ts := as.s.TrustState()
	if !ts.Pending || ts.Resources.Empty() {
		return true, nil
	}
	files := ts.Resources.Files
	var desc strings.Builder
	fmt.Fprintf(&desc, "This project has instruction files the model would read (%d):\n", ts.Resources.Total)
	for _, f := range files {
		fmt.Fprintf(&desc, "- %s\n", f)
	}
	if extra := ts.Resources.Total - len(files); extra > 0 {
		fmt.Fprintf(&desc, "- and %d more\n", extra)
	}
	desc.WriteString("\nLoad them only if you trust this repository: they shape what the agent does.")
	content := textBlock(desc.String())
	var res permissionResult
	err := as.sv.c.call(ctx, "session/request_permission", permissionParams{
		SessionID: as.id,
		ToolCall: toolCallUpdate{
			ToolCallID: trustCallID,
			Title:      "Load this project's instructions?",
			Kind:       "read",
			Status:     "pending",
			Content:    []toolCallContent{{Type: "content", Content: &content}},
		},
		Options: []permissionOption{
			{OptionID: trustOnce, Name: "Trust for this session", Kind: "allow_once"},
			{OptionID: trustAlways, Name: "Always trust this project", Kind: "allow_always"},
			{OptionID: declineOnce, Name: "Not now", Kind: "reject_once"},
			{OptionID: declineAlways, Name: "Never for this project", Kind: "reject_always"},
		},
	}, &res)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, errClosed) {
			return false, nil
		}
		return false, err
	}
	if res.Outcome.Outcome != "selected" {
		return false, nil
	}
	var decision coding.TrustDecision
	remember := false
	switch res.Outcome.OptionID {
	case trustOnce:
		decision = coding.TrustTrusted
	case trustAlways:
		decision, remember = coding.TrustTrusted, true
	case declineOnce:
		decision = coding.TrustUntrusted
	case declineAlways:
		decision, remember = coding.TrustUntrusted, true
	default:
		return false, nil // never read an unknown answer as consent
	}
	if err := as.s.Trust(decision, remember); err != nil {
		return false, err
	}
	return true, nil
}

// Config option ids.
const (
	configModel    = "model"
	configThinking = "thought_level"
)

// configOptions are the session's model and thinking level.
func (as *acpSession) configOptions() []configOption {
	pc := as.s.Provider()
	current := pc.Name + "/" + as.s.Model()
	as.mu.Lock()
	live := as.models
	as.mu.Unlock()

	seen := map[string]bool{}
	var values []configValue
	add := func(provider, model string) {
		ref := provider + "/" + model
		if model == "" || seen[ref] {
			return
		}
		seen[ref] = true
		values = append(values, configValue{Value: ref, Name: model, Description: provider})
	}
	add(pc.Name, as.s.Model())
	ids := live
	if ids == nil {
		ids = pc.Models
	}
	for _, m := range ids {
		add(pc.Name, m)
	}
	names := []string{}
	configs := as.s.Settings().ProviderConfigs()
	for name := range configs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		other := configs[name]
		if name == pc.Name || other.APIKeyEnv == "" && other.APIKey == "" {
			continue
		}
		if _, err := other.ResolveAPIKey(); err != nil {
			continue // a provider without a key would only fail
		}
		for _, m := range other.Models {
			add(name, m)
		}
	}
	opts := []configOption{{
		ID: configModel, Name: "Model", Category: "model", Type: "select",
		CurrentValue: current, Options: values,
	}}
	if levels := pc.ThinkingLevels; len(levels) > 1 {
		lv := make([]configValue, len(levels))
		for i, l := range levels {
			lv[i] = configValue{Value: l, Name: l}
		}
		opts = append(opts, configOption{
			ID: configThinking, Name: "Thinking", Category: "thought_level", Type: "select",
			CurrentValue: as.s.ThinkingLevel(), Options: lv,
		})
	}
	return opts
}

func (as *acpSession) setConfig(id, value string) error {
	for _, o := range as.configOptions() {
		if o.ID != id {
			continue
		}
		listed := false
		for _, v := range o.Options {
			listed = listed || v.Value == value
		}
		if !listed {
			return invalidParams(fmt.Errorf("%q is not one of %s's values", value, id))
		}
		var err error
		switch id {
		case configModel:
			err = as.s.SetModel(value)
		case configThinking:
			err = as.s.SetThinkingLevel(value)
		}
		as.s.Flush() // a thinking_level change is reported before the reply
		return err
	}
	return invalidParams(fmt.Errorf("unknown config option %q", id))
}

// fetchModels asks the provider for its models in the background, so
// opening a session never waits on the network, and offers them once they
// arrive.
func (as *acpSession) fetchModels() {
	ctx, cancel := context.WithTimeout(context.Background(), modelListTimeout)
	defer cancel()
	ids, err := as.s.Models(ctx)
	if err != nil || len(ids) == 0 {
		return
	}
	as.mu.Lock()
	if as.closed {
		as.mu.Unlock()
		return
	}
	as.models = ids
	as.mu.Unlock()
	as.update(configOptionUpdate{SessionUpdate: "config_option_update", ConfigOptions: as.configOptions()})
}

// replay sends the session's history as session/update notifications, for
// session/load: user text, the assistant's text and thinking, and each tool
// call with its outcome.
func (as *acpSession) replay() {
	results := map[string]*agent.ToolResultMessage{}
	msgs := as.s.Harness.Messages()
	for _, m := range msgs {
		if r, ok := m.(*agent.ToolResultMessage); ok {
			results[r.ToolCallID] = r
		}
	}
	for _, m := range msgs {
		switch m := m.(type) {
		case *agent.UserMessage:
			if text := m.Content.String(); strings.TrimSpace(text) != "" {
				as.update(chunkUpdate{SessionUpdate: "user_message_chunk", Content: textBlock(text)})
			}
		case *agent.AssistantMessage:
			for _, c := range m.Content {
				switch c := c.(type) {
				case *agent.ThinkingContent:
					if c.Thinking != "" && !c.Redacted {
						as.update(chunkUpdate{SessionUpdate: "agent_thought_chunk", Content: textBlock(c.Thinking)})
					}
				case *agent.TextContent:
					if c.Text != "" {
						as.update(chunkUpdate{SessionUpdate: "agent_message_chunk", Content: textBlock(c.Text)})
					}
				case *agent.ToolCall:
					u := as.toolStart(c.ID, c.Name, c.Arguments)
					if r := results[c.ID]; r != nil {
						end := as.toolEnd(c.ID, c.Name, agent.ToolResult{Content: r.Content, Details: r.Details}, r.IsError)
						u.Status, u.Content = end.Status, end.Content
					} else {
						u.Status = toolFailed // never finished
					}
					as.update(u)
				}
			}
		}
	}
}
