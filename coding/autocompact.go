package coding

import (
	"context"

	"github.com/ddombrow/malachi/agent"
)

const (
	// autoCompactReserveDivisor holds back a fraction of the context window
	// for the reply and for the turns that follow it before the next check.
	// Compacting is lossy and costs a model call, so the threshold waits until
	// the window is genuinely close rather than reacting at the first sign of
	// growth.
	autoCompactReserveDivisor = 8
	// autoCompactMinReserve keeps the reserve meaningful for small windows,
	// where a fixed fraction would leave nothing to send.
	autoCompactMinReserve = 16_000
)

// autoCompactNote is the instruction given to the model when the session
// compacts itself rather than being asked. It states why, so the handover is
// written for the situation it has to survive.
const autoCompactNote = `This compaction was started automatically: the conversation reached roughly %d tokens against a configured limit of %d, and the rest of the window is reserved for the reply and for what follows.

Write the handover so the work can continue without the earlier turns. Keep what is still needed to act: what is being built, decisions already made and why, constraints and conventions in force, files and symbols that matter, and anything left unfinished or explicitly rejected. Prefer recording a concrete path, name, or decision over recounting the conversation.`

// AutoCompactionThreshold is the estimated prompt size at which the session
// should compact before sending. It is the configured window less a reserve
// for the reply.
func (s *Session) AutoCompactionThreshold() int {
	window := s.ContextWindow()
	reserve := max(window/autoCompactReserveDivisor, autoCompactMinReserve)
	return max(1, window-reserve)
}

// NeedsCompaction reports whether the conversation has grown past the point
// where sending it risks the context window, along with the estimate and the
// threshold it was compared against. It is false when there is not enough
// history to fold: compacting a conversation that cannot be shortened would
// only cost a model call.
func (s *Session) NeedsCompaction() (estimated, threshold int, needed bool) {
	threshold = s.AutoCompactionThreshold()
	stats := s.ContextStats()
	// The provider's own figure once there is one, the local estimate before
	// that, which is what makes this work on a resumed session.
	estimated = int(stats.EffectivePrompt())
	if estimated <= threshold {
		return estimated, threshold, false
	}
	if _, err := compactBoundary(s.Harness.Messages()); err != nil {
		return estimated, threshold, false
	}
	return estimated, threshold, true
}

// recoverOverflow compacts the conversation and prepares the request to be
// retried after the provider rejected it for exceeding the context window.
//
// It declines anything that is not a context rejection. Retrying an
// authentication failure, a malformed request, or a cancelled run would only
// fail again, more slowly.
func (s *Session) recoverOverflow(ctx context.Context, req agent.Request, failed *agent.AssistantMessage) (agent.Request, []agent.Message, bool) {
	if ctx.Err() != nil || !isContextLimit(failed) || !s.AutoCompactionEnabled() {
		return req, nil, false
	}
	if _, err := compactBoundary(s.Harness.Messages()); err != nil {
		// Too little history to fold. Summarizing cannot shrink a conversation
		// that is already short, so the request is genuinely as small as it
		// will get and a retry would be pointless.
		s.diag.LogOverflowRecoveryFailed(err)
		return req, nil, false
	}
	result, aborted, err := s.compactDuring(ctx,
		CompactionOverflow, "The conversation exceeded the model's context window and is being compacted so the request can be retried.", "")
	if aborted {
		return req, nil, false
	}
	if err != nil {
		s.diag.LogOverflowRecoveryFailed(err)
		return req, nil, false
	}
	// Continue from the compacted transcript, not the loop's copy, and rebuild
	// the request through the same preparation every other request gets.
	history := s.Harness.Messages()
	retry := req
	retry.Messages = agent.ProviderContext(history)
	retry = s.preparer.prepare(retry)
	s.diag.LogOverflowRecovery(result.Replaced, result.Kept, result.Usage)
	return retry, history, true
}
