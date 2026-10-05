package coding

import (
	"context"
	"errors"
	"fmt"

	"github.com/ddombrow/malachi/agent"
)

// The session owns the rules for when work may start: one run at a time, no
// run while a compaction is replacing the transcript, and no session swap or
// model change in the middle of either. They used to live in the TUI, where
// any other frontend would have bypassed them.

var (
	// ErrStreaming is returned for a prompt sent while the agent is running
	// without saying whether to steer the run or follow it up.
	ErrStreaming = errors.New("the agent is already running; send this as a steer or a follow-up")
	// ErrBusy is returned for work that cannot start until a run or a
	// compaction has finished.
	ErrBusy = errors.New("session is busy")
)

// Streaming behaviours for Submit while a run is in progress.
const (
	BehaviorSteer    = "steer"    // inject before the run's next provider call
	BehaviorFollowUp = "followUp" // send when the run would otherwise stop
)

// RunState is a snapshot of what the session is doing.
type RunState struct {
	Running    bool // a prompt is being worked on (including compacting first)
	Compacting bool // a summary is being written
	// HeldPrompt is a prompt submitted during a manual compaction, sent when
	// it finishes.
	HeldPrompt string
	Steering   int // messages queued to steer the run
	FollowUp   int // messages queued to follow it up
}

// State reports what the session is doing right now.
func (s *Session) State() RunState {
	s.runMu.Lock()
	st := RunState{Running: s.running, Compacting: s.compacting, HeldPrompt: s.held}
	s.runMu.Unlock()
	steer, follow := s.Harness.Queued()
	st.Steering, st.FollowUp = len(steer), len(follow)
	return st
}

// busyLocked explains why work cannot start now, or returns nil. Callers hold
// runMu.
func (s *Session) busyLocked() error {
	switch {
	case s.running:
		return fmt.Errorf("%w: wait for the current run to finish (or abort it)", ErrBusy)
	case s.compacting:
		return fmt.Errorf("%w: wait for the compaction to finish (or abort it)", ErrBusy)
	}
	return nil
}

// busy is busyLocked for callers that do not hold runMu.
func (s *Session) busy() error {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	return s.busyLocked()
}

// SetAutoCompaction turns automatic compaction on or off: both compacting
// before a prompt that would not fit and rescuing a request the provider
// rejected for size. It is on by default.
func (s *Session) SetAutoCompaction(enabled bool) {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	s.autoCompactOff = !enabled
}

// AutoCompactionEnabled reports whether automatic compaction is on.
func (s *Session) AutoCompactionEnabled() bool {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	return !s.autoCompactOff
}

// Submit sends text to the agent and returns without waiting for it.
//
//   - Idle: a run starts, compacting first if the conversation has outgrown
//     the context window. AgentSettledEvent marks the end of all its work.
//   - Running: behavior decides. BehaviorSteer or BehaviorFollowUp queues the
//     text; an empty behavior returns ErrStreaming rather than guessing.
//   - Compacting: the text is held and sent once the compaction finishes, or
//     handed back in CompactionEndEvent.HeldPrompt if it is aborted. Only one
//     prompt is held; a second returns ErrBusy.
//
// ctx bounds the run's lifetime; Abort also stops it.
func (s *Session) Submit(ctx context.Context, text, behavior string) error {
	switch behavior {
	case "", BehaviorSteer, BehaviorFollowUp:
	default:
		return fmt.Errorf("unknown streaming behavior %q (want %q or %q)", behavior, BehaviorSteer, BehaviorFollowUp)
	}
	s.runMu.Lock()
	switch {
	case s.running:
		s.runMu.Unlock()
		msg := agent.NewUserText(text)
		switch behavior {
		case BehaviorSteer:
			s.Harness.Steer(msg)
		case BehaviorFollowUp:
			s.Harness.FollowUp(msg)
		default:
			return ErrStreaming
		}
		s.emitQueue()
		return nil
	case s.compacting:
		defer s.runMu.Unlock()
		if s.held != "" {
			return fmt.Errorf("%w: a prompt is already waiting for the compaction", ErrBusy)
		}
		s.held = text
		return nil
	}
	runCtx := s.startRunLocked(ctx)
	s.runMu.Unlock()
	go s.run(runCtx, text)
	return nil
}

// Prompt sends text and waits until all the work it started has finished. It
// is Submit for callers that want to block, such as print mode.
func (s *Session) Prompt(ctx context.Context, text string) error {
	s.runMu.Lock()
	if err := s.busyLocked(); err != nil {
		s.runMu.Unlock()
		return err
	}
	runCtx := s.startRunLocked(ctx)
	s.runMu.Unlock()
	s.run(runCtx, text)
	return nil
}

// startRunLocked marks the session running and returns the run's context.
// Callers hold runMu.
func (s *Session) startRunLocked(ctx context.Context) context.Context {
	runCtx, cancel := context.WithCancel(ctx)
	s.running, s.runCancel = true, cancel
	return runCtx
}

// run does the work for one submitted prompt.
func (s *Session) run(ctx context.Context, text string) {
	defer func() {
		s.runMu.Lock()
		cancel := s.runCancel
		s.running, s.runCancel = false, nil
		s.runMu.Unlock()
		if cancel != nil {
			cancel()
		}
		s.emit(AgentSettledEvent{})
	}()

	if s.AutoCompactionEnabled() {
		if estimated, threshold, needed := s.NeedsCompaction(); needed {
			note := fmt.Sprintf(autoCompactNote, estimated, threshold)
			_, aborted, _ := s.compactDuring(ctx, CompactionThreshold, note, text)
			if aborted {
				return // the prompt was handed back with the end event
			}
			// Any other failure is reported in the end event; the prompt is
			// still sent, since failing to send it would mean retyping it
			// because the session got long.
		}
	}
	if ctx.Err() != nil {
		return
	}
	s.diag.NewRun()
	_ = s.Harness.Prompt(ctx, agent.NewUserText(text))
	// A steer or follow-up queued as the run was finishing would otherwise
	// wait for the next prompt. Keep going while anything is queued.
	for ctx.Err() == nil {
		if st := s.State(); st.Steering+st.FollowUp == 0 {
			break
		}
		_ = s.Harness.Continue(ctx)
	}
}

// Compact replaces the conversation's prefix with a summary the model writes,
// reporting progress as session events. It refuses while a run or another
// compaction is in progress; a prompt submitted while it runs is held and
// sent when it finishes.
func (s *Session) Compact(ctx context.Context, instructions string) (*SummarizeResult, error) {
	s.runMu.Lock()
	if err := s.busyLocked(); err != nil {
		s.runMu.Unlock()
		return nil, err
	}
	// Nothing to fold is a refusal, not a compaction that failed: no start
	// or end is reported for it.
	if _, err := compactBoundary(s.Harness.Messages()); err != nil {
		s.runMu.Unlock()
		return nil, err
	}
	cctx := s.startCompactingLocked(ctx)
	s.runMu.Unlock()
	res, aborted, held, err := s.finishCompaction(cctx, CompactionManual, instructions, "")
	if held != "" && !aborted {
		// Sent from the session's own lifetime, not the compaction's, which
		// is over.
		_ = s.Submit(s.life, held, "")
	}
	return res, err
}

// compactDuring compacts inside a run (before a prompt, or rescuing a
// rejected request). pending is the prompt waiting on it, handed back if the
// compaction is aborted.
func (s *Session) compactDuring(ctx context.Context, reason CompactionReason, instructions, pending string) (*SummarizeResult, bool, error) {
	s.runMu.Lock()
	cctx := s.startCompactingLocked(ctx)
	s.runMu.Unlock()
	res, aborted, _, err := s.finishCompaction(cctx, reason, instructions, pending)
	return res, aborted, err
}

// startCompactingLocked marks the session compacting and returns a context
// Abort can cancel. Callers hold runMu.
func (s *Session) startCompactingLocked(ctx context.Context) context.Context {
	cctx, cancel := context.WithCancel(ctx)
	s.compacting, s.compactCancel = true, cancel
	return cctx
}

// finishCompaction runs the summary and reports it. It returns the result,
// whether it was aborted, and the prompt held during it (manual only).
func (s *Session) finishCompaction(ctx context.Context, reason CompactionReason, instructions, pending string) (*SummarizeResult, bool, string, error) {
	s.emit(CompactionStartEvent{Reason: reason})
	res, err := s.Summarize(ctx, instructions, func(phase string) {
		s.emit(CompactionProgressEvent{Reason: reason, Phase: phase})
	})

	s.runMu.Lock()
	cancel := s.compactCancel
	s.compacting, s.compactCancel = false, nil
	held := s.held
	s.held = ""
	s.runMu.Unlock()
	if cancel != nil {
		cancel()
	}

	// A result means the transcript was replaced, even if an abort arrived
	// while it was being written.
	aborted := res == nil && (errors.Is(err, context.Canceled) || ctx.Err() != nil)
	end := CompactionEndEvent{Reason: reason, Result: res}
	switch {
	case aborted:
		end.Aborted = true
		end.HeldPrompt = pending
		if held != "" {
			end.HeldPrompt = held
		}
	case err != nil:
		end.ErrorMessage = err.Error()
	case reason == CompactionOverflow:
		end.WillRetry = true
	}
	s.emit(end)
	return res, aborted, held, err
}

// Abort stops whatever the session is doing: a compaction, a run, or both.
// Messages queued to steer or follow up the run are kept; ClearQueue drops
// them.
func (s *Session) Abort() {
	s.runMu.Lock()
	runCancel, compactCancel := s.runCancel, s.compactCancel
	s.runMu.Unlock()
	if compactCancel != nil {
		compactCancel()
	}
	if runCancel != nil {
		runCancel()
	}
	s.Harness.Cancel()
}

// ClearQueue drops queued steering and follow-up messages.
func (s *Session) ClearQueue() {
	s.Harness.ClearQueues()
	s.emitQueue()
}

// emitQueue reports the queue when it has changed since it was last reported.
func (s *Session) emitQueue() {
	steer, follow := s.Harness.Queued()
	ev := QueueUpdateEvent{Steering: texts(steer), FollowUp: texts(follow)}
	s.runMu.Lock()
	// An empty queue keys as "", the starting value, so a run does not open
	// with a report that nothing is queued.
	key := ""
	if len(ev.Steering)+len(ev.FollowUp) > 0 {
		key = fmt.Sprint(ev.Steering, ev.FollowUp)
	}
	changed := key != s.lastQueue
	s.lastQueue = key
	s.runMu.Unlock()
	if changed {
		s.emit(ev)
	}
}

func texts(ms []agent.Message) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, agent.MessageText(m))
	}
	return out
}
