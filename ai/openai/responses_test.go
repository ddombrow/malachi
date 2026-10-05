package openai

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ddombrow/malachi/agent"
)

// responsesSSE frames Responses events the way the API streams them.
func responsesSSE(events ...map[string]any) string {
	var b strings.Builder
	for _, e := range events {
		raw, _ := json.Marshal(e)
		b.WriteString("event: " + e["type"].(string) + "\ndata: " + string(raw) + "\n\n")
	}
	return b.String()
}

// gateway serves /chat/completions and /responses, recording which was hit
// and with what body.
type gateway struct {
	mu        sync.Mutex
	paths     []string
	bodies    []map[string]any
	chat      func(w http.ResponseWriter)
	responses string
}

func (g *gateway) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		g.mu.Lock()
		g.paths = append(g.paths, r.URL.Path)
		g.bodies = append(g.bodies, body)
		g.mu.Unlock()
		switch r.URL.Path {
		case "/v1/responses":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, g.responses)
		case "/v1/chat/completions":
			g.chat(w)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

var reasoningItem = map[string]any{"id": "rs_1", "type": "reasoning", "encrypted_content": "ENC", "summary": []any{}}

// toolTurn is a Responses stream with reasoning, text and a tool call whose
// arguments arrive in pieces.
var toolTurn = responsesSSE(
	map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_1"}},
	map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"id": "rs_1", "type": "reasoning"}},
	map[string]any{"type": "response.reasoning_summary_text.delta", "item_id": "rs_1", "delta": "need the file"},
	map[string]any{"type": "response.reasoning_summary_part.done", "item_id": "rs_1"},
	map[string]any{"type": "response.output_item.done", "output_index": 0, "item": reasoningItem},
	map[string]any{"type": "response.output_text.delta", "delta": "Reading."},
	map[string]any{"type": "response.output_item.added", "output_index": 2, "item": map[string]any{"id": "fc_1", "type": "function_call", "call_id": "call_abc", "name": "read"}},
	map[string]any{"type": "response.function_call_arguments.delta", "item_id": "fc_1", "delta": `{"path":`},
	map[string]any{"type": "response.function_call_arguments.delta", "item_id": "fc_1", "delta": `"a.go"}`},
	map[string]any{"type": "response.output_item.done", "output_index": 2, "item": map[string]any{"id": "fc_1", "type": "function_call", "call_id": "call_abc", "name": "read", "arguments": `{"path":"a.go"}`}},
	map[string]any{"type": "response.completed", "response": map[string]any{
		"id": "resp_1", "model": "gpt-6-luna", "status": "completed",
		"usage": map[string]any{"input_tokens": 100, "output_tokens": 20, "input_tokens_details": map[string]any{"cached_tokens": 60}, "output_tokens_details": map[string]any{"reasoning_tokens": 7}},
	}},
)

func TestResponsesStreamParsesReasoningTextAndToolCalls(t *testing.T) {
	g := &gateway{responses: toolTurn}
	srv := g.serve(t)
	p := New(Config{Name: "opencode-go", BaseURL: srv.URL + "/v1", ResponsesModels: []string{"gpt-6-luna"}})
	m := final(t, collect(p, agent.Request{Model: "gpt-6-luna", Messages: []agent.Message{agent.NewUserText("go")}}))

	if g.paths[0] != "/v1/responses" {
		t.Fatalf("configured model went to %s", g.paths[0])
	}
	if m.API != ResponsesAPI || m.StopReason != agent.StopToolUse || m.Text() != "Reading." {
		t.Fatalf("message: api=%s stop=%s text=%q", m.API, m.StopReason, m.Text())
	}
	if !strings.HasPrefix(m.ThinkingText(), "need the file") {
		t.Fatalf("thinking: %q", m.ThinkingText())
	}
	calls := m.ToolCalls()
	if len(calls) != 1 || calls[0].ID != "call_abc" || calls[0].Name != "read" || calls[0].Arguments["path"] != "a.go" {
		t.Fatalf("tool call: %+v", calls)
	}
	th := m.Content[0].(*agent.ThinkingContent)
	if !strings.Contains(th.ThinkingSignature, `"encrypted_content":"ENC"`) {
		t.Fatalf("reasoning item not kept for replay: %q", th.ThinkingSignature)
	}
	if m.Usage.Input != 40 || m.Usage.CacheRead != 60 || m.Usage.Output != 20 || *m.Usage.Reasoning != 7 {
		t.Fatalf("usage: %+v", m.Usage)
	}
	if m.ResponseID != "resp_1" {
		t.Fatalf("response id: %q", m.ResponseID)
	}
}

// The request carries the transcript as Responses input items, replaying the
// reasoning item a previous turn produced.
func TestResponsesRequestShape(t *testing.T) {
	g := &gateway{responses: responsesSSE(map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed"}})}
	srv := g.serve(t)
	p := New(Config{BaseURL: srv.URL + "/v1", ResponsesModels: []string{"m"}, SessionHeader: "x-opencode-session"})

	sig, _ := json.Marshal(reasoningItem)
	prev := agent.NewAssistantMessage("m")
	prev.Content = []agent.Content{
		&agent.ThinkingContent{Thinking: "need the file", ThinkingSignature: string(sig)},
		&agent.TextContent{Text: "Reading."},
		&agent.ToolCall{ID: "call_abc", Name: "read", Arguments: map[string]any{"path": "a.go"}},
	}
	chatThinking := agent.NewAssistantMessage("m")
	chatThinking.Content = []agent.Content{&agent.ThinkingContent{Thinking: "x", ThinkingSignature: "reasoning_content"}, &agent.TextContent{Text: "ok"}}
	req := agent.Request{
		Model: "m", System: "be brief", ThinkingLevel: "high",
		Messages: []agent.Message{
			agent.NewUserText("go"), prev,
			&agent.ToolResultMessage{ToolCallID: "call_abc", ToolName: "read", Content: []agent.Content{&agent.TextContent{Text: "package main"}}},
			chatThinking,
		},
		Tools: []*agent.Tool{{Name: "read", Description: "Read a file", Parameters: map[string]any{"type": "object"}}},
	}
	final(t, collect(p, req))

	body := g.bodies[0]
	if body["instructions"] != "be brief" || body["store"] != false || body["stream"] != true {
		t.Fatalf("payload: %v", body)
	}
	if r := body["reasoning"].(map[string]any); r["effort"] != "high" || r["summary"] != "auto" {
		t.Fatalf("reasoning: %v", r)
	}
	tool := body["tools"].([]any)[0].(map[string]any)
	if tool["type"] != "function" || tool["name"] != "read" || tool["parameters"] == nil {
		t.Fatalf("tool: %v", tool)
	}
	var kinds []string
	for _, it := range body["input"].([]any) {
		item := it.(map[string]any)
		kind, _ := item["type"].(string)
		if kind == "" {
			kind = "msg:" + item["role"].(string)
		}
		kinds = append(kinds, kind)
	}
	// The chat-path thinking ("reasoning_content") has no item to replay.
	want := "msg:user reasoning msg:assistant function_call function_call_output msg:assistant"
	if strings.Join(kinds, " ") != want {
		t.Fatalf("input items: %v\nwant %s", kinds, want)
	}
	call := body["input"].([]any)[3].(map[string]any)
	if call["call_id"] != "call_abc" || call["arguments"] != `{"path":"a.go"}` {
		t.Fatalf("function_call item: %v", call)
	}
}

// A model the gateway refuses on chat for protocol moves to /responses, once,
// and stays there for later requests.
func TestProtocolMismatchFallsBackToResponsesAndRemembers(t *testing.T) {
	g := &gateway{
		responses: responsesSSE(
			map[string]any{"type": "response.output_text.delta", "delta": "hi"},
			map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed"}},
		),
		chat: func(w http.ResponseWriter) {
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"error":{"message":"Model does not support this protocol."}}`)
		},
	}
	srv := g.serve(t)
	p := New(Config{BaseURL: srv.URL + "/v1"})
	for i := 0; i < 2; i++ {
		if m := final(t, collect(p, agent.Request{Model: "gpt-7"})); m.Text() != "hi" || m.StopReason != agent.StopStop {
			t.Fatalf("request %d: %+v", i, m)
		}
	}
	want := "/v1/chat/completions /v1/responses /v1/responses"
	if got := strings.Join(g.paths, " "); got != want {
		t.Fatalf("paths %s, want %s", got, want)
	}
}

// Other 400s stay errors: only the protocol refusal is a reason to switch.
func TestOtherBadRequestsDoNotFallBack(t *testing.T) {
	g := &gateway{chat: func(w http.ResponseWriter) {
		w.WriteHeader(400)
		_, _ = io.WriteString(w, `{"error":{"message":"invalid tool schema"}}`)
	}}
	srv := g.serve(t)
	m := final(t, collect(New(Config{BaseURL: srv.URL + "/v1"}), agent.Request{Model: "m"}))
	if m.StopReason != agent.StopError || len(g.paths) != 1 {
		t.Fatalf("stop=%s paths=%v", m.StopReason, g.paths)
	}
}

func TestResponsesFailuresBecomeErrorMessages(t *testing.T) {
	cases := map[string]string{
		responsesSSE(map[string]any{"type": "response.failed", "response": map[string]any{"error": map[string]any{"message": "overloaded"}}}): "overloaded",
		responsesSSE(map[string]any{"type": "error", "message": "rate limited"}):                                                              "rate limited",
		responsesSSE(map[string]any{"type": "response.output_text.delta", "delta": "par"}):                                                    "ended before completion",
	}
	for stream, want := range cases {
		g := &gateway{responses: stream}
		srv := g.serve(t)
		m := final(t, collect(New(Config{BaseURL: srv.URL + "/v1", ResponsesModels: []string{"m"}}), agent.Request{Model: "m"}))
		if m.StopReason != agent.StopError || !strings.Contains(m.ErrorMessage, want) {
			t.Errorf("want error containing %q, got stop=%s %q", want, m.StopReason, m.ErrorMessage)
		}
	}
}

func TestIncompleteResponseStopsForLength(t *testing.T) {
	g := &gateway{responses: responsesSSE(
		map[string]any{"type": "response.output_text.delta", "delta": "cut"},
		map[string]any{"type": "response.incomplete", "response": map[string]any{"status": "incomplete"}},
	)}
	srv := g.serve(t)
	m := final(t, collect(New(Config{BaseURL: srv.URL + "/v1", ResponsesModels: []string{"m"}}), agent.Request{Model: "m"}))
	if m.StopReason != agent.StopLength || m.Text() != "cut" {
		t.Fatalf("stop=%s text=%q", m.StopReason, m.Text())
	}
}
