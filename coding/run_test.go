package coding

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/ai"
	"github.com/ddombrow/malachi/ai/fake"
)

// events collects everything a session reports, for waiting on.
type events struct {
	t  *testing.T
	ch chan any
}

func watch(t *testing.T, s *Session) *events {
	t.Helper()
	e := &events{t: t, ch: make(chan any, 1024)}
	s.Subscribe(func(v any) { e.ch <- v })
	return e
}

// next returns the first event satisfying match, failing after a timeout.
func (e *events) next(what string, match func(any) bool) any {
	e.t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case v := <-e.ch:
			if match(v) {
				return v
			}
		case <-timeout:
			e.t.Fatalf("timed out waiting for %s", what)
			return nil
		}
	}
}

func (e *events) settled() {
	e.t.Helper()
	e.next("agent_settled", func(v any) bool { _, ok := v.(AgentSettledEvent); return ok })
}

func (e *events) compactionEnd() CompactionEndEvent {
	e.t.Helper()
	return e.next("compaction_end", func(v any) bool { _, ok := v.(CompactionEndEvent); return ok }).(CompactionEndEvent)
}

// blockingReply answers text once release is closed, so a test can act while
// the run is in progress.
func blockingReply(release <-chan struct{}, text string) fake.Script {
	return func(ctx context.Context, _ agent.Request, b *ai.Builder) {
		select {
		case <-release:
		case <-ctx.Done():
			return
		}
		b.Text(text)
		b.Done(agent.StopStop)
	}
}

func openRunSession(t *testing.T, p agent.Provider) *Session {
	t.Helper()
	s, err := Open(Options{Cwd: t.TempDir(), Home: t.TempDir(), Settings: &Settings{}, Provider: p, NoSession: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func userTexts(msgs []agent.Message) []string {
	var out []string
	for _, m := range msgs {
		if u, ok := m.(*agent.UserMessage); ok {
			out = append(out, u.Content.String())
		}
	}
	return out
}

func TestSubmitRunsAndSettles(t *testing.T) {
	s := openRunSession(t, fake.New(fake.Text("hi")))
	ev := watch(t, s)
	if err := s.Submit(context.Background(), "hello", ""); err != nil {
		t.Fatal(err)
	}
	// Nothing was ever queued, so nothing about the queue is reported.
	if q, ok := ev.next("queue_update or agent_settled", func(v any) bool {
		_, q := v.(QueueUpdateEvent)
		_, settled := v.(AgentSettledEvent)
		return q || settled
	}).(QueueUpdateEvent); ok {
		t.Fatalf("a run with an empty queue reported it: %+v", q)
	}
	if st := s.State(); st.Running || st.Compacting {
		t.Fatalf("state after settling: %+v", st)
	}
	if msgs := s.Harness.Messages(); len(msgs) != 2 {
		t.Fatalf("transcript has %d messages", len(msgs))
	}
}

func TestSubmitWhileRunningNeedsABehavior(t *testing.T) {
	release := make(chan struct{})
	p := fake.New(blockingReply(release, "first"), fake.Text("after steer"))
	s := openRunSession(t, p)
	ev := watch(t, s)
	if err := s.Submit(context.Background(), "start", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Submit(context.Background(), "again", ""); !errors.Is(err, ErrStreaming) {
		t.Fatalf("want ErrStreaming, got %v", err)
	}
	if err := s.Submit(context.Background(), "steer this", BehaviorSteer); err != nil {
		t.Fatal(err)
	}
	q := ev.next("queue_update", func(v any) bool { q, ok := v.(QueueUpdateEvent); return ok && len(q.Steering) == 1 }).(QueueUpdateEvent)
	if q.Steering[0] != "steer this" {
		t.Fatalf("queue: %+v", q)
	}
	close(release)
	ev.settled()
	if got := userTexts(s.Harness.Messages()); strings.Join(got, "|") != "start|steer this" {
		t.Fatalf("user turns: %v", got)
	}
	// The queue emptied when the steer was injected, and that was reported.
	if st := s.State(); st.Steering != 0 {
		t.Fatalf("steer still queued: %+v", st)
	}
}

func TestPromptDuringCompactionIsHeldThenSent(t *testing.T) {
	release := make(chan struct{})
	p := fake.New(blockingReply(release, goodSummary), fake.Text("answer to held"))
	s := openRunSession(t, p)
	s.Harness.ReplaceMessages(longConversation(15))
	ev := watch(t, s)

	done := make(chan error, 1)
	go func() { _, err := s.Compact(context.Background(), ""); done <- err }()
	ev.next("compaction_start", func(v any) bool { _, ok := v.(CompactionStartEvent); return ok })

	if err := s.Submit(context.Background(), "typed during the summary", ""); err != nil {
		t.Fatal(err)
	}
	if st := s.State(); st.Running || st.HeldPrompt != "typed during the summary" {
		t.Fatalf("prompt not held: %+v", st)
	}
	if err := s.Submit(context.Background(), "second", ""); !errors.Is(err, ErrBusy) {
		t.Fatalf("a second held prompt should be refused, got %v", err)
	}
	if _, err := s.Reopen(""); !errors.Is(err, ErrBusy) {
		t.Fatalf("Reopen during a compaction: %v", err)
	}
	if err := s.SetModel("glm-5.2"); !errors.Is(err, ErrBusy) {
		t.Fatalf("SetModel during a compaction: %v", err)
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	ev.settled()
	got := userTexts(s.Harness.Messages())
	if got[len(got)-1] != "typed during the summary" {
		t.Fatalf("held prompt was not sent after the compaction: %v", got)
	}
}

func TestAbortedCompactionHandsTheHeldPromptBack(t *testing.T) {
	release := make(chan struct{})
	p := fake.New(blockingReply(release, goodSummary))
	s := openRunSession(t, p)
	s.Harness.ReplaceMessages(longConversation(15))
	before := len(s.Harness.Messages())
	ev := watch(t, s)

	go func() { _, _ = s.Compact(context.Background(), "") }()
	ev.next("compaction_start", func(v any) bool { _, ok := v.(CompactionStartEvent); return ok })
	_ = s.Submit(context.Background(), "keep me", "")
	s.Abort()

	end := ev.compactionEnd()
	if !end.Aborted || end.HeldPrompt != "keep me" {
		t.Fatalf("end event: %+v", end)
	}
	if st := s.State(); st.Running || st.Compacting || st.HeldPrompt != "" {
		t.Fatalf("state after abort: %+v", st)
	}
	if len(s.Harness.Messages()) != before {
		t.Fatal("an aborted compaction changed the transcript")
	}
}

func TestCompactRefusedWhileRunning(t *testing.T) {
	release := make(chan struct{})
	s := openRunSession(t, fake.New(blockingReply(release, "x")))
	ev := watch(t, s)
	_ = s.Submit(context.Background(), "go", "")
	if _, err := s.Compact(context.Background(), ""); !errors.Is(err, ErrBusy) {
		t.Fatalf("Compact during a run: %v", err)
	}
	if _, err := s.Reopen(""); !errors.Is(err, ErrBusy) {
		t.Fatalf("Reopen during a run: %v", err)
	}
	close(release)
	ev.settled()
}

func TestThresholdCompactionBeforePrompt(t *testing.T) {
	fp := &overflowProvider{reply: goodSummary}
	s := newTestSession(t, 1_000, fp)
	seedHistory(t, s, longTranscript(40))
	ev := watch(t, s)
	if err := s.Submit(context.Background(), "next step", ""); err != nil {
		t.Fatal(err)
	}
	start := ev.next("compaction_start", func(v any) bool { _, ok := v.(CompactionStartEvent); return ok }).(CompactionStartEvent)
	if start.Reason != CompactionThreshold {
		t.Fatalf("reason = %s", start.Reason)
	}
	ev.settled()
	msgs := s.Harness.Messages()
	if _, ok := msgs[0].(*agent.CompactionSummaryMessage); !ok {
		t.Fatalf("transcript does not start with the summary: %T", msgs[0])
	}
	if got := userTexts(msgs); got[len(got)-1] != "next step" {
		t.Fatalf("prompt not sent after compacting: %v", got)
	}
}

func TestAutoCompactionCanBeTurnedOff(t *testing.T) {
	fp := &overflowProvider{reply: "ok"}
	s := newTestSession(t, 1_000, fp)
	seedHistory(t, s, longTranscript(40))
	s.SetAutoCompaction(false)
	if err := s.Prompt(context.Background(), "next"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Harness.Messages()[0].(*agent.CompactionSummaryMessage); ok {
		t.Fatal("compacted although auto-compaction was off")
	}
}

func TestFailedSetModelChangesNothing(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	s := openRunSession(t, fake.New())
	provider, model := s.Provider().Name, s.Model()
	if err := s.SetModel("openai/gpt-5.1"); err == nil {
		t.Fatal("expected a missing-key error")
	}
	if s.Provider().Name != provider || s.Model() != model {
		t.Fatalf("failed SetModel changed the selection to %s/%s", s.Provider().Name, s.Model())
	}
}
