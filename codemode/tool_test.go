package codemode

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ddombrow/malachi/agent"
)

func fakeTool(name string, parallel bool, fn func(args map[string]any) (agent.ToolResult, error)) *agent.Tool {
	return &agent.Tool{
		Name:        name,
		Description: "the " + name + " tool",
		Parallel:    parallel,
		Parameters:  map[string]any{"type": "object"},
		Execute: func(_ context.Context, _ string, args map[string]any, _ func(agent.ToolResult)) (agent.ToolResult, error) {
			return fn(args)
		},
	}
}

func scriptTool(tools []*agent.Tool, opts Options) *agent.Tool {
	opts.Engine = func() (*Engine, error) { return sharedEngine, engineErr }
	return NewTool(tools, opts)
}

func runScript(t *testing.T, tool *agent.Tool, code string) (agent.ToolResult, bool) {
	t.Helper()
	engine(t)
	call := &agent.ToolCall{ID: "s1", Name: ToolName, Arguments: map[string]any{"code": code}}
	return agent.ExecuteTool(context.Background(), tool, call, func(agent.ToolResult) {})
}

func TestScriptToolRunsTools(t *testing.T) {
	read := fakeTool("read", true, func(args map[string]any) (agent.ToolResult, error) {
		r := agent.TextResult("contents of " + args["path"].(string))
		r.Details = map[string]any{"lines": 3}
		return r, nil
	})
	fail := fakeTool("fail", false, func(map[string]any) (agent.ToolResult, error) {
		return agent.ToolResult{}, errors.New("disk on fire")
	})
	var calls []string
	tool := scriptTool([]*agent.Tool{read, fail}, Options{
		OnCall: func(c *agent.ToolCall, r agent.ToolResult, isError bool) {
			calls = append(calls, c.ID+" "+c.Name+" "+r.Text())
		},
	})
	res, isError := runScript(t, tool, `
		const [a, b] = await Promise.all([tools.read({path: "x"}), tools.read({path: "y"})]);
		const f = await tools.fail({});
		const n = await tools.nope({});
		print(a.text, a.details.lines);
		return [b.text, f.isError, f.text, n.text.split(".")[0]];`)
	if isError {
		t.Fatalf("failed: %s", res.Text())
	}
	want := "contents of x 3\n[\n  \"contents of y\",\n  true,\n  \"disk on fire\",\n  \"Tool nope not found\"\n]"
	if res.Text() != want {
		t.Errorf("text %q\nwant %q", res.Text(), want)
	}
	d := res.Details.(Details)
	if len(d.Calls) != 4 || d.Batches != 3 || d.Calls[2].Status != StatusError || d.Calls[0].Status != StatusDone {
		t.Errorf("details %+v", d)
	}
	// The two reads ran together, so either may be reported first.
	if len(calls) != 4 || !slices.Contains(calls, "s1/1 read contents of x") || calls[2] != "s1/3 fail disk on fire" {
		t.Errorf("OnCall saw %q", calls)
	}
}

// A script that fails still reports the calls it made.
func TestScriptToolFailureKeepsDetails(t *testing.T) {
	write := fakeTool("write", false, func(map[string]any) (agent.ToolResult, error) { return agent.TextResult("ok"), nil })
	tool := scriptTool([]*agent.Tool{write}, Options{})
	res, isError := runScript(t, tool, `print("before"); await tools.write({path: "a", content: "x".repeat(1000)}); throw new Error("boom")`)
	if !isError || !strings.HasPrefix(res.Text(), "before\nScript failed: Error: boom") {
		t.Fatalf("result %v %q", isError, res.Text())
	}
	d := res.Details.(Details)
	if len(d.Calls) != 1 || !strings.Contains(d.Error, "boom") {
		t.Fatalf("details %+v", d)
	}
	args := d.Calls[0].Args.(map[string]any)
	if s := args["content"].(string); len(s) > argPreview+30 || !strings.HasSuffix(s, "(1000 bytes)") {
		t.Errorf("long argument kept as %q", s)
	}
}

// Parallel tools in one batch run together; others run one at a time.
func TestScriptToolParallelism(t *testing.T) {
	var mu sync.Mutex
	inFlight, peak := 0, 0
	track := func(map[string]any) (agent.ToolResult, error) {
		mu.Lock()
		inFlight++
		peak = max(peak, inFlight)
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		return agent.TextResult("ok"), nil
	}
	tools := []*agent.Tool{fakeTool("read", true, track), fakeTool("edit", false, track)}
	for _, c := range []struct {
		code string
		want int
	}{
		{`await Promise.all([1, 2, 3, 4].map(() => tools.read({})))`, 4},
		{`await Promise.all([1, 2, 3].map(() => tools.edit({})))`, 1},
	} {
		peak = 0
		if res, isError := runScript(t, scriptTool(tools, Options{}), c.code); isError {
			t.Fatal(res.Text())
		}
		if peak != c.want {
			t.Errorf("%s: peak %d, want %d", c.code, peak, c.want)
		}
	}
	peak = 0
	runScript(t, scriptTool(tools, Options{MaxParallel: 2}), `await Promise.all([1, 2, 3, 4].map(() => tools.read({})))`)
	if peak != 2 {
		t.Errorf("MaxParallel 2: peak %d", peak)
	}
}

func TestScriptToolHooks(t *testing.T) {
	ran := false
	rm := fakeTool("rm", false, func(map[string]any) (agent.ToolResult, error) { ran = true; return agent.TextResult("gone"), nil })
	read := fakeTool("read", true, func(map[string]any) (agent.ToolResult, error) { return agent.TextResult("secret"), nil })
	tool := scriptTool([]*agent.Tool{rm, read}, Options{
		BeforeToolCall: func(_ context.Context, c *agent.ToolCall) (bool, string) {
			return c.Name == "rm", "rm is not allowed"
		},
		AfterToolCall: func(_ context.Context, c *agent.ToolCall, r agent.ToolResult, isError bool) (agent.ToolResult, bool) {
			if c.Name == "read" {
				return agent.TextResult("[redacted]"), isError
			}
			return r, isError
		},
	})
	res, _ := runScript(t, tool, `const a = await tools.rm({}); const b = await tools.read({}); return a.text + " / " + b.text`)
	if ran || res.Text() != "rm is not allowed / [redacted]" {
		t.Errorf("ran %v, result %q", ran, res.Text())
	}
}

func TestScriptToolTimeout(t *testing.T) {
	tool := scriptTool(nil, Options{})
	engine(t)
	call := &agent.ToolCall{ID: "s", Name: ToolName, Arguments: map[string]any{"code": "for (;;) {}", "timeout": 0.1}}
	res, isError := agent.ExecuteTool(context.Background(), tool, call, func(agent.ToolResult) {})
	if !isError || !strings.Contains(res.Text(), "timed out after 100ms") {
		t.Errorf("%v %q", isError, res.Text())
	}
}

func TestScriptToolProgress(t *testing.T) {
	read := fakeTool("read", true, func(map[string]any) (agent.ToolResult, error) { return agent.TextResult("ok"), nil })
	tool := scriptTool([]*agent.Tool{read}, Options{})
	engine(t)
	var updates []Details
	call := &agent.ToolCall{ID: "s", Name: ToolName, Arguments: map[string]any{"code": `await Promise.all([tools.read({}), tools.read({})])`}}
	agent.ExecuteTool(context.Background(), tool, call, func(r agent.ToolResult) { updates = append(updates, r.Details.(Details)) })
	if len(updates) != 4 {
		t.Fatalf("%d updates", len(updates))
	}
	if first := updates[0].Calls; len(first) != 1 || first[0].Status != StatusRunning {
		t.Errorf("first update %+v", first)
	}
	if last := updates[3]; len(last.Calls) != 2 || last.Calls[0].Status != StatusDone || last.Calls[1].Status != StatusDone || last.Batches != 1 {
		t.Errorf("last update %+v", last)
	}
}

func TestScriptToolNoOutput(t *testing.T) {
	res, isError := runScript(t, scriptTool(nil, Options{}), `const x = 1`)
	if isError || res.Text() != "(no output)" {
		t.Errorf("%v %q", isError, res.Text())
	}
	if res.Details.(Details).Calls == nil {
		t.Error("calls should be an empty list, not null")
	}
}

// Tools can tell a script's calls from the model's.
func TestInScript(t *testing.T) {
	var seen []bool
	probe := &agent.Tool{Name: "probe", Execute: func(ctx context.Context, _ string, _ map[string]any, _ func(agent.ToolResult)) (agent.ToolResult, error) {
		seen = append(seen, InScript(ctx))
		return agent.TextResult("ok"), nil
	}}
	runScript(t, scriptTool([]*agent.Tool{probe}, Options{}), `await tools.probe({})`)
	if len(seen) != 1 || !seen[0] || InScript(context.Background()) {
		t.Errorf("seen %v", seen)
	}
}
