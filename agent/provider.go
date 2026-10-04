package agent

import (
	"context"
	"iter"
)

// Request is one model call: the full replayable context plus tools.
type Request struct {
	Model         string
	System        string
	Messages      []Message
	Tools         []*Tool
	ThinkingLevel string // "" or "off" disables reasoning; otherwise provider-specific
	SessionID     string // routing / prompt-cache affinity hint; may be ignored
}

// RequestPreparer may derive a provider-specific context view before a request
// is streamed. It receives a detached copy of the transcript; changes to the
// request do not alter the harness history or persisted session.
type RequestPreparer func(Request) Request

// Provider streams one assistant response.
//
// Contract: the sequence always ends with exactly one *AssistantDone or
// *AssistantError event carrying the final message. Transport failures and
// cancellation (ctx done) are reported as AssistantError, never out of band.
type Provider interface {
	Stream(ctx context.Context, req Request) iter.Seq[AssistantEvent]
}

// AssistantEvent is a provider-neutral streaming event for one assistant
// message. Every event except the terminal ones carries a Partial snapshot.
// The JSON discriminator is "type".
type AssistantEvent interface{ assistantEventType() string }

type AssistantStart struct {
	Partial *AssistantMessage `json:"partial"`
}

type TextStart struct {
	ContentIndex int               `json:"contentIndex"`
	Partial      *AssistantMessage `json:"partial"`
}

type TextDelta struct {
	ContentIndex int               `json:"contentIndex"`
	Delta        string            `json:"delta"`
	Partial      *AssistantMessage `json:"partial"`
}

type TextEnd struct {
	ContentIndex int               `json:"contentIndex"`
	Content      string            `json:"content"`
	Partial      *AssistantMessage `json:"partial"`
}

type ThinkingStart struct {
	ContentIndex int               `json:"contentIndex"`
	Partial      *AssistantMessage `json:"partial"`
}

type ThinkingDelta struct {
	ContentIndex int               `json:"contentIndex"`
	Delta        string            `json:"delta"`
	Partial      *AssistantMessage `json:"partial"`
}

type ThinkingEnd struct {
	ContentIndex int               `json:"contentIndex"`
	Content      string            `json:"content"`
	Partial      *AssistantMessage `json:"partial"`
}

type ToolCallStart struct {
	ContentIndex int               `json:"contentIndex"`
	Partial      *AssistantMessage `json:"partial"`
}

type ToolCallDelta struct {
	ContentIndex int               `json:"contentIndex"`
	Delta        string            `json:"delta"`
	Partial      *AssistantMessage `json:"partial"`
}

type ToolCallEnd struct {
	ContentIndex int               `json:"contentIndex"`
	ToolCall     *ToolCall         `json:"toolCall"`
	Partial      *AssistantMessage `json:"partial"`
}

// AssistantDone ends a successful stream. Reason is stop, length, or toolUse.
type AssistantDone struct {
	Reason  StopReason        `json:"reason"`
	Message *AssistantMessage `json:"message"`
}

// AssistantError ends a failed stream. Reason is error or aborted.
type AssistantError struct {
	Reason StopReason        `json:"reason"`
	Error  *AssistantMessage `json:"error"`
}

func (*AssistantStart) assistantEventType() string { return "start" }
func (*TextStart) assistantEventType() string      { return "text_start" }
func (*TextDelta) assistantEventType() string      { return "text_delta" }
func (*TextEnd) assistantEventType() string        { return "text_end" }
func (*ThinkingStart) assistantEventType() string  { return "thinking_start" }
func (*ThinkingDelta) assistantEventType() string  { return "thinking_delta" }
func (*ThinkingEnd) assistantEventType() string    { return "thinking_end" }
func (*ToolCallStart) assistantEventType() string  { return "toolcall_start" }
func (*ToolCallDelta) assistantEventType() string  { return "toolcall_delta" }
func (*ToolCallEnd) assistantEventType() string    { return "toolcall_end" }
func (*AssistantDone) assistantEventType() string  { return "done" }
func (*AssistantError) assistantEventType() string { return "error" }

// AssistantEventType returns the Pi wire discriminator of an event.
func AssistantEventType(e AssistantEvent) string { return e.assistantEventType() }
