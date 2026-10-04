// Package agent is malachi's portable agent brain: messages, tools, events,
// the provider/tool loop, and the stateful harness. It imports nothing from
// the rest of the module and knows nothing about terminals or file layout.
package agent

import (
	"strings"
	"time"
)

// NowMillis returns the current Unix timestamp in milliseconds.
func NowMillis() int64 { return time.Now().UnixMilli() }

// Content is one block inside a message: text, thinking, image, or tool call.
type Content interface{ contentType() string }

type TextContent struct {
	Text          string `json:"text"`
	TextSignature string `json:"textSignature,omitempty"`
}

type ThinkingContent struct {
	Thinking          string `json:"thinking"`
	ThinkingSignature string `json:"thinkingSignature,omitempty"`
	Redacted          bool   `json:"redacted"`
}

type ImageContent struct {
	Data     string `json:"data"`
	MimeType string `json:"mimeType"`
}

// ToolCall is a tool invocation requested by the assistant.
type ToolCall struct {
	ID               string         `json:"id"`
	Name             string         `json:"name"`
	Arguments        map[string]any `json:"arguments"`
	ThoughtSignature string         `json:"thoughtSignature,omitempty"`
}

func (*TextContent) contentType() string     { return "text" }
func (*ThinkingContent) contentType() string { return "thinking" }
func (*ImageContent) contentType() string    { return "image" }
func (*ToolCall) contentType() string        { return "toolCall" }

// UserContent is either a plain string or a list of text/image blocks.
// A nil Blocks slice means the string form.
type UserContent struct {
	Text   string
	Blocks []Content
}

// String returns the visible text of user content.
func (c UserContent) String() string {
	if c.Blocks == nil {
		return c.Text
	}
	return textOf(c.Blocks)
}

// Message is a transcript entry. Concrete types are pointers to the structs
// below; the JSON discriminator is "role".
type Message interface {
	Role() string
	Time() int64
}

type UserMessage struct {
	Content   UserContent `json:"content"`
	Timestamp int64       `json:"timestamp"`
}

type StopReason string

const (
	StopStop    StopReason = "stop"
	StopLength  StopReason = "length"
	StopToolUse StopReason = "toolUse"
	StopError   StopReason = "error"
	StopAborted StopReason = "aborted"
)

type UsageCost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
	Total      float64 `json:"total"`
}

type Usage struct {
	// Input excludes cache. Adapters normalise to that split, so TotalTokens
	// adds up regardless of how a provider reports it.
	Input        int64     `json:"input"`
	Output       int64     `json:"output"`
	CacheRead    int64     `json:"cacheRead"`
	CacheWrite   int64     `json:"cacheWrite"`
	CacheWrite1h *int64    `json:"cacheWrite1h,omitempty"`
	Reasoning    *int64    `json:"reasoning,omitempty"`
	TotalTokens  int64     `json:"totalTokens"`
	Cost         UsageCost `json:"cost"`
}

// PromptTokens is the size of the request as sent: everything the provider
// read for it, fresh or served from cache.
//
// This is the number that answers "how full is the context window". Input
// alone undercounts, and on a normal session the cached prefix is most of the
// conversation. Output is deliberately excluded: it is the reply, not part of
// the next request.
func (u Usage) PromptTokens() int64 { return u.Input + u.CacheRead + u.CacheWrite }

// Add returns the field-wise sum of two usages.
func (u Usage) Add(o Usage) Usage {
	addOpt := func(a, b *int64) *int64 {
		if a == nil && b == nil {
			return nil
		}
		var s int64
		if a != nil {
			s += *a
		}
		if b != nil {
			s += *b
		}
		return &s
	}
	return Usage{
		Input:        u.Input + o.Input,
		Output:       u.Output + o.Output,
		CacheRead:    u.CacheRead + o.CacheRead,
		CacheWrite:   u.CacheWrite + o.CacheWrite,
		CacheWrite1h: addOpt(u.CacheWrite1h, o.CacheWrite1h),
		Reasoning:    addOpt(u.Reasoning, o.Reasoning),
		TotalTokens:  u.TotalTokens + o.TotalTokens,
		Cost: UsageCost{
			Input:      u.Cost.Input + o.Cost.Input,
			Output:     u.Cost.Output + o.Cost.Output,
			CacheRead:  u.Cost.CacheRead + o.Cost.CacheRead,
			CacheWrite: u.Cost.CacheWrite + o.Cost.CacheWrite,
			Total:      u.Cost.Total + o.Cost.Total,
		},
	}
}

type ResponseTiming struct {
	TimeToFirstOutputMs *int64 `json:"timeToFirstOutputMs,omitempty"`
	TotalDurationMs     int64  `json:"totalDurationMs"`
}

type DiagnosticError struct {
	Name    string `json:"name,omitempty"`
	Message string `json:"message"`
	Stack   string `json:"stack,omitempty"`
	Code    any    `json:"code,omitempty"`
}

type Diagnostic struct {
	Type      string           `json:"type"`
	Timestamp int64            `json:"timestamp"`
	Error     *DiagnosticError `json:"error,omitempty"`
	Details   map[string]any   `json:"details,omitempty"`
}

type AssistantMessage struct {
	Content          []Content       `json:"content"`
	API              string          `json:"api"`
	Provider         string          `json:"provider"`
	Model            string          `json:"model"`
	ResponseModel    string          `json:"responseModel,omitempty"`
	ResponseProvider string          `json:"responseProvider,omitempty"`
	ResponseID       string          `json:"responseId,omitempty"`
	Diagnostics      []Diagnostic    `json:"diagnostics,omitempty"`
	Usage            Usage           `json:"usage"`
	Timing           *ResponseTiming `json:"timing,omitempty"`
	StopReason       StopReason      `json:"stopReason"`
	ErrorMessage     string          `json:"errorMessage,omitempty"`
	Timestamp        int64           `json:"timestamp"`
}

// NewAssistantMessage returns an empty assistant message with tau's defaults.
func NewAssistantMessage(model string) *AssistantMessage {
	return &AssistantMessage{
		Content:    []Content{},
		API:        "unknown",
		Provider:   "unknown",
		Model:      model,
		StopReason: StopStop,
		Timestamp:  NowMillis(),
	}
}

// Text returns the concatenated visible text blocks.
func (m *AssistantMessage) Text() string { return textOf(m.Content) }

// ThinkingText returns the concatenated thinking blocks.
func (m *AssistantMessage) ThinkingText() string {
	var b strings.Builder
	for _, c := range m.Content {
		if t, ok := c.(*ThinkingContent); ok {
			b.WriteString(t.Thinking)
		}
	}
	return b.String()
}

// ToolCalls returns the tool call blocks in order.
func (m *AssistantMessage) ToolCalls() []*ToolCall {
	var calls []*ToolCall
	for _, c := range m.Content {
		if t, ok := c.(*ToolCall); ok {
			calls = append(calls, t)
		}
	}
	return calls
}

// Clone returns a deep-enough copy for handing partial snapshots to
// subscribers: the content slice and each block are copied.
func (m *AssistantMessage) Clone() *AssistantMessage {
	c := *m
	c.Content = make([]Content, len(m.Content))
	for i, b := range m.Content {
		switch v := b.(type) {
		case *TextContent:
			x := *v
			c.Content[i] = &x
		case *ThinkingContent:
			x := *v
			c.Content[i] = &x
		case *ImageContent:
			x := *v
			c.Content[i] = &x
		case *ToolCall:
			x := *v
			c.Content[i] = &x
		default:
			c.Content[i] = b
		}
	}
	c.Diagnostics = append([]Diagnostic(nil), m.Diagnostics...)
	return &c
}

type ToolResultMessage struct {
	ToolCallID     string    `json:"toolCallId"`
	ToolName       string    `json:"toolName"`
	Content        []Content `json:"content"`
	Details        any       `json:"details,omitempty"`
	AddedToolNames []string  `json:"addedToolNames,omitempty"`
	IsError        bool      `json:"isError"`
	Timestamp      int64     `json:"timestamp"`
}

func (m *ToolResultMessage) Text() string { return textOf(m.Content) }

type BashExecutionMessage struct {
	Command            string `json:"command"`
	Output             string `json:"output"`
	ExitCode           *int   `json:"exitCode,omitempty"`
	Cancelled          bool   `json:"cancelled"`
	Truncated          bool   `json:"truncated"`
	FullOutputPath     string `json:"fullOutputPath,omitempty"`
	Timestamp          int64  `json:"timestamp"`
	ExcludeFromContext bool   `json:"excludeFromContext"`
}

type CustomMessage struct {
	CustomType string      `json:"customType"`
	Content    UserContent `json:"content"`
	Display    bool        `json:"display"`
	Details    any         `json:"details,omitempty"`
	Timestamp  int64       `json:"timestamp"`
}

type BranchSummaryMessage struct {
	Summary   string `json:"summary"`
	FromID    string `json:"fromId"`
	Timestamp int64  `json:"timestamp"`
}

type CompactionSummaryMessage struct {
	Summary      string `json:"summary"`
	TokensBefore int64  `json:"tokensBefore"`
	Timestamp    int64  `json:"timestamp"`
}

func (*UserMessage) Role() string              { return "user" }
func (*AssistantMessage) Role() string         { return "assistant" }
func (*ToolResultMessage) Role() string        { return "toolResult" }
func (*BashExecutionMessage) Role() string     { return "bashExecution" }
func (*CustomMessage) Role() string            { return "custom" }
func (*BranchSummaryMessage) Role() string     { return "branchSummary" }
func (*CompactionSummaryMessage) Role() string { return "compactionSummary" }

func (m *UserMessage) Time() int64              { return m.Timestamp }
func (m *AssistantMessage) Time() int64         { return m.Timestamp }
func (m *ToolResultMessage) Time() int64        { return m.Timestamp }
func (m *BashExecutionMessage) Time() int64     { return m.Timestamp }
func (m *CustomMessage) Time() int64            { return m.Timestamp }
func (m *BranchSummaryMessage) Time() int64     { return m.Timestamp }
func (m *CompactionSummaryMessage) Time() int64 { return m.Timestamp }

// NewUserText returns a string-form user message stamped now.
func NewUserText(text string) *UserMessage {
	return &UserMessage{Content: UserContent{Text: text}, Timestamp: NowMillis()}
}

// MessageText returns the user-visible text of any message.
func MessageText(m Message) string {
	switch v := m.(type) {
	case *UserMessage:
		return v.Content.String()
	case *AssistantMessage:
		return v.Text()
	case *ToolResultMessage:
		return v.Text()
	case *CustomMessage:
		return v.Content.String()
	case *BranchSummaryMessage:
		return v.Summary
	case *CompactionSummaryMessage:
		return v.Summary
	case *BashExecutionMessage:
		return v.Output
	}
	return ""
}

func textOf(blocks []Content) string {
	var b strings.Builder
	for _, c := range blocks {
		if t, ok := c.(*TextContent); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}
