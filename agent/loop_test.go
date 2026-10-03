package agent_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/ai"
	"github.com/ddombrow/malachi/ai/fake"
)

type recorder struct{ events []agent.Event }

func (r *recorder) emit(e agent.Event) { r.events = append(r.events, e) }

func (r *recorder) types() []string {
	out := make([]string, len(r.events))
	for i, e := range r.events {
		out[i] = agent.EventType(e)
	}
	return out
}

// ended returns every message announced with message_end, in order.
func (r *recorder) ended() []agent.Message {
	var out []agent.Message
	for _, e := range r.events {
		if me, ok := e.(*agent.MessageEndEvent); ok {
			out = append(out, me.Message)
		}
	}
	return out
}

func echoTool(name string) *agent.Tool {
	return &agent.Tool{
		Name: name,
		Execute: func(_ context.Context, _ string, args map[string]any, _ func(agent.ToolResult)) (agent.ToolResult, error) {
			r := agent.TextResult("contents of " + args["path"].(string))
			r.Details = map[string]any{"path": args["path"]}
			return r, nil
		},
	}
}

func run(t *testing.T, p agent.Provider, cfg agent.LoopConfig, prompt string) (*recorder, []agent.Message) {
	t.Helper()
	cfg.Provider = p
	if cfg.Model == "" {
		cfg.Model = "fake"
	}
	r := &recorder{}
	out := agent.Run(context.Background(), cfg, nil, []agent.Message{agent.NewUserText(prompt)}, r.emit)
	return r, out
}

func TestLoopStreamsCanonicalEvents(t *testing.T) {
	r, out := run(t, fake.New(fake.Text("Hello")), agent.LoopConfig{}, "Say hello")
	want := []string{
		"agent_start", "turn_start",
		"message_start", "message_end", // prompt
		"message_start", "message_update", "message_update", "message_update", "message_end",
		"turn_end", "agent_end",
	}
	if got := r.types(); !slices.Equal(got, want) {
		t.Fatalf("events\n got %v\nwant %v", got, want)
	}
	if len(out) != 2 || out[1].(*agent.AssistantMessage).Text() != "Hello" {
		t.Fatalf("unexpected new messages: %#v", out)
	}
	if !slices.Equal(r.ended(), out) {
		t.Fatal("message_end messages must equal returned messages")
	}
	if out[1].(*agent.AssistantMessage).Timing == nil {
		t.Fatal("timing not recorded")
	}
}

func TestLoopExecutesToolAndContinues(t *testing.T) {
	p := fake.New(
		fake.ToolCalls("Reading.", fake.Call{ID: "call-1", Name: "read", Args: map[string]any{"path": "README.md"}}),
		fake.Text("Done."),
	)
	r, out := run(t, p, agent.LoopConfig{Tools: []*agent.Tool{echoTool("read")}}, "Read README.md")

	if len(out) != 4 {
		t.Fatalf("want user, assistant, toolResult, assistant; got %d", len(out))
	}
	res := out[2].(*agent.ToolResultMessage)
	if res.ToolName != "read" || res.Text() != "contents of README.md" || res.IsError {
		t.Fatalf("bad result: %+v", res)
	}
	if p.Calls() != 2 || len(p.Requests[1].Messages) != 3 {
		t.Fatalf("second call should see user+assistant+result, got %d calls", p.Calls())
	}
	if !slices.Contains(r.types(), "tool_execution_end") {
		t.Fatal("missing tool_execution_end")
	}
}

func TestLoopUnknownToolIsErrorResult(t *testing.T) {
	p := fake.New(fake.ToolCalls("", fake.Call{ID: "c", Name: "nope"}), fake.Text("ok"))
	_, out := run(t, p, agent.LoopConfig{}, "x")
	res := out[2].(*agent.ToolResultMessage)
	if !res.IsError || res.Text() != "Tool nope not found" {
		t.Fatalf("got %+v", res)
	}
}

func TestLoopToolErrorAndPanicBecomeResults(t *testing.T) {
	failing := &agent.Tool{Name: "fail", Execute: func(context.Context, string, map[string]any, func(agent.ToolResult)) (agent.ToolResult, error) {
		return agent.ToolResult{}, errors.New("disk on fire")
	}}
	panicky := &agent.Tool{Name: "panic", Execute: func(context.Context, string, map[string]any, func(agent.ToolResult)) (agent.ToolResult, error) {
		panic("boom")
	}}
	p := fake.New(fake.ToolCalls("", fake.Call{ID: "a", Name: "fail"}, fake.Call{ID: "b", Name: "panic"}), fake.Text("ok"))
	_, out := run(t, p, agent.LoopConfig{Tools: []*agent.Tool{failing, panicky}}, "x")
	a, b := out[2].(*agent.ToolResultMessage), out[3].(*agent.ToolResultMessage)
	if !a.IsError || a.Text() != "disk on fire" {
		t.Fatalf("error result: %+v", a)
	}
	if !b.IsError || b.Text() != "tool panic panicked: boom" {
		t.Fatalf("panic result: %+v", b)
	}
}

func TestLoopBeforeToolCallBlocks(t *testing.T) {
	called := false
	tool := &agent.Tool{Name: "rm", Execute: func(context.Context, string, map[string]any, func(agent.ToolResult)) (agent.ToolResult, error) {
		called = true
		return agent.TextResult("deleted"), nil
	}}
	p := fake.New(fake.ToolCalls("", fake.Call{ID: "c", Name: "rm"}), fake.Text("ok"))
	_, out := run(t, p, agent.LoopConfig{
		Tools: []*agent.Tool{tool},
		BeforeToolCall: func(context.Context, *agent.ToolCall) (bool, string) {
			return true, "not allowed"
		},
	}, "x")
	res := out[2].(*agent.ToolResultMessage)
	if called || !res.IsError || res.Text() != "not allowed" {
		t.Fatalf("called=%v result=%+v", called, res)
	}
}

func TestLoopProviderErrorStopsRun(t *testing.T) {
	p := fake.New(fake.Fail("503 upstream"))
	_, out := run(t, p, agent.LoopConfig{}, "x")
	a := out[len(out)-1].(*agent.AssistantMessage)
	if a.StopReason != agent.StopError || a.ErrorMessage != "503 upstream" {
		t.Fatalf("got %+v", a)
	}
}

func TestProviderContextDropsEmptyFailedTurns(t *testing.T) {
	failed := agent.NewAssistantMessage("m")
	failed.StopReason = agent.StopError
	ctx := agent.ProviderContext([]agent.Message{agent.NewUserText("a"), failed, agent.NewUserText("b")})
	if len(ctx) != 2 {
		t.Fatalf("want 2 messages, got %d", len(ctx))
	}
}

func TestLoopSteeringAndFollowUp(t *testing.T) {
	work := &agent.Tool{Name: "work", Execute: func(context.Context, string, map[string]any, func(agent.ToolResult)) (agent.ToolResult, error) {
		return agent.TextResult("ok"), nil
	}}
	p := fake.New(fake.ToolCalls("", fake.Call{ID: "c", Name: "work"}), fake.Text("second"), fake.Text("third"))
	steering := []agent.Message{agent.NewUserText("steer")}
	follow := []agent.Message{agent.NewUserText("follow up")}
	pop := func(q *[]agent.Message) func() []agent.Message {
		return func() []agent.Message {
			if len(*q) == 0 {
				return nil
			}
			m := (*q)[:1]
			*q = (*q)[1:]
			return m
		}
	}
	firstPoll := true
	_, out := run(t, p, agent.LoopConfig{
		Tools: []*agent.Tool{work},
		// The loop polls steering once before the first turn; hold the
		// message until after the tool turn like a user typing mid-run.
		GetSteeringMessages: func() []agent.Message {
			if firstPoll {
				firstPoll = false
				return nil
			}
			return pop(&steering)()
		},
		GetFollowUpMessages: pop(&follow),
	}, "start")

	var users []string
	for _, m := range out {
		if u, ok := m.(*agent.UserMessage); ok {
			users = append(users, u.Content.String())
		}
	}
	if !slices.Equal(users, []string{"start", "steer", "follow up"}) {
		t.Fatalf("users: %v", users)
	}
	if p.Calls() != 3 {
		t.Fatalf("calls: %d", p.Calls())
	}
}

func TestLoopMaxTurns(t *testing.T) {
	p := fake.New(fake.ToolCalls("", fake.Call{ID: "c", Name: "missing"}))
	_, out := run(t, p, agent.LoopConfig{MaxTurns: 1}, "loop")
	e := out[len(out)-1].(*agent.AssistantMessage)
	if e.StopReason != agent.StopError || e.ErrorMessage != "Agent stopped after max_turns=1" || p.Calls() != 1 {
		t.Fatalf("got %+v after %d calls", e, p.Calls())
	}
}

func TestLoopCancelDuringStreamIsAborted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := fake.New(func(ctx context.Context, _ agent.Request, b *ai.Builder) {
		b.Text("partial")
		cancel()
		<-ctx.Done()
	})
	r := &recorder{}
	out := agent.Run(ctx, agent.LoopConfig{Provider: p, Model: "fake"}, nil, []agent.Message{agent.NewUserText("x")}, r.emit)
	a := out[len(out)-1].(*agent.AssistantMessage)
	if a.StopReason != agent.StopAborted || a.Text() != "partial" {
		t.Fatalf("got %+v", a)
	}
}

func TestLoopCancelDuringToolStopsAfterTurn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	slow := &agent.Tool{Name: "slow", Execute: func(ctx context.Context, _ string, _ map[string]any, _ func(agent.ToolResult)) (agent.ToolResult, error) {
		cancel()
		select {
		case <-ctx.Done():
			return agent.ToolResult{}, ctx.Err()
		case <-time.After(5 * time.Second):
			return agent.TextResult("late"), nil
		}
	}}
	p := fake.New(fake.ToolCalls("", fake.Call{ID: "a", Name: "slow"}, fake.Call{ID: "b", Name: "slow"}))
	r := &recorder{}
	out := agent.Run(ctx, agent.LoopConfig{Provider: p, Model: "fake", Tools: []*agent.Tool{slow}}, nil, []agent.Message{agent.NewUserText("x")}, r.emit)
	if p.Calls() != 1 {
		t.Fatalf("provider should not be called again after cancel, calls=%d", p.Calls())
	}
	for _, m := range out[2:] {
		res := m.(*agent.ToolResultMessage)
		if !res.IsError || res.Text() != "Operation aborted" {
			t.Fatalf("got %+v", res)
		}
	}
	if len(out) != 4 {
		t.Fatalf("want both calls answered, got %d messages", len(out))
	}
}

func TestToolUpdatesAreEmitted(t *testing.T) {
	tool := &agent.Tool{Name: "prog", Execute: func(_ context.Context, _ string, _ map[string]any, up func(agent.ToolResult)) (agent.ToolResult, error) {
		up(agent.TextResult("50%"))
		return agent.TextResult("done"), nil
	}}
	p := fake.New(fake.ToolCalls("", fake.Call{ID: "c", Name: "prog"}), fake.Text("ok"))
	r, _ := run(t, p, agent.LoopConfig{Tools: []*agent.Tool{tool}}, "x")
	if !slices.Contains(r.types(), "tool_execution_update") {
		t.Fatal("missing tool_execution_update")
	}
}

func TestRepairToolHistory(t *testing.T) {
	call := &agent.ToolCall{ID: "c1", Name: "read", Arguments: map[string]any{}}
	asst := agent.NewAssistantMessage("m")
	asst.Content = []agent.Content{call}
	orphan := &agent.ToolResultMessage{ToolCallID: "zzz", ToolName: "x"}
	user := agent.NewUserText("hi")
	real := &agent.ToolResultMessage{ToolCallID: "c1", ToolName: "read", Content: []agent.Content{&agent.TextContent{Text: "data"}}}

	// Missing result is synthesized; orphan is dropped.
	got := agent.RepairToolHistory([]agent.Message{asst, orphan, user})
	if len(got) != 3 || got[1].(*agent.ToolResultMessage).Text() != agent.InterruptedToolResult {
		t.Fatalf("synthesize: %#v", got)
	}
	// Out-of-place result is moved beside its call.
	got = agent.RepairToolHistory([]agent.Message{asst, user, real})
	if got[1] != real || got[2] != user {
		t.Fatalf("reorder: %#v", got)
	}
}
