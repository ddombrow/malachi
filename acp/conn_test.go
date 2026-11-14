package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// echo answers "echo" with its params, "ask" by calling the client, and
// records notifications.
type echo struct {
	c  *conn
	mu sync.Mutex
	n  []string
}

func (h *echo) request(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case "echo":
		return params, nil
	case "ask":
		var got map[string]any
		err := h.c.call(ctx, "client/q", map[string]any{"q": 1}, &got)
		return got, err
	}
	return nil, &Error{Code: codeMethodNotFound, Message: "no " + method}
}

func (h *echo) notify(method string, _ json.RawMessage) {
	h.mu.Lock()
	h.n = append(h.n, method)
	h.mu.Unlock()
}

// pipeConn runs a conn over pipes; it returns the client's ends.
func pipeConn(t *testing.T) (*echo, io.WriteCloser, *bufio.Scanner, chan error) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	h := &echo{}
	h.c = newConn(inR, outW)
	done := make(chan error, 1)
	go func() {
		err := h.c.serve(context.Background(), h)
		h.c.wait()
		outW.Close()
		done <- err
	}()
	sc := bufio.NewScanner(outR)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	t.Cleanup(func() { inW.Close() })
	return h, inW, sc, done
}

func readMsg(t *testing.T, sc *bufio.Scanner) map[string]any {
	t.Helper()
	if !sc.Scan() {
		t.Fatalf("no message: %v", sc.Err())
	}
	var m map[string]any
	if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
		t.Fatalf("bad message %q: %v", sc.Text(), err)
	}
	return m
}

func TestConnAnswersRequestsAndErrors(t *testing.T) {
	h, in, sc, done := pipeConn(t)
	io.WriteString(in, `{"jsonrpc":"2.0","id":7,"method":"echo","params":{"a":"<b>"}}`+"\n")
	m := readMsg(t, sc)
	if m["id"] != float64(7) || m["result"].(map[string]any)["a"] != "<b>" {
		t.Fatalf("echo: %v", m)
	}
	io.WriteString(in, `{"jsonrpc":"2.0","id":"x","method":"nope"}`+"\r\n")
	m = readMsg(t, sc)
	if m["id"] != "x" || m["error"].(map[string]any)["code"] != float64(codeMethodNotFound) {
		t.Fatalf("unknown method: %v", m)
	}
	io.WriteString(in, "{not json\n")
	m = readMsg(t, sc)
	if m["id"] != nil || m["error"].(map[string]any)["code"] != float64(codeParseError) {
		t.Fatalf("parse error: %v", m)
	}
	io.WriteString(in, `{"jsonrpc":"2.0","method":"session/cancel","params":{}}`+"\n")
	io.WriteString(in, strings.Repeat("x", maxMessageBytes+10)+"\n")
	m = readMsg(t, sc)
	if !strings.Contains(m["error"].(map[string]any)["message"].(string), "over") {
		t.Fatalf("oversized: %v", m)
	}
	io.WriteString(in, `{"jsonrpc":"2.0","id":8,"method":"echo","params":1}`+"\n")
	if m = readMsg(t, sc); m["result"] != float64(1) {
		t.Fatalf("reading did not continue: %v", m)
	}
	in.Close()
	if err := <-done; err != nil {
		t.Fatalf("serve at EOF: %v", err)
	}
	if len(h.n) != 1 || h.n[0] != "session/cancel" {
		t.Fatalf("notifications: %v", h.n)
	}
}

func TestConnCallsTheClient(t *testing.T) {
	_, in, sc, done := pipeConn(t)
	io.WriteString(in, `{"jsonrpc":"2.0","id":1,"method":"ask"}`+"\n")
	q := readMsg(t, sc)
	if q["method"] != "client/q" || q["id"] == nil {
		t.Fatalf("agent request: %v", q)
	}
	id, _ := json.Marshal(q["id"])
	io.WriteString(in, `{"jsonrpc":"2.0","id":`+string(id)+`,"result":{"answer":42}}`+"\n")
	if m := readMsg(t, sc); m["id"] != float64(1) || m["result"].(map[string]any)["answer"] != float64(42) {
		t.Fatalf("answer: %v", m)
	}

	// A call still waiting when the input ends fails rather than hangs.
	io.WriteString(in, `{"jsonrpc":"2.0","id":2,"method":"ask"}`+"\n")
	readMsg(t, sc)
	in.Close()
	m := readMsg(t, sc)
	if m["id"] != float64(2) || !strings.Contains(m["error"].(map[string]any)["message"].(string), "closed") {
		t.Fatalf("pending call at EOF: %v", m)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return")
	}
}

func TestConnServeEndsWithContext(t *testing.T) {
	inR, _ := io.Pipe()
	c := newConn(inR, io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.serve(ctx, &echo{c: c}) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve ignored ctx")
	}
}
