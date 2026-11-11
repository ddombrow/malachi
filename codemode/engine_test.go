package codemode

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tetratelabs/wazero"
)

var (
	sharedEngine *Engine
	engineOnce   sync.Once
	engineErr    error
)

func engine(t *testing.T) *Engine {
	t.Helper()
	engineOnce.Do(func() { sharedEngine, engineErr = NewEngine(context.Background(), "") })
	if engineErr != nil {
		t.Fatal(engineErr)
	}
	return sharedEngine
}

// echo answers every call with its name and arguments.
func echo(_ context.Context, calls []Call) []Result {
	out := make([]Result, len(calls))
	for i, c := range calls {
		out[i] = Result{Text: c.Name + " " + string(c.Args)}
	}
	return out
}

func run(t *testing.T, code string, batch Batch) Outcome {
	t.Helper()
	if batch == nil {
		batch = echo
	}
	out, err := engine(t).Run(context.Background(), code, Limits{}, batch)
	if err != nil {
		t.Fatalf("run: %v (outcome %+v)", err, out)
	}
	return out
}

func TestReturnsAndPrints(t *testing.T) {
	out := run(t, `print("hello", 1, {a: [1, 2]}); console.log("again"); return {ok: true, n: 2 ** 10};`, nil)
	if out.Error != "" {
		t.Fatal(out.Error)
	}
	if out.Output != "hello 1 {\n  \"a\": [\n    1,\n    2\n  ]\n}\nagain\n" {
		t.Errorf("output %q", out.Output)
	}
	if out.Result != "{\n  \"ok\": true,\n  \"n\": 1024\n}" {
		t.Errorf("result %q", out.Result)
	}
	if out := run(t, `return "plain"`, nil); out.Result != "plain" {
		t.Errorf("string result %q", out.Result)
	}
	if out := run(t, `const x = 1;`, nil); out.Result != "" || out.Error != "" {
		t.Errorf("no return: %+v", out)
	}
}

// Calls made together reach the host as one batch and may run concurrently;
// awaiting one at a time makes one batch each.
func TestPromiseAllBatchesCalls(t *testing.T) {
	var batches [][]string
	var inFlight, peak atomic.Int32
	batch := func(ctx context.Context, calls []Call) []Result {
		names := make([]string, len(calls))
		for i, c := range calls {
			names[i] = c.Name
		}
		batches = append(batches, names)
		out := make([]Result, len(calls))
		var wg sync.WaitGroup
		for i, c := range calls {
			wg.Go(func() {
				n := inFlight.Add(1)
				for {
					p := peak.Load()
					if n <= p || peak.CompareAndSwap(p, n) {
						break
					}
				}
				time.Sleep(20 * time.Millisecond)
				inFlight.Add(-1)
				out[i] = Result{Text: c.Name}
			})
		}
		wg.Wait()
		return out
	}
	out := run(t, `
		const [a, b, c] = await Promise.all([tools.read({path: "a"}), tools.grep({pattern: "x"}), tools.glob({})]);
		const d = await tools.read({path: "d"});
		return [a.text, b.text, c.text, d.text].join(",");`, batch)
	if out.Error != "" {
		t.Fatal(out.Error)
	}
	if out.Result != "read,grep,glob,read" {
		t.Errorf("result %q", out.Result)
	}
	if len(batches) != 2 || len(batches[0]) != 3 || len(batches[1]) != 1 {
		t.Errorf("batches %v, want three calls then one", batches)
	}
	if peak.Load() != 3 {
		t.Errorf("peak concurrency %d, want 3", peak.Load())
	}
	if out.Calls != 4 || out.Batches != 2 {
		t.Errorf("calls %d batches %d", out.Calls, out.Batches)
	}
}

func TestToolResultsAndArguments(t *testing.T) {
	var got []Call
	batch := func(_ context.Context, calls []Call) []Result {
		got = append(got, calls...)
		return []Result{{Text: "nope", IsError: true, Details: map[string]any{"code": 2}}}
	}
	out := run(t, `const r = await tools.bash({command: "false", timeout: 5}); return [r.isError, r.text, r.details.code];`, batch)
	if out.Result != "[\n  true,\n  \"nope\",\n  2\n]" {
		t.Errorf("result %q (error %q)", out.Result, out.Error)
	}
	var args map[string]any
	if len(got) != 1 || got[0].Name != "bash" || json.Unmarshal(got[0].Args, &args) != nil || args["command"] != "false" {
		t.Errorf("calls %+v", got)
	}
	if out := run(t, `return (await tools.ls()).text`, nil); out.Result != "ls {}" {
		t.Errorf("call without arguments sent %q", out.Result)
	}
}

func TestScriptErrors(t *testing.T) {
	for _, c := range []struct{ name, code, want string }{
		{"throw", `throw new Error("boom")`, "Error: boom"},
		{"syntax", `return (`, "SyntaxError"},
		{"reject", `await Promise.reject(new TypeError("bad"))`, "TypeError: bad"},
		{"stuck", `await new Promise(() => {})`, "nothing will settle"},
		{"line numbers", "\n\nnull.x", "script.js:3"},
	} {
		out := run(t, c.code, nil)
		if !strings.Contains(out.Error, c.want) {
			t.Errorf("%s: error %q, want %q", c.name, out.Error, c.want)
		}
	}
}

// The guest has no I/O of its own: no quickjs-libc modules, no Node or
// browser globals.
func TestNoAmbientIO(t *testing.T) {
	out := run(t, `return ["std", "os", "require", "process", "fetch", "XMLHttpRequest", "setTimeout", "__cm_batch", "__cm_print", "scriptArgs"]
		.filter(n => typeof globalThis[n] !== "undefined")`, nil)
	if out.Result != "[]" {
		t.Errorf("ambient globals present: %s (error %q)", out.Result, out.Error)
	}
	out = run(t, `try { await import("qjs:std"); return "imported" } catch (e) { return "refused" }`, nil)
	if out.Result != "refused" {
		t.Errorf("module import: %q %q", out.Result, out.Error)
	}
	// A clock is the one thing it has.
	out = run(t, `return Date.now() > 1.7e12`, nil)
	if out.Result != "true" {
		t.Errorf("clock: %q %q", out.Result, out.Error)
	}
}

// The module imports the four host functions and a few WASI calls, nothing
// else; and the WASI it gets has no files, variables or arguments.
func TestImports(t *testing.T) {
	ctx := context.Background()
	rt := wazero.NewRuntime(ctx)
	defer rt.Close(ctx)
	m, err := rt.CompileModule(ctx, quickjsWasm)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range m.ImportedFunctions() {
		module, name, _ := f.Import()
		switch module {
		case "malachi":
			switch name {
			case "batch", "take", "output", "interrupted":
			default:
				t.Errorf("unexpected host import %s", name)
			}
		case "wasi_snapshot_preview1":
			// Writing to stdout and stderr (discarded) and the clock. A
			// rebuild that reaches for files, the environment or sockets
			// fails here.
			switch name {
			case "fd_write", "fd_seek", "fd_close", "fd_fdstat_get", "clock_time_get":
			default:
				t.Errorf("unexpected WASI import %s", name)
			}
		default:
			t.Errorf("unexpected import %s.%s", module, name)
		}
	}
	if len(m.ImportedMemories()) != 0 {
		t.Error("module imports memory")
	}
}

func TestRunawayLoopIsStopped(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := engine(t).Run(ctx, `for (;;) {}`, Limits{}, echo)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %v to stop", d)
	}
	// The engine still works afterwards.
	if out := run(t, `return 1 + 1`, nil); out.Result != "2" {
		t.Errorf("after interrupt: %+v", out)
	}
}

// Cancelling while tools run stops the script too.
func TestCancelDuringBatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	batch := func(ctx context.Context, calls []Call) []Result {
		cancel()
		<-ctx.Done()
		return make([]Result, len(calls))
	}
	_, err := engine(t).Run(ctx, `await tools.slow(); for (;;) {}`, Limits{}, batch)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
}

func TestMemoryLimit(t *testing.T) {
	out, err := engine(t).Run(context.Background(), `const a = []; for (;;) a.push("x".repeat(1 << 16) + a.length);`, Limits{Memory: 32 << 20}, echo)
	if err != nil {
		t.Fatalf("err %v", err)
	}
	if !strings.Contains(out.Error, "out of memory") {
		t.Errorf("error %q, want out of memory", out.Error)
	}
}

func TestCallLimit(t *testing.T) {
	out, err := engine(t).Run(context.Background(), `for (let i = 0; ; i++) await tools.read({i});`, Limits{Calls: 5}, echo)
	if err != nil {
		t.Fatal(err)
	}
	if out.Calls != 5 || !strings.Contains(out.Error, "tool call limit") {
		t.Errorf("calls %d error %q", out.Calls, out.Error)
	}
}

func TestOutputLimit(t *testing.T) {
	out, err := engine(t).Run(context.Background(), `for (let i = 0; i < 100; i++) print("0123456789");`, Limits{Output: 25}, echo)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Output) != 25 || out.OutputDropped != 100*11-25 {
		t.Errorf("output %d bytes, dropped %d", len(out.Output), out.OutputDropped)
	}
}

// Scripts share nothing: globals set by one are gone in the next.
func TestScriptsAreIsolated(t *testing.T) {
	run(t, `globalThis.leak = 42`, nil)
	if out := run(t, `return typeof leak`, nil); out.Result != "undefined" {
		t.Errorf("global survived: %q", out.Result)
	}
}

func TestConcurrentScripts(t *testing.T) {
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			out, err := engine(t).Run(context.Background(), `const r = await tools.id({}); return r.text`, Limits{}, echo)
			if err != nil || out.Result != "id {}" {
				t.Errorf("script %d: %v %+v", i, err, out)
			}
		})
	}
	wg.Wait()
}
