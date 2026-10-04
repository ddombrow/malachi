package agent

import (
	"bufio"
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func readLines(t *testing.T, path string) [][]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var lines [][]byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if len(sc.Bytes()) > 0 {
			lines = append(lines, append([]byte(nil), sc.Bytes()...))
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return lines
}

// assertSameJSON compares two JSON documents structurally (key order and
// float formatting like 0.0 vs 0 are ignored).
func assertSameJSON(t *testing.T, want, got []byte) {
	t.Helper()
	var w, g any
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("want: %v", err)
	}
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("got: %v", err)
	}
	if !reflect.DeepEqual(w, g) {
		t.Errorf("JSON mismatch\nwant: %s\n got: %s", want, got)
	}
}

// Fixtures are generated from tau's pydantic models; round-tripping them
// proves malachi speaks the same Pi-compatible wire format.
func TestGoldenMessagesRoundTrip(t *testing.T) {
	for _, line := range readLines(t, "testdata/messages.jsonl") {
		m, err := DecodeMessage(line)
		if err != nil {
			t.Fatalf("decode %s: %v", line, err)
		}
		out, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		assertSameJSON(t, line, out)
	}
}

func TestUserContentForms(t *testing.T) {
	b, _ := json.Marshal(NewUserText("hi"))
	m, err := DecodeMessage(b)
	if err != nil {
		t.Fatal(err)
	}
	u := m.(*UserMessage)
	if u.Content.Blocks != nil || u.Content.Text != "hi" {
		t.Fatalf("string form not preserved: %+v", u.Content)
	}
}

func TestNilSlicesMarshalAsEmpty(t *testing.T) {
	b, _ := json.Marshal(&ToolResultMessage{ToolCallID: "c", ToolName: "x"})
	assertSameJSON(t, []byte(`{"role":"toolResult","toolCallId":"c","toolName":"x","content":[],"isError":false,"timestamp":0}`), b)
	b, _ = json.Marshal(&ToolCall{ID: "c", Name: "n"})
	assertSameJSON(t, []byte(`{"type":"toolCall","id":"c","name":"n","arguments":{}}`), b)
}

func TestGoldenAssistantEventsRoundTrip(t *testing.T) {
	for _, line := range readLines(t, "testdata/provider_events.jsonl") {
		e, err := DecodeAssistantEvent(line)
		if err != nil {
			t.Fatalf("decode %s: %v", line, err)
		}
		out, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		assertSameJSON(t, line, out)
	}
}

func TestGoldenAgentEventsRoundTrip(t *testing.T) {
	for _, line := range readLines(t, "testdata/events.jsonl") {
		e, err := DecodeEvent(line)
		if err != nil {
			t.Fatalf("decode %s: %v", line, err)
		}
		out, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		assertSameJSON(t, line, out)
	}
}
