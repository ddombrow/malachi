package agent_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/ai"
	"github.com/ddombrow/malachi/ai/fake"
)

// withUsage wraps a script so its message reports the given usage.
func withUsage(s fake.Script, input, output int64) fake.Script {
	return func(ctx context.Context, req agent.Request, b *ai.Builder) {
		b.Message().Usage = agent.Usage{Input: input, Output: output, TotalTokens: input + output}
		s(ctx, req, b)
	}
}

func TestCompleteReturnsAnswerUsageAndCalls(t *testing.T) {
	p := fake.New(
		withUsage(fake.ToolCalls("", fake.Call{ID: "c1", Name: "read", Args: map[string]any{"path": "a"}}), 100, 10),
		withUsage(fake.Text("done"), 150, 5),
	)
	cfg := agent.LoopConfig{Provider: p, Model: "fake", Tools: []*agent.Tool{echoTool("read")}}
	c, err := agent.Complete(context.Background(), cfg, "go")
	if err != nil {
		t.Fatal(err)
	}
	if c.Text() != "done" || c.Calls != 2 {
		t.Fatalf("text %q, calls %d", c.Text(), c.Calls)
	}
	if c.Usage.Input != 250 || c.Usage.Output != 15 || c.Usage.TotalTokens != 265 {
		t.Fatalf("usage %+v", c.Usage)
	}
	// prompt, tool call, tool result, answer
	if len(c.Messages) != 4 {
		t.Fatalf("messages: %d", len(c.Messages))
	}
	if _, ok := c.Messages[0].(*agent.UserMessage); !ok {
		t.Fatalf("first message %T", c.Messages[0])
	}
}

func TestCompleteProviderErrorIsClassified(t *testing.T) {
	p := fake.New(fake.Fail("429 Too Many Requests: rate limit exceeded"))
	c, err := agent.Complete(context.Background(), agent.LoopConfig{Provider: p, Model: "fake"}, "go")
	var re *agent.RunError
	if !errors.As(err, &re) || re.Kind != agent.ErrorRateLimit || !re.Kind.Retryable() {
		t.Fatalf("err %v", err)
	}
	if c == nil || c.Message != re.Message || c.Calls != 1 {
		t.Fatalf("completion %+v", c)
	}
}

func TestCompleteMaxTurns(t *testing.T) {
	p := fake.New(fake.ToolCalls("", fake.Call{ID: "c", Name: "missing"}))
	c, err := agent.Complete(context.Background(), agent.LoopConfig{Provider: p, Model: "fake", MaxTurns: 1}, "go")
	var re *agent.RunError
	if !errors.As(err, &re) || re.Kind != agent.ErrorMaxTurns || re.Kind.Retryable() {
		t.Fatalf("err %v", err)
	}
	// The max-turns message is the loop's, not a model call.
	if c.Calls != 1 {
		t.Fatalf("calls %d", c.Calls)
	}
}

func TestCompleteCancelledIsAborted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := fake.New(func(ctx context.Context, _ agent.Request, b *ai.Builder) {
		cancel()
		<-ctx.Done()
	})
	_, err := agent.Complete(ctx, agent.LoopConfig{Provider: p, Model: "fake"}, "go")
	var re *agent.RunError
	if !errors.As(err, &re) || re.Kind != agent.ErrorAborted || !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
}

func TestCompleteCancelledDuringToolsIsAborted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := fake.New(fake.ToolCalls("", fake.Call{ID: "c", Name: "stop"}))
	stop := &agent.Tool{
		Name: "stop",
		Execute: func(context.Context, string, map[string]any, func(agent.ToolResult)) (agent.ToolResult, error) {
			cancel()
			return agent.TextResult("stopped"), nil
		},
	}
	c, err := agent.Complete(ctx, agent.LoopConfig{Provider: p, Model: "fake", Tools: []*agent.Tool{stop}}, "go")
	if !errors.Is(err, context.Canceled) || c.Calls != 1 {
		t.Fatalf("err %v, calls %d", err, c.Calls)
	}
}

func TestCompleteNeedsProvider(t *testing.T) {
	if _, err := agent.Complete(context.Background(), agent.LoopConfig{}, "go"); err == nil {
		t.Fatal("expected an error")
	}
}
