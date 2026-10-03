package agent

import (
	"context"
	"errors"
	"sync"
)

// ErrRunning is returned when a run is started while another is active.
// Use Steer or FollowUp to queue messages instead.
var ErrRunning = errors.New("agent harness is already running; use Steer or FollowUp to queue messages")

// QueueMode controls how many queued messages are injected per poll.
type QueueMode string

const (
	QueueOneAtATime QueueMode = "one_at_a_time"
	QueueAll        QueueMode = "all"
)

// HarnessConfig configures a Harness. Changes made with Harness.Update take
// effect on the next run.
type HarnessConfig struct {
	Provider       Provider
	Model          string
	System         string
	Tools          []*Tool
	ThinkingLevel  string
	MaxTurns       int
	QueueMode      QueueMode
	SessionID      string
	BeforeToolCall BeforeToolCall
	AfterToolCall  AfterToolCall
}

// Listener receives every event of every run, synchronously and in order,
// on the run's goroutine.
type Listener func(Event)

// Harness is the reusable, stateful agent brain: it owns the transcript,
// message queues, and cancellation, and fans events out to listeners. It is
// independent of coding tools, storage, and UI.
//
// Port of tau_agent/harness.py.
type Harness struct {
	mu        sync.Mutex
	cfg       HarnessConfig
	messages  []Message
	listeners []*Listener
	running   bool
	cancel    context.CancelFunc
	steering  []Message
	followUp  []Message
}

// NewHarness returns a harness seeded with an existing transcript.
func NewHarness(cfg HarnessConfig, messages []Message) *Harness {
	if cfg.QueueMode == "" {
		cfg.QueueMode = QueueOneAtATime
	}
	return &Harness{cfg: cfg, messages: append([]Message(nil), messages...)}
}

// Config returns a copy of the current configuration.
func (h *Harness) Config() HarnessConfig {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cfg
}

// Update mutates the configuration (model, tools, system prompt, ...).
func (h *Harness) Update(f func(*HarnessConfig)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f(&h.cfg)
}

// Messages returns a copy of the transcript.
func (h *Harness) Messages() []Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Message(nil), h.messages...)
}

// ReplaceMessages swaps the transcript, e.g. when switching sessions.
func (h *Harness) ReplaceMessages(ms []Message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.messages = append([]Message(nil), ms...)
}

// IsRunning reports whether a run is active.
func (h *Harness) IsRunning() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.running
}

// Subscribe registers a listener and returns a function that removes it.
func (h *Harness) Subscribe(l Listener) (unsubscribe func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	p := &l
	h.listeners = append(h.listeners, p)
	return func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		for i, q := range h.listeners {
			if q == p {
				h.listeners = append(h.listeners[:i], h.listeners[i+1:]...)
				return
			}
		}
	}
}

// Steer queues a message to inject before the next provider call of the
// active run (or the next run).
func (h *Harness) Steer(m Message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.steering = append(h.steering, m)
}

// FollowUp queues a message to send when the agent would otherwise stop.
func (h *Harness) FollowUp(m Message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.followUp = append(h.followUp, m)
}

// Queued returns copies of the steering and follow-up queues.
func (h *Harness) Queued() (steering, followUp []Message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Message(nil), h.steering...), append([]Message(nil), h.followUp...)
}

// ClearQueues empties both queues and returns what they held.
func (h *Harness) ClearQueues() (steering, followUp []Message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	steering, followUp = h.steering, h.followUp
	h.steering, h.followUp = nil, nil
	return steering, followUp
}

// Cancel aborts the active run, if any.
func (h *Harness) Cancel() {
	h.mu.Lock()
	cancel := h.cancel
	h.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Prompt appends prompt and runs the agent until it stops. It blocks; run it
// on a goroutine to keep a UI responsive.
func (h *Harness) Prompt(ctx context.Context, prompt Message) error {
	return h.run(ctx, []Message{prompt})
}

// Continue runs the agent on the existing transcript without a new prompt.
func (h *Harness) Continue(ctx context.Context) error {
	return h.run(ctx, nil)
}

func (h *Harness) run(parent context.Context, prompts []Message) error {
	h.mu.Lock()
	if h.running {
		h.mu.Unlock()
		return ErrRunning
	}
	h.running = true
	ctx, cancel := context.WithCancel(parent)
	h.cancel = cancel
	cfg := h.cfg
	h.mu.Unlock()

	defer func() {
		cancel()
		h.mu.Lock()
		h.running = false
		h.cancel = nil
		h.mu.Unlock()
	}()

	// Repair dangling tool calls from an earlier interrupted run so the
	// synthetic results flow through listeners (persistence) too.
	h.emitRepairs()

	loopCfg := LoopConfig{
		Provider:            cfg.Provider,
		Model:               cfg.Model,
		System:              cfg.System,
		Tools:               cfg.Tools,
		ThinkingLevel:       cfg.ThinkingLevel,
		SessionID:           cfg.SessionID,
		MaxTurns:            cfg.MaxTurns,
		GetSteeringMessages: func() []Message { return h.drain(&h.steering, cfg.QueueMode) },
		GetFollowUpMessages: func() []Message { return h.drain(&h.followUp, cfg.QueueMode) },
		BeforeToolCall:      cfg.BeforeToolCall,
		AfterToolCall:       cfg.AfterToolCall,
	}
	Run(ctx, loopCfg, h.Messages(), prompts, h.dispatch)

	if ctx.Err() != nil {
		h.emitRepairs()
	}
	return nil
}

// dispatch records completed messages, then notifies listeners. Recording
// first means a listener calling Messages() sees the message it was told of.
func (h *Harness) dispatch(e Event) {
	h.mu.Lock()
	if me, ok := e.(*MessageEndEvent); ok {
		h.messages = append(h.messages, me.Message)
	}
	listeners := append([]*Listener(nil), h.listeners...)
	h.mu.Unlock()
	for _, l := range listeners {
		(*l)(e)
	}
}

func (h *Harness) emitRepairs() {
	for _, m := range h.interruptedToolResults() {
		h.dispatch(&MessageStartEvent{Message: m})
		h.dispatch(&MessageEndEvent{Message: m})
	}
}

// interruptedToolResults returns synthetic error results for every tool call
// in the transcript that has no result.
func (h *Harness) interruptedToolResults() []Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	returned := map[string]bool{}
	for _, m := range h.messages {
		if r, ok := m.(*ToolResultMessage); ok {
			returned[r.ToolCallID] = true
		}
	}
	var out []Message
	for _, m := range h.messages {
		a, ok := m.(*AssistantMessage)
		if !ok {
			continue
		}
		for _, c := range a.ToolCalls() {
			if returned[c.ID] {
				continue
			}
			returned[c.ID] = true
			out = append(out, &ToolResultMessage{
				ToolCallID: c.ID,
				ToolName:   c.Name,
				Content:    []Content{&TextContent{Text: InterruptedToolResult}},
				IsError:    true,
				Timestamp:  NowMillis(),
			})
		}
	}
	return out
}

func (h *Harness) drain(q *[]Message, mode QueueMode) []Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(*q) == 0 {
		return nil
	}
	if mode == QueueAll {
		out := *q
		*q = nil
		return out
	}
	out := (*q)[:1:1]
	*q = (*q)[1:]
	return out
}
