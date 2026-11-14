package acp

import "encoding/json"

// The ACP v1 messages malachi reads and writes
// (https://agentclientprotocol.com/protocol/schema). Keys are camelCase and
// optional fields are omitted when empty.

// protocolVersion is the ACP major version malachi speaks.
const protocolVersion = 1

type implementation struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

type initializeParams struct {
	ProtocolVersion    int                `json:"protocolVersion"`
	ClientCapabilities clientCapabilities `json:"clientCapabilities"`
	ClientInfo         *implementation    `json:"clientInfo,omitempty"`
}

// clientCapabilities records what the client said it supports. malachi uses
// neither the client's file system nor its terminal; the sandbox, not the
// editor, is what confines the agent.
type clientCapabilities struct {
	Session struct {
		Compaction *struct{} `json:"compaction,omitempty"`
	} `json:"session"`
}

type initializeResult struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentCapabilities agentCapabilities `json:"agentCapabilities"`
	AgentInfo         implementation    `json:"agentInfo"`
	AuthMethods       []any             `json:"authMethods"`
}

type agentCapabilities struct {
	LoadSession         bool                `json:"loadSession"`
	PromptCapabilities  promptCapabilities  `json:"promptCapabilities"`
	MCPCapabilities     mcpCapabilities     `json:"mcpCapabilities"`
	SessionCapabilities sessionCapabilities `json:"sessionCapabilities"`
}

type promptCapabilities struct {
	Image           bool `json:"image"`
	Audio           bool `json:"audio"`
	EmbeddedContext bool `json:"embeddedContext"`
}

// mcpCapabilities: stdio only, which every agent supports.
type mcpCapabilities struct {
	HTTP bool `json:"http"`
	SSE  bool `json:"sse"`
}

type sessionCapabilities struct {
	List *struct{} `json:"list,omitempty"`
}

// mcpServer is one server a client passes with a session. Only stdio
// entries (no type) are ever run.
type mcpServer struct {
	Type    string   `json:"type,omitempty"`
	Name    string   `json:"name"`
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	Env     []envVar `json:"env,omitempty"`
	URL     string   `json:"url,omitempty"`
	Headers []envVar `json:"headers,omitempty"`
}

type envVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type newSessionParams struct {
	Cwd        string      `json:"cwd"`
	MCPServers []mcpServer `json:"mcpServers"`
}

type newSessionResult struct {
	SessionID     string         `json:"sessionId"`
	ConfigOptions []configOption `json:"configOptions"`
}

type loadSessionParams struct {
	SessionID  string      `json:"sessionId"`
	Cwd        string      `json:"cwd"`
	MCPServers []mcpServer `json:"mcpServers"`
}

type loadSessionResult struct {
	ConfigOptions []configOption `json:"configOptions"`
}

type listSessionsParams struct {
	Cwd    string `json:"cwd,omitempty"`
	Cursor string `json:"cursor,omitempty"`
}

type sessionInfo struct {
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	Title     string `json:"title,omitempty"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

type listSessionsResult struct {
	Sessions []sessionInfo `json:"sessions"`
}

type promptParams struct {
	SessionID string         `json:"sessionId"`
	Prompt    []contentBlock `json:"prompt"`
}

type promptResult struct {
	StopReason string `json:"stopReason"`
}

// Stop reasons.
const (
	stopEndTurn   = "end_turn"
	stopMaxTokens = "max_tokens"
	stopCancelled = "cancelled"
)

type cancelParams struct {
	SessionID string `json:"sessionId"`
}

type setConfigOptionParams struct {
	SessionID string          `json:"sessionId"`
	ConfigID  string          `json:"configId"`
	Value     json.RawMessage `json:"value"`
}

type setConfigOptionResult struct {
	ConfigOptions []configOption `json:"configOptions"`
}

type configOption struct {
	ID           string        `json:"id"`
	Name         string        `json:"name"`
	Description  string        `json:"description,omitempty"`
	Category     string        `json:"category,omitempty"`
	Type         string        `json:"type"`
	CurrentValue string        `json:"currentValue"`
	Options      []configValue `json:"options"`
}

type configValue struct {
	Value       string `json:"value"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// contentBlock is a prompt or message content block. Prompts may carry
// text, resource links and embedded resources; malachi sends text.
type contentBlock struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	URI      string    `json:"uri,omitempty"`
	Name     string    `json:"name,omitempty"`
	Resource *resource `json:"resource,omitempty"`
}

type resource struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
	Blob     string `json:"blob,omitempty"`
}

func textBlock(s string) contentBlock { return contentBlock{Type: "text", Text: s} }

// sessionNotification is the params of session/update.
type sessionNotification struct {
	SessionID string `json:"sessionId"`
	Update    any    `json:"update"`
}

type chunkUpdate struct {
	SessionUpdate string       `json:"sessionUpdate"` // agent_message_chunk, agent_thought_chunk, user_message_chunk
	Content       contentBlock `json:"content"`
}

type toolCallUpdate struct {
	SessionUpdate string            `json:"sessionUpdate,omitempty"` // tool_call or tool_call_update
	ToolCallID    string            `json:"toolCallId"`
	Title         string            `json:"title,omitempty"`
	Kind          string            `json:"kind,omitempty"`
	Status        string            `json:"status,omitempty"`
	Content       []toolCallContent `json:"content,omitempty"`
	Locations     []toolLocation    `json:"locations,omitempty"`
	RawInput      any               `json:"rawInput,omitempty"`
	RawOutput     any               `json:"rawOutput,omitempty"`
}

// Tool call statuses.
const (
	toolInProgress = "in_progress"
	toolCompleted  = "completed"
	toolFailed     = "failed"
)

type toolCallContent struct {
	Type    string        `json:"type"` // content
	Content *contentBlock `json:"content,omitempty"`
}

type toolLocation struct {
	Path string `json:"path"`
	Line int    `json:"line,omitempty"`
}

type configOptionUpdate struct {
	SessionUpdate string         `json:"sessionUpdate"` // config_option_update
	ConfigOptions []configOption `json:"configOptions"`
}

type usageUpdate struct {
	SessionUpdate string `json:"sessionUpdate"` // usage_update
	Used          int64  `json:"used"`
	Size          int64  `json:"size"`
}

type compactionUpdate struct {
	SessionUpdate string `json:"sessionUpdate"` // compaction_update
	CompactionID  string `json:"compactionId"`
	Status        string `json:"status"`
	Error         string `json:"error,omitempty"`
}

type permissionParams struct {
	SessionID string             `json:"sessionId"`
	ToolCall  toolCallUpdate     `json:"toolCall"`
	Options   []permissionOption `json:"options"`
}

type permissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"` // allow_once, allow_always, reject_once, reject_always
}

type permissionResult struct {
	Outcome struct {
		Outcome  string `json:"outcome"` // selected or cancelled
		OptionID string `json:"optionId,omitempty"`
	} `json:"outcome"`
}
