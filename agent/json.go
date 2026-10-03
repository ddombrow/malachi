package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// tagged prepends a discriminator key to an already-marshaled JSON object.
func tagged(key, value string, body []byte, err error) ([]byte, error) {
	if err != nil {
		return nil, err
	}
	head := fmt.Sprintf("{%q:%q", key, value)
	if bytes.Equal(body, []byte("{}")) {
		return []byte(head + "}"), nil
	}
	return append([]byte(head+","), body[1:]...), nil
}

func peekField(data []byte, key string) (string, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return "", err
	}
	raw, ok := probe[key]
	if !ok {
		return "", fmt.Errorf("missing %q discriminator", key)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("%q discriminator: %w", key, err)
	}
	return s, nil
}

func nonNil(blocks []Content) []Content {
	if blocks == nil {
		return []Content{}
	}
	return blocks
}

// ---- content blocks ----

func (c TextContent) MarshalJSON() ([]byte, error) {
	type alias TextContent
	b, err := json.Marshal(alias(c))
	return tagged("type", "text", b, err)
}

func (c ThinkingContent) MarshalJSON() ([]byte, error) {
	type alias ThinkingContent
	b, err := json.Marshal(alias(c))
	return tagged("type", "thinking", b, err)
}

func (c ImageContent) MarshalJSON() ([]byte, error) {
	type alias ImageContent
	b, err := json.Marshal(alias(c))
	return tagged("type", "image", b, err)
}

func (c ToolCall) MarshalJSON() ([]byte, error) {
	type alias ToolCall
	if c.Arguments == nil {
		c.Arguments = map[string]any{}
	}
	b, err := json.Marshal(alias(c))
	return tagged("type", "toolCall", b, err)
}

// DecodeContent decodes one content block by its "type" discriminator.
func DecodeContent(data []byte) (Content, error) {
	kind, err := peekField(data, "type")
	if err != nil {
		return nil, err
	}
	var c Content
	switch kind {
	case "text":
		c = &TextContent{}
	case "thinking":
		c = &ThinkingContent{}
	case "image":
		c = &ImageContent{}
	case "toolCall":
		c = &ToolCall{}
	default:
		return nil, fmt.Errorf("unknown content type %q", kind)
	}
	// Decode through a value alias so the discriminator is ignored.
	switch v := c.(type) {
	case *TextContent:
		type alias TextContent
		err = json.Unmarshal(data, (*alias)(v))
	case *ThinkingContent:
		type alias ThinkingContent
		err = json.Unmarshal(data, (*alias)(v))
	case *ImageContent:
		type alias ImageContent
		err = json.Unmarshal(data, (*alias)(v))
	case *ToolCall:
		type alias ToolCall
		err = json.Unmarshal(data, (*alias)(v))
		if v.Arguments == nil {
			v.Arguments = map[string]any{}
		}
	}
	return c, err
}

func decodeContents(data json.RawMessage) ([]Content, error) {
	if len(data) == 0 || string(data) == "null" {
		return []Content{}, nil
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(data, &raws); err != nil {
		return nil, err
	}
	out := make([]Content, 0, len(raws))
	for _, r := range raws {
		c, err := DecodeContent(r)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func (c UserContent) MarshalJSON() ([]byte, error) {
	if c.Blocks == nil {
		return json.Marshal(c.Text)
	}
	return json.Marshal(c.Blocks)
}

func (c *UserContent) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		c.Blocks = nil
		return json.Unmarshal(data, &c.Text)
	}
	blocks, err := decodeContents(data)
	c.Text, c.Blocks = "", blocks
	return err
}

// ---- messages ----

func (m UserMessage) MarshalJSON() ([]byte, error) {
	type alias UserMessage
	b, err := json.Marshal(alias(m))
	return tagged("role", "user", b, err)
}

func (m AssistantMessage) MarshalJSON() ([]byte, error) {
	type alias AssistantMessage
	m.Content = nonNil(m.Content)
	b, err := json.Marshal(alias(m))
	return tagged("role", "assistant", b, err)
}

func (m *AssistantMessage) UnmarshalJSON(data []byte) error {
	type alias AssistantMessage
	aux := struct {
		*alias
		Content json.RawMessage `json:"content"`
	}{alias: (*alias)(m)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	var err error
	m.Content, err = decodeContents(aux.Content)
	return err
}

func (m ToolResultMessage) MarshalJSON() ([]byte, error) {
	type alias ToolResultMessage
	m.Content = nonNil(m.Content)
	b, err := json.Marshal(alias(m))
	return tagged("role", "toolResult", b, err)
}

func (m *ToolResultMessage) UnmarshalJSON(data []byte) error {
	type alias ToolResultMessage
	aux := struct {
		*alias
		Content json.RawMessage `json:"content"`
	}{alias: (*alias)(m)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	var err error
	m.Content, err = decodeContents(aux.Content)
	return err
}

func (m BashExecutionMessage) MarshalJSON() ([]byte, error) {
	type alias BashExecutionMessage
	b, err := json.Marshal(alias(m))
	return tagged("role", "bashExecution", b, err)
}

func (m CustomMessage) MarshalJSON() ([]byte, error) {
	type alias CustomMessage
	b, err := json.Marshal(alias(m))
	return tagged("role", "custom", b, err)
}

func (m BranchSummaryMessage) MarshalJSON() ([]byte, error) {
	type alias BranchSummaryMessage
	b, err := json.Marshal(alias(m))
	return tagged("role", "branchSummary", b, err)
}

func (m CompactionSummaryMessage) MarshalJSON() ([]byte, error) {
	type alias CompactionSummaryMessage
	b, err := json.Marshal(alias(m))
	return tagged("role", "compactionSummary", b, err)
}

// DecodeMessage decodes one message by its "role" discriminator.
func DecodeMessage(data []byte) (Message, error) {
	role, err := peekField(data, "role")
	if err != nil {
		return nil, err
	}
	var m Message
	switch role {
	case "user":
		v := &UserMessage{}
		type alias UserMessage
		err, m = json.Unmarshal(data, (*alias)(v)), v
	case "assistant":
		v := &AssistantMessage{}
		err, m = json.Unmarshal(data, v), v
	case "toolResult":
		v := &ToolResultMessage{}
		err, m = json.Unmarshal(data, v), v
	case "bashExecution":
		v := &BashExecutionMessage{}
		type alias BashExecutionMessage
		err, m = json.Unmarshal(data, (*alias)(v)), v
	case "custom":
		v := &CustomMessage{Display: true}
		type alias CustomMessage
		err, m = json.Unmarshal(data, (*alias)(v)), v
	case "branchSummary":
		v := &BranchSummaryMessage{}
		type alias BranchSummaryMessage
		err, m = json.Unmarshal(data, (*alias)(v)), v
	case "compactionSummary":
		v := &CompactionSummaryMessage{}
		type alias CompactionSummaryMessage
		err, m = json.Unmarshal(data, (*alias)(v)), v
	default:
		return nil, fmt.Errorf("unknown message role %q", role)
	}
	if err != nil {
		return nil, fmt.Errorf("decode %s message: %w", role, err)
	}
	return m, nil
}

// DecodeMessages decodes a JSON array of messages.
func DecodeMessages(data []byte) ([]Message, error) {
	var raws []json.RawMessage
	if err := json.Unmarshal(data, &raws); err != nil {
		return nil, err
	}
	out := make([]Message, 0, len(raws))
	for _, r := range raws {
		m, err := DecodeMessage(r)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}
