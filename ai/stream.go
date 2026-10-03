// Package ai holds provider-side helpers shared by malachi's model providers.
// Concrete providers live in subpackages and translate wire formats into
// agent.AssistantEvent streams.
package ai

import (
	"encoding/json"
	"strings"

	"github.com/ddombrow/malachi/agent"
)

// Builder accumulates a partial assistant message and emits the canonical
// start/delta/end event sequence for it. Providers feed it raw deltas;
// Builder handles block boundaries and snapshots.
//
// If the consumer stops iterating, Stopped reports true and further calls are
// no-ops, so providers can bail out of their read loops.
type Builder struct {
	msg     *agent.AssistantMessage
	yield   func(agent.AssistantEvent) bool
	active  int // index of the open text/thinking block, or -1
	tools   map[int]*toolAcc
	started bool
	stopped bool
	done    bool
}

type toolAcc struct {
	args  strings.Builder
	ended bool
}

// NewBuilder wraps msg (usually from agent.NewAssistantMessage) and yield.
func NewBuilder(msg *agent.AssistantMessage, yield func(agent.AssistantEvent) bool) *Builder {
	return &Builder{msg: msg, yield: yield, active: -1, tools: map[int]*toolAcc{}}
}

// Message returns the live message being built (not a snapshot).
func (b *Builder) Message() *agent.AssistantMessage { return b.msg }

// Stopped reports whether the consumer stopped or the stream was finished.
func (b *Builder) Stopped() bool { return b.stopped || b.done }

func (b *Builder) emit(e agent.AssistantEvent) {
	if b.stopped {
		return
	}
	if !b.yield(e) {
		b.stopped = true
	}
}

// Start emits the start event. It is called implicitly by the first delta.
func (b *Builder) Start() {
	if b.started {
		return
	}
	b.started = true
	b.emit(&agent.AssistantStart{Partial: b.msg.Clone()})
}

func (b *Builder) endActive() {
	if b.active < 0 {
		return
	}
	i := b.active
	b.active = -1
	switch blk := b.msg.Content[i].(type) {
	case *agent.TextContent:
		b.emit(&agent.TextEnd{ContentIndex: i, Content: blk.Text, Partial: b.msg.Clone()})
	case *agent.ThinkingContent:
		b.emit(&agent.ThinkingEnd{ContentIndex: i, Content: blk.Thinking, Partial: b.msg.Clone()})
	}
}

// Text appends visible text, opening a new text block if needed.
func (b *Builder) Text(delta string) {
	if delta == "" || b.Stopped() {
		return
	}
	b.Start()
	if b.active < 0 || !isText(b.msg.Content[b.active]) {
		b.endActive()
		b.msg.Content = append(b.msg.Content, &agent.TextContent{})
		b.active = len(b.msg.Content) - 1
		b.emit(&agent.TextStart{ContentIndex: b.active, Partial: b.msg.Clone()})
	}
	blk := b.msg.Content[b.active].(*agent.TextContent)
	blk.Text += delta
	b.emit(&agent.TextDelta{ContentIndex: b.active, Delta: delta, Partial: b.msg.Clone()})
}

// Thinking appends reasoning text, opening a new thinking block if needed.
func (b *Builder) Thinking(delta string) {
	if delta == "" || b.Stopped() {
		return
	}
	b.Start()
	if b.active < 0 || !isThinking(b.msg.Content[b.active]) {
		b.endActive()
		b.msg.Content = append(b.msg.Content, &agent.ThinkingContent{})
		b.active = len(b.msg.Content) - 1
		b.emit(&agent.ThinkingStart{ContentIndex: b.active, Partial: b.msg.Clone()})
	}
	blk := b.msg.Content[b.active].(*agent.ThinkingContent)
	blk.Thinking += delta
	b.emit(&agent.ThinkingDelta{ContentIndex: b.active, Delta: delta, Partial: b.msg.Clone()})
}

// SetThinkingSignature records an opaque signature on the open thinking block.
func (b *Builder) SetThinkingSignature(sig string) {
	if b.active >= 0 {
		if blk, ok := b.msg.Content[b.active].(*agent.ThinkingContent); ok {
			blk.ThinkingSignature = sig
		}
	}
}

// ToolCallStart opens a tool call block and returns its content index.
func (b *Builder) ToolCallStart(id, name string) int {
	b.Start()
	b.endActive()
	b.msg.Content = append(b.msg.Content, &agent.ToolCall{ID: id, Name: name, Arguments: map[string]any{}})
	idx := len(b.msg.Content) - 1
	b.tools[idx] = &toolAcc{}
	b.emit(&agent.ToolCallStart{ContentIndex: idx, Partial: b.msg.Clone()})
	return idx
}

// ToolCall returns the tool call block at idx so providers can fill in an
// id or name that arrives late.
func (b *Builder) ToolCall(idx int) *agent.ToolCall {
	return b.msg.Content[idx].(*agent.ToolCall)
}

// ToolCallDelta appends raw JSON argument text to the tool call at idx.
func (b *Builder) ToolCallDelta(idx int, delta string) {
	acc, ok := b.tools[idx]
	if !ok || delta == "" || b.Stopped() {
		return
	}
	acc.args.WriteString(delta)
	b.emit(&agent.ToolCallDelta{ContentIndex: idx, Delta: delta, Partial: b.msg.Clone()})
}

// ToolCallEnd parses the accumulated arguments and closes the tool call.
// Unparseable arguments are kept under "_raw" so the tool can report them.
func (b *Builder) ToolCallEnd(idx int) {
	acc, ok := b.tools[idx]
	if !ok || acc.ended {
		return
	}
	acc.ended = true
	call := b.msg.Content[idx].(*agent.ToolCall)
	if raw := strings.TrimSpace(acc.args.String()); raw != "" {
		var args map[string]any
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			args = map[string]any{"_raw": raw, "_parseError": err.Error()}
		}
		call.Arguments = args
	}
	c := *call
	b.emit(&agent.ToolCallEnd{ContentIndex: idx, ToolCall: &c, Partial: b.msg.Clone()})
}

func (b *Builder) closeAll() {
	b.endActive()
	for i := range b.msg.Content {
		if _, ok := b.tools[i]; ok {
			b.ToolCallEnd(i)
		}
	}
}

// Done closes open blocks and emits the terminal done event. A stop reason
// of "stop" with tool calls present is upgraded to "toolUse".
func (b *Builder) Done(reason agent.StopReason) {
	if b.done {
		return
	}
	b.Start()
	b.closeAll()
	if reason == "" || (reason == agent.StopStop && len(b.msg.ToolCalls()) > 0) {
		if len(b.msg.ToolCalls()) > 0 {
			reason = agent.StopToolUse
		} else {
			reason = agent.StopStop
		}
	}
	b.msg.StopReason = reason
	b.done = true
	b.emit(&agent.AssistantDone{Reason: reason, Message: b.msg.Clone()})
}

// Error closes open blocks and emits the terminal error event. reason is
// agent.StopError or agent.StopAborted.
func (b *Builder) Error(reason agent.StopReason, message string) {
	if b.done {
		return
	}
	b.closeAll()
	b.msg.StopReason = reason
	b.msg.ErrorMessage = message
	b.done = true
	b.emit(&agent.AssistantError{Reason: reason, Error: b.msg.Clone()})
}

func isText(c agent.Content) bool     { _, ok := c.(*agent.TextContent); return ok }
func isThinking(c agent.Content) bool { _, ok := c.(*agent.ThinkingContent); return ok }
