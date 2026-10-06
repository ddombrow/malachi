package rpc

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/coding"
)

// modelListTimeout bounds the network call behind get_available_models.
const modelListTimeout = 15 * time.Second

// dispatch runs one command. Commands that wait on the network or a model
// (compact, get_available_models) answer from a goroutine, so abort and
// get_state stay responsive while they run.
func (sv *Server) dispatch(id any, typ string, cmd map[string]any) {
	s := sv.Session()
	switch typ {
	case "prompt", "steer", "follow_up":
		sv.submit(s, id, typ, cmd)
	case "abort":
		s.Abort()
		sv.ok(id, typ, nil)
	case "get_state":
		sv.ok(id, typ, stateOf(s))
	case "get_messages":
		msgs := s.Harness.Messages()
		if msgs == nil {
			msgs = []agent.Message{}
		}
		sv.ok(id, typ, map[string]any{"messages": msgs})
	case "get_last_assistant_text":
		var text *string
		msgs := s.Harness.Messages()
		for i := len(msgs) - 1; i >= 0; i-- {
			if a, ok := msgs[i].(*agent.AssistantMessage); ok {
				t := a.Text()
				text = &t
				break
			}
		}
		sv.ok(id, typ, map[string]any{"text": text})
	case "get_available_models":
		sv.background(func() { sv.ok(id, typ, map[string]any{"models": availableModels(sv.ctx, s)}) })
	case "set_model":
		sv.setModel(s, id, typ, cmd)
	case "get_available_thinking_levels":
		levels := s.Provider().ThinkingLevels
		if levels == nil {
			levels = []string{}
		}
		sv.ok(id, typ, map[string]any{"levels": levels})
	case "set_thinking_level":
		level, err := requiredString(cmd, "level")
		if err == nil {
			err = s.SetThinkingLevel(level)
		}
		s.Flush() // thinking_level_changed before the response
		sv.reply(id, typ, nil, err)
	case "compact":
		instructions, err := optionalString(cmd, "customInstructions")
		if err != nil {
			sv.fail(id, typ, err.Error())
			return
		}
		sv.background(func() {
			res, err := s.Compact(sv.ctx, instructions)
			// The compaction's events describe the work this response reports,
			// so they go first. Events are delivered asynchronously; without
			// this the response could overtake compaction_start.
			s.Flush()
			sv.reply(id, typ, res, err)
		})
	case "set_auto_compaction":
		enabled, ok := cmd["enabled"].(bool)
		if !ok {
			sv.fail(id, typ, "enabled must be a boolean")
			return
		}
		s.SetAutoCompaction(enabled)
		sv.ok(id, typ, nil)
	case "new_session":
		sv.reopen(s, id, typ, "")
	case "switch_session":
		ref, _ := cmd["sessionPath"].(string)
		if ref == "" {
			ref, _ = cmd["sessionId"].(string)
		}
		if ref == "" {
			sv.fail(id, typ, "switch_session requires sessionPath")
			return
		}
		sv.reopen(s, id, typ, ref)
	case "get_session_stats":
		sv.ok(id, typ, statsOf(s))
	case "set_trust":
		sv.setTrust(s, id, typ, cmd)
	case "set_network":
		// A malachi extension: the user switching sandboxed commands'
		// network access for this session. Answers with the sandbox state.
		on, ok := cmd["enabled"].(bool)
		if !ok {
			sv.fail(id, typ, "enabled must be a boolean")
			return
		}
		if err := s.SetNetwork(on); err != nil {
			sv.fail(id, typ, err.Error())
			return
		}
		sv.ok(id, typ, sandboxOf(s.Sandbox()))
	default:
		sv.fail(id, typ, "Unknown command: "+typ)
	}
}

func (sv *Server) background(f func()) {
	sv.bg.Add(1)
	go func() {
		defer sv.bg.Done()
		f()
	}()
}

func (sv *Server) reply(id any, typ string, data any, err error) {
	if err != nil {
		sv.fail(id, typ, err.Error())
		return
	}
	sv.ok(id, typ, data)
}

// submit handles prompt, steer and follow_up. The write lock is held across
// Submit and the response, so the response precedes every event of the work
// it started.
func (sv *Server) submit(s *coding.Session, id any, typ string, cmd map[string]any) {
	message, err := requiredString(cmd, "message")
	if err != nil {
		sv.fail(id, typ, err.Error())
		return
	}
	behavior := ""
	switch typ {
	case "steer":
		behavior = coding.BehaviorSteer
	case "follow_up":
		behavior = coding.BehaviorFollowUp
	}
	if v, ok := cmd["streamingBehavior"]; ok && v != nil {
		switch v {
		case "steer":
			behavior = coding.BehaviorSteer
		case "followUp":
			behavior = coding.BehaviorFollowUp
		default:
			sv.fail(id, typ, "streamingBehavior must be 'steer' or 'followUp'")
			return
		}
	}

	sv.wmu.Lock()
	defer sv.wmu.Unlock()
	if err := s.Submit(sv.ctx, message, behavior); err != nil {
		if errors.Is(err, coding.ErrStreaming) {
			// tau's wording, which clients may show as is.
			err = errors.New("Agent is already streaming; set streamingBehavior to steer or followUp")
		}
		sv.failLocked(id, typ, err.Error())
		return
	}
	sv.okLocked(id, typ, nil)
}

// setModel switches model. An explicit provider must exist: malachi's model
// references would otherwise read "unknown/model" as a model id on the
// default provider, silently switching somewhere else.
func (sv *Server) setModel(s *coding.Session, id any, typ string, cmd map[string]any) {
	modelID, err := requiredString(cmd, "modelId")
	if err != nil {
		sv.fail(id, typ, err.Error())
		return
	}
	provider := s.Provider().Name
	if v, ok := cmd["provider"]; ok {
		p, ok := v.(string)
		if !ok {
			sv.fail(id, typ, "provider must be a string")
			return
		}
		provider = p
	}
	if _, known := s.Settings().ProviderConfigs()[provider]; !known {
		sv.fail(id, typ, "Unknown provider: "+provider)
		return
	}
	if err := s.SetModel(provider + "/" + modelID); err != nil {
		sv.fail(id, typ, err.Error())
		return
	}
	s.Flush() // a thinking_level_changed the switch caused goes first
	sv.ok(id, typ, modelOf(s.Provider(), s.Model()))
}

// reopen replaces the session (new_session, switch_session) and moves the
// event stream to the new one.
func (sv *Server) reopen(s *coding.Session, id any, typ, resume string) {
	next, err := s.Reopen(resume)
	if err != nil {
		sv.fail(id, typ, err.Error())
		return
	}
	// Events still queued on the old session are delivered before the switch.
	s.Flush()
	sv.detach()
	sv.attach(next)
	sv.ok(id, typ, map[string]any{"cancelled": false})
}

// setTrust is a malachi extension: it answers the question a withheld
// AGENTS.md raises, the way /trust does in the TUI. decision is "trusted" or
// "untrusted"; remember saves it for this directory; scope "parent" saves
// trust for the parent directory instead.
func (sv *Server) setTrust(s *coding.Session, id any, typ string, cmd map[string]any) {
	decision, err := requiredString(cmd, "decision")
	if err != nil {
		sv.fail(id, typ, err.Error())
		return
	}
	remember, _ := cmd["remember"].(bool)
	scope, _ := cmd["scope"].(string)
	switch {
	case scope == "parent":
		if decision != string(coding.TrustTrusted) {
			err = errors.New("only trust can be shared with a parent")
		} else {
			err = s.TrustParent(true)
		}
	case scope != "":
		err = fmt.Errorf("unknown scope %q (want \"parent\")", scope)
	case decision == string(coding.TrustTrusted) || decision == string(coding.TrustUntrusted):
		err = s.Trust(coding.TrustDecision(decision), remember)
	default:
		err = fmt.Errorf("decision must be %q or %q", coding.TrustTrusted, coding.TrustUntrusted)
	}
	if err != nil {
		sv.fail(id, typ, err.Error())
		return
	}
	sv.ok(id, typ, trustOf(s.TrustState()))
}

// availableModels lists the current provider's live models, then the preset
// models of every other provider that has a usable key.
func availableModels(ctx context.Context, s *coding.Session) []modelWire {
	out := []modelWire{}
	current := s.Provider()
	lctx, cancel := context.WithTimeout(ctx, modelListTimeout)
	ids, _ := s.Models(lctx)
	cancel()
	if len(ids) == 0 {
		ids = []string{s.Model()}
	}
	for _, m := range ids {
		out = append(out, modelOf(current, m))
	}
	for name, pc := range s.Settings().ProviderConfigs() {
		if name == current.Name || pc.APIKeyEnv == "" && pc.APIKey == "" {
			continue
		}
		if _, err := pc.ResolveAPIKey(); err != nil {
			continue
		}
		for _, m := range pc.Models {
			out = append(out, modelOf(pc, m))
		}
	}
	return out
}

func requiredString(cmd map[string]any, key string) (string, error) {
	v, ok := cmd[key].(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", key)
	}
	return v, nil
}

func optionalString(cmd map[string]any, key string) (string, error) {
	v, ok := cmd[key]
	if !ok || v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", key)
	}
	return s, nil
}
