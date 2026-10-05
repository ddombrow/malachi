// Package openai implements the OpenAI-compatible /chat/completions
// streaming API over raw net/http. It serves OpenAI itself and the many
// compatible endpoints (OpenCode Go/Zen, OpenRouter, Ollama, llama.cpp, ...).
//
// Port of tau_ai/openai_compatible.py (chat-completions path).
package openai

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/ai"
	"github.com/ddombrow/malachi/ai/sse"
)

// API is the value recorded in AssistantMessage.API.
const API = "openai-completions"

// Thinking formats: how a reasoning level is expressed in the request.
const (
	ThinkingOpenAI     = "openai"     // reasoning_effort: <level>
	ThinkingDeepSeek   = "deepseek"   // thinking: {type}, reasoning_effort
	ThinkingZAI        = "zai"        // thinking: {type}, reasoning_effort
	ThinkingQwen       = "qwen"       // enable_thinking: bool
	ThinkingOpenRouter = "openrouter" // reasoning: {effort}
)

// Config configures one OpenAI-compatible endpoint.
type Config struct {
	Name    string // provider name recorded on messages, e.g. "opencode-go"
	BaseURL string // e.g. https://opencode.ai/zen/go/v1
	APIKey  string
	Headers map[string]string
	// SessionHeader, if set, carries agent.Request.SessionID on every
	// request (e.g. "x-opencode-session"), letting the gateway route a
	// conversation consistently and reuse its prompt cache.
	SessionHeader  string
	UserAgent      string // default "malachi"
	ThinkingFormat string // default ThinkingOpenAI
	MaxTokens      int    // 0 omits the field
	SupportsImages bool
	MaxRetries     int           // default 3; negative disables retries
	MaxRetryDelay  time.Duration // default 30s
	HTTPClient     *http.Client  // default: no overall timeout (streams are long)
	// ResponsesModels are served over /responses rather than
	// /chat/completions. A model the gateway refuses on chat for protocol is
	// also moved there, once, and remembered.
	ResponsesModels []string
}

// Provider streams chat completions, or Responses for the models that need
// it.
type Provider struct {
	cfg     Config
	learned sync.Map // model -> struct{}: refused on chat for protocol
}

// New returns a provider for cfg.
func New(cfg Config) *Provider {
	if cfg.ThinkingFormat == "" {
		cfg.ThinkingFormat = ThinkingOpenAI
	}
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = 3
	}
	if cfg.MaxRetryDelay == 0 {
		cfg.MaxRetryDelay = 30 * time.Second
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{}
	}
	if cfg.Name == "" {
		cfg.Name = "openai-compatible"
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "malachi"
	}
	return &Provider{cfg: cfg}
}

// failRequest ends the stream for a request that never produced a response.
func (p *Provider) failRequest(ctx context.Context, b *ai.Builder, err error) {
	if ctx.Err() != nil {
		b.Error(agent.StopAborted, "Operation aborted")
		return
	}
	var he *httpError
	if errors.As(err, &he) {
		msg := b.Message()
		msg.Diagnostics = append(msg.Diagnostics, agent.Diagnostic{
			Type:      "http_error",
			Timestamp: agent.NowMillis(),
			Error:     &agent.DiagnosticError{Name: "HTTPError", Message: he.body, Code: he.status},
		})
	}
	b.Error(agent.StopError, err.Error())
}

// isProtocolMismatch reports a gateway refusing a model on this endpoint
// because it is served over the other one.
func isProtocolMismatch(err error) bool {
	var he *httpError
	return errors.As(err, &he) && he.status == http.StatusBadRequest &&
		strings.Contains(strings.ToLower(errorText(he.body)), protocolMismatch)
}

// Stream implements agent.Provider.
func (p *Provider) Stream(ctx context.Context, req agent.Request) iter.Seq[agent.AssistantEvent] {
	return func(yield func(agent.AssistantEvent) bool) {
		msg := agent.NewAssistantMessage(req.Model)
		msg.API, msg.Provider = API, p.cfg.Name
		b := ai.NewBuilder(msg, yield)

		if p.usesResponses(req.Model) {
			p.streamResponses(ctx, req, b)
			return
		}
		body, err := json.Marshal(p.buildPayload(req))
		if err != nil {
			b.Error(agent.StopError, "encode request: "+err.Error())
			return
		}
		resp, err := p.post(ctx, "/chat/completions", body, req.SessionID)
		if err != nil {
			// A model the gateway serves only over /responses: move it there
			// for the rest of the session and try again, once.
			if isProtocolMismatch(err) {
				p.learned.Store(req.Model, struct{}{})
				p.streamResponses(ctx, req, b)
				return
			}
			p.failRequest(ctx, b, err)
			return
		}
		defer resp.Body.Close()
		if hp := resp.Header.Get("x-provider"); hp != "" {
			msg.ResponseProvider = hp
		}
		parseStream(ctx, resp.Body, b)
	}
}

type httpError struct {
	status int
	body   string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.status, errorText(e.body))
}

// errorText extracts error.message from a JSON error body when present.
func errorText(body string) string {
	var parsed struct {
		Error any `json:"error"`
	}
	if json.Unmarshal([]byte(body), &parsed) == nil {
		switch v := parsed.Error.(type) {
		case map[string]any:
			if m, ok := v["message"].(string); ok && m != "" {
				return m
			}
		case string:
			return v
		}
	}
	body = strings.TrimSpace(body)
	if len(body) > 500 {
		body = body[:500] + "…"
	}
	return body
}

func isTransient(status int) bool {
	switch status {
	case 408, 409, 425, 429:
		return true
	}
	return status >= 500
}

// post sends the request, retrying transient failures that happen before
// any of the response body has been consumed.
func (p *Provider) post(ctx context.Context, path string, body []byte, sessionID string) (*http.Response, error) {
	url := strings.TrimRight(p.cfg.BaseURL, "/") + path
	for attempt := 0; ; attempt++ {
		hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		hreq.Header.Set("Content-Type", "application/json")
		hreq.Header.Set("Accept", "text/event-stream")
		hreq.Header.Set("User-Agent", p.cfg.UserAgent)
		if p.cfg.SessionHeader != "" && sessionID != "" {
			hreq.Header.Set(p.cfg.SessionHeader, sessionID)
		}
		if p.cfg.APIKey != "" {
			hreq.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
		}
		for k, v := range p.cfg.Headers {
			hreq.Header.Set(k, v)
		}

		resp, err := p.cfg.HTTPClient.Do(hreq)
		var retryAfter time.Duration
		switch {
		case err != nil:
			if ctx.Err() != nil || attempt >= p.cfg.MaxRetries {
				return nil, err
			}
		case resp.StatusCode >= 400:
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
			resp.Body.Close()
			herr := &httpError{status: resp.StatusCode, body: string(raw)}
			if !isTransient(resp.StatusCode) || attempt >= p.cfg.MaxRetries {
				return nil, herr
			}
			if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
				retryAfter = time.Duration(s) * time.Second
			}
		default:
			return resp, nil
		}

		delay := min(p.cfg.MaxRetryDelay, time.Second<<attempt)
		if retryAfter > 0 {
			delay = min(p.cfg.MaxRetryDelay, retryAfter)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
}

// ---- request payload ----

func (p *Provider) buildPayload(req agent.Request) map[string]any {
	msgs := []any{}
	if req.System != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": req.System})
	}
	msgs = append(msgs, toChatMessages(req.Messages, p.cfg.SupportsImages)...)
	payload := map[string]any{
		"model":          req.Model,
		"stream":         true,
		"messages":       msgs,
		"stream_options": map[string]any{"include_usage": true},
	}
	if p.cfg.MaxTokens > 0 {
		payload["max_completion_tokens"] = p.cfg.MaxTokens
	}
	applyReasoning(payload, p.cfg.ThinkingFormat, req.ThinkingLevel)
	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			params := t.Parameters
			if params == nil {
				params = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  params,
				},
			})
		}
		payload["tools"] = tools
	}
	return payload
}

func applyReasoning(payload map[string]any, format, level string) {
	enabled := level != "" && level != "off" && level != "none"
	switch format {
	case ThinkingDeepSeek, ThinkingZAI:
		payload["thinking"] = map[string]any{"type": map[bool]string{true: "enabled", false: "disabled"}[enabled]}
		if enabled {
			payload["reasoning_effort"] = level
		}
	case ThinkingQwen:
		payload["enable_thinking"] = enabled
	case ThinkingOpenRouter:
		if enabled {
			payload["reasoning"] = map[string]any{"effort": level}
		}
	default:
		if enabled {
			payload["reasoning_effort"] = level
		}
	}
}

var portableID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// PortableToolCallID keeps provider-native ids that fit the common format
// and hashes the rest, so ids from other providers replay safely.
func PortableToolCallID(id string) string {
	if portableID.MatchString(id) {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return "tc_" + hex.EncodeToString(sum[:])[:40]
}

func imagePart(img *agent.ImageContent) map[string]any {
	return map[string]any{
		"type":      "image_url",
		"image_url": map[string]any{"url": "data:" + img.MimeType + ";base64," + img.Data},
	}
}

// splitContent returns the text of blocks and any images.
func splitContent(blocks []agent.Content) (string, []*agent.ImageContent) {
	var text strings.Builder
	var imgs []*agent.ImageContent
	for _, c := range blocks {
		switch v := c.(type) {
		case *agent.TextContent:
			text.WriteString(v.Text)
		case *agent.ImageContent:
			imgs = append(imgs, v)
		}
	}
	return text.String(), imgs
}

func toChatMessages(messages []agent.Message, vision bool) []any {
	var out []any
	var pendingImages []*agent.ImageContent
	flushImages := func() {
		if len(pendingImages) == 0 {
			return
		}
		parts := []any{map[string]any{"type": "text", "text": "Attached image(s) from tool result:"}}
		for _, img := range pendingImages {
			parts = append(parts, imagePart(img))
		}
		out = append(out, map[string]any{"role": "user", "content": parts})
		pendingImages = nil
	}

	for _, m := range messages {
		if _, ok := m.(*agent.ToolResultMessage); !ok {
			flushImages()
		}
		switch v := m.(type) {
		case *agent.UserMessage:
			text, imgs := v.Content.Text, []*agent.ImageContent(nil)
			if v.Content.Blocks != nil {
				text, imgs = splitContent(v.Content.Blocks)
			}
			if len(imgs) > 0 && !vision {
				text += "\n(image omitted: model does not accept images)"
				imgs = nil
			}
			if len(imgs) == 0 {
				out = append(out, map[string]any{"role": "user", "content": text})
				continue
			}
			parts := []any{}
			if text != "" {
				parts = append(parts, map[string]any{"type": "text", "text": text})
			}
			for _, img := range imgs {
				parts = append(parts, imagePart(img))
			}
			out = append(out, map[string]any{"role": "user", "content": parts})

		case *agent.AssistantMessage:
			item := map[string]any{"role": "assistant"}
			text := v.Text()
			calls := v.ToolCalls()
			if text != "" || len(calls) == 0 {
				item["content"] = text
			} else {
				item["content"] = nil
			}
			// Replay reasoning under the field it arrived in; several
			// OpenAI-compatible reasoning models require it on follow-ups.
			if thinking := v.ThinkingText(); thinking != "" {
				field := "reasoning_content"
				for _, c := range v.Content {
					if t, ok := c.(*agent.ThinkingContent); ok && t.ThinkingSignature != "" {
						switch t.ThinkingSignature {
						case "reasoning_content", "reasoning", "thinking":
							field = t.ThinkingSignature
						}
						break
					}
				}
				item[field] = thinking
			}
			if len(calls) > 0 {
				tc := make([]any, 0, len(calls))
				for _, c := range calls {
					args, _ := json.Marshal(c.Arguments)
					tc = append(tc, map[string]any{
						"id":   PortableToolCallID(c.ID),
						"type": "function",
						"function": map[string]any{
							"name":      c.Name,
							"arguments": string(args),
						},
					})
				}
				item["tool_calls"] = tc
			}
			out = append(out, item)

		case *agent.ToolResultMessage:
			text, imgs := splitContent(v.Content)
			if text == "" {
				text = "(no tool output)"
				if len(imgs) > 0 {
					text = "(see attached image)"
				}
			}
			out = append(out, map[string]any{
				"role":         "tool",
				"tool_call_id": PortableToolCallID(v.ToolCallID),
				"name":         v.ToolName,
				"content":      text,
			})
			if vision {
				pendingImages = append(pendingImages, imgs...)
			}

		default:
			out = append(out, map[string]any{"role": "user", "content": agent.MessageText(m)})
		}
	}
	flushImages()
	return out
}

// ---- response stream ----

type chunk struct {
	ID      string          `json:"id"`
	Model   string          `json:"model"`
	Choices []chunkChoice   `json:"choices"`
	Usage   *chunkUsage     `json:"usage"`
	Error   json.RawMessage `json:"error"`
}

type chunkChoice struct {
	Delta        chunkDelta  `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
	Usage        *chunkUsage `json:"usage"`
}

type chunkDelta struct {
	Content          *string         `json:"content"`
	ReasoningContent *string         `json:"reasoning_content"`
	Reasoning        *string         `json:"reasoning"`
	Thinking         *string         `json:"thinking"`
	ToolCalls        []toolCallDelta `json:"tool_calls"`
}

type toolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chunkUsage struct {
	PromptTokens        int64  `json:"prompt_tokens"`
	CompletionTokens    int64  `json:"completion_tokens"`
	PromptCacheHitToken *int64 `json:"prompt_cache_hit_tokens"`
	PromptTokensDetails *struct {
		CachedTokens     *int64 `json:"cached_tokens"`
		CacheWriteTokens int64  `json:"cache_write_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// toUsage ports Pi's parseChunkUsage: cached tokens are cache reads, writes
// are subtracted from the prompt to leave fresh input.
func (u *chunkUsage) toUsage() agent.Usage {
	var cacheRead, cacheWrite int64
	var cached *int64
	if d := u.PromptTokensDetails; d != nil {
		cached, cacheWrite = d.CachedTokens, d.CacheWriteTokens
	}
	if cached == nil {
		cached = u.PromptCacheHitToken
	}
	if cached != nil {
		cacheRead = *cached
	}
	fresh := max(0, u.PromptTokens-cacheRead-cacheWrite)
	out := agent.Usage{
		Input:       fresh,
		Output:      u.CompletionTokens,
		CacheRead:   cacheRead,
		CacheWrite:  cacheWrite,
		TotalTokens: fresh + u.CompletionTokens + cacheRead + cacheWrite,
	}
	if d := u.CompletionTokensDetails; d != nil {
		r := d.ReasoningTokens
		out.Reasoning = &r
	}
	return out
}

func mapFinish(reason string) agent.StopReason {
	switch reason {
	case "length":
		return agent.StopLength
	case "tool_calls", "function_call":
		return agent.StopToolUse
	}
	return agent.StopStop
}

func parseStream(ctx context.Context, body io.Reader, b *ai.Builder) {
	msg := b.Message()
	r := sse.NewReader(body)
	toolIdx := map[int]int{} // provider tool index -> content index
	var finish string
	sawDone := false

	for !b.Stopped() {
		ev, err := r.Next()
		if err != nil {
			if err == io.EOF {
				break
			}
			if ctx.Err() != nil {
				b.Error(agent.StopAborted, "Operation aborted")
			} else {
				b.Error(agent.StopError, "stream read: "+err.Error())
			}
			return
		}
		data := strings.TrimSpace(ev.Data)
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			sawDone = true
			break
		}
		var c chunk
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			b.Error(agent.StopError, "Provider returned invalid JSON chunk: "+truncate(data, 200))
			return
		}
		if len(c.Error) > 0 && string(c.Error) != "null" {
			b.Error(agent.StopError, "Provider stream error: "+errorText(`{"error":`+string(c.Error)+`}`))
			return
		}
		if c.ID != "" && msg.ResponseID == "" {
			msg.ResponseID = c.ID
		}
		if c.Model != "" && msg.ResponseModel == "" && c.Model != msg.Model {
			msg.ResponseModel = c.Model
		}
		if c.Usage != nil {
			msg.Usage = c.Usage.toUsage()
		}
		if len(c.Choices) == 0 {
			continue
		}
		ch := c.Choices[0]
		if c.Usage == nil && ch.Usage != nil {
			msg.Usage = ch.Usage.toUsage()
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			finish = *ch.FinishReason
		}
		d := ch.Delta
		for _, f := range []struct {
			name string
			v    *string
		}{{"reasoning_content", d.ReasoningContent}, {"reasoning", d.Reasoning}, {"thinking", d.Thinking}} {
			if f.v != nil && *f.v != "" {
				b.Thinking(*f.v)
				b.SetThinkingSignature(f.name)
				break
			}
		}
		if d.Content != nil {
			b.Text(*d.Content)
		}
		for _, tc := range d.ToolCalls {
			idx, ok := toolIdx[tc.Index]
			if !ok {
				id := tc.ID
				if id == "" {
					id = fmt.Sprintf("tool-call-%d", tc.Index)
				}
				idx = b.ToolCallStart(id, tc.Function.Name)
				toolIdx[tc.Index] = idx
			} else {
				call := b.ToolCall(idx)
				if tc.ID != "" && strings.HasPrefix(call.ID, "tool-call-") {
					call.ID = tc.ID
				}
				if tc.Function.Name != "" && call.Name == "" {
					call.Name = tc.Function.Name
				}
			}
			b.ToolCallDelta(idx, tc.Function.Arguments)
		}
	}

	if b.Stopped() {
		return
	}
	if ctx.Err() != nil {
		b.Error(agent.StopAborted, "Operation aborted")
		return
	}
	if !sawDone && finish == "" {
		b.Error(agent.StopError, "Provider stream ended before completion")
		return
	}
	if finish == "content_filter" {
		b.Error(agent.StopError, "Response blocked by provider content filter")
		return
	}
	b.Done(mapFinish(finish))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
