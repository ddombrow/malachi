package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/ai"
	"github.com/ddombrow/malachi/ai/fake"
	"github.com/ddombrow/malachi/coding"
)

// testSettings use a provider with no key, so nothing reaches the network:
// listing its models fails at once.
func testSettings() *coding.Settings {
	return &coding.Settings{
		DefaultProvider: "test",
		DefaultModel:    "model-a",
		Providers: map[string]coding.ProviderConfig{"test": {
			BaseURL: "http://127.0.0.1:1", APIKeyEnv: "MALACHI_ACP_TEST_NO_SUCH_KEY",
			Models: []string{"model-a", "model-b"}, ThinkingLevels: []string{"off", "low", "high"},
		}},
	}
}

// client is a scripted ACP client talking to a server over pipes.
type client struct {
	t    *testing.T
	in   io.WriteCloser
	msgs chan map[string]any
	done chan error
	next int
	sv   *Server
	// onRequest answers the agent's requests; nil answers with an error.
	onRequest func(method string, params map[string]any) any
}

// providers hands each opened session the next provider.
type providers struct {
	mu   sync.Mutex
	list []agent.Provider
}

func (p *providers) open(o coding.Options) (*coding.Session, error) {
	p.mu.Lock()
	if len(p.list) > 0 {
		o.Provider, p.list = p.list[0], p.list[1:]
	} else {
		o.Provider = fake.New()
	}
	p.mu.Unlock()
	return coding.Open(o)
}

func start(t *testing.T, home string, settings *coding.Settings, ps ...agent.Provider) *client {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	p := &providers{list: ps}
	sv := New(Options{
		Session: coding.Options{Home: home, Settings: settings},
		Open:    p.open,
		Log:     io.Discard,
	}, inR, outW)
	c := &client{t: t, in: inW, sv: sv, msgs: make(chan map[string]any, 1024), done: make(chan error, 1)}
	go func() {
		err := sv.Run(context.Background())
		outW.Close()
		c.done <- err
	}()
	go func() {
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		for sc.Scan() {
			var m map[string]any
			if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
				t.Errorf("agent wrote a non-JSON line %q", sc.Text())
				continue
			}
			c.msgs <- m
		}
		close(c.msgs)
	}()
	t.Cleanup(func() {
		inW.Close()
		select {
		case <-c.done:
		case <-time.After(30 * time.Second):
			t.Error("server did not stop")
		}
	})
	return c
}

func (c *client) write(v any) {
	c.t.Helper()
	b, _ := json.Marshal(v)
	if _, err := c.in.Write(append(b, '\n')); err != nil {
		c.t.Fatal(err)
	}
}

// call sends a request and returns its id.
func (c *client) call(method string, params any) float64 {
	c.next++
	c.write(map[string]any{"jsonrpc": "2.0", "id": c.next, "method": method, "params": params})
	return float64(c.next)
}

func (c *client) notify(method string, params any) {
	c.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// await reads until the response to id, answering the agent's requests,
// and returns it with the updates sent before it.
func (c *client) await(id float64) (resp map[string]any, updates []map[string]any) {
	c.t.Helper()
	timeout := time.After(20 * time.Second)
	for {
		select {
		case m, ok := <-c.msgs:
			if !ok {
				c.t.Fatalf("connection closed waiting for %v", id)
			}
			c.handle(m, &updates)
			if m["method"] == nil && m["id"] == id {
				return m, updates
			}
		case <-timeout:
			c.t.Fatalf("no response to %v; updates so far: %v", id, updates)
		}
	}
}

// until reads updates until one satisfies ok.
func (c *client) until(what string, ok func(map[string]any) bool) map[string]any {
	c.t.Helper()
	timeout := time.After(20 * time.Second)
	for {
		select {
		case m := <-c.msgs:
			var us []map[string]any
			c.handle(m, &us)
			for _, u := range us {
				if ok(u) {
					return u
				}
			}
		case <-timeout:
			c.t.Fatalf("no %s", what)
		}
	}
}

func (c *client) handle(m map[string]any, updates *[]map[string]any) {
	method, _ := m["method"].(string)
	switch {
	case method == "session/update":
		*updates = append(*updates, m["params"].(map[string]any)["update"].(map[string]any))
	case method != "" && m["id"] != nil:
		params, _ := m["params"].(map[string]any)
		if c.onRequest == nil {
			c.write(map[string]any{"jsonrpc": "2.0", "id": m["id"], "error": map[string]any{"code": -32601, "message": "no"}})
			return
		}
		c.write(map[string]any{"jsonrpc": "2.0", "id": m["id"], "result": c.onRequest(method, params)})
	}
}

func result(t *testing.T, resp map[string]any) map[string]any {
	t.Helper()
	if resp["error"] != nil {
		t.Fatalf("error response: %v", resp["error"])
	}
	r, _ := resp["result"].(map[string]any)
	return r
}

func (c *client) initialize() map[string]any {
	c.t.Helper()
	resp, _ := c.await(c.call("initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}))
	return result(c.t, resp)
}

func (c *client) newSession(cwd string) (string, map[string]any) {
	c.t.Helper()
	resp, _ := c.await(c.call("session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}}))
	r := result(c.t, resp)
	return r["sessionId"].(string), r
}

func (c *client) prompt(id, text string) (string, []map[string]any) {
	c.t.Helper()
	resp, updates := c.await(c.call("session/prompt", map[string]any{
		"sessionId": id, "prompt": []any{map[string]any{"type": "text", "text": text}},
	}))
	return result(c.t, resp)["stopReason"].(string), updates
}

func kinds(us []map[string]any) []string {
	var out []string
	for _, u := range us {
		out = append(out, u["sessionUpdate"].(string))
	}
	return out
}

func text(us []map[string]any, kind string) string {
	var b strings.Builder
	for _, u := range us {
		if u["sessionUpdate"] == kind {
			b.WriteString(u["content"].(map[string]any)["text"].(string))
		}
	}
	return b.String()
}

func TestInitializeAnswersVersionOne(t *testing.T) {
	c := start(t, t.TempDir(), testSettings())
	resp, _ := c.await(c.call("initialize", map[string]any{"protocolVersion": 2, "clientCapabilities": map[string]any{}}))
	r := result(t, resp)
	if r["protocolVersion"] != float64(1) {
		t.Fatalf("version: %v", r["protocolVersion"])
	}
	caps := r["agentCapabilities"].(map[string]any)
	if caps["loadSession"] != true || caps["sessionCapabilities"].(map[string]any)["list"] == nil {
		t.Fatalf("capabilities: %v", caps)
	}
	if mcp := caps["mcpCapabilities"].(map[string]any); mcp["http"] != false || mcp["sse"] != false {
		t.Fatalf("remote MCP advertised: %v", mcp)
	}
	if r["agentInfo"].(map[string]any)["name"] != "malachi" {
		t.Fatalf("agentInfo: %v", r["agentInfo"])
	}
}

func TestPromptStreamsTextThinkingAndToolCalls(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(cwd, "hello.txt"), []byte("hi from file\n"), 0o644)
	c := start(t, home, testSettings(), fake.New(
		fake.ToolCalls("Reading.", fake.Call{ID: "c1", Name: "read", Args: map[string]any{"path": "hello.txt"}}),
		fake.Thinking("it greets", "It says hi."),
	))
	c.initialize()
	id, _ := c.newSession(cwd)
	stop, us := c.prompt(id, "what does hello.txt say?")
	if stop != "end_turn" {
		t.Fatalf("stop reason %q", stop)
	}
	if got := text(us, "agent_message_chunk"); got != "Reading.It says hi." {
		t.Fatalf("message text %q (updates %v)", got, kinds(us))
	}
	if got := text(us, "agent_thought_chunk"); got != "it greets" {
		t.Fatalf("thinking %q", got)
	}
	var call, done map[string]any
	for _, u := range us {
		switch {
		case u["sessionUpdate"] == "tool_call":
			call = u
		case u["sessionUpdate"] == "tool_call_update" && u["status"] == "completed":
			done = u
		}
	}
	if call == nil || call["toolCallId"] != "c1" || call["kind"] != "read" || call["title"] != "Read hello.txt" || call["status"] != "in_progress" {
		t.Fatalf("tool_call: %v", call)
	}
	if loc := call["locations"].([]any)[0].(map[string]any)["path"]; loc != filepath.Join(cwd, "hello.txt") {
		t.Fatalf("location %v", loc)
	}
	if done == nil || !strings.Contains(done["content"].([]any)[0].(map[string]any)["content"].(map[string]any)["text"].(string), "hi from file") {
		t.Fatalf("tool result: %v", done)
	}
}

func TestProviderErrorIsShownAndEndsTheTurn(t *testing.T) {
	c := start(t, t.TempDir(), testSettings(), fake.New(fake.Fail("quota exceeded")))
	c.initialize()
	id, _ := c.newSession(t.TempDir())
	stop, us := c.prompt(id, "hi")
	if stop != "end_turn" || !strings.Contains(text(us, "agent_message_chunk"), "quota exceeded") {
		t.Fatalf("stop %q, text %q", stop, text(us, "agent_message_chunk"))
	}
}

func TestCancelAnswersCancelled(t *testing.T) {
	c := start(t, t.TempDir(), testSettings(), fake.New(func(ctx context.Context, _ agent.Request, b *ai.Builder) {
		b.Text("working")
		<-ctx.Done()
	}))
	c.initialize()
	id, _ := c.newSession(t.TempDir())
	pid := c.call("session/prompt", map[string]any{"sessionId": id, "prompt": []any{map[string]any{"type": "text", "text": "go"}}})
	c.until("first chunk", func(u map[string]any) bool { return u["sessionUpdate"] == "agent_message_chunk" })

	// A second prompt while one runs is refused, not queued.
	resp, _ := c.await(c.call("session/prompt", map[string]any{"sessionId": id, "prompt": []any{map[string]any{"type": "text", "text": "again"}}}))
	if resp["error"] == nil {
		t.Fatalf("concurrent prompt accepted: %v", resp)
	}

	c.notify("session/cancel", map[string]any{"sessionId": id})
	resp, _ = c.await(pid)
	if r := result(t, resp); r["stopReason"] != "cancelled" {
		t.Fatalf("after cancel: %v", r)
	}
}

func TestLoadReplaysHistoryBeforeAnswering(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	c := start(t, home, testSettings(), fake.New(fake.Text("Paris.")))
	c.initialize()
	id, _ := c.newSession(cwd)
	if stop, _ := c.prompt(id, "capital of France?"); stop != "end_turn" {
		t.Fatal(stop)
	}

	c2 := start(t, home, testSettings())
	c2.initialize()
	resp, _ := c2.await(c2.call("session/list", map[string]any{"cwd": cwd}))
	sessions := result(t, resp)["sessions"].([]any)
	if len(sessions) != 1 || sessions[0].(map[string]any)["sessionId"] != id || sessions[0].(map[string]any)["cwd"] != cwd {
		t.Fatalf("list: %v", sessions)
	}
	resp, us := c2.await(c2.call("session/load", map[string]any{"sessionId": id, "cwd": cwd, "mcpServers": []any{}}))
	result(t, resp)
	if text(us, "user_message_chunk") != "capital of France?" || text(us, "agent_message_chunk") != "Paris." {
		t.Fatalf("replay: %v", us)
	}

	for _, bad := range []string{"../" + id, filepath.Join(home, "x.jsonl"), "", "nope"} {
		resp, _ := c2.await(c2.call("session/load", map[string]any{"sessionId": bad, "cwd": cwd}))
		if resp["error"] == nil {
			t.Errorf("load %q accepted", bad)
		}
	}
}

func TestConfigOptionsSetThinkingAndModel(t *testing.T) {
	// Switching model builds a provider, which needs a key; nothing listens
	// at the provider's address, so listing models fails at once.
	t.Setenv("MALACHI_ACP_TEST_NO_SUCH_KEY", "k")
	c := start(t, t.TempDir(), testSettings())
	c.initialize()
	id, r := c.newSession(t.TempDir())
	opts := map[string]map[string]any{}
	for _, o := range r["configOptions"].([]any) {
		o := o.(map[string]any)
		opts[o["id"].(string)] = o
	}
	if opts["model"]["currentValue"] != "test/model-a" || len(opts["model"]["options"].([]any)) != 2 {
		t.Fatalf("model option: %v", opts["model"])
	}
	if opts["thought_level"] == nil {
		t.Fatalf("no thinking option: %v", opts)
	}

	resp, us := c.await(c.call("session/set_config_option", map[string]any{"sessionId": id, "configId": "thought_level", "value": "high"}))
	if !strings.Contains(mustJSON(result(t, resp)), `"currentValue":"high"`) {
		t.Fatalf("after set: %v", resp)
	}
	if !slices.Contains(kinds(us), "config_option_update") {
		t.Fatalf("no config_option_update: %v", kinds(us))
	}
	resp, _ = c.await(c.call("session/set_config_option", map[string]any{"sessionId": id, "configId": "model", "value": "test/model-b"}))
	if !strings.Contains(mustJSON(result(t, resp)), `"currentValue":"test/model-b"`) {
		t.Fatalf("model not set: %v", resp)
	}
	resp, _ = c.await(c.call("session/set_config_option", map[string]any{"sessionId": id, "configId": "model", "value": "other/x"}))
	if resp["error"] == nil {
		t.Fatal("unlisted model accepted")
	}
}

func TestNetworkIsAConfigOptionWhileSandboxed(t *testing.T) {
	c := start(t, t.TempDir(), testSettings())
	c.initialize()
	id, r := c.newSession(t.TempDir())
	if o := option(r, "network"); o == nil || o["currentValue"] != "off" || o["category"] != "_sandbox" {
		t.Fatalf("no network option, off by default: %s", mustJSON(r["configOptions"]))
	}
	as, _ := c.sv.session(id)
	resp, us := c.await(c.call("session/set_config_option", map[string]any{"sessionId": id, "configId": "network", "value": "on"}))
	if o := option(result(t, resp), "network"); o == nil || o["currentValue"] != "on" {
		t.Fatalf("after set: %v", resp)
	}
	if sb := as.s.Sandbox(); !sb.Enabled || !sb.Network {
		t.Fatalf("policy not changed: %+v", sb.Policy)
	}
	if !slices.Contains(kinds(us), "config_option_update") {
		t.Fatalf("no config_option_update: %v", kinds(us))
	}
	resp, _ = c.await(c.call("session/set_config_option", map[string]any{"sessionId": id, "configId": "network", "value": "maybe"}))
	if resp["error"] == nil {
		t.Fatal("unlisted value accepted")
	}

	// With the sandbox off, commands have the network anyway: no option.
	inR, inW := io.Pipe()
	off := New(Options{
		Session: coding.Options{Home: t.TempDir(), Settings: testSettings(), Sandbox: "off"},
		Open:    (&providers{}).open, Log: io.Discard,
	}, inR, io.Discard)
	defer inW.Close()
	res, err := off.request(context.Background(), "session/new", json.RawMessage(mustJSON(map[string]any{"cwd": t.TempDir()})))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(mustJSON(res), `"network"`) {
		t.Fatalf("network offered with the sandbox off: %s", mustJSON(res))
	}
	for _, as := range off.sessions {
		as.close()
	}
}

// option finds a config option by id in a result carrying configOptions.
func option(r map[string]any, id string) map[string]any {
	opts, _ := r["configOptions"].([]any)
	for _, o := range opts {
		if o := o.(map[string]any); o["id"] == id {
			return o
		}
	}
	return nil
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestTrustIsAskedThroughThePermissionDialog(t *testing.T) {
	for _, tc := range []struct {
		answer  any
		stop    string
		trusted bool
		saved   bool
	}{
		{map[string]any{"outcome": "selected", "optionId": trustOnce}, "end_turn", true, false},
		{map[string]any{"outcome": "selected", "optionId": trustAlways}, "end_turn", true, true},
		{map[string]any{"outcome": "selected", "optionId": declineOnce}, "end_turn", false, false},
		{map[string]any{"outcome": "selected", "optionId": declineAlways}, "end_turn", false, true},
		{map[string]any{"outcome": "cancelled"}, "cancelled", false, false},
		{map[string]any{"outcome": "selected", "optionId": "yes-please"}, "cancelled", false, false},
	} {
		home, cwd := t.TempDir(), t.TempDir()
		os.WriteFile(filepath.Join(cwd, "AGENTS.md"), []byte("project rules"), 0o644)
		p := fake.New(fake.Text("ok"))
		c := start(t, home, testSettings(), p)
		var asked map[string]any
		c.onRequest = func(method string, params map[string]any) any {
			if method == "session/request_permission" {
				asked = params
			}
			return map[string]any{"outcome": tc.answer}
		}
		c.initialize()
		id, _ := c.newSession(cwd)
		stop, _ := c.prompt(id, "hi")
		if asked == nil || !strings.Contains(mustJSON(asked), "AGENTS.md") {
			t.Fatalf("%v: not asked, or the files were not named: %v", tc.answer, asked)
		}
		if stop != tc.stop {
			t.Errorf("%v: stop %q, want %q", tc.answer, stop, tc.stop)
		}
		if tc.stop == "end_turn" {
			sys := p.Requests[0].System
			if strings.Contains(sys, "project rules") != tc.trusted {
				t.Errorf("%v: project rules in prompt = %v", tc.answer, !tc.trusted)
			}
		} else if p.Calls() != 0 {
			t.Errorf("%v: prompt sent with trust undecided", tc.answer)
		}
		_, err := os.Stat(coding.TrustStorePath(home))
		if (err == nil) != tc.saved {
			t.Errorf("%v: decision saved = %v", tc.answer, err == nil)
		}
	}
}

func TestEditorMCPServersAreOptIn(t *testing.T) {
	servers := []any{map[string]any{"name": "zed thing", "command": "npx", "args": []any{"-y", "pkg"}, "env": []any{map[string]any{"name": "TOKEN", "value": "s3cret"}}}}
	for _, on := range []bool{false, true} {
		st := testSettings()
		st.ACP.EditorMCPServers = on
		home := t.TempDir()
		var log strings.Builder
		var opened *coding.Session
		inR, inW := io.Pipe()
		outR, outW := io.Pipe()
		go io.Copy(io.Discard, outR)
		sv := New(Options{
			Session: coding.Options{Home: home, Settings: st},
			Open: func(o coding.Options) (*coding.Session, error) {
				o.Provider = fake.New()
				s, err := coding.Open(o)
				opened = s
				return s, err
			},
			Log: &log,
		}, inR, outW)
		done := make(chan struct{})
		go func() { sv.Run(context.Background()); close(done) }()
		_, err := sv.request(context.Background(), "session/new", json.RawMessage(mustJSON(map[string]any{"cwd": t.TempDir(), "mcpServers": servers})))
		if err != nil {
			t.Fatal(err)
		}
		names := []string{}
		for _, s := range opened.MCPState().Servers {
			names = append(names, s.Name+":"+s.Error)
		}
		if !on {
			if len(names) != 0 || !strings.Contains(log.String(), "zed thing") {
				t.Errorf("off: servers %v, log %q", names, log.String())
			}
		} else if len(names) != 1 || !strings.HasPrefix(names[0], "zed-thing:") || !strings.Contains(names[0], "fetches and runs packages") {
			t.Errorf("on: an editor's launcher must be refused by mcp.json's rules: %v", names)
		}
		if strings.Contains(log.String(), "s3cret") {
			t.Error("an env value was logged")
		}
		inW.Close()
		<-done
	}
}

func TestAllowCommandsWidenTheSandboxWithoutTheModel(t *testing.T) {
	dir, err := os.MkdirTemp(os.Getenv("HOME"), "malachi-acp-allow-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	p := fake.New(fake.Text("a path, not a command"))
	c := start(t, t.TempDir(), testSettings(), p)
	c.initialize()
	id, _ := c.newSession(t.TempDir())
	ad := c.until("available_commands_update", func(u map[string]any) bool { return u["sessionUpdate"] == "available_commands_update" })
	if s := mustJSON(ad["availableCommands"]); !strings.Contains(s, `"sandbox"`) || !strings.Contains(s, `"allow-read"`) || !strings.Contains(s, `"allow-write"`) {
		t.Fatalf("commands: %s", s)
	}
	as, _ := c.sv.session(id)

	stop, us := c.prompt(id, "/allow-read "+dir)
	if stop != "end_turn" || !strings.Contains(text(us, "agent_message_chunk"), "Added readable root") {
		t.Fatalf("allow-read: %q %q", stop, text(us, "agent_message_chunk"))
	}
	if err := as.s.Sandbox().CheckRead(filepath.Join(dir, "f")); err != nil {
		t.Fatalf("not readable after /allow-read: %v", err)
	}
	if _, us = c.prompt(id, "/allow-write "+dir); !strings.Contains(text(us, "agent_message_chunk"), "Added writable root") {
		t.Fatalf("allow-write: %q", text(us, "agent_message_chunk"))
	}
	if err := as.s.Sandbox().CheckWrite(filepath.Join(dir, "f")); err != nil {
		t.Fatalf("not writable after /allow-write: %v", err)
	}
	// Off, or where commands get a loopback of their own, that and no more.
	_, us = c.prompt(id, "/sandbox")
	shown := text(us, "agent_message_chunk")
	if !(strings.Contains(shown, "network: off") || strings.Contains(shown, "network: loopback only")) || !strings.Contains(shown, dir) {
		t.Fatalf("/sandbox does not show the network and the added root: %q", shown)
	}
	if _, us = c.prompt(id, "/allow-write"); !strings.Contains(text(us, "agent_message_chunk"), "Usage: /allow-write") {
		t.Fatalf("no usage: %q", text(us, "agent_message_chunk"))
	}
	if _, us = c.prompt(id, "/allow-write "+os.Getenv("HOME")); !strings.Contains(text(us, "agent_message_chunk"), "Not added") {
		t.Fatalf("whole home accepted: %q", text(us, "agent_message_chunk"))
	}
	if p.Calls() != 0 {
		t.Fatalf("a command reached the model (%d calls)", p.Calls())
	}

	// A prompt that merely starts with a slash is the model's.
	if _, us = c.prompt(id, "/usr/bin has what?"); text(us, "agent_message_chunk") != "a path, not a command" || p.Calls() != 1 {
		t.Fatalf("path prompt: %q, %d calls", text(us, "agent_message_chunk"), p.Calls())
	}
}

// finals returns each tool call's last update with a final status, by id.
func finals(us []map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, u := range us {
		if u["sessionUpdate"] == "tool_call_update" && (u["status"] == "completed" || u["status"] == "failed") {
			out[u["toolCallId"].(string)] = u
		}
	}
	return out
}

func TestToolCardsShowDiffsAndFailedCommands(t *testing.T) {
	cwd := t.TempDir()
	os.WriteFile(filepath.Join(cwd, "a.txt"), []byte("one\ntwo\n"), 0o644)
	c := start(t, t.TempDir(), testSettings(), fake.New(
		fake.ToolCalls("", fake.Call{ID: "edit", Name: "edit", Args: map[string]any{"path": "a.txt", "edits": []any{map[string]any{"oldText": "two", "newText": "TWO"}}}}),
		fake.ToolCalls("", fake.Call{ID: "new", Name: "write", Args: map[string]any{"path": "b.txt", "content": "fresh\n"}}),
		fake.ToolCalls("", fake.Call{ID: "fail", Name: "bash", Args: map[string]any{"command": "echo nope; exit 3"}}),
		fake.ToolCalls("", fake.Call{ID: "ok", Name: "bash", Args: map[string]any{"command": "echo fine"}}),
		fake.Text("done"),
	))
	c.initialize()
	id, _ := c.newSession(cwd)
	_, us := c.prompt(id, "go")
	f := finals(us)

	diff := func(callID string) map[string]any {
		content, _ := f[callID]["content"].([]any)
		if len(content) != 1 || content[0].(map[string]any)["type"] != "diff" {
			t.Fatalf("%s: want one diff, got %v", callID, f[callID])
		}
		return content[0].(map[string]any)
	}
	if d := diff("edit"); d["path"] != filepath.Join(cwd, "a.txt") || d["oldText"] != "one\ntwo\n" || d["newText"] != "one\nTWO\n" {
		t.Errorf("edit diff: %v", d)
	}
	if d := diff("new"); d["oldText"] != nil || d["newText"] != "fresh\n" {
		t.Errorf("new file diff: %v", d)
	}
	if f["fail"]["status"] != "failed" || !strings.Contains(mustJSON(f["fail"]), "nope") {
		t.Errorf("exit 3 should be failed, with its output: %v", f["fail"])
	}
	if f["ok"]["status"] != "completed" {
		t.Errorf("exit 0: %v", f["ok"])
	}
}

func TestSandboxNoticeIsGivenOnce(t *testing.T) {
	off := testSettings()
	disabled := false
	off.Sandbox.Enabled = &disabled

	// A client with notices gets one after the session opens.
	c := start(t, t.TempDir(), off, fake.New(fake.Text("hi")))
	resp, _ := c.await(c.call("initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{"session": map[string]any{"notices": map[string]any{}}}}))
	result(t, resp)
	id, _ := c.newSession(t.TempDir())
	n := c.until("notice", func(u map[string]any) bool { return u["sessionUpdate"] == "notice" })
	if n["severity"] != "warning" || !strings.Contains(n["title"].(string), "Sandbox off") {
		t.Fatalf("notice: %v", n)
	}
	if _, us := c.prompt(id, "hi"); strings.Contains(text(us, "agent_message_chunk"), "Sandbox off") {
		t.Fatalf("warned twice: %q", text(us, "agent_message_chunk"))
	}

	// One without them hears it at the start of the first reply, once.
	c = start(t, t.TempDir(), off, fake.New(fake.Text("hi"), fake.Text("again")))
	c.initialize()
	id, _ = c.newSession(t.TempDir())
	if _, us := c.prompt(id, "hi"); !strings.HasPrefix(text(us, "agent_message_chunk"), "**Sandbox off") {
		t.Fatalf("first reply: %q", text(us, "agent_message_chunk"))
	}
	if _, us := c.prompt(id, "hi"); text(us, "agent_message_chunk") != "again" {
		t.Fatalf("second reply: %q", text(us, "agent_message_chunk"))
	}

	// A sandbox in full force says nothing.
	c = start(t, t.TempDir(), testSettings(), fake.New(fake.Text("hi")))
	c.initialize()
	id, _ = c.newSession(t.TempDir())
	if _, us := c.prompt(id, "hi"); text(us, "agent_message_chunk") != "hi" {
		t.Fatalf("sandboxed reply: %q", text(us, "agent_message_chunk"))
	}
}

func TestAdditionalDirectoriesAreReadableOnly(t *testing.T) {
	dir, err := os.MkdirTemp(os.Getenv("HOME"), "malachi-acp-extra-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	c := start(t, t.TempDir(), testSettings())
	if caps := c.initialize()["agentCapabilities"].(map[string]any)["sessionCapabilities"].(map[string]any); caps["additionalDirectories"] == nil {
		t.Fatalf("not advertised: %v", caps)
	}
	resp, _ := c.await(c.call("session/new", map[string]any{"cwd": t.TempDir(), "mcpServers": []any{}, "additionalDirectories": []any{dir}}))
	as, _ := c.sv.session(result(t, resp)["sessionId"].(string))
	if err := as.s.Sandbox().CheckRead(filepath.Join(dir, "f")); err != nil {
		t.Errorf("not readable: %v", err)
	}
	if err := as.s.Sandbox().CheckWrite(filepath.Join(dir, "f")); err == nil {
		t.Error("an additional directory became writable")
	}
	resp, _ = c.await(c.call("session/new", map[string]any{"cwd": t.TempDir(), "additionalDirectories": []any{"relative/dir"}}))
	if resp["error"] == nil {
		t.Error("relative additional directory accepted")
	}
}
