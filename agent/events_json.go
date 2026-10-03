package agent

import (
	"encoding/json"
	"fmt"
)

func typed[A any](tag string, a A) ([]byte, error) {
	b, err := json.Marshal(a)
	return tagged("type", tag, b, err)
}

// ---- assistant stream events ----

func (e AssistantStart) MarshalJSON() ([]byte, error) {
	type a AssistantStart
	return typed("start", a(e))
}
func (e TextStart) MarshalJSON() ([]byte, error) { type a TextStart; return typed("text_start", a(e)) }
func (e TextDelta) MarshalJSON() ([]byte, error) { type a TextDelta; return typed("text_delta", a(e)) }
func (e TextEnd) MarshalJSON() ([]byte, error)   { type a TextEnd; return typed("text_end", a(e)) }
func (e ThinkingStart) MarshalJSON() ([]byte, error) {
	type a ThinkingStart
	return typed("thinking_start", a(e))
}
func (e ThinkingDelta) MarshalJSON() ([]byte, error) {
	type a ThinkingDelta
	return typed("thinking_delta", a(e))
}
func (e ThinkingEnd) MarshalJSON() ([]byte, error) {
	type a ThinkingEnd
	return typed("thinking_end", a(e))
}
func (e ToolCallStart) MarshalJSON() ([]byte, error) {
	type a ToolCallStart
	return typed("toolcall_start", a(e))
}
func (e ToolCallDelta) MarshalJSON() ([]byte, error) {
	type a ToolCallDelta
	return typed("toolcall_delta", a(e))
}
func (e ToolCallEnd) MarshalJSON() ([]byte, error) {
	type a ToolCallEnd
	return typed("toolcall_end", a(e))
}
func (e AssistantDone) MarshalJSON() ([]byte, error) {
	type a AssistantDone
	return typed("done", a(e))
}
func (e AssistantError) MarshalJSON() ([]byte, error) {
	type a AssistantError
	return typed("error", a(e))
}

// DecodeAssistantEvent decodes one assistant stream event by "type".
func DecodeAssistantEvent(data []byte) (AssistantEvent, error) {
	kind, err := peekField(data, "type")
	if err != nil {
		return nil, err
	}
	var e AssistantEvent
	switch kind {
	case "start":
		e = &AssistantStart{}
	case "text_start":
		e = &TextStart{}
	case "text_delta":
		e = &TextDelta{}
	case "text_end":
		e = &TextEnd{}
	case "thinking_start":
		e = &ThinkingStart{}
	case "thinking_delta":
		e = &ThinkingDelta{}
	case "thinking_end":
		e = &ThinkingEnd{}
	case "toolcall_start":
		e = &ToolCallStart{}
	case "toolcall_delta":
		e = &ToolCallDelta{}
	case "toolcall_end":
		e = &ToolCallEnd{}
	case "done":
		e = &AssistantDone{}
	case "error":
		e = &AssistantError{}
	default:
		return nil, fmt.Errorf("unknown assistant event type %q", kind)
	}
	// None of these types define UnmarshalJSON, so the "type" key is ignored.
	if err := json.Unmarshal(data, e); err != nil {
		return nil, fmt.Errorf("decode %s event: %w", kind, err)
	}
	return e, nil
}

// ---- agent events ----

func nonNilMessages(ms []Message) []Message {
	if ms == nil {
		return []Message{}
	}
	return ms
}

func (e AgentStartEvent) MarshalJSON() ([]byte, error) { return []byte(`{"type":"agent_start"}`), nil }
func (e TurnStartEvent) MarshalJSON() ([]byte, error)  { return []byte(`{"type":"turn_start"}`), nil }
func (e AgentEndEvent) MarshalJSON() ([]byte, error) {
	type a AgentEndEvent
	e.Messages = nonNilMessages(e.Messages)
	return typed("agent_end", a(e))
}
func (e TurnEndEvent) MarshalJSON() ([]byte, error) {
	type a TurnEndEvent
	if e.ToolResults == nil {
		e.ToolResults = []*ToolResultMessage{}
	}
	return typed("turn_end", a(e))
}
func (e MessageStartEvent) MarshalJSON() ([]byte, error) {
	type a MessageStartEvent
	return typed("message_start", a(e))
}
func (e MessageUpdateEvent) MarshalJSON() ([]byte, error) {
	type a MessageUpdateEvent
	return typed("message_update", a(e))
}
func (e MessageEndEvent) MarshalJSON() ([]byte, error) {
	type a MessageEndEvent
	return typed("message_end", a(e))
}
func (e ToolExecutionStartEvent) MarshalJSON() ([]byte, error) {
	type a ToolExecutionStartEvent
	if e.Args == nil {
		e.Args = map[string]any{}
	}
	return typed("tool_execution_start", a(e))
}
func (e ToolExecutionUpdateEvent) MarshalJSON() ([]byte, error) {
	type a ToolExecutionUpdateEvent
	if e.Args == nil {
		e.Args = map[string]any{}
	}
	return typed("tool_execution_update", a(e))
}
func (e ToolExecutionEndEvent) MarshalJSON() ([]byte, error) {
	type a ToolExecutionEndEvent
	return typed("tool_execution_end", a(e))
}

func (r ToolResult) MarshalJSON() ([]byte, error) {
	type a ToolResult
	r.Content = nonNil(r.Content)
	return json.Marshal(a(r))
}

func (r *ToolResult) UnmarshalJSON(data []byte) error {
	type a ToolResult
	aux := struct {
		*a
		Content json.RawMessage `json:"content"`
	}{a: (*a)(r)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	var err error
	r.Content, err = decodeContents(aux.Content)
	return err
}

// DecodeEvent decodes one agent event by "type".
func DecodeEvent(data []byte) (Event, error) {
	kind, err := peekField(data, "type")
	if err != nil {
		return nil, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	msg := func(key string) (Message, error) { return DecodeMessage(raw[key]) }
	switch kind {
	case "agent_start":
		return &AgentStartEvent{}, nil
	case "turn_start":
		return &TurnStartEvent{}, nil
	case "agent_end":
		ms, err := DecodeMessages(raw["messages"])
		return &AgentEndEvent{Messages: ms}, err
	case "turn_end":
		m, err := msg("message")
		if err != nil {
			return nil, err
		}
		e := &TurnEndEvent{Message: m}
		err = json.Unmarshal(raw["toolResults"], &e.ToolResults)
		return e, err
	case "message_start":
		m, err := msg("message")
		return &MessageStartEvent{Message: m}, err
	case "message_end":
		m, err := msg("message")
		return &MessageEndEvent{Message: m}, err
	case "message_update":
		m, err := msg("message")
		if err != nil {
			return nil, err
		}
		ae, err := DecodeAssistantEvent(raw["assistantMessageEvent"])
		return &MessageUpdateEvent{Message: m, AssistantMessageEvent: ae}, err
	case "tool_execution_start":
		e := &ToolExecutionStartEvent{}
		return e, json.Unmarshal(data, e)
	case "tool_execution_update":
		e := &ToolExecutionUpdateEvent{}
		return e, json.Unmarshal(data, e)
	case "tool_execution_end":
		e := &ToolExecutionEndEvent{}
		return e, json.Unmarshal(data, e)
	}
	return nil, fmt.Errorf("unknown event type %q", kind)
}
