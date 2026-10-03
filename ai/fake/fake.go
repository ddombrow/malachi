// Package fake is a deterministic scripted provider for tests.
package fake

import (
	"context"
	"encoding/json"
	"iter"
	"sync"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/ai"
)

// Script produces one assistant response through the builder. It must leave
// the builder finished (Done or Error); Provider does so if it does not.
type Script func(ctx context.Context, req agent.Request, b *ai.Builder)

// Provider replays one Script per Stream call, in order.
type Provider struct {
	mu       sync.Mutex
	scripts  []Script
	Requests []agent.Request
}

func New(scripts ...Script) *Provider { return &Provider{scripts: scripts} }

// Calls returns how many times Stream was invoked.
func (p *Provider) Calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.Requests)
}

func (p *Provider) Stream(ctx context.Context, req agent.Request) iter.Seq[agent.AssistantEvent] {
	p.mu.Lock()
	req.Messages = append([]agent.Message(nil), req.Messages...)
	p.Requests = append(p.Requests, req)
	var script Script
	if len(p.scripts) > 0 {
		script, p.scripts = p.scripts[0], p.scripts[1:]
	}
	p.mu.Unlock()

	return func(yield func(agent.AssistantEvent) bool) {
		msg := agent.NewAssistantMessage(req.Model)
		msg.API, msg.Provider = "fake", "fake"
		b := ai.NewBuilder(msg, yield)
		if ctx.Err() != nil {
			b.Error(agent.StopAborted, "Operation aborted")
			return
		}
		if script == nil {
			b.Error(agent.StopError, "fake provider: no scripted response left")
			return
		}
		script(ctx, req, b)
		if ctx.Err() != nil {
			b.Error(agent.StopAborted, "Operation aborted")
		}
		b.Done("")
	}
}

// Text replies with plain text.
func Text(text string) Script {
	return func(_ context.Context, _ agent.Request, b *ai.Builder) {
		b.Text(text)
		b.Done(agent.StopStop)
	}
}

// Call describes a scripted tool call.
type Call struct {
	ID   string
	Name string
	Args map[string]any
}

// ToolCalls replies with optional text followed by tool calls.
func ToolCalls(text string, calls ...Call) Script {
	return func(_ context.Context, _ agent.Request, b *ai.Builder) {
		b.Text(text)
		for _, c := range calls {
			idx := b.ToolCallStart(c.ID, c.Name)
			raw, _ := json.Marshal(c.Args)
			b.ToolCallDelta(idx, string(raw))
			b.ToolCallEnd(idx)
		}
		b.Done(agent.StopToolUse)
	}
}

// Thinking replies with a thinking block then text.
func Thinking(thinking, text string) Script {
	return func(_ context.Context, _ agent.Request, b *ai.Builder) {
		b.Thinking(thinking)
		b.Text(text)
		b.Done(agent.StopStop)
	}
}

// Fail ends the stream with a provider error.
func Fail(message string) Script {
	return func(_ context.Context, _ agent.Request, b *ai.Builder) {
		b.Error(agent.StopError, message)
	}
}
