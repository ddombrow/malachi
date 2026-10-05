package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/ai"
	"github.com/ddombrow/malachi/ai/sse"
)

// The OpenAI Responses API (/responses). Some models are served only over it:
// on OpenCode Go, the GPT, Grok and Muse models answer /chat/completions with
// "Model does not support this protocol". Port of tau's responses path in
// tau_ai/openai_compatible.py (_build_responses_payload, _ResponsesStreamParser).

// ResponsesAPI is the value recorded in AssistantMessage.API for this path.
const ResponsesAPI = "openai-responses"

// protocolMismatch is how a gateway says a model needs the other endpoint.
const protocolMismatch = "does not support this protocol"

// usesResponses reports whether model is served over /responses, by
// configuration or because a chat request for it was refused for protocol.
func (p *Provider) usesResponses(model string) bool {
	for _, m := range p.cfg.ResponsesModels {
		if m == model {
			return true
		}
	}
	_, learned := p.learned.Load(model)
	return learned
}

func (p *Provider) buildResponsesPayload(req agent.Request) map[string]any {
	payload := map[string]any{
		"model":  req.Model,
		"stream": true,
		// The whole transcript is sent every turn, so nothing needs to be
		// kept server-side; store:false also suits zero-retention accounts.
		"store":        false,
		"instructions": req.System,
		"input":        toResponsesInput(req.Messages, p.cfg.SupportsImages),
	}
	if p.cfg.MaxTokens > 0 {
		payload["max_output_tokens"] = p.cfg.MaxTokens
	}
	if level := req.ThinkingLevel; level != "" && level != "off" && level != "none" {
		// summary:auto streams the reasoning summary, so thinking is visible
		// as it is on the chat path.
		payload["reasoning"] = map[string]any{"effort": level, "summary": "auto"}
	}
	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			params := t.Parameters
			if params == nil {
				params = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			tools = append(tools, map[string]any{
				"type":        "function",
				"name":        t.Name,
				"description": t.Description,
				"parameters":  params,
			})
		}
		payload["tools"] = tools
	}
	return payload
}

func responsesImage(img *agent.ImageContent) map[string]any {
	return map[string]any{
		"type":      "input_image",
		"detail":    "auto",
		"image_url": "data:" + img.MimeType + ";base64," + img.Data,
	}
}

// toResponsesInput maps the transcript to Responses input items.
func toResponsesInput(messages []agent.Message, vision bool) []any {
	items := []any{}
	for _, m := range messages {
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
				items = append(items, map[string]any{"role": "user", "content": text})
				continue
			}
			parts := []any{}
			if text != "" {
				parts = append(parts, map[string]any{"type": "input_text", "text": text})
			}
			for _, img := range imgs {
				parts = append(parts, responsesImage(img))
			}
			items = append(items, map[string]any{"role": "user", "content": parts})

		case *agent.AssistantMessage:
			// A reasoning item this path produced is replayed as it was
			// received; reasoning from other protocols has no item to send.
			for _, c := range v.Content {
				if t, ok := c.(*agent.ThinkingContent); ok && strings.HasPrefix(t.ThinkingSignature, "{") {
					var item map[string]any
					if json.Unmarshal([]byte(t.ThinkingSignature), &item) == nil && item["type"] == "reasoning" {
						items = append(items, item)
					}
				}
			}
			if text := v.Text(); text != "" {
				items = append(items, map[string]any{"role": "assistant", "content": text})
			}
			for _, c := range v.ToolCalls() {
				args, _ := json.Marshal(c.Arguments)
				items = append(items, map[string]any{
					"type":      "function_call",
					"call_id":   PortableToolCallID(c.ID),
					"name":      c.Name,
					"arguments": string(args),
				})
			}

		case *agent.ToolResultMessage:
			text, imgs := splitContent(v.Content)
			var output any = text
			if len(imgs) > 0 && vision {
				parts := []any{}
				if text != "" {
					parts = append(parts, map[string]any{"type": "input_text", "text": text})
				}
				for _, img := range imgs {
					parts = append(parts, responsesImage(img))
				}
				output = parts
			} else if text == "" {
				output = "(no tool output)"
			}
			items = append(items, map[string]any{
				"type":    "function_call_output",
				"call_id": PortableToolCallID(v.ToolCallID),
				"output":  output,
			})

		default:
			items = append(items, map[string]any{"role": "user", "content": agent.MessageText(m)})
		}
	}
	return items
}

// streamResponses runs one request over /responses, feeding b.
func (p *Provider) streamResponses(ctx context.Context, req agent.Request, b *ai.Builder) {
	msg := b.Message()
	msg.API = ResponsesAPI
	body, err := json.Marshal(p.buildResponsesPayload(req))
	if err != nil {
		b.Error(agent.StopError, "encode request: "+err.Error())
		return
	}
	resp, err := p.post(ctx, "/responses", body, req.SessionID)
	if err != nil {
		p.failRequest(ctx, b, err)
		return
	}
	defer resp.Body.Close()
	parseResponsesStream(ctx, resp.Body, b)
}

// responsesEvent is the subset of a Responses stream event malachi reads.
type responsesEvent struct {
	Type        string          `json:"type"`
	Delta       string          `json:"delta"`
	ItemID      string          `json:"item_id"`
	Arguments   string          `json:"arguments"`
	Item        json.RawMessage `json:"item"`
	Response    json.RawMessage `json:"response"`
	Message     string          `json:"message"`
	Error       *apiError       `json:"error"`
	OutputIndex int             `json:"output_index"`
}

type apiError struct {
	Message string `json:"message"`
}

type responsesItem struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type responsesBody struct {
	ID     string    `json:"id"`
	Model  string    `json:"model"`
	Status string    `json:"status"`
	Error  *apiError `json:"error"`
	Usage  *struct {
		InputTokens        int64 `json:"input_tokens"`
		OutputTokens       int64 `json:"output_tokens"`
		TotalTokens        int64 `json:"total_tokens"`
		InputTokensDetails *struct {
			CachedTokens     int64 `json:"cached_tokens"`
			CacheWriteTokens int64 `json:"cache_write_tokens"`
		} `json:"input_tokens_details"`
		OutputTokensDetails *struct {
			ReasoningTokens int64 `json:"reasoning_tokens"`
		} `json:"output_tokens_details"`
	} `json:"usage"`
}

// usage converts Responses usage: cache reads and writes are subtracted from
// input, leaving fresh input, as on the chat path.
func (r *responsesBody) usage() (agent.Usage, bool) {
	if r.Usage == nil {
		return agent.Usage{}, false
	}
	u := r.Usage
	var cacheRead, cacheWrite int64
	if d := u.InputTokensDetails; d != nil {
		cacheRead, cacheWrite = d.CachedTokens, d.CacheWriteTokens
	}
	fresh := max(0, u.InputTokens-cacheRead-cacheWrite)
	out := agent.Usage{
		Input:       fresh,
		Output:      u.OutputTokens,
		CacheRead:   cacheRead,
		CacheWrite:  cacheWrite,
		TotalTokens: fresh + u.OutputTokens + cacheRead + cacheWrite,
	}
	if d := u.OutputTokensDetails; d != nil {
		r := d.ReasoningTokens
		out.Reasoning = &r
	}
	return out, true
}

// responsesCall tracks one streamed function_call item.
type responsesCall struct {
	idx      int  // content index in the message
	hasDelta bool // arguments arrived as deltas
}

func parseResponsesStream(ctx context.Context, body io.Reader, b *ai.Builder) {
	msg := b.Message()
	r := sse.NewReader(body)
	calls := map[string]*responsesCall{} // item id -> call
	lastReasoning := ""                  // JSON of the latest reasoning item

	startCall := func(item responsesItem) *responsesCall {
		if c, ok := calls[item.ID]; ok {
			return c
		}
		id := item.CallID
		if id == "" {
			id = item.ID
		}
		c := &responsesCall{idx: b.ToolCallStart(id, item.Name)}
		calls[item.ID] = c
		return c
	}
	finish := func(reason agent.StopReason) {
		if lastReasoning != "" {
			// The reasoning item goes with the thinking it produced, to be
			// replayed on the next turn.
			for i := len(msg.Content) - 1; i >= 0; i-- {
				if t, ok := msg.Content[i].(*agent.ThinkingContent); ok {
					t.ThinkingSignature = lastReasoning
					break
				}
			}
		}
		b.Done(reason)
	}

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
		if data == "" || data == "[DONE]" {
			continue // the Responses API ends with a terminal event, not [DONE]
		}
		var e responsesEvent
		if json.Unmarshal([]byte(data), &e) != nil {
			continue
		}
		switch e.Type {
		case "response.output_text.delta", "response.refusal.delta":
			b.Text(e.Delta)
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			b.Thinking(e.Delta)
		case "response.reasoning_summary_part.done":
			if msg.ThinkingText() != "" {
				b.Thinking("\n\n")
			}
		case "response.output_item.added", "response.output_item.done":
			var item responsesItem
			if json.Unmarshal(e.Item, &item) != nil {
				continue
			}
			switch item.Type {
			case "reasoning":
				if e.Type == "response.output_item.done" || lastReasoning == "" {
					lastReasoning = string(e.Item)
				}
			case "function_call":
				c := startCall(item)
				if e.Type == "response.output_item.done" {
					call := b.ToolCall(c.idx)
					if item.CallID != "" {
						call.ID = item.CallID
					}
					if item.Name != "" {
						call.Name = item.Name
					}
					if !c.hasDelta && item.Arguments != "" {
						b.ToolCallDelta(c.idx, item.Arguments)
						c.hasDelta = true
					}
					b.ToolCallEnd(c.idx)
				}
			}
		case "response.function_call_arguments.delta":
			if c, ok := calls[e.ItemID]; ok && e.Delta != "" {
				b.ToolCallDelta(c.idx, e.Delta)
				c.hasDelta = true
			}
		case "response.function_call_arguments.done":
			if c, ok := calls[e.ItemID]; ok && !c.hasDelta && e.Arguments != "" {
				b.ToolCallDelta(c.idx, e.Arguments)
				c.hasDelta = true
			}
		case "response.completed", "response.incomplete":
			var res responsesBody
			_ = json.Unmarshal(e.Response, &res)
			if u, ok := res.usage(); ok {
				msg.Usage = u
			}
			if res.ID != "" {
				msg.ResponseID = res.ID
			}
			if res.Model != "" && res.Model != msg.Model {
				msg.ResponseModel = res.Model
			}
			reason := agent.StopStop
			if res.Status == "incomplete" {
				reason = agent.StopLength
			}
			finish(reason) // Done upgrades stop to toolUse when calls exist
			return
		case "response.failed":
			var res responsesBody
			_ = json.Unmarshal(e.Response, &res)
			text := "Provider response failed"
			if res.Error != nil && res.Error.Message != "" {
				text = res.Error.Message
			}
			b.Error(agent.StopError, text)
			return
		case "error":
			text := e.Message
			if text == "" && e.Error != nil {
				text = e.Error.Message
			}
			if text == "" {
				text = "Provider stream error"
			}
			b.Error(agent.StopError, fmt.Sprintf("Provider stream error: %s", text))
			return
		}
	}
	if b.Stopped() {
		return
	}
	if ctx.Err() != nil {
		b.Error(agent.StopAborted, "Operation aborted")
		return
	}
	b.Error(agent.StopError, "Provider stream ended before completion")
}
