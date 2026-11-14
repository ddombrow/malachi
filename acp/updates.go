package acp

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/coding"
)

// event turns one session event into session/update notifications. It runs
// on the session's event goroutine, in order.
func (as *acpSession) event(e any) {
	switch e := e.(type) {
	case *agent.MessageUpdateEvent:
		switch d := e.AssistantMessageEvent.(type) {
		case *agent.TextDelta:
			if d.Delta != "" {
				as.update(chunkUpdate{SessionUpdate: "agent_message_chunk", Content: textBlock(d.Delta)})
			}
		case *agent.ThinkingDelta:
			if d.Delta != "" {
				as.update(chunkUpdate{SessionUpdate: "agent_thought_chunk", Content: textBlock(d.Delta)})
			}
		}
	case *agent.MessageEndEvent:
		if m, ok := e.Message.(*agent.AssistantMessage); ok {
			as.mu.Lock()
			if as.turn != nil {
				as.turn.last = m
			}
			as.mu.Unlock()
		}
	case *agent.ToolExecutionStartEvent:
		as.update(as.toolStart(e.ToolCallID, e.ToolName, e.Args))
	case *agent.ToolExecutionUpdateEvent:
		if text := e.PartialResult.Text(); text != "" {
			c := textBlock(text)
			as.update(toolCallUpdate{
				SessionUpdate: "tool_call_update", ToolCallID: e.ToolCallID,
				Content: []toolCallContent{{Type: "content", Content: &c}},
			})
		}
	case *agent.ToolExecutionEndEvent:
		as.update(as.toolEnd(e.ToolCallID, e.ToolName, e.Result, e.IsError))
	case coding.AgentSettledEvent:
		as.finish()
	case coding.ThinkingLevelChangedEvent:
		as.update(configOptionUpdate{SessionUpdate: "config_option_update", ConfigOptions: as.configOptions()})
	case coding.CompactionStartEvent:
		if as.sv.clientCaps().Session.Compaction != nil {
			as.mu.Lock()
			as.compactions++
			id := fmt.Sprintf("compaction-%d", as.compactions)
			as.mu.Unlock()
			as.update(compactionUpdate{SessionUpdate: "compaction_update", CompactionID: id, Status: "in_progress"})
		}
	case coding.CompactionEndEvent:
		if as.sv.clientCaps().Session.Compaction != nil {
			as.mu.Lock()
			id := fmt.Sprintf("compaction-%d", as.compactions)
			as.mu.Unlock()
			u := compactionUpdate{SessionUpdate: "compaction_update", CompactionID: id, Status: "completed"}
			switch {
			case e.Aborted:
				u.Status = "cancelled"
			case e.ErrorMessage != "":
				u.Status, u.Error = "failed", e.ErrorMessage
			}
			as.update(u)
		}
		if e.Aborted && e.Reason == coding.CompactionManual && e.HeldPrompt != "" {
			// A prompt held by a manual compaction never runs when the
			// compaction is aborted, so no settle will end its turn.
			as.mu.Lock()
			if as.turn != nil {
				as.turn.cancelled = true
			}
			as.mu.Unlock()
			as.finish()
		}
	}
}

// toolStart describes a tool call as it begins.
func (as *acpSession) toolStart(id, name string, args map[string]any) toolCallUpdate {
	u := toolCallUpdate{
		SessionUpdate: "tool_call", ToolCallID: id,
		Title: toolTitle(name, args), Kind: toolKind(name), Status: toolInProgress,
		RawInput: args,
	}
	if p, ok := args["path"].(string); ok && p != "" {
		if !filepath.IsAbs(p) {
			p = filepath.Join(as.cwd, p)
		}
		u.Locations = []toolLocation{{Path: p}}
	}
	return u
}

// toolEnd reports a finished call: its output, plus an edit's patch.
func (as *acpSession) toolEnd(id, _ string, r agent.ToolResult, isError bool) toolCallUpdate {
	u := toolCallUpdate{SessionUpdate: "tool_call_update", ToolCallID: id, Status: toolCompleted}
	if isError {
		u.Status = toolFailed
	}
	if text := r.Text(); text != "" {
		c := textBlock(text)
		u.Content = append(u.Content, toolCallContent{Type: "content", Content: &c})
	}
	if d, ok := r.Details.(map[string]any); ok {
		if patch, ok := d["patch"].(string); ok && patch != "" {
			fence := "```"
			for strings.Contains(patch, fence) {
				fence += "`"
			}
			c := textBlock(fence + "diff\n" + strings.TrimRight(patch, "\n") + "\n" + fence)
			u.Content = append(u.Content, toolCallContent{Type: "content", Content: &c})
		}
	}
	return u
}

// toolKind maps a tool to the icon a client shows for it.
func toolKind(name string) string {
	switch name {
	case "read":
		return "read"
	case "write", "edit":
		return "edit"
	case "bash":
		return "execute"
	case "fetch":
		return "fetch"
	case "grep", "glob":
		return "search"
	}
	return "other"
}

// toolTitle is a call's one-line description.
func toolTitle(name string, args map[string]any) string {
	str := func(k string) string {
		s, _ := args[k].(string)
		return firstLine(s, 80)
	}
	switch name {
	case "read":
		return "Read " + str("path")
	case "write":
		return "Write " + str("path")
	case "edit":
		return "Edit " + str("path")
	case "bash":
		return str("command")
	case "fetch":
		return "Fetch " + str("url")
	case "grep":
		return "Search for " + str("pattern")
	case "glob":
		return "Find " + str("pattern")
	case "script":
		return "Run script"
	}
	if rest, ok := strings.CutPrefix(name, "mcp__"); ok {
		server, tool, _ := strings.Cut(rest, "__")
		return server + ": " + tool
	}
	return name
}
