package mcp

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ddombrow/malachi/agent"
)

// TestEverythingServer runs against the reference server-everything when
// MCP_EVERYTHING names its installed binary (npm i
// @modelcontextprotocol/server-everything, never npx). It serves both
// protocol eras, so the client is checked against each.
func TestEverythingServer(t *testing.T) {
	bin := os.Getenv("MCP_EVERYTHING")
	if bin == "" {
		t.Skip("MCP_EVERYTHING not set")
	}
	for _, era := range []struct {
		name    string
		legacy  bool
		version string
	}{{"modern", false, "2026-07-28"}, {"legacy", true, "2025-11-25"}} {
		t.Run(era.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			var changed atomic.Int32
			p, err := Start(ctx, exec.Command(bin, "stdio"), Options{Client: testClient, legacyOnly: era.legacy,
				OnToolsChanged: func() { changed.Add(1) }})
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			if p.Version() != era.version || p.Server().Name != "mcp-servers/everything" || p.Instructions() == "" {
				t.Errorf("version %q server %+v", p.Version(), p.Server())
			}
			tools, err := p.ListTools(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, tl := range tools {
				names = append(names, tl.Name)
			}
			for _, want := range []string{"echo", "get-sum", "get-tiny-image", "trigger-long-running-operation", "get-structured-content"} {
				if !slices.Contains(names, want) {
					t.Errorf("no %s in %v", want, names)
				}
			}
			call := func(name string, args map[string]any, onProgress func(Progress)) agent.ToolResult {
				t.Helper()
				res, err := p.CallTool(ctx, name, args, onProgress)
				if err != nil || res.IsError {
					t.Fatalf("%s: %v %+v", name, err, res)
				}
				return res.Result()
			}
			if got := call("echo", map[string]any{"message": "hi"}, nil).Text(); got != "Echo: hi" {
				t.Errorf("echo %q", got)
			}
			if got := call("get-sum", map[string]any{"a": 2, "b": 3}, nil).Text(); !strings.Contains(got, "5") {
				t.Errorf("sum %q", got)
			}
			img := call("get-tiny-image", nil, nil)
			if !slices.ContainsFunc(img.Content, func(c agent.Content) bool { _, ok := c.(*agent.ImageContent); return ok }) {
				t.Errorf("no image in %+v", img.Content)
			}
			sc := call("get-structured-content", map[string]any{"location": "Chicago"}, nil)
			if d, _ := sc.Details.(map[string]any); d["structuredContent"] == nil {
				t.Errorf("no structured content: %+v", sc)
			}
			var steps []Progress
			call("trigger-long-running-operation", map[string]any{"duration": 0.3, "steps": 3}, func(pr Progress) { steps = append(steps, pr) })
			if len(steps) != 3 {
				t.Errorf("progress %+v", steps)
			}
			// A call cancelled mid-flight leaves the connection usable.
			cctx, ccancel := context.WithTimeout(ctx, 100*time.Millisecond)
			_, err = p.CallTool(cctx, "trigger-long-running-operation", map[string]any{"duration": 5, "steps": 5}, nil)
			ccancel()
			if err == nil {
				t.Error("cancelled call returned")
			}
			if got := call("echo", map[string]any{"message": "still here"}, nil).Text(); got != "Echo: still here" {
				t.Errorf("after cancel %q", got)
			}
			// List changes arrive: on subscriptions/listen in the modern era,
			// as plain notifications in the legacy one.
			call("trigger-list-changed", map[string]any{"list": "tools"}, nil)
			deadline := time.Now().Add(5 * time.Second)
			for changed.Load() == 0 && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if changed.Load() == 0 {
				t.Error("no tools list_changed notification")
			}
		})
	}
}
