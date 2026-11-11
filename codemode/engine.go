// Package codemode runs short JavaScript programs that call tools: the model
// writes one script in place of a run of tool calls, and only what the script
// prints or returns goes back to it.
//
// Scripts run in QuickJS-ng compiled to WebAssembly (quickjs.wasm, built from
// wasm/ by `go tool task wasm`) under wazero. The guest gets no filesystem,
// network, environment or timers: it can print, read a clock, and hand tool
// calls to the host, which runs them through the caller's Batch. Whatever a
// script does, it does through tools.
package codemode

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

//go:embed quickjs.wasm
var quickjsWasm []byte

//go:embed prelude.js
var prelude string

// Call is one tool call a script made.
type Call struct {
	ID   int             `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

// Result is what a script sees for a call: the tool's text, its details, and
// whether it failed. A failed call is a value the script can inspect, not an
// exception.
type Result struct {
	Text    string `json:"text"`
	Details any    `json:"details,omitempty"`
	IsError bool   `json:"isError"`
}

// Batch runs calls a script made while it had nothing else to do, and
// returns their results in the same order. Calls in one batch were made
// together (say, under Promise.all) and may run concurrently.
type Batch func(ctx context.Context, calls []Call) []Result

// Limits bound one script. Zero fields take the defaults.
type Limits struct {
	// Memory is the JavaScript heap, in bytes.
	Memory int
	// Output is how much printed text is kept; the rest is counted.
	Output int
	// Calls is the most tool calls one script may make.
	Calls int
}

const (
	DefaultMemory = 256 << 20
	DefaultOutput = 1 << 20
	DefaultCalls  = 200
)

func (l Limits) withDefaults() Limits {
	if l.Memory <= 0 {
		l.Memory = DefaultMemory
	}
	if l.Output <= 0 {
		l.Output = DefaultOutput
	}
	if l.Calls <= 0 {
		l.Calls = DefaultCalls
	}
	return l
}

// Outcome is how a script ended.
type Outcome struct {
	// Result is the script's return value as text: a string as is, anything
	// else as JSON.
	Result string
	// Output is what the script printed, up to Limits.Output.
	Output string
	// OutputDropped counts printed bytes beyond Limits.Output.
	OutputDropped int
	// Error is set when the script threw, failed to parse, or never
	// finished: the message and, for an Error, its stack.
	Error string
	// Calls is how many tool calls the script made.
	Calls int
	// Batches is how many times the script handed calls to the host.
	Batches int
}

// Engine runs scripts. It compiles the interpreter once; each script gets a
// fresh instance, so nothing carries over between scripts. An Engine is safe
// for concurrent use.
type Engine struct {
	rt       wazero.Runtime
	compiled wazero.CompiledModule
}

// maxMemoryPages caps the wasm memory of any one script, behind the heap
// limit (which a script's own data structures count against): 64 KiB pages,
// 512 MiB in all, the most Limits.Memory can sensibly ask for plus room for
// the interpreter itself.
const maxMemoryPages = 8192

// NewEngine compiles the interpreter. With cacheDir set, the compiled form
// is cached there, so later processes start scripts without compiling.
func NewEngine(ctx context.Context, cacheDir string) (*Engine, error) {
	cfg := wazero.NewRuntimeConfig().
		WithMemoryLimitPages(maxMemoryPages).
		WithCloseOnContextDone(true)
	if cacheDir != "" {
		cache, err := wazero.NewCompilationCacheWithDir(cacheDir)
		if err != nil {
			return nil, fmt.Errorf("codemode: compilation cache: %w", err)
		}
		cfg = cfg.WithCompilationCache(cache)
	}
	rt := wazero.NewRuntimeWithConfig(ctx, cfg)
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		rt.Close(ctx)
		return nil, err
	}
	if err := instantiateHost(ctx, rt); err != nil {
		rt.Close(ctx)
		return nil, err
	}
	compiled, err := rt.CompileModule(ctx, quickjsWasm)
	if err != nil {
		rt.Close(ctx)
		return nil, fmt.Errorf("codemode: compile: %w", err)
	}
	return &Engine{rt: rt, compiled: compiled}, nil
}

// Close releases the engine.
func (e *Engine) Close(ctx context.Context) error { return e.rt.Close(ctx) }

// Run runs code as the body of an async function: it can await tool calls
// and return a value. Run returns an error only when the script could not be
// run at all or was stopped by ctx; a script that throws ends with
// Outcome.Error.
func (e *Engine) Run(ctx context.Context, code string, lim Limits, batch Batch) (Outcome, error) {
	lim = lim.withDefaults()
	st := &runState{ctx: ctx, lim: lim, batch: batch}
	cfg := wazero.NewModuleConfig().
		WithName(""). // anonymous, so scripts can run side by side
		WithStartFunctions("_initialize").
		WithSysWalltime().
		WithSysNanotime().
		WithRandSource(rand.Reader).
		WithStdout(io.Discard).
		WithStderr(io.Discard)
	// No WithFS, WithEnv or WithArgs: the guest sees no files, variables or
	// arguments.
	mod, err := e.rt.InstantiateModule(withRun(ctx, st), e.compiled, cfg)
	if err != nil {
		return Outcome{}, stopped(ctx, fmt.Errorf("codemode: instantiate: %w", err))
	}
	defer mod.Close(context.WithoutCancel(ctx))

	rctx := withRun(ctx, st)
	pre, err := putString(rctx, mod, prelude)
	if err != nil {
		return Outcome{}, err
	}
	src, err := putString(rctx, mod, code)
	if err != nil {
		return Outcome{}, err
	}
	res, err := mod.ExportedFunction("cm_run").Call(rctx,
		api.EncodeU32(pre.ptr), api.EncodeU32(pre.n), api.EncodeU32(src.ptr), api.EncodeU32(src.n),
		api.EncodeI32(int32(lim.Memory)))
	out := st.outcome()
	if err != nil {
		return out, stopped(ctx, fmt.Errorf("codemode: %w", err))
	}
	if ctx.Err() != nil {
		return out, ctx.Err()
	}
	switch api.DecodeI32(res[0]) {
	case 0, 1: // finished, or the script failed: Outcome says which
	default:
		if out.Error == "" {
			out.Error = "internal error"
		}
		return out, fmt.Errorf("codemode: %s", out.Error)
	}
	return out, nil
}

// stopped prefers ctx's error when ctx ended the run.
func stopped(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

type guestString struct{ ptr, n uint32 }

// putString copies s into the guest's memory.
func putString(ctx context.Context, mod api.Module, s string) (guestString, error) {
	res, err := mod.ExportedFunction("cm_alloc").Call(ctx, api.EncodeI32(int32(len(s)+1)))
	if err != nil {
		return guestString{}, fmt.Errorf("codemode: alloc: %w", err)
	}
	ptr := api.DecodeU32(res[0])
	if ptr == 0 || !mod.Memory().WriteString(ptr, s) {
		return guestString{}, errors.New("codemode: out of memory placing the script")
	}
	return guestString{ptr, uint32(len(s))}, nil
}

// runState is one script's side of the host functions.
type runState struct {
	ctx   context.Context
	lim   Limits
	batch Batch

	mu      sync.Mutex
	out     Outcome
	printed int
	pending []byte // the last batch response, until the guest takes it
}

func (st *runState) outcome() Outcome {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.out
}

type runKey struct{}

func withRun(ctx context.Context, st *runState) context.Context {
	return context.WithValue(ctx, runKey{}, st)
}

func runOf(ctx context.Context) *runState {
	st, _ := ctx.Value(runKey{}).(*runState)
	return st
}

const (
	outPrint  = 0
	outResult = 1
	outError  = 2
)

// instantiateHost defines the module glue.c imports.
func instantiateHost(ctx context.Context, rt wazero.Runtime) error {
	_, err := rt.NewHostModuleBuilder("malachi").
		NewFunctionBuilder().WithFunc(hostBatch).Export("batch").
		NewFunctionBuilder().WithFunc(hostTake).Export("take").
		NewFunctionBuilder().WithFunc(hostOutput).Export("output").
		NewFunctionBuilder().WithFunc(hostInterrupted).Export("interrupted").
		Instantiate(ctx)
	return err
}

// hostBatch runs a batch of tool calls. It returns the response's length;
// a refusal returns -(length+1) and the reason, which the guest throws.
func hostBatch(ctx context.Context, mod api.Module, ptr, n uint32) int32 {
	st := runOf(ctx)
	refuse := func(msg string) int32 {
		st.mu.Lock()
		st.pending = []byte(msg)
		st.mu.Unlock()
		return -int32(len(msg)) - 1
	}
	req, ok := mod.Memory().Read(ptr, n)
	if !ok {
		return refuse("batch request out of range")
	}
	var calls []Call
	if err := json.Unmarshal(req, &calls); err != nil {
		return refuse("malformed batch: " + err.Error())
	}
	st.mu.Lock()
	if st.out.Calls+len(calls) > st.lim.Calls {
		st.mu.Unlock()
		return refuse(fmt.Sprintf("tool call limit reached: a script may make %d calls", st.lim.Calls))
	}
	st.out.Calls += len(calls)
	st.out.Batches++
	st.mu.Unlock()

	results := st.batch(st.ctx, calls)
	if len(results) != len(calls) {
		return refuse(fmt.Sprintf("host returned %d results for %d calls", len(results), len(calls)))
	}
	resp, err := json.Marshal(results)
	if err != nil {
		return refuse("encoding results: " + err.Error())
	}
	st.mu.Lock()
	st.pending = resp
	st.mu.Unlock()
	return int32(len(resp))
}

// hostTake copies the pending batch response into the guest's buffer.
func hostTake(ctx context.Context, mod api.Module, ptr uint32) {
	st := runOf(ctx)
	st.mu.Lock()
	resp := st.pending
	st.pending = nil
	st.mu.Unlock()
	mod.Memory().Write(ptr, resp)
}

func hostOutput(ctx context.Context, mod api.Module, kind int32, ptr, n uint32) {
	st := runOf(ctx)
	b, ok := mod.Memory().Read(ptr, n)
	if !ok {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	switch kind {
	case outPrint:
		room := max(0, st.lim.Output-st.printed)
		keep := min(room, len(b))
		st.out.Output += string(b[:keep])
		st.out.OutputDropped += len(b) - keep
		st.printed += keep
	case outResult:
		st.out.Result = string(b)
	case outError:
		st.out.Error = string(b)
	}
}

// hostInterrupted stops the interpreter once the run's context is done.
// QuickJS calls it every few thousand operations, so it must be cheap.
func hostInterrupted(ctx context.Context) int32 {
	if st := runOf(ctx); st != nil && st.ctx.Err() != nil {
		return 1
	}
	return 0
}
