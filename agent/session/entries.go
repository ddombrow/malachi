// Package session implements append-only JSONL session trees, compatible
// with tau's on-disk format. Entries are never rewritten; state is rebuilt by
// replaying the path from the root to the active tip.
//
// Port of tau_agent/session.
package session

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/ddombrow/malachi/agent"
)

// Entry types written or understood by malachi. Other types (written by tau
// or future versions) are preserved verbatim.
const (
	TypeMessage             = "message"
	TypeCustomMessage       = "custom_message"
	TypeModelChange         = "model_change"
	TypeThinkingLevelChange = "thinking_level_change"
	TypeCompaction          = "compaction"
	TypeBranchSummary       = "branch_summary"
	TypeLabel               = "label"
	TypeLeaf                = "leaf" // legacy; read but never written
	TypeSessionInfo         = "session_info"
	TypeCustom              = "custom"
)

// Entry is one line of a session file. Message holds the decoded message for
// message entries; every other type-specific field lives in Fields as raw
// JSON so unknown entry types round-trip unchanged.
type Entry struct {
	ID        string
	ParentID  string
	Timestamp float64 // Unix seconds, as tau writes it
	Type      string
	Message   agent.Message
	Fields    map[string]json.RawMessage
}

// NewID returns a random 32-hex-character entry id, like tau's uuid4().hex.
func NewID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func now() float64 { return float64(time.Now().UnixMicro()) / 1e6 }

func newEntry(typ string) *Entry {
	return &Entry{ID: NewID(), Timestamp: now(), Type: typ, Fields: map[string]json.RawMessage{}}
}

// Set stores a type-specific field. Nil values are omitted like tau's
// exclude_none.
func (e *Entry) Set(key string, value any) {
	if value == nil {
		delete(e.Fields, key)
		return
	}
	raw, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("session: marshal %s: %v", key, err))
	}
	e.Fields[key] = raw
}

// String returns a string field, or "".
func (e *Entry) String(key string) string {
	var s string
	if raw, ok := e.Fields[key]; ok {
		_ = json.Unmarshal(raw, &s)
	}
	return s
}

// Int returns an integer field, or 0.
func (e *Entry) Int(key string) int64 {
	var n float64
	if raw, ok := e.Fields[key]; ok {
		_ = json.Unmarshal(raw, &n)
	}
	return int64(n)
}

// NewMessageEntry wraps a transcript message.
func NewMessageEntry(m agent.Message) *Entry {
	e := newEntry(TypeMessage)
	e.Message = m
	return e
}

// NewCompactionEntry records an agent-written summary standing in for the
// conversation before firstKeptID, in the shape tau writes. The messages it
// replaces stay in the file: replay skips them, not the disk.
//
// first_kept_entry_id is the id of the entry where the retained tail begins,
// which is what lets replay find the tail among entries that were written
// before this one. Without it, replay would keep only what follows this entry
// and lose a tail that was already on disk. Pass "" only when nothing
// retained has been persisted yet.
func NewCompactionEntry(summary, firstKeptID string, tokensBefore int, u agent.Usage, provider, model string) *Entry {
	e := newEntry(TypeCompaction)
	e.Set("summary", summary)
	if firstKeptID != "" {
		e.Set("first_kept_entry_id", firstKeptID)
	}
	if tokensBefore > 0 {
		e.Set("tokens_before", int64(tokensBefore))
	}
	if provider != "" {
		e.Set("provider", provider)
	}
	if model != "" {
		e.Set("model", model)
	}
	if u.TotalTokens > 0 || u.Input > 0 || u.CacheRead > 0 {
		e.Set("usage", map[string]any{
			"input":      u.Input,
			"output":     u.Output,
			"cacheRead":  u.CacheRead,
			"cacheWrite": u.CacheWrite,
			"total":      u.TotalTokens,
		})
	}
	return e
}

// NewSessionInfo records session metadata; it is normally the root entry.
func NewSessionInfo(cwd, title string) *Entry {
	e := newEntry(TypeSessionInfo)
	e.Set("created_at", e.Timestamp)
	if cwd != "" {
		e.Set("cwd", cwd)
	}
	if title != "" {
		e.Set("title", title)
	}
	return e
}

// NewModelChange records a provider/model switch.
func NewModelChange(provider, model string) *Entry {
	e := newEntry(TypeModelChange)
	e.Set("model", model)
	if provider != "" {
		e.Set("provider", provider)
	}
	return e
}

// NewThinkingLevelChange records a reasoning level switch.
func NewThinkingLevelChange(level string) *Entry {
	e := newEntry(TypeThinkingLevelChange)
	if level != "" {
		e.Set("thinking_level", level)
	}
	return e
}

// NewLabel bookmarks target with label ("" clears it).
func NewLabel(targetID, label string) *Entry {
	e := newEntry(TypeLabel)
	e.Set("target_id", targetID)
	if label != "" {
		e.Set("label", label)
	}
	return e
}

func (e *Entry) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	write := func(key string, raw []byte) {
		if b.Len() > 1 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(key)
		b.Write(k)
		b.WriteByte(':')
		b.Write(raw)
	}
	enc := func(v any) []byte { raw, _ := json.Marshal(v); return raw }
	write("id", enc(e.ID))
	if e.ParentID != "" {
		write("parent_id", enc(e.ParentID))
	}
	write("timestamp", enc(e.Timestamp))
	write("type", enc(e.Type))
	if e.Message != nil {
		raw, err := json.Marshal(e.Message)
		if err != nil {
			return nil, err
		}
		write("message", raw)
	}
	keys := make([]string, 0, len(e.Fields))
	for k := range e.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		write(k, e.Fields[k])
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func (e *Entry) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	str := func(key string) string {
		var s string
		_ = json.Unmarshal(raw[key], &s)
		delete(raw, key)
		return s
	}
	e.ID, e.ParentID, e.Type = str("id"), str("parent_id"), str("type")
	if ts, ok := raw["timestamp"]; ok {
		if err := json.Unmarshal(ts, &e.Timestamp); err != nil {
			return fmt.Errorf("timestamp: %w", err)
		}
		delete(raw, "timestamp")
	}
	if e.ID == "" || e.Type == "" {
		return fmt.Errorf("entry missing id or type")
	}
	if e.Type == TypeMessage {
		m, err := agent.DecodeMessage(raw["message"])
		if err != nil {
			return err
		}
		e.Message = m
		delete(raw, "message")
	}
	e.Fields = raw
	return nil
}
