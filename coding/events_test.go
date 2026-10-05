package coding

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/ai/fake"
)

func assertJSON(t *testing.T, v any, want string) {
	t.Helper()
	got, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var g, w any
	_ = json.Unmarshal(got, &g)
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("bad want: %v", err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("JSON mismatch\n got %s\nwant %s", got, want)
	}
}

// The shapes are tau_coding/events.py's, so Pi and tau frontends can read them.
func TestSessionEventJSON(t *testing.T) {
	assertJSON(t, CompactionStartEvent{Reason: CompactionManual}, `{"type":"compaction_start","reason":"manual"}`)
	assertJSON(t, CompactionEndEvent{Reason: CompactionOverflow, Aborted: true, ErrorMessage: "x", HeldPrompt: "private"},
		`{"type":"compaction_end","reason":"overflow","aborted":true,"willRetry":false,"errorMessage":"x"}`)
	assertJSON(t, QueueUpdateEvent{Steering: []string{"a"}}, `{"type":"queue_update","steering":["a"],"followUp":[]}`)
	assertJSON(t, AgentSettledEvent{}, `{"type":"agent_settled"}`)
	assertJSON(t, ThinkingLevelChangedEvent{Level: "high"}, `{"type":"thinking_level_changed","level":"high"}`)
	assertJSON(t, CompactionProgressEvent{Reason: CompactionManual, Phase: "writing"}, `{"type":"compaction_progress","reason":"manual","phase":"writing"}`)
}

func TestSummarizeResultWireShape(t *testing.T) {
	r := SummarizeResult{Summary: "s", TokensBefore: 10, TokensAfter: 3, Replaced: 4, Kept: 2, FirstKeptID: "e1"}
	raw, _ := json.Marshal(r)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	for _, k := range []string{"summary", "firstKeptEntryId", "tokensBefore", "estimatedTokensAfter", "details"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing tau key %q in %s", k, raw)
		}
	}
}

// Subscribe carries the agent's events and the session's own, in order, on a
// goroutine of its own.
func TestSubscribeDeliversAgentEventsInOrder(t *testing.T) {
	s, err := Open(Options{Cwd: t.TempDir(), Home: t.TempDir(), Settings: &Settings{}, Provider: fake.New(fake.Text("hi")), NoSession: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got := make(chan string, 64)
	s.Subscribe(func(e any) {
		switch v := e.(type) {
		case agent.Event:
			got <- agent.EventType(v)
		case ThinkingLevelChangedEvent:
			got <- "thinking_level_changed"
		}
	})
	if err := s.Prompt(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	s.emit(ThinkingLevelChangedEvent{Level: "low"})

	var seq []string
	timeout := time.After(2 * time.Second)
	for len(seq) == 0 || seq[len(seq)-1] != "thinking_level_changed" {
		select {
		case e := <-got:
			seq = append(seq, e)
		case <-timeout:
			t.Fatalf("events stopped arriving: %v", seq)
		}
	}
	if seq[0] != "agent_start" || seq[len(seq)-2] != "agent_end" {
		t.Fatalf("events out of order: %v", seq)
	}
}

// A listener that blocks cannot stall the session: emitting never waits.
func TestEmitNeverBlocksOnAListener(t *testing.T) {
	s, err := Open(Options{Cwd: t.TempDir(), Home: t.TempDir(), Settings: &Settings{}, Provider: fake.New(), NoSession: true})
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	s.Subscribe(func(any) { <-release })
	done := make(chan struct{})
	go func() {
		for i := 0; i < 10_000; i++ {
			s.emit(AgentSettledEvent{})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("emit blocked behind a stuck listener")
	}
	close(release)
	s.Close()
}
