package agent

import (
	"context"
	"fmt"
	"iter"
	"sync/atomic"
)

// Completion is the outcome of a run started by Complete.
type Completion struct {
	// Message is the run's last assistant message: the answer when the run
	// succeeded, the failure when it did not. It is nil only if the run
	// never reached the model.
	Message *AssistantMessage
	// Messages are the messages the run added, prompt first, in transcript
	// order. Pass them as history to continue the conversation.
	Messages []Message
	// Usage is summed over every model call in the run, including a call
	// that failed and was retried after overflow recovery.
	Usage Usage
	// Calls is how many times the provider was called.
	Calls int
}

// Text is the final answer's text, or "" when there is no final message.
func (c *Completion) Text() string {
	if c.Message == nil {
		return ""
	}
	return c.Message.Text()
}

// RunError is a run that ended without an answer. Kind says why, in terms an
// application can act on; Message is the failed assistant message, when there
// is one.
type RunError struct {
	Kind    ErrorKind
	Message *AssistantMessage
	// cause is the context's error for an aborted run, so errors.Is
	// recognises context.Canceled and context.DeadlineExceeded.
	cause error
}

func (e *RunError) Error() string {
	text := "run aborted"
	if e.Message != nil && e.Message.ErrorMessage != "" {
		text = e.Message.ErrorMessage
	} else if e.cause != nil {
		text = e.cause.Error()
	}
	return fmt.Sprintf("agent: %s: %s", e.Kind, text)
}

func (e *RunError) Unwrap() error { return e.cause }

// Complete runs the loop on a single text prompt with no history and waits
// for the answer. It is for background jobs that want a result rather than a
// stream of events: a run that needs progress, steering or history uses Run
// or Harness instead.
//
// The error is a *RunError when the run ended in a provider error, was
// cancelled, or reached cfg.MaxTurns; the Completion is returned either way,
// so usage is accounted for failed runs too.
func Complete(ctx context.Context, cfg LoopConfig, prompt string) (*Completion, error) {
	return CompleteMessages(ctx, cfg, nil, NewUserText(prompt))
}

// CompleteMessages is Complete with an existing transcript and arbitrary
// prompt messages, such as a user message with images.
func CompleteMessages(ctx context.Context, cfg LoopConfig, history []Message, prompts ...Message) (*Completion, error) {
	if cfg.Provider == nil {
		return nil, fmt.Errorf("agent: Complete needs a provider")
	}
	counter := &countingProvider{Provider: cfg.Provider}
	cfg.Provider = counter

	added := Run(ctx, cfg, history, prompts, func(Event) {})
	c := &Completion{Messages: added, Calls: int(counter.calls.Load())}
	for _, m := range added {
		if a, ok := m.(*AssistantMessage); ok {
			c.Message = a
			c.Usage = c.Usage.Add(a.Usage)
		}
	}

	switch {
	case c.Message != nil && (c.Message.StopReason == StopError || c.Message.StopReason == StopAborted):
		err := &RunError{Kind: Classify(c.Message), Message: c.Message}
		if err.Kind == ErrorAborted {
			err.cause = ctx.Err()
		}
		return c, err
	case ctx.Err() != nil:
		// Cancelled while tools ran: the transcript ends with tool results,
		// not an answer.
		return c, &RunError{Kind: ErrorAborted, Message: c.Message, cause: ctx.Err()}
	}
	return c, nil
}

// countingProvider counts Stream calls, which is the only reliable count of
// model calls: the loop also adds assistant messages no provider produced.
type countingProvider struct {
	Provider
	calls atomic.Int64
}

func (p *countingProvider) Stream(ctx context.Context, req Request) iter.Seq[AssistantEvent] {
	p.calls.Add(1)
	return p.Provider.Stream(ctx, req)
}
