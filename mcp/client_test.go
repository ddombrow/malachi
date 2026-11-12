package mcp

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ddombrow/malachi/agent"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv("MCP_FAKE"); mode != "" {
		fakeServer(mode)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

var testClient = Implementation{Name: "malachi-test", Version: "0"}

func startFake(t *testing.T, mode string, opts Options) (*Process, error) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "MCP_FAKE="+mode)
	if opts.Client.Name == "" {
		opts.Client = testClient
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	p, err := Start(ctx, cmd, opts)
	if p != nil {
		t.Cleanup(p.Close)
	}
	return p, err
}

func mustStart(t *testing.T, mode string, opts Options) *Process {
	t.Helper()
	p, err := startFake(t, mode, opts)
	if err != nil {
		t.Fatalf("start %s: %v", mode, err)
	}
	return p
}

func TestNegotiation(t *testing.T) {
	for _, c := range []struct {
		mode, version, server string
		instructions          string
	}{
		{"modern", "2026-07-28", "fake", "use echo"},
		{"legacy", "2025-11-25", "fake-legacy", ""},
		{"silent", "2025-11-25", "fake-silent", ""},
		{"old-only", "2025-11-25", "fake-old-only", ""},
	} {
		t.Run(c.mode, func(t *testing.T) {
			p := mustStart(t, c.mode, Options{ProbeTimeout: 300 * time.Millisecond})
			if p.Version() != c.version || p.Server().Name != c.server || p.Instructions() != c.instructions {
				t.Errorf("version %q server %+v instructions %q", p.Version(), p.Server(), p.Instructions())
			}
			// Every era lists and calls tools the same way.
			tools, err := p.ListTools(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(tools) != 7 || tools[0].Name != "echo" || !tools[0].ReadOnly() || tools[1].ReadOnly() {
				t.Errorf("tools %+v", tools)
			}
			res, err := p.CallTool(context.Background(), "echo", map[string]any{"message": "hi"}, nil)
			if err != nil || res.Result().Text() != "hi" {
				t.Errorf("echo: %v %+v", err, res)
			}
		})
	}
}

// A legacy server may ping; the client answers.
func TestLegacyPing(t *testing.T) {
	p := mustStart(t, "legacy", Options{})
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(p.Stderr(), `pong "p1"`) {
		if time.Now().After(deadline) {
			t.Fatalf("no pong; stderr %q", p.Stderr())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestResultContent(t *testing.T) {
	p := mustStart(t, "modern", Options{})
	ctx := context.Background()

	res, err := p.CallTool(ctx, "image", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := res.Result()
	img, ok := r.Content[0].(*agent.ImageContent)
	if !ok || img.MimeType != "image/png" || img.Data != "aGVsbG8=" {
		t.Errorf("image %+v", r.Content[0])
	}
	if got := r.Text(); !strings.Contains(got, "[resource notes: file:///notes.md]") ||
		!strings.Contains(got, "[resource mem://a]\ninline") || !strings.Contains(got, "[audio (audio/wav, 3 bytes) not shown]") {
		t.Errorf("text %q", got)
	}

	res, err = p.CallTool(ctx, "structured", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	r = res.Result()
	if r.Text() != "{\n  \"sum\": 3\n}" || r.Details.(map[string]any)["structuredContent"].(map[string]any)["sum"] != 3.0 {
		t.Errorf("structured %q %v", r.Text(), r.Details)
	}

	res, err = p.CallTool(ctx, "fails", nil, nil)
	if err != nil || !res.IsError || res.Result().Text() != "no such thing" {
		t.Errorf("isError: %v %+v", err, res)
	}

	_, err = p.CallTool(ctx, "nope", nil, nil)
	var rpcErr *Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != CodeInvalidParams {
		t.Errorf("unknown tool: %v", err)
	}
}

func TestProgress(t *testing.T) {
	p := mustStart(t, "modern", Options{})
	var got []Progress
	res, err := p.CallTool(context.Background(), "progress", nil, func(pr Progress) { got = append(got, pr) })
	if err != nil || res.Result().Text() != "done" {
		t.Fatal(err, res)
	}
	if len(got) != 3 || got[2].Progress != 3 || got[2].Total != 3 || got[2].Message != "step 3" {
		t.Errorf("progress %+v", got)
	}
}

// Cancelling a call tells the server, which stops.
func TestCancellation(t *testing.T) {
	p := mustStart(t, "modern", Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := p.CallTool(ctx, "slow", nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(p.Stderr(), "cancelled ") {
		if time.Now().After(deadline) {
			t.Fatalf("server not told; stderr %q", p.Stderr())
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The connection still works.
	if res, err := p.CallTool(context.Background(), "echo", map[string]any{"message": "after"}, nil); err != nil || res.Result().Text() != "after" {
		t.Errorf("after cancel: %v", err)
	}
}

// Both eras report tool list changes: a modern server on its
// subscriptions/listen stream, a legacy one as a plain notification.
func TestToolsChanged(t *testing.T) {
	for _, mode := range []string{"modern", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			var changed atomic.Int32
			p := mustStart(t, mode, Options{OnToolsChanged: func() { changed.Add(1) }})
			if _, err := p.CallTool(context.Background(), "change", nil, nil); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for changed.Load() == 0 {
				if time.Now().After(deadline) {
					t.Fatal("no change reported")
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}

// A server that dies says why: its exit status and the end of its stderr.
// A server that dies before the handshake is explained by its exit and
// stderr, even when a write to it fails (broken pipe) before its output is
// seen to end.
func TestEarlyExitExplains(t *testing.T) {
	_, err := startFake(t, "deaf", Options{})
	if err == nil || !strings.Contains(err.Error(), "no token") || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("error: %v", err)
	}
}

func TestCrashExplains(t *testing.T) {
	_, err := startFake(t, "crash", Options{})
	if err == nil || !strings.Contains(err.Error(), "exit status 3") || !strings.Contains(err.Error(), "boom: config file missing") {
		t.Fatalf("err %v", err)
	}
}

// Closing stops the server and fails later calls.
func TestCloseStopsTheServer(t *testing.T) {
	p := mustStart(t, "modern", Options{})
	p.Close()
	select {
	case <-p.Exited():
	case <-time.After(10 * time.Second):
		t.Fatal("server still running after Close")
	}
	if _, err := p.CallTool(context.Background(), "echo", nil, nil); !errors.Is(err, ErrClosed) {
		t.Errorf("call after close: %v", err)
	}
}

// A server that ignores its closed input is killed.
func TestCloseKillsALingeringServer(t *testing.T) {
	p := mustStart(t, "modern-linger", Options{})
	start := time.Now()
	p.Close()
	if d := time.Since(start); d > 3*shutdownGrace {
		t.Errorf("took %v", d)
	}
}
