package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/ddombrow/malachi/agent"
)

// maxListPages bounds tools/list pagination, against a server whose cursor
// never runs out.
const maxListPages = 100

// Tool is a tool a server offers.
type Tool struct {
	Name         string           `json:"name"`
	Title        string           `json:"title,omitempty"`
	Description  string           `json:"description,omitempty"`
	InputSchema  map[string]any   `json:"inputSchema"`
	OutputSchema map[string]any   `json:"outputSchema,omitempty"`
	Annotations  *ToolAnnotations `json:"annotations,omitempty"`
}

// ToolAnnotations are a server's own hints about a tool. They are claims,
// not guarantees: malachi uses them only where a false one costs nothing
// but ordering.
type ToolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    *bool  `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool  `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool  `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool  `json:"openWorldHint,omitempty"`
}

// ReadOnly reports whether the server says the tool changes nothing.
func (t Tool) ReadOnly() bool {
	return t.Annotations != nil && t.Annotations.ReadOnlyHint != nil && *t.Annotations.ReadOnlyHint
}

// ListTools returns every tool the server offers, following pagination.
func (cl *Client) ListTools(ctx context.Context) ([]Tool, error) {
	var all []Tool
	cursor := ""
	for range maxListPages {
		p := map[string]any{}
		if cursor != "" {
			p["cursor"] = cursor
		}
		raw, err := cl.c.request(ctx, "tools/list", cl.params(p))
		if err != nil {
			return nil, fmt.Errorf("mcp: tools/list: %w", err)
		}
		var res struct {
			Tools      []Tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			return nil, fmt.Errorf("mcp: tools/list: %w", err)
		}
		all = append(all, res.Tools...)
		if res.NextCursor == "" {
			return all, nil
		}
		cursor = res.NextCursor
	}
	return nil, fmt.Errorf("mcp: tools/list: still paginating after %d pages", maxListPages)
}

// CallResult is what a tool call returned.
type CallResult struct {
	Content           []json.RawMessage `json:"content"`
	StructuredContent json.RawMessage   `json:"structuredContent,omitempty"`
	IsError           bool              `json:"isError,omitempty"`
	ResultType        string            `json:"resultType,omitempty"`
}

// CallTool calls a tool. onProgress, if set, hears the server's progress
// notifications for the call.
func (cl *Client) CallTool(ctx context.Context, name string, args map[string]any, onProgress func(Progress)) (CallResult, error) {
	if args == nil {
		args = map[string]any{}
	}
	p := map[string]any{"name": name, "arguments": args}
	if onProgress != nil {
		cl.mu.Lock()
		cl.nextTok++
		tok := cl.nextTok
		cl.progress[strconv.Itoa(tok)] = onProgress
		cl.mu.Unlock()
		defer func() {
			cl.mu.Lock()
			delete(cl.progress, strconv.Itoa(tok))
			cl.mu.Unlock()
		}()
		p["_meta"] = map[string]any{"progressToken": tok}
	}
	raw, err := cl.c.request(ctx, "tools/call", cl.params(p))
	if err != nil {
		return CallResult{}, err
	}
	var res CallResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return CallResult{}, fmt.Errorf("mcp: tools/call: %w", err)
	}
	switch res.ResultType {
	case "", "complete":
	case "input_required":
		// malachi declares no capabilities, so a server has nothing to ask
		// it for; one that asks anyway gets a failed call.
		return CallResult{}, errors.New("mcp: the server asked for input (sampling, elicitation or roots), which malachi does not provide")
	default:
		return CallResult{}, fmt.Errorf("mcp: tools/call: unknown result type %q", res.ResultType)
	}
	return res, nil
}

// Result turns a call result into an agent result: text as text, images as
// images, and everything else (audio, resources, links) as a line saying
// what it was. Structured content goes to the details, and stands in for
// text when the server sent none.
func (r CallResult) Result() agent.ToolResult {
	var out []agent.Content
	text := func(s string) { out = append(out, &agent.TextContent{Text: s}) }
	for _, raw := range r.Content {
		var b struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Data     string `json:"data"`
			MimeType string `json:"mimeType"`
			Name     string `json:"name"`
			URI      string `json:"uri"`
			Resource *struct {
				URI      string  `json:"uri"`
				MimeType string  `json:"mimeType"`
				Text     *string `json:"text"`
				Blob     string  `json:"blob"`
			} `json:"resource"`
		}
		if json.Unmarshal(raw, &b) != nil {
			continue
		}
		switch b.Type {
		case "text":
			text(b.Text)
		case "image":
			if _, err := base64.StdEncoding.DecodeString(b.Data); err != nil || b.MimeType == "" {
				text("[an image the server sent malformed]")
				continue
			}
			out = append(out, &agent.ImageContent{Data: b.Data, MimeType: b.MimeType})
		case "audio":
			text(fmt.Sprintf("[audio (%s, %d bytes) not shown]", b.MimeType, base64.StdEncoding.DecodedLen(len(b.Data))))
		case "resource_link":
			text(fmt.Sprintf("[resource %s: %s]", firstNonEmpty(b.Name, b.URI), b.URI))
		case "resource":
			switch {
			case b.Resource == nil:
			case b.Resource.Text != nil:
				text(fmt.Sprintf("[resource %s]\n%s", b.Resource.URI, *b.Resource.Text))
			default:
				text(fmt.Sprintf("[resource %s (%s, %d bytes) not shown]", b.Resource.URI, b.Resource.MimeType,
					base64.StdEncoding.DecodedLen(len(b.Resource.Blob))))
			}
		default:
			text(fmt.Sprintf("[%s content not shown]", b.Type))
		}
	}
	var details map[string]any
	if len(r.StructuredContent) > 0 && string(r.StructuredContent) != "null" {
		var sc any
		if json.Unmarshal(r.StructuredContent, &sc) == nil {
			details = map[string]any{"structuredContent": sc}
			if len(out) == 0 {
				pretty, _ := json.MarshalIndent(sc, "", "  ")
				text(string(pretty))
			}
		}
	}
	if len(out) == 0 {
		text("(no output)")
	}
	res := agent.ToolResult{Content: out}
	if details != nil {
		res.Details = details
	}
	return res
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
