package agent

import "context"

// ToolResult is the final or partial output of a tool.
type ToolResult struct {
	Content        []Content `json:"content"`
	Details        any       `json:"details,omitempty"`
	AddedToolNames []string  `json:"addedToolNames,omitempty"`
	Terminate      *bool     `json:"terminate,omitempty"`
}

// TextResult returns a result with a single text block.
func TextResult(text string) ToolResult {
	return ToolResult{Content: []Content{&TextContent{Text: text}}}
}

// Text returns the concatenated text blocks.
func (r ToolResult) Text() string { return textOf(r.Content) }

// ToolExecutor runs one tool call. A returned error becomes an error tool
// result whose text is err.Error(). onUpdate may be called with partial
// results while the tool runs; it is never nil.
type ToolExecutor func(ctx context.Context, callID string, args map[string]any, onUpdate func(ToolResult)) (ToolResult, error)

// Tool is a capability exposed to the model.
type Tool struct {
	Name        string
	Label       string
	Description string
	// Parameters is the JSON Schema for the arguments object.
	Parameters map[string]any
	Execute    ToolExecutor
	// PromptSnippet and PromptGuidelines are optional text that a coding
	// application may fold into its system prompt.
	PromptSnippet    string
	PromptGuidelines []string
}
