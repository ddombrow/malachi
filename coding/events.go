package coding

import (
	"encoding/json"
	"fmt"
	"sync"
)

// Session events are what a coding session reports beyond the agent's own
// events: compaction, the steering queue, and the end of all work for a
// prompt. They mirror tau_coding/events.py, so their JSON is what Pi and tau
// frontends already understand. Frontends receive them, interleaved with the
// agent's events, from Session.Subscribe.

// CompactionReason says why a compaction happened.
type CompactionReason string

const (
	// CompactionManual is the user's /compact (or RPC compact).
	CompactionManual CompactionReason = "manual"
	// CompactionThreshold folds the conversation before sending a prompt
	// that would not fit the context window.
	CompactionThreshold CompactionReason = "threshold"
	// CompactionOverflow rescues a request the provider rejected for size.
	CompactionOverflow CompactionReason = "overflow"
)

type CompactionStartEvent struct {
	Reason CompactionReason `json:"reason"`
}

type CompactionEndEvent struct {
	Reason CompactionReason `json:"reason"`
	// Result is the compaction's outcome; nil when it failed or was aborted.
	Result       *SummarizeResult `json:"result,omitempty"`
	Aborted      bool             `json:"aborted"`
	WillRetry    bool             `json:"willRetry"`
	ErrorMessage string           `json:"errorMessage,omitempty"`
	// HeldPrompt is a prompt that was waiting for this compaction and was not
	// sent because it was aborted; a frontend gives it back to the user. Not
	// part of the wire format: it is the user's own text, already shown.
	HeldPrompt string `json:"-"`
}

// CompactionProgressEvent reports a compaction moving between steps. It is a
// malachi addition; Pi clients that switch on type ignore it.
type CompactionProgressEvent struct {
	Reason CompactionReason `json:"reason"`
	Phase  string           `json:"phase"`
}

// QueueUpdateEvent reports the messages waiting to be injected into the run.
type QueueUpdateEvent struct {
	Steering []string `json:"steering"`
	FollowUp []string `json:"followUp"`
}

// AgentSettledEvent marks the end of all work started by a prompt, including
// any compaction before it and any prompt held for after it.
type AgentSettledEvent struct{}

type ThinkingLevelChangedEvent struct {
	Level string `json:"level"`
}

func (e CompactionStartEvent) MarshalJSON() ([]byte, error) {
	type a CompactionStartEvent
	return withType("compaction_start", a(e))
}

func (e CompactionEndEvent) MarshalJSON() ([]byte, error) {
	type a CompactionEndEvent
	return withType("compaction_end", a(e))
}

func (e CompactionProgressEvent) MarshalJSON() ([]byte, error) {
	type a CompactionProgressEvent
	return withType("compaction_progress", a(e))
}

func (e QueueUpdateEvent) MarshalJSON() ([]byte, error) {
	type a QueueUpdateEvent
	if e.Steering == nil {
		e.Steering = []string{}
	}
	if e.FollowUp == nil {
		e.FollowUp = []string{}
	}
	return withType("queue_update", a(e))
}

func (AgentSettledEvent) MarshalJSON() ([]byte, error) {
	return []byte(`{"type":"agent_settled"}`), nil
}

func (e ThinkingLevelChangedEvent) MarshalJSON() ([]byte, error) {
	type a ThinkingLevelChangedEvent
	return withType("thinking_level_changed", a(e))
}

// withType marshals v and prepends the Pi "type" discriminator.
func withType(tag string, v any) ([]byte, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	head := fmt.Sprintf(`{"type":%q`, tag)
	if string(body) == "{}" {
		return []byte(head + "}"), nil
	}
	return append([]byte(head+","), body[1:]...), nil
}

// eventBus delivers events to subscribers in order on its own goroutine.
//
// Delivery is asynchronous on purpose. Session methods emit events while a
// frontend may be holding its own lock (an RPC server holds its output lock
// across Submit so that a prompt's response precedes the prompt's events);
// calling listeners synchronously there would deadlock. The queue is
// unbounded for the same reason: a full queue would block the emitter on a
// listener that is waiting for the emitter.
//
// Persistence does not go through here. It stays synchronous on the harness,
// so a message is on disk before any frontend hears of it.
type eventBus struct {
	mu        sync.Mutex
	cond      *sync.Cond
	queue     []any
	listeners []*func(any)
	closed    bool
}

func newEventBus() *eventBus {
	b := &eventBus{}
	b.cond = sync.NewCond(&b.mu)
	go b.loop()
	return b
}

func (b *eventBus) publish(e any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.queue = append(b.queue, e)
	b.cond.Signal()
}

func (b *eventBus) subscribe(l func(any)) (unsubscribe func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p := &l
	b.listeners = append(b.listeners, p)
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		for i, q := range b.listeners {
			if q == p {
				b.listeners = append(b.listeners[:i], b.listeners[i+1:]...)
				return
			}
		}
	}
}

// flushMarker is queued by flush; the loop closes done when it reaches it,
// after every event published before it has been delivered.
type flushMarker struct{ done chan struct{} }

// flush waits until every event published so far has been delivered, or the
// bus has stopped.
func (b *eventBus) flush() {
	m := flushMarker{done: make(chan struct{})}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.queue = append(b.queue, m)
	b.cond.Signal()
	b.mu.Unlock()
	<-m.done
}

// close stops accepting events. Events already queued are still delivered;
// close does not wait for that, so a stuck listener cannot hang a caller.
func (b *eventBus) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	b.cond.Broadcast()
}

func (b *eventBus) loop() {
	for {
		b.mu.Lock()
		for len(b.queue) == 0 && !b.closed {
			b.cond.Wait()
		}
		if len(b.queue) == 0 {
			b.mu.Unlock()
			return
		}
		e := b.queue[0]
		b.queue[0] = nil
		b.queue = b.queue[1:]
		listeners := append([]*func(any){}, b.listeners...)
		b.mu.Unlock()
		if m, ok := e.(flushMarker); ok {
			close(m.done)
			continue
		}
		for _, l := range listeners {
			(*l)(e)
		}
	}
}

// Subscribe registers l for every event of this session, in order: the
// agent's events (agent.Event values, as from Harness.Subscribe) interleaved
// with session events (the *Event types in this file). l runs on the
// session's delivery goroutine, never on the caller's.
func (s *Session) Subscribe(l func(any)) (unsubscribe func()) {
	return s.bus.subscribe(l)
}

// Flush waits until every event so far has reached the subscribers. A
// frontend calls it before exiting so nothing queued is lost.
func (s *Session) Flush() { s.bus.flush() }

// emit publishes a session event.
func (s *Session) emit(e any) { s.bus.publish(e) }
