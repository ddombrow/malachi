package agent

// Event is emitted by the loop and harness for frontends and persistence.
// The JSON discriminator is "type".
type Event interface{ eventType() string }

type AgentStartEvent struct{}

type AgentEndEvent struct {
	Messages []Message `json:"messages"`
}

type TurnStartEvent struct{}

type TurnEndEvent struct {
	Message     Message              `json:"message"`
	ToolResults []*ToolResultMessage `json:"toolResults"`
}

type MessageStartEvent struct {
	Message Message `json:"message"`
}

// MessageUpdateEvent carries a streaming assistant delta plus the partial
// message it produced.
type MessageUpdateEvent struct {
	Message               Message        `json:"message"`
	AssistantMessageEvent AssistantEvent `json:"assistantMessageEvent"`
}

// MessageEndEvent marks a message as complete. It is the durable-message
// boundary: persistence listeners write the message when they see this.
type MessageEndEvent struct {
	Message Message `json:"message"`
}

type ToolExecutionStartEvent struct {
	ToolCallID string         `json:"toolCallId"`
	ToolName   string         `json:"toolName"`
	Args       map[string]any `json:"args"`
}

type ToolExecutionUpdateEvent struct {
	ToolCallID    string         `json:"toolCallId"`
	ToolName      string         `json:"toolName"`
	Args          map[string]any `json:"args"`
	PartialResult ToolResult     `json:"partialResult"`
}

type ToolExecutionEndEvent struct {
	ToolCallID string     `json:"toolCallId"`
	ToolName   string     `json:"toolName"`
	Result     ToolResult `json:"result"`
	IsError    bool       `json:"isError"`
}

func (*AgentStartEvent) eventType() string          { return "agent_start" }
func (*AgentEndEvent) eventType() string            { return "agent_end" }
func (*TurnStartEvent) eventType() string           { return "turn_start" }
func (*TurnEndEvent) eventType() string             { return "turn_end" }
func (*MessageStartEvent) eventType() string        { return "message_start" }
func (*MessageUpdateEvent) eventType() string       { return "message_update" }
func (*MessageEndEvent) eventType() string          { return "message_end" }
func (*ToolExecutionStartEvent) eventType() string  { return "tool_execution_start" }
func (*ToolExecutionUpdateEvent) eventType() string { return "tool_execution_update" }
func (*ToolExecutionEndEvent) eventType() string    { return "tool_execution_end" }

// EventType returns the Pi wire discriminator of an event.
func EventType(e Event) string { return e.eventType() }
