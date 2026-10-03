package sse

import (
	"io"
	"strings"
	"testing"
)

func TestReader(t *testing.T) {
	in := ": keep-alive\n\ndata: {\"a\":1}\n\nevent: ping\ndata: line1\ndata: line2\r\n\r\ndata: [DONE]"
	r := NewReader(strings.NewReader(in))
	want := []Event{{Data: `{"a":1}`}, {Event: "ping", Data: "line1\nline2"}, {Data: "[DONE]"}}
	for i, w := range want {
		got, err := r.Next()
		if err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
		if got != w {
			t.Fatalf("event %d: got %+v want %+v", i, got, w)
		}
	}
	if _, err := r.Next(); err != io.EOF {
		t.Fatalf("want EOF, got %v", err)
	}
}
