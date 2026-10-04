package agent

import (
	"context"
	"fmt"
	"time"
)

// BeforeToolCall may block a tool call; a non-empty reason is reported to
// the model as the error result.
type BeforeToolCall func(ctx context.Context, call *ToolCall) (block bool, reason string)

// AfterToolCall may rewrite a tool's result before it enters the transcript.
type AfterToolCall func(ctx context.Context, call *ToolCall, result ToolResult, isError bool) (ToolResult, bool)

// LoopConfig is everything one run of the provider/tool loop needs.
type LoopConfig struct {
	Provider      Provider
	Model         string
	System        string
	Tools         []*Tool
	ThinkingLevel string
	SessionID     string
	MaxTurns      int // 0 means unlimited

	// GetSteeringMessages is polled before every turn after the first; its
	// messages are injected before the next provider call.
	GetSteeringMessages func() []Message
	// GetFollowUpMessages is polled when the agent would otherwise stop.
	GetFollowUpMessages func() []Message

	BeforeToolCall BeforeToolCall
	AfterToolCall  AfterToolCall
	PrepareRequest RequestPreparer

	// RecoverOverflow may rebuild a request whose provider call failed because
	// the model ran out of context, typically by compacting history first.
	// Returning true retries the call exactly once with the returned request.
	// history is the canonical transcript to continue from, which recovery
	// replaces whenever it compacted; the loop keeps its own copy otherwise
	// and would otherwise carry on from the pre-compaction one.
	//
	// It is called only for an errored attempt and only while the run is not
	// already cancelled, so a cancelled run is never revived here.
	RecoverOverflow func(ctx context.Context, req Request, failed *AssistantMessage) (retry Request, history []Message, ok bool)
}

// Run executes the provider/tool loop and reports progress through emit,
// which is called synchronously in order. history is the transcript so far
// (not modified); prompts are new messages to append before the first call.
//
// Every message added to the transcript is announced with a MessageEndEvent,
// so history + all MessageEnd messages == the new transcript. Run returns
// the new messages in order.
//
// Port of tau_agent/loop.py run_agent_loop.
func Run(ctx context.Context, cfg LoopConfig, history []Message, prompts []Message, emit func(Event)) []Message {
	messages := append(append([]Message(nil), history...), prompts...)
	newMessages := append([]Message(nil), prompts...)
	add := func(m Message) {
		messages = append(messages, m)
		newMessages = append(newMessages, m)
		emit(&MessageStartEvent{Message: m})
		emit(&MessageEndEvent{Message: m})
	}
	finish := func() []Message {
		emit(&AgentEndEvent{Messages: newMessages})
		return newMessages
	}

	emit(&AgentStartEvent{})
	emit(&TurnStartEvent{})
	for _, p := range prompts {
		emit(&MessageStartEvent{Message: p})
		emit(&MessageEndEvent{Message: p})
	}

	if cfg.MaxTurns < 0 {
		e := errorMessage(cfg.Model, "max_turns must be at least 1")
		add(e)
		emit(&TurnEndEvent{Message: e})
		return finish()
	}

	toolsByName := make(map[string]*Tool, len(cfg.Tools))
	for _, t := range cfg.Tools {
		toolsByName[t.Name] = t
	}
	poll := func(f func() []Message) []Message {
		if f == nil {
			return nil
		}
		return f()
	}

	turn := 1
	firstTurn := true
	pending := poll(cfg.GetSteeringMessages)
	for {
		hasMoreTools := true
		for hasMoreTools || len(pending) > 0 {
			if !firstTurn {
				emit(&TurnStartEvent{})
			}
			firstTurn = false

			for _, m := range pending {
				add(m)
			}
			pending = nil

			if cfg.MaxTurns > 0 && turn > cfg.MaxTurns {
				e := errorMessage(cfg.Model, fmt.Sprintf("Agent stopped after max_turns=%d", cfg.MaxTurns))
				add(e)
				emit(&TurnEndEvent{Message: e})
				return finish()
			}

			req := Request{
				Model:         cfg.Model,
				System:        cfg.System,
				Messages:      ProviderContext(messages),
				Tools:         cfg.Tools,
				ThinkingLevel: cfg.ThinkingLevel,
				SessionID:     cfg.SessionID,
			}
			if cfg.PrepareRequest != nil {
				req = cfg.PrepareRequest(req)
			}
			assistant := streamAssistant(ctx, cfg, req, emit)
			// One retry, and only when recovery asks for it: it has already
			// decided whether the failure was the context running out, so a
			// provider error that compaction cannot fix is not retried here.
			if cfg.RecoverOverflow != nil && assistant.StopReason == StopError && ctx.Err() == nil {
				if retry, history, ok := cfg.RecoverOverflow(ctx, req, assistant); ok {
					// The failed turn was emitted and persisted like any other
					// turn, so it stays in the transcript; the history recovery
					// returns already accounts for it.
					messages = history
					newMessages = append(newMessages, assistant)
					assistant = streamAssistant(ctx, cfg, retry, emit)
				}
			}
			messages = append(messages, assistant)
			newMessages = append(newMessages, assistant)
			if assistant.StopReason == StopError || assistant.StopReason == StopAborted {
				emit(&TurnEndEvent{Message: assistant})
				return finish()
			}

			calls := assistant.ToolCalls()
			hasMoreTools = len(calls) > 0
			var results []*ToolResultMessage
			for _, call := range calls {
				r := executeToolCall(ctx, cfg, call, toolsByName, emit)
				messages = append(messages, r)
				newMessages = append(newMessages, r)
				results = append(results, r)
			}
			emit(&TurnEndEvent{Message: assistant, ToolResults: results})

			// Cancellation during tools: every call has a result, so the
			// transcript is consistent. Stop instead of calling the provider
			// with a dead context.
			if ctx.Err() != nil {
				return finish()
			}
			turn++
			pending = poll(cfg.GetSteeringMessages)
		}

		if followUps := poll(cfg.GetFollowUpMessages); len(followUps) > 0 {
			pending = followUps
			continue
		}
		break
	}
	return finish()
}

// ProviderContext returns the replayable transcript: empty failed/aborted
// assistant turns are dropped (providers reject empty assistant messages)
// and tool call/result pairing is repaired.
func ProviderContext(messages []Message) []Message {
	out := make([]Message, 0, len(messages))
	for _, m := range messages {
		if a, ok := m.(*AssistantMessage); ok && len(a.Content) == 0 &&
			(a.StopReason == StopError || a.StopReason == StopAborted) {
			continue
		}
		out = append(out, m)
	}
	return RepairToolHistory(out)
}

// streamAssistant consumes one provider stream, translating it into message
// lifecycle events, and returns the final assistant message.
func streamAssistant(ctx context.Context, cfg LoopConfig, req Request, emit func(Event)) *AssistantMessage {
	var (
		final        *AssistantMessage
		started      bool
		begin        = time.Now()
		consumerTime time.Duration
		firstOutput  *time.Duration
	)
	timing := func() *ResponseTiming {
		t := &ResponseTiming{TotalDurationMs: (time.Since(begin) - consumerTime).Milliseconds()}
		if firstOutput != nil {
			ms := firstOutput.Milliseconds()
			t.TimeToFirstOutputMs = &ms
		}
		return t
	}

	for ev := range cfg.Provider.Stream(ctx, req) {
		if firstOutput == nil && isOutputEvent(ev) {
			d := time.Since(begin) - consumerTime
			firstOutput = &d
		}
		handled := time.Now()
		switch e := ev.(type) {
		case *AssistantStart:
			started = true
			emit(&MessageStartEvent{Message: e.Partial})
		case *AssistantDone:
			final = e.Message
		case *AssistantError:
			final = e.Error
		default:
			emit(&MessageUpdateEvent{Message: partialOf(ev), AssistantMessageEvent: ev})
		}
		consumerTime += time.Since(handled)
		if final != nil {
			break
		}
	}

	if final == nil {
		reason, text := StopError, "Provider produced no assistant message"
		if ctx.Err() != nil {
			reason, text = StopAborted, "Operation aborted"
		}
		final = NewAssistantMessage(cfg.Model)
		final.StopReason, final.ErrorMessage = reason, text
	}
	final.Timing = timing()
	if !started {
		emit(&MessageStartEvent{Message: final})
	}
	emit(&MessageEndEvent{Message: final})
	return final
}

func isOutputEvent(e AssistantEvent) bool {
	switch e.(type) {
	case *TextDelta, *ThinkingDelta, *ToolCallStart, *ToolCallDelta, *ToolCallEnd:
		return true
	}
	return false
}

func partialOf(e AssistantEvent) *AssistantMessage {
	switch v := e.(type) {
	case *AssistantStart:
		return v.Partial
	case *TextStart:
		return v.Partial
	case *TextDelta:
		return v.Partial
	case *TextEnd:
		return v.Partial
	case *ThinkingStart:
		return v.Partial
	case *ThinkingDelta:
		return v.Partial
	case *ThinkingEnd:
		return v.Partial
	case *ToolCallStart:
		return v.Partial
	case *ToolCallDelta:
		return v.Partial
	case *ToolCallEnd:
		return v.Partial
	}
	return nil
}

func executeToolCall(ctx context.Context, cfg LoopConfig, call *ToolCall, tools map[string]*Tool, emit func(Event)) *ToolResultMessage {
	emit(&ToolExecutionStartEvent{ToolCallID: call.ID, ToolName: call.Name, Args: call.Arguments})

	var (
		result  ToolResult
		isError bool
	)
	blocked, reason := false, ""
	if cfg.BeforeToolCall != nil {
		blocked, reason = cfg.BeforeToolCall(ctx, call)
	}
	switch tool := tools[call.Name]; {
	case blocked:
		if reason == "" {
			reason = "Tool execution was blocked"
		}
		result, isError = TextResult(reason), true
	case ctx.Err() != nil:
		result, isError = TextResult("Operation aborted"), true
	case tool == nil:
		result, isError = TextResult(fmt.Sprintf("Tool %s not found", call.Name)), true
	default:
		onUpdate := func(partial ToolResult) {
			emit(&ToolExecutionUpdateEvent{ToolCallID: call.ID, ToolName: call.Name, Args: call.Arguments, PartialResult: partial})
		}
		result, isError = runTool(ctx, tool, call, onUpdate)
	}

	if cfg.AfterToolCall != nil {
		result, isError = cfg.AfterToolCall(ctx, call, result, isError)
	}
	emit(&ToolExecutionEndEvent{ToolCallID: call.ID, ToolName: call.Name, Result: result, IsError: isError})

	msg := &ToolResultMessage{
		ToolCallID:     call.ID,
		ToolName:       call.Name,
		Content:        result.Content,
		Details:        result.Details,
		AddedToolNames: result.AddedToolNames,
		IsError:        isError,
		Timestamp:      NowMillis(),
	}
	emit(&MessageStartEvent{Message: msg})
	emit(&MessageEndEvent{Message: msg})
	return msg
}

// runTool executes a tool, converting errors and panics into error results:
// tools are an isolation boundary.
func runTool(ctx context.Context, tool *Tool, call *ToolCall, onUpdate func(ToolResult)) (result ToolResult, isError bool) {
	defer func() {
		if r := recover(); r != nil {
			result, isError = TextResult(fmt.Sprintf("tool %s panicked: %v", tool.Name, r)), true
		}
	}()
	res, err := tool.Execute(ctx, call.ID, call.Arguments, onUpdate)
	if err != nil {
		if ctx.Err() != nil {
			return TextResult("Operation aborted"), true
		}
		return TextResult(err.Error()), true
	}
	return res, false
}

func errorMessage(model, text string) *AssistantMessage {
	m := NewAssistantMessage(model)
	m.StopReason = StopError
	m.ErrorMessage = text
	return m
}
