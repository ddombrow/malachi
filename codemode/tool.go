package codemode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ddombrow/malachi/agent"
)

// ToolName is the name of the tool NewTool returns.
const ToolName = "script"

const (
	// DefaultTimeout bounds a script the model gave no timeout for.
	DefaultTimeout = 10 * time.Minute
	// DefaultMaxParallel is how many of a script's calls run at once.
	DefaultMaxParallel = 8
	// argPreview is how much of a long string argument the call record
	// keeps: enough to recognize it, not a second copy of a file written.
	argPreview = 200
)

// Options configure the script tool. The zero value is usable but for
// Engine, which is required.
type Options struct {
	// Engine returns the engine to run scripts on. It is called on each
	// run, so the interpreter is compiled only once a script is run.
	Engine func() (*Engine, error)
	// Limits bound each script; zero fields take the defaults.
	Limits Limits
	// Timeout applies when the model gives none. Zero means DefaultTimeout;
	// negative means none.
	Timeout time.Duration
	// MaxParallel caps a batch's concurrent calls. Zero means
	// DefaultMaxParallel.
	MaxParallel int
	// BeforeToolCall and AfterToolCall wrap each call a script makes, as
	// the harness's hooks wrap the model's own.
	BeforeToolCall agent.BeforeToolCall
	AfterToolCall  agent.AfterToolCall
	// OnCall hears every call a script made, with its final result: where a
	// host logs what it would log for the model's own calls. Calls are
	// reported one at a time.
	OnCall func(call *agent.ToolCall, result agent.ToolResult, isError bool)
	// OnScript hears how every script ended.
	OnScript func(Details)
	// Extra, if set, adds tools when each script starts: tools whose set
	// changes while a session runs, such as MCP servers'. It may wait, bounded
	// by ctx, for tools that are on their way.
	Extra func(ctx context.Context) []*agent.Tool
	// NotFound, if set, explains a call to a tool no script may call, beyond
	// listing the tools there are: why a tool the model was told of is
	// missing. It returns "" when it has nothing to add.
	NotFound func(name string) string
}

// Details is the script tool's result details, and its progress while it
// runs: every call the script made, so what it did is on the record.
type Details struct {
	Calls         []CallRecord `json:"calls"`
	Batches       int          `json:"batches"`
	OutputDropped int          `json:"outputDropped,omitempty"`
	Error         string       `json:"error,omitempty"`
	DurationMs    int64        `json:"durationMs"`
}

// CallRecord is one call a script made.
type CallRecord struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	// Args are the call's arguments, long strings shortened.
	Args       any    `json:"args,omitempty"`
	Status     string `json:"status"` // "running", "done" or "error"
	DurationMs int64  `json:"durationMs,omitempty"`
}

const (
	StatusRunning = "running"
	StatusDone    = "done"
	StatusError   = "error"
)

// NewTool returns the script tool, whose scripts call tools. tools are what
// scripts may call; each call runs the tool as the agent loop would, so a
// script can do nothing the model could not do one call at a time.
func NewTool(tools []*agent.Tool, opts Options) *agent.Tool {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
	}
	r := &runner{tools: tools, opts: opts}
	return &agent.Tool{
		Name:  ToolName,
		Label: "Script",
		Description: "Run a short JavaScript program that calls tools, and get back only what it prints and returns. " +
			"The code is the body of an async function. Every tool is a function: `await tools.read({path: \"go.mod\"})` " +
			"returns {text, details, isError}; a failed call is a result with isError set, not an exception. " +
			"Calls made together run together: `await Promise.all([tools.read(...), tools.grep(...)])`. " +
			"print() and console.log() write output; `return` a value to end with it (non-strings as JSON). " +
			"ALL_TOOLS lists the tools, describeTool(name) gives one's parameters, searchTools(query) finds tools by keyword. " +
			"There is no require, filesystem, network, process or timers: everything goes through tools. " +
			"Available tools: " + strings.Join(names, ", ") + ".",
		PromptSnippet: "Run JavaScript that calls the other tools and returns only what matters",
		PromptGuidelines: []string{
			"Use script to chain or fan out tool calls whose intermediate results you don't need to see: reading several files, filtering search results, or the same edit across files. Return just the lines that matter.",
			"Use ordinary tool calls for a single step, or when you need to see a result before deciding the next one.",
		},
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"code":    map[string]any{"type": "string", "description": "JavaScript: the body of an async function"},
				"timeout": map[string]any{"type": "number", "description": "Timeout in seconds (optional)"},
			},
			"required": []string{"code"},
		},
		Execute: r.execute,
	}
}

type runner struct {
	tools []*agent.Tool
	opts  Options
}

// toolset is what one script may call: the tool's own tools plus any Extra
// gave when it started.
type toolset struct {
	byName  map[string]*agent.Tool
	names   []string
	catalog []ToolInfo
}

func (r *runner) toolset(ctx context.Context) *toolset {
	tools := r.tools
	if r.opts.Extra != nil {
		tools = append(slices.Clip(tools), r.opts.Extra(ctx)...)
	}
	ts := &toolset{byName: make(map[string]*agent.Tool, len(tools))}
	for _, t := range tools {
		if _, dup := ts.byName[t.Name]; dup {
			continue
		}
		ts.byName[t.Name] = t
		ts.names = append(ts.names, t.Name)
		ts.catalog = append(ts.catalog, ToolInfo{Name: t.Name, Description: t.Description, Parameters: t.Parameters})
	}
	return ts
}

func (r *runner) execute(ctx context.Context, callID string, args map[string]any, onUpdate func(agent.ToolResult)) (agent.ToolResult, error) {
	code, _ := args["code"].(string)
	if strings.TrimSpace(code) == "" {
		return agent.ToolResult{}, errors.New(`argument "code" is required`)
	}
	timeout := r.opts.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	if v, ok := args["timeout"]; ok && v != nil {
		secs, ok := v.(float64)
		if !ok || secs <= 0 {
			return agent.ToolResult{}, errors.New("timeout must be a number of seconds greater than 0")
		}
		timeout = time.Duration(secs * float64(time.Second))
	}
	if r.opts.Engine == nil {
		return agent.ToolResult{}, errors.New("script: no engine configured")
	}
	eng, err := r.opts.Engine()
	if err != nil {
		return agent.ToolResult{}, fmt.Errorf("script: %w", err)
	}

	runCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	rec := &recorder{start: time.Now(), onUpdate: onUpdate}
	ts := r.toolset(runCtx)
	batch := func(bctx context.Context, calls []Call) []Result {
		return r.runBatch(bctx, ts, callID, calls, rec)
	}
	out, err := eng.Run(runCtx, Script{Code: code, Tools: ts.catalog, Limits: r.opts.Limits}, batch)

	switch {
	case err == nil:
	case ctx.Err() != nil:
		out.Error = "Operation aborted"
	case errors.Is(err, context.DeadlineExceeded):
		out.Error = fmt.Sprintf("Script timed out after %s", timeout)
	default:
		out.Error = err.Error()
	}
	details := rec.finish(out)
	if r.opts.OnScript != nil {
		r.opts.OnScript(details)
	}
	result := agent.ToolResult{Content: []agent.Content{&agent.TextContent{Text: render(out)}}, Details: details}
	if out.Error != "" {
		return agent.ToolResult{}, agent.FailWith(result)
	}
	return result, nil
}

// render is what the model sees: the printed output, then the return value
// or the error.
func render(out Outcome) string {
	var b strings.Builder
	b.WriteString(out.Output)
	if out.OutputDropped > 0 {
		fmt.Fprintf(&b, "[%d more bytes of output not kept]\n", out.OutputDropped)
	}
	switch {
	case out.Error != "":
		b.WriteString("Script failed: " + out.Error)
	case out.Result != "":
		b.WriteString(out.Result)
	}
	if b.Len() == 0 {
		return "(no output)"
	}
	return strings.TrimRight(b.String(), "\n")
}

// runBatch runs a batch as the agent loop runs a turn's calls: consecutive
// Parallel tools together, any other tool alone.
func (r *runner) runBatch(ctx context.Context, ts *toolset, parentID string, calls []Call, rec *recorder) []Result {
	results := make([]Result, len(calls))
	rec.mu.Lock()
	rec.batches++
	rec.mu.Unlock()
	for _, c := range calls {
		rec.started(c)
	}
	maxPar := r.opts.MaxParallel
	if maxPar <= 0 {
		maxPar = DefaultMaxParallel
	}
	parallel := func(c Call) bool {
		t := ts.byName[c.Name]
		return t == nil || t.Parallel
	}
	for i := 0; i < len(calls); {
		j := i + 1
		if parallel(calls[i]) {
			for j < len(calls) && parallel(calls[j]) {
				j++
			}
		}
		sem := make(chan struct{}, maxPar)
		var wg sync.WaitGroup
		for k := i; k < j; k++ {
			wg.Go(func() {
				sem <- struct{}{}
				defer func() { <-sem }()
				results[k] = r.runCall(ctx, ts, parentID, calls[k], rec)
			})
		}
		wg.Wait()
		i = j
	}
	return results
}

func (r *runner) runCall(ctx context.Context, ts *toolset, parentID string, c Call, rec *recorder) Result {
	start := time.Now()
	call := &agent.ToolCall{ID: parentID + "/" + strconv.Itoa(c.ID), Name: c.Name}
	result, isError := r.call(ctx, ts, call, c.Args)
	if r.opts.AfterToolCall != nil {
		result, isError = r.opts.AfterToolCall(ctx, call, result, isError)
	}
	if r.opts.OnCall != nil {
		rec.updating.Lock()
		r.opts.OnCall(call, result, isError)
		rec.updating.Unlock()
	}
	rec.ended(c.ID, isError, time.Since(start))

	text := result.Text()
	if slices.ContainsFunc(result.Content, func(c agent.Content) bool { _, ok := c.(*agent.ImageContent); return ok }) {
		text += "\n[image omitted: scripts see text only]"
	}
	return Result{Text: text, Details: result.Details, IsError: isError}
}

func (r *runner) call(ctx context.Context, ts *toolset, call *agent.ToolCall, raw json.RawMessage) (agent.ToolResult, bool) {
	tool := ts.byName[call.Name]
	if tool == nil {
		msg := fmt.Sprintf("Tool %s not found.", call.Name)
		if r.opts.NotFound != nil {
			if why := r.opts.NotFound(call.Name); why != "" {
				msg += " " + why
			}
		}
		return agent.TextResult(msg + " Available: " + strings.Join(ts.names, ", ")), true
	}
	var args map[string]any
	if err := json.Unmarshal(raw, &args); err != nil || args == nil {
		return agent.TextResult(fmt.Sprintf("tools.%s takes one object of arguments", call.Name)), true
	}
	call.Arguments = args
	if r.opts.BeforeToolCall != nil {
		if blocked, reason := r.opts.BeforeToolCall(ctx, call); blocked {
			if reason == "" {
				reason = "Tool execution was blocked"
			}
			return agent.TextResult(reason), true
		}
	}
	if ctx.Err() != nil {
		return agent.TextResult("Operation aborted"), true
	}
	return agent.ExecuteTool(context.WithValue(ctx, inScriptKey{}, true), tool, call, func(agent.ToolResult) {})
}

type inScriptKey struct{}

// InScript reports whether ctx is a call a script made. A tool can use it
// to add structured data to its details, which a script can use and the
// model never sees: the script tool records only its calls, not their
// results.
func InScript(ctx context.Context) bool {
	in, _ := ctx.Value(inScriptKey{}).(bool)
	return in
}

// recorder keeps the call records and reports progress.
type recorder struct {
	start    time.Time
	onUpdate func(agent.ToolResult)
	// updating serializes onUpdate and OnCall: a batch's calls end
	// concurrently, and the host's callbacks need not be safe for that.
	updating sync.Mutex

	mu      sync.Mutex
	calls   []CallRecord
	index   map[int]int
	batches int
}

func (rec *recorder) started(c Call) {
	rec.mu.Lock()
	if rec.index == nil {
		rec.index = map[int]int{}
	}
	var args any
	if json.Unmarshal(c.Args, &args) == nil {
		args = shorten(args)
	}
	rec.index[c.ID] = len(rec.calls)
	rec.calls = append(rec.calls, CallRecord{ID: c.ID, Name: c.Name, Args: args, Status: StatusRunning})
	rec.mu.Unlock()
	rec.update()
}

func (rec *recorder) ended(id int, isError bool, d time.Duration) {
	rec.mu.Lock()
	if i, ok := rec.index[id]; ok {
		rec.calls[i].Status = StatusDone
		if isError {
			rec.calls[i].Status = StatusError
		}
		rec.calls[i].DurationMs = d.Milliseconds()
	}
	rec.mu.Unlock()
	rec.update()
}

func (rec *recorder) snapshot() Details {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return Details{
		Calls:      slices.Clone(rec.calls),
		Batches:    rec.batches,
		DurationMs: time.Since(rec.start).Milliseconds(),
	}
}

func (rec *recorder) update() {
	if rec.onUpdate == nil {
		return
	}
	rec.updating.Lock()
	defer rec.updating.Unlock()
	d := rec.snapshot()
	running := 0
	for _, c := range d.Calls {
		if c.Status == StatusRunning {
			running++
		}
	}
	text := fmt.Sprintf("%d calls, %d running", len(d.Calls), running)
	rec.onUpdate(agent.ToolResult{Content: []agent.Content{&agent.TextContent{Text: text}}, Details: d})
}

func (rec *recorder) finish(out Outcome) Details {
	d := rec.snapshot()
	if d.Calls == nil {
		d.Calls = []CallRecord{}
	}
	d.Batches = out.Batches
	d.OutputDropped = out.OutputDropped
	d.Error = out.Error
	return d
}

// shorten cuts long strings in decoded JSON, so a call record does not keep
// a second copy of every file a script wrote.
func shorten(v any) any {
	switch v := v.(type) {
	case string:
		if len(v) > argPreview {
			cut := argPreview
			for cut > 0 && !utf8Start(v[cut]) {
				cut--
			}
			return fmt.Sprintf("%s… (%d bytes)", v[:cut], len(v))
		}
		return v
	case map[string]any:
		for k, x := range v {
			v[k] = shorten(x)
		}
		return v
	case []any:
		for i, x := range v {
			v[i] = shorten(x)
		}
		return v
	}
	return v
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
