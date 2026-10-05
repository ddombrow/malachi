package session

import (
	"encoding/json"

	"github.com/ddombrow/malachi/agent"
)

// Tip returns the active tip: the last non-leaf entry in file order.
func Tip(entries []*Entry) *Entry {
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Type != TypeLeaf {
			return entries[i]
		}
	}
	return nil
}

// BranchPath returns the entries from the root to tipID by following
// parent_id links.
func BranchPath(entries []*Entry, tipID string) []*Entry {
	byID := make(map[string]*Entry, len(entries))
	for _, e := range entries {
		byID[e.ID] = e
	}
	var path []*Entry
	seen := map[string]bool{}
	for id := tipID; id != "" && !seen[id]; {
		e, ok := byID[id]
		if !ok {
			break
		}
		seen[id] = true
		path = append(path, e)
		id = e.ParentID
	}
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	return path
}

// State is what replaying a branch path yields.
type State struct {
	Messages []agent.Message
	// EntryIDs[i] is the id of the entry Messages[i] was replayed from, or ""
	// for a message synthesized during replay (a compaction's summary). A
	// resumed session needs these to point later entries, such as a new
	// compaction's first kept entry, at messages already on disk.
	EntryIDs      []string
	Provider      string
	Model         string
	ThinkingLevel string
	Title         string
	Cwd           string
}

// Replay rebuilds conversation state from a root-to-tip path. The latest
// compaction replaces everything before its first kept entry with a summary.
func Replay(path []*Entry) State { return replay(path, true) }

// ReplayAll rebuilds the conversation ignoring compaction entries, recovering
// the full history a summary replaced. A summary is lossy by construction, so
// the dropped messages have to remain reachable from disk; this is how.
func ReplayAll(path []*Entry) State { return replay(path, false) }

func replay(path []*Entry, honorCompaction bool) State {
	var st State
	start := 0
	var compaction *Entry
	if honorCompaction {
		for i, e := range path {
			if e.Type == TypeCompaction {
				compaction, start = e, i
			}
		}
	}
	if compaction != nil {
		// Keep entries from first_kept_entry_id (if on the path) onward.
		if kept := compaction.String("first_kept_entry_id"); kept != "" {
			for i, e := range path[:start] {
				if e.ID == kept {
					start = i
					break
				}
			}
		}
		st.add(&agent.CompactionSummaryMessage{
			Summary:      compaction.String("summary"),
			TokensBefore: compaction.Int("tokens_before"),
			Timestamp:    int64(compaction.Timestamp * 1000),
		}, "")
	}

	for i, e := range path {
		switch e.Type {
		case TypeCompaction:
			// ReplayAll keeps the messages a compaction replaced, so the entry
			// itself contributes nothing to the conversation.
			continue
		case TypeSessionInfo:
			st.Title, st.Cwd = e.String("title"), e.String("cwd")
		case TypeModelChange:
			st.Model, st.Provider = e.String("model"), e.String("provider")
		case TypeThinkingLevelChange:
			st.ThinkingLevel = e.String("thinking_level")
		}
		if i < start {
			continue
		}
		switch e.Type {
		case TypeMessage:
			st.add(e.Message, e.ID)
		case TypeCustomMessage:
			m := &agent.CustomMessage{
				CustomType: e.String("custom_type"),
				Display:    true,
				Timestamp:  int64(e.Timestamp * 1000),
			}
			if raw, ok := e.Fields["content"]; ok {
				_ = json.Unmarshal(raw, &m.Content)
			}
			if raw, ok := e.Fields["display"]; ok {
				_ = json.Unmarshal(raw, &m.Display)
			}
			st.add(m, e.ID)
		case TypeBranchSummary:
			st.add(&agent.BranchSummaryMessage{
				Summary:   e.String("summary"),
				FromID:    e.String("branch_root_id"),
				Timestamp: int64(e.Timestamp * 1000),
			}, e.ID)
		}
	}
	return st
}

func (st *State) add(m agent.Message, entryID string) {
	st.Messages = append(st.Messages, m)
	st.EntryIDs = append(st.EntryIDs, entryID)
}
