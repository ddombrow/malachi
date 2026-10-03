package agent_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/ai"
	"github.com/ddombrow/malachi/ai/fake"
)

func newHarness(p agent.Provider, tools ...*agent.Tool) *agent.Harness {
	return agent.NewHarness(agent.HarnessConfig{Provider: p, Model: "fake", Tools: tools}, nil)
}

func TestHarnessPromptAppendsTranscript(t *testing.T) {
	h := newHarness(fake.New(fake.Text("hi")))
	if err := h.Prompt(context.Background(), agent.NewUserText("hello")); err != nil {
		t.Fatal(err)
	}
	ms := h.Messages()
	if len(ms) != 2 || ms[0].Role() != "user" || ms[1].(*agent.AssistantMessage).Text() != "hi" {
		t.Fatalf("transcript: %#v", ms)
	}
}

func TestHarnessSubscribeAndUnsubscribe(t *testing.T) {
	h := newHarness(fake.New(fake.Text("a"), fake.Text("b")))
	var seen []string
	var transcriptLenAtEnd []int
	unsub := h.Subscribe(func(e agent.Event) {
		seen = append(seen, agent.EventType(e))
		if _, ok := e.(*agent.MessageEndEvent); ok {
			transcriptLenAtEnd = append(transcriptLenAtEnd, len(h.Messages()))
		}
	})
	_ = h.Prompt(context.Background(), agent.NewUserText("1"))
	if !slices.Contains(seen, "message_update") {
		t.Fatal("listener missed streaming updates")
	}
	// Listeners observe the transcript already containing the ended message.
	if !slices.Equal(transcriptLenAtEnd, []int{1, 2}) {
		t.Fatalf("transcript lengths at message_end: %v", transcriptLenAtEnd)
	}
	n := len(seen)
	unsub()
	_ = h.Prompt(context.Background(), agent.NewUserText("2"))
	if len(seen) != n {
		t.Fatal("unsubscribed listener still called")
	}
}

func TestHarnessRejectsOverlapAndDrainsFollowUps(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	p := fake.New(
		func(ctx context.Context, _ agent.Request, b *ai.Builder) {
			close(started)
			<-release
			b.Text("first")
			b.Done(agent.StopStop)
		},
		fake.Text("second"),
	)
	h := newHarness(p)
	errc := make(chan error)
	go func() { errc <- h.Prompt(context.Background(), agent.NewUserText("go")) }()
	<-started
	if err := h.Prompt(context.Background(), agent.NewUserText("again")); !errors.Is(err, agent.ErrRunning) {
		t.Fatalf("want ErrRunning, got %v", err)
	}
	h.FollowUp(agent.NewUserText("later"))
	close(release)
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, m := range h.Messages() {
		texts = append(texts, agent.MessageText(m))
	}
	if !slices.Equal(texts, []string{"go", "first", "later", "second"}) {
		t.Fatalf("transcript: %v", texts)
	}
}

func TestHarnessQueueModeAll(t *testing.T) {
	p := fake.New(fake.Text("one"), fake.Text("two"))
	h := agent.NewHarness(agent.HarnessConfig{Provider: p, Model: "fake", QueueMode: agent.QueueAll}, nil)
	h.FollowUp(agent.NewUserText("a"))
	h.FollowUp(agent.NewUserText("b"))
	_ = h.Prompt(context.Background(), agent.NewUserText("go"))
	if p.Calls() != 2 {
		t.Fatalf("both follow-ups should be injected together; calls=%d", p.Calls())
	}
	if len(p.Requests[1].Messages) != 4 {
		t.Fatalf("second request: %d messages", len(p.Requests[1].Messages))
	}
}

func TestHarnessCancelPersistsInterruptedToolResults(t *testing.T) {
	var h *agent.Harness
	// The provider streams a tool call, then the user cancels before the
	// stream completes: the aborted message carries a call with no result.
	p := fake.New(func(ctx context.Context, _ agent.Request, b *ai.Builder) {
		idx := b.ToolCallStart("c1", "bash")
		b.ToolCallDelta(idx, `{"command":"sleep 10"}`)
		h.Cancel()
		<-ctx.Done()
	})
	h = newHarness(p)
	var ended []agent.Message
	h.Subscribe(func(e agent.Event) {
		if me, ok := e.(*agent.MessageEndEvent); ok {
			ended = append(ended, me.Message)
		}
	})
	_ = h.Prompt(context.Background(), agent.NewUserText("run it"))

	last := ended[len(ended)-1].(*agent.ToolResultMessage)
	if last.ToolCallID != "c1" || last.Text() != agent.InterruptedToolResult {
		t.Fatalf("listeners should get the repair, got %+v", last)
	}
	ms := h.Messages()
	if ms[1].(*agent.AssistantMessage).StopReason != agent.StopAborted || ms[2] != last {
		t.Fatalf("transcript: %#v", ms)
	}
	if h.IsRunning() {
		t.Fatal("still running after cancel")
	}
}

func TestHarnessRepairsLoadedTranscriptOnNextRun(t *testing.T) {
	asst := agent.NewAssistantMessage("m")
	asst.Content = []agent.Content{&agent.ToolCall{ID: "old", Name: "read", Arguments: map[string]any{}}}
	asst.StopReason = agent.StopToolUse
	p := fake.New(fake.Text("ok"))
	h := agent.NewHarness(agent.HarnessConfig{Provider: p, Model: "fake"}, []agent.Message{agent.NewUserText("x"), asst})
	_ = h.Prompt(context.Background(), agent.NewUserText("next"))
	req := p.Requests[0].Messages
	if r, ok := req[2].(*agent.ToolResultMessage); !ok || r.ToolCallID != "old" {
		t.Fatalf("request should include synthetic result: %#v", req)
	}
}
