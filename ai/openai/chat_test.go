package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ddombrow/malachi/agent"
)

func sseBody(chunks ...string) string {
	var b strings.Builder
	for _, c := range chunks {
		fmt.Fprintf(&b, "data: %s\n\n", c)
	}
	return b.String()
}

type captured struct {
	payload map[string]any
	auth    string
	header  http.Header
}

func server(t *testing.T, status int, body string, cap *captured) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path %s", r.URL.Path)
		}
		if cap != nil {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &cap.payload)
			cap.auth = r.Header.Get("Authorization")
			cap.header = r.Header.Clone()
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func collect(p *Provider, req agent.Request) []agent.AssistantEvent {
	var out []agent.AssistantEvent
	for e := range p.Stream(context.Background(), req) {
		out = append(out, e)
	}
	return out
}

func final(t *testing.T, evs []agent.AssistantEvent) *agent.AssistantMessage {
	t.Helper()
	switch e := evs[len(evs)-1].(type) {
	case *agent.AssistantDone:
		return e.Message
	case *agent.AssistantError:
		return e.Error
	}
	t.Fatalf("last event %T is not terminal", evs[len(evs)-1])
	return nil
}

func TestStreamsTextReasoningAndUsage(t *testing.T) {
	body := sseBody(
		`{"id":"r1","model":"kimi-k2.7-code","choices":[{"delta":{"role":"assistant","reasoning_content":"think"}}]}`,
		`{"id":"r1","choices":[{"delta":{"content":"Hel"}}]}`,
		`{"id":"r1","choices":[{"delta":{"content":"lo"},"finish_reason":"stop"}]}`,
		`{"id":"r1","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":40},"completion_tokens_details":{"reasoning_tokens":5}}}`,
		`[DONE]`,
	)
	cap := &captured{}
	srv := server(t, 200, body, cap)
	p := New(Config{Name: "opencode-go", BaseURL: srv.URL + "/v1", APIKey: "sk-test", SessionHeader: "x-opencode-session", UserAgent: "malachi/test"})
	evs := collect(p, agent.Request{Model: "kimi-k2.7-code", System: "sys", Messages: []agent.Message{agent.NewUserText("hi")}, ThinkingLevel: "high", SessionID: "sess-123"})
	m := final(t, evs)

	if m.StopReason != agent.StopStop || m.Text() != "Hello" || m.ThinkingText() != "think" {
		t.Fatalf("message: %+v", m)
	}
	if th := m.Content[0].(*agent.ThinkingContent); th.ThinkingSignature != "reasoning_content" {
		t.Fatalf("signature: %q", th.ThinkingSignature)
	}
	if m.Usage.Input != 60 || m.Usage.CacheRead != 40 || m.Usage.Output != 20 || *m.Usage.Reasoning != 5 || m.Usage.TotalTokens != 120 {
		t.Fatalf("usage: %+v", m.Usage)
	}
	if m.ResponseID != "r1" || m.Provider != "opencode-go" || m.API != API {
		t.Fatalf("metadata: %+v", m)
	}
	if cap.auth != "Bearer sk-test" || cap.payload["reasoning_effort"] != "high" || cap.payload["stream"] != true {
		t.Fatalf("request: auth=%q payload=%v", cap.auth, cap.payload)
	}
	if cap.header.Get("x-opencode-session") != "sess-123" || cap.header.Get("User-Agent") != "malachi/test" {
		t.Fatalf("headers: %v", cap.header)
	}
	msgs := cap.payload["messages"].([]any)
	if msgs[0].(map[string]any)["role"] != "system" || msgs[1].(map[string]any)["content"] != "hi" {
		t.Fatalf("messages: %v", msgs)
	}
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, agent.AssistantEventType(e))
	}
	want := "start thinking_start thinking_delta thinking_end text_start text_delta text_delta text_end done"
	if strings.Join(kinds, " ") != want {
		t.Fatalf("events: %v", kinds)
	}
}

func TestStreamsInterleavedToolCalls(t *testing.T) {
	body := sseBody(
		`{"choices":[{"delta":{"content":"Let me look."}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"read","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","function":{"name":"bash","arguments":"{\"command\":\"ls\"}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.go\"}"}}]},"finish_reason":"tool_calls"}]}`,
		`[DONE]`,
	)
	srv := server(t, 200, body, nil)
	m := final(t, collect(New(Config{BaseURL: srv.URL + "/v1"}), agent.Request{Model: "m"}))
	calls := m.ToolCalls()
	if m.StopReason != agent.StopToolUse || len(calls) != 2 {
		t.Fatalf("message: %+v", m)
	}
	if calls[0].ID != "call_a" || calls[0].Arguments["path"] != "a.go" || calls[1].Name != "bash" || calls[1].Arguments["command"] != "ls" {
		t.Fatalf("calls: %+v %+v", calls[0], calls[1])
	}
}

func TestHTTPErrorBecomesErrorMessage(t *testing.T) {
	srv := server(t, 401, `{"error":{"message":"invalid api key"}}`, nil)
	m := final(t, collect(New(Config{BaseURL: srv.URL + "/v1"}), agent.Request{Model: "m"}))
	if m.StopReason != agent.StopError || m.ErrorMessage != "HTTP 401: invalid api key" || len(m.Diagnostics) != 1 {
		t.Fatalf("message: %+v", m)
	}
}

// Every failure path leaves a diagnostic agent.Classify can read, so an app
// tells a bad key from a rate limit without matching on error text.
func TestFailuresAreClassified(t *testing.T) {
	srv := server(t, 401, `{"error":{"message":"invalid api key"}}`, nil)
	if k := agent.Classify(final(t, collect(New(Config{BaseURL: srv.URL + "/v1"}), agent.Request{Model: "m"}))); k != agent.ErrorAuth {
		t.Errorf("401: %q", k)
	}

	srv = server(t, 200, sseBody(`{"error":{"message":"slow down","type":"rate_limit_error"}}`), nil)
	m := final(t, collect(New(Config{BaseURL: srv.URL + "/v1"}), agent.Request{Model: "m"}))
	if k := agent.Classify(m); k != agent.ErrorRateLimit || !strings.Contains(m.ErrorMessage, "slow down") {
		t.Errorf("stream error: %q %q", k, m.ErrorMessage)
	}

	// Nothing is listening: a transport failure, not an HTTP one.
	srv = httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	p := New(Config{BaseURL: url + "/v1", MaxRetries: -1})
	if k := agent.Classify(final(t, collect(p, agent.Request{Model: "m"}))); k != agent.ErrorNetwork {
		t.Errorf("refused connection: %q", k)
	}
}

func TestRetriesTransientStatus(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(503)
			return
		}
		_, _ = io.WriteString(w, sseBody(`{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`, "[DONE]"))
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL + "/v1", MaxRetryDelay: time.Millisecond})
	m := final(t, collect(p, agent.Request{Model: "m"}))
	if m.Text() != "ok" || hits.Load() != 2 {
		t.Fatalf("text=%q hits=%d", m.Text(), hits.Load())
	}
}

func TestTruncatedStreamIsError(t *testing.T) {
	srv := server(t, 200, sseBody(`{"choices":[{"delta":{"content":"par"}}]}`), nil)
	m := final(t, collect(New(Config{BaseURL: srv.URL + "/v1"}), agent.Request{Model: "m"}))
	if m.StopReason != agent.StopError || m.Text() != "par" {
		t.Fatalf("message: %+v", m)
	}
}

func TestCancelMidStreamIsAborted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, sseBody(`{"choices":[{"delta":{"content":"slow"}}]}`))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var last agent.AssistantEvent
	for e := range New(Config{BaseURL: srv.URL + "/v1"}).Stream(ctx, agent.Request{Model: "m"}) {
		if _, ok := e.(*agent.TextDelta); ok {
			cancel()
		}
		last = e
	}
	if e, ok := last.(*agent.AssistantError); !ok || e.Reason != agent.StopAborted || e.Error.Text() != "slow" {
		t.Fatalf("last: %#v", last)
	}
}

func TestMessageConversion(t *testing.T) {
	asst := agent.NewAssistantMessage("m")
	asst.Content = []agent.Content{
		&agent.ThinkingContent{Thinking: "hmm", ThinkingSignature: "reasoning"},
		&agent.ToolCall{ID: "call|weird/id", Name: "read", Arguments: map[string]any{"path": "x"}},
	}
	msgs := toChatMessages([]agent.Message{
		agent.NewUserText("q"),
		asst,
		&agent.ToolResultMessage{ToolCallID: "call|weird/id", ToolName: "read"},
		&agent.BranchSummaryMessage{Summary: "earlier"},
	}, false)
	a := msgs[1].(map[string]any)
	if a["content"] != nil || a["reasoning"] != "hmm" {
		t.Fatalf("assistant: %v", a)
	}
	id := a["tool_calls"].([]any)[0].(map[string]any)["id"].(string)
	tool := msgs[2].(map[string]any)
	if !strings.HasPrefix(id, "tc_") || tool["tool_call_id"] != id || tool["content"] != "(no tool output)" {
		t.Fatalf("ids: %q %v", id, tool)
	}
	if _, ok := tool["name"]; ok {
		t.Fatalf("tool message carries a name, which Anthropic-translating gateways reject: %v", tool)
	}
	if u := msgs[3].(map[string]any); u["role"] != "user" || u["content"] != "earlier" {
		t.Fatalf("summary: %v", u)
	}
}

// Reasoning goes back only for the turn in progress: a tool-call chain after
// the last user message keeps it, earlier turns do not. Resending every
// turn's reasoning made it the largest part of a long session's requests.
func TestReasoningReplayedOnlyForCurrentTurn(t *testing.T) {
	thinking := func(text string) *agent.AssistantMessage {
		a := agent.NewAssistantMessage("m")
		a.Content = []agent.Content{
			&agent.ThinkingContent{Thinking: text, ThinkingSignature: "reasoning_content"},
			&agent.ToolCall{ID: text, Name: "read", Arguments: map[string]any{"path": "x"}},
		}
		return a
	}
	result := func(id string) agent.Message {
		return &agent.ToolResultMessage{ToolCallID: id, ToolName: "read",
			Content: []agent.Content{&agent.TextContent{Text: "ok"}}}
	}
	msgs := toChatMessages([]agent.Message{
		agent.NewUserText("first"),
		thinking("old"), result("old"),
		agent.NewUserText("second"),
		thinking("now-1"), result("now-1"),
		thinking("now-2"), result("now-2"),
	}, false)
	reasoning := func(i int) any { return msgs[i].(map[string]any)["reasoning_content"] }
	if r := reasoning(1); r != nil {
		t.Errorf("a previous turn's reasoning was replayed: %v", r)
	}
	if reasoning(4) != "now-1" || reasoning(6) != "now-2" {
		t.Errorf("the current tool-call chain lost its reasoning: %v, %v", reasoning(4), reasoning(6))
	}

	// With no user message in view (a tail that opens mid-turn), everything
	// is the current turn.
	msgs = toChatMessages([]agent.Message{thinking("tail"), result("tail")}, false)
	if reasoning(0) != "tail" {
		t.Errorf("a mid-turn tail lost its reasoning: %v", reasoning(0))
	}
}

func TestListModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer k" || r.Header.Get("User-Agent") != "malachi" {
			w.WriteHeader(403)
			return
		}
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"zeta"},{"id":"alpha"},{"id":""}]}`)
	}))
	defer srv.Close()
	ids, err := New(Config{BaseURL: srv.URL + "/v1", APIKey: "k"}).ListModels(context.Background())
	if err != nil || strings.Join(ids, ",") != "alpha,zeta" {
		t.Fatalf("ids=%v err=%v", ids, err)
	}
}
