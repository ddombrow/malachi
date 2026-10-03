package agent

// InterruptedToolResult is the text of synthesized results for tool calls
// whose real result never arrived (cancellation, crash).
const InterruptedToolResult = "Tool call interrupted by user"

// RepairToolHistory returns history where every tool call is immediately
// followed by exactly one result, which providers require.
//
// Existing results are moved beside their calls. Missing results get a
// synthetic interruption error. Results with no matching call are dropped.
// When duplicates exist, a real result is preferred over a synthetic one.
// Port of tau_agent/tool_history.py.
func RepairToolHistory(messages []Message) []Message {
	type occ struct {
		msgIdx, offset int
		call           *ToolCall
	}
	type res struct {
		pos int // -1 for synthesized
		msg *ToolResultMessage
	}
	var occs []occ
	resultsByID := map[string][]res{}
	for i, m := range messages {
		switch v := m.(type) {
		case *AssistantMessage:
			for j, c := range v.ToolCalls() {
				occs = append(occs, occ{i, j + 1, c})
			}
		case *ToolResultMessage:
			resultsByID[v.ToolCallID] = append(resultsByID[v.ToolCallID], res{i, v})
		}
	}
	if len(occs) == 0 && len(resultsByID) == 0 {
		return messages
	}

	key := func(o occ) [2]int { return [2]int{o.msgIdx, o.offset} }
	selected := map[[2]int]res{}
	used := map[int]bool{}

	// Reserve already-adjacent pairs first.
	for _, o := range occs {
		pos := o.msgIdx + o.offset
		if pos >= len(messages) {
			continue
		}
		if r, ok := messages[pos].(*ToolResultMessage); ok && r.ToolCallID == o.call.ID && !used[pos] {
			selected[key(o)] = res{pos, r}
			used[pos] = true
		}
	}

	for _, o := range occs {
		if _, ok := selected[key(o)]; ok {
			continue
		}
		var candidates, after []res
		for _, r := range resultsByID[o.call.ID] {
			if !used[r.pos] {
				candidates = append(candidates, r)
				if r.pos > o.msgIdx {
					after = append(after, r)
				}
			}
		}
		if len(candidates) > 0 {
			pool := candidates
			if len(after) > 0 {
				pool = after
			}
			pick := pool[0]
			for _, r := range pool {
				if !isInterruption(r.msg) {
					pick = r
					break
				}
			}
			selected[key(o)] = pick
			used[pick.pos] = true
			continue
		}
		selected[key(o)] = res{-1, &ToolResultMessage{
			ToolCallID: o.call.ID,
			ToolName:   o.call.Name,
			Content:    []Content{&TextContent{Text: InterruptedToolResult}},
			IsError:    true,
			Timestamp:  NowMillis(),
		}}
	}

	// Prefer a leftover real result over a selected interruption.
	for _, o := range occs {
		sel := selected[key(o)]
		if sel.pos < 0 || !isInterruption(sel.msg) {
			continue
		}
		for _, r := range resultsByID[o.call.ID] {
			if !used[r.pos] && !isInterruption(r.msg) {
				delete(used, sel.pos)
				used[r.pos] = true
				selected[key(o)] = r
				break
			}
		}
	}

	out := make([]Message, 0, len(messages))
	for i, m := range messages {
		if _, ok := m.(*ToolResultMessage); ok {
			continue
		}
		out = append(out, m)
		if a, ok := m.(*AssistantMessage); ok {
			for j := range a.ToolCalls() {
				out = append(out, selected[[2]int{i, j + 1}].msg)
			}
		}
	}
	return out
}

func isInterruption(m *ToolResultMessage) bool {
	return m.IsError && m.Text() == InterruptedToolResult
}
