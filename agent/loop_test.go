package agent_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
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

func TestParallelToolsRunConcurrentlyAndPreserveOrder(t *testing.T) {
	firstStarted := make(chan struct{})
	secondFinished := make(chan struct{})
	tool := &agent.Tool{
		Name:     "read",
		Parallel: true,
		Execute: func(_ context.Context, callID string, _ map[string]any, up func(agent.ToolResult)) (agent.ToolResult, error) {
			switch callID {
			case "c1":
				close(firstStarted)
				<-secondFinished
				up(agent.TextResult("c1-part"))
				return agent.TextResult("res-1"), nil
			case "c2":
				<-firstStarted
				up(agent.TextResult("c2-part"))
				close(secondFinished)
				return agent.TextResult("res-2"), nil
			default:
				return agent.TextResult("other"), nil
			}
		},
	}

	var beforeOrder, afterOrder []string
	p := fake.New(
		fake.ToolCalls("", fake.Call{ID: "c1", Name: "read"}, fake.Call{ID: "c2", Name: "read"}),
		fake.Text("done"),
	)
	r, out := run(t, p, agent.LoopConfig{
		Tools: []*agent.Tool{tool},
		BeforeToolCall: func(_ context.Context, call *agent.ToolCall) (bool, string) {
			beforeOrder = append(beforeOrder, call.ID)
			return false, ""
		},
		AfterToolCall: func(_ context.Context, call *agent.ToolCall, res agent.ToolResult, isErr bool) (agent.ToolResult, bool) {
			afterOrder = append(afterOrder, call.ID)
			return res, isErr
		},
	}, "read both")

	if !slices.Equal(beforeOrder, []string{"c1", "c2"}) {
		t.Fatalf("BeforeToolCall order = %v, want [c1 c2]", beforeOrder)
	}
	if !slices.Equal(afterOrder, []string{"c1", "c2"}) {
		t.Fatalf("AfterToolCall order = %v, want [c1 c2]", afterOrder)
	}
	r1, r2 := out[2].(*agent.ToolResultMessage), out[3].(*agent.ToolResultMessage)
	if r1.ToolCallID != "c1" || r1.Text() != "res-1" || r2.ToolCallID != "c2" || r2.Text() != "res-2" {
		t.Fatalf("results out of order: %+v, %+v", r1, r2)
	}

	var toolEvents []string
	for _, ev := range r.events {
		switch e := ev.(type) {
		case *agent.ToolExecutionStartEvent:
			toolEvents = append(toolEvents, "start:"+e.ToolCallID)
		case *agent.ToolExecutionEndEvent:
			toolEvents = append(toolEvents, "end:"+e.ToolCallID)
		}
	}
	wantEvents := []string{"start:c1", "start:c2", "end:c1", "end:c2"}
	if !slices.Equal(toolEvents, wantEvents) {
		t.Fatalf("tool events = %v, want %v", toolEvents, wantEvents)
	}
}

func TestSequentialToolActsAsBarrierBetweenParallelGroups(t *testing.T) {
	var (
		mu     sync.Mutex
		phases []string
		active atomic.Int32
	)
	record := func(s string) {
		mu.Lock()
		phases = append(phases, s)
		mu.Unlock()
	}

	g1Ready := make(chan struct{})
	par := &agent.Tool{
		Name:     "par",
		Parallel: true,
		Execute: func(_ context.Context, callID string, _ map[string]any, _ func(agent.ToolResult)) (agent.ToolResult, error) {
			active.Add(1)
			defer active.Add(-1)
			switch callID {
			case "p1":
				close(g1Ready)
			case "p2":
				<-g1Ready
			}
			record("run:" + callID)
			return agent.TextResult(callID), nil
		},
	}
	seq := &agent.Tool{
		Name:     "seq",
		Parallel: false,
		Execute: func(_ context.Context, callID string, _ map[string]any, _ func(agent.ToolResult)) (agent.ToolResult, error) {
			if n := active.Load(); n != 0 {
				t.Errorf("sequential tool ran while %d parallel tools were active", n)
			}
			record("run:" + callID)
			return agent.TextResult(callID), nil
		},
	}

	p := fake.New(
		fake.ToolCalls("",
			fake.Call{ID: "p1", Name: "par"},
			fake.Call{ID: "p2", Name: "par"},
			fake.Call{ID: "s1", Name: "seq"},
			fake.Call{ID: "p3", Name: "par"},
		),
		fake.Text("done"),
	)
	_, out := run(t, p, agent.LoopConfig{Tools: []*agent.Tool{par, seq}}, "go")
	if len(out) != 7 {
		t.Fatalf("want 7 messages, got %d", len(out))
	}
	if phases[2] != "run:s1" || phases[3] != "run:p3" {
		t.Fatalf("barrier violated: phases = %v", phases)
	}
}

func TestMaxParallelCapsConcurrentTools(t *testing.T) {
	var current, maxSeen atomic.Int32
	par := &agent.Tool{
		Name:     "par",
		Parallel: true,
		Execute: func(_ context.Context, id string, _ map[string]any, _ func(agent.ToolResult)) (agent.ToolResult, error) {
			n := current.Add(1)
			for {
				prev := maxSeen.Load()
				if n <= prev || maxSeen.CompareAndSwap(prev, n) {
					break
				}
			}
			time.Sleep(15 * time.Millisecond)
			current.Add(-1)
			return agent.TextResult(id), nil
		},
	}
	p := fake.New(
		fake.ToolCalls("",
			fake.Call{ID: "1", Name: "par"},
			fake.Call{ID: "2", Name: "par"},
			fake.Call{ID: "3", Name: "par"},
			fake.Call{ID: "4", Name: "par"},
		),
		fake.Text("done"),
	)
	_, out := run(t, p, agent.LoopConfig{Tools: []*agent.Tool{par}, MaxParallel: 2}, "go")
	if len(out) != 7 {
		t.Fatalf("want 7 messages, got %d", len(out))
	}
	if got := maxSeen.Load(); got > 2 {
		t.Fatalf("max concurrent tools = %d, want <= 2", got)
	}
}

func TestParallelToolCancellationAnswersEveryCall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	par := &agent.Tool{
		Name:     "par",
		Parallel: true,
		Execute: func(ctx context.Context, id string, _ map[string]any, _ func(agent.ToolResult)) (agent.ToolResult, error) {
			if id == "a" {
				cancel()
			}
			<-ctx.Done()
			return agent.ToolResult{}, ctx.Err()
		},
	}
	seq := &agent.Tool{
		Name: "seq",
		Execute: func(ctx context.Context, _ string, _ map[string]any, _ func(agent.ToolResult)) (agent.ToolResult, error) {
			return agent.TextResult("should not run"), nil
		},
	}
	p := fake.New(fake.ToolCalls("",
		fake.Call{ID: "a", Name: "par"},
		fake.Call{ID: "b", Name: "par"},
		fake.Call{ID: "c", Name: "seq"},
	))
	r := &recorder{}
	out := agent.Run(ctx, agent.LoopConfig{Provider: p, Model: "fake", Tools: []*agent.Tool{par, seq}}, nil, []agent.Message{agent.NewUserText("x")}, r.emit)
	if p.Calls() != 1 {
		t.Fatalf("provider should not be called again after cancel, calls=%d", p.Calls())
	}
	if len(out) != 5 {
		t.Fatalf("want all 3 calls answered, got %d messages", len(out))
	}
	for i, id := range []string{"a", "b", "c"} {
		res := out[2+i].(*agent.ToolResultMessage)
		if res.ToolCallID != id || !res.IsError || res.Text() != "Operation aborted" {
			t.Fatalf("result %d: %+v", i, res)
		}
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
