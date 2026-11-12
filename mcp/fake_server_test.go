package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// The test binary doubles as a fake server: run with MCP_FAKE set to a mode,
// TestMain serves MCP on stdin and stdout instead of running tests.
//
//	modern       server/discover; every request must carry the metadata
//	legacy       refuses server/discover; needs initialize, then pings us
//	silent       never answers server/discover; needs initialize
//	old-only     refuses our version with -32022, offering 2025-11-25
//	crash        writes to stderr and exits 3 before answering
//	deaf         closes its input at once (our writes fail), then exits 3
func fakeServer(mode string) {
	// "-linger" keeps any mode running after its input closes.
	linger := strings.HasSuffix(mode, "-linger")
	mode = strings.TrimSuffix(mode, "-linger")
	if mode == "crash" {
		fmt.Fprintln(os.Stderr, "boom: config file missing")
		os.Exit(3)
	}
	if mode == "deaf" {
		os.Stdin.Close()
		fmt.Fprintln(os.Stderr, "boom: no token")
		time.Sleep(300 * time.Millisecond)
		os.Exit(3)
	}
	var wmu sync.Mutex
	write := func(v any) {
		b, _ := json.Marshal(v)
		wmu.Lock()
		os.Stdout.Write(append(b, '\n'))
		wmu.Unlock()
	}
	reply := func(id json.RawMessage, result any) {
		write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
	fail := func(id json.RawMessage, code int, msg string, data any) {
		e := map[string]any{"code": code, "message": msg}
		if data != nil {
			e["data"] = data
		}
		write(map[string]any{"jsonrpc": "2.0", "id": id, "error": e})
	}
	// A stray line on stdout, which the client must skip.
	fmt.Println("not json")

	initialized := false
	var listenID json.RawMessage
	cancelled := map[string]chan struct{}{}
	var cmu sync.Mutex

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params map[string]any  `json:"params"`
			Result any             `json:"result"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		meta, _ := m.Params["_meta"].(map[string]any)
		modernReq := meta["io.modelcontextprotocol/protocolVersion"] != nil

		switch {
		case m.Method == "" && m.ID != nil:
			// Our ping's answer.
			fmt.Fprintf(os.Stderr, "pong %s\n", m.ID)
			continue
		case m.Method == "notifications/initialized":
			initialized = true
			if mode == "legacy" {
				write(map[string]any{"jsonrpc": "2.0", "id": "p1", "method": "ping"})
			}
			continue
		case m.Method == "notifications/cancelled":
			id := fmt.Sprint(m.Params["requestId"])
			fmt.Fprintf(os.Stderr, "cancelled %s\n", id)
			cmu.Lock()
			if ch := cancelled[id]; ch != nil {
				close(ch)
			}
			cmu.Unlock()
			continue
		}

		switch mode {
		case "modern":
			if !modernReq || meta["io.modelcontextprotocol/clientCapabilities"] == nil {
				fail(m.ID, CodeInvalidParams, "missing request metadata", nil)
				continue
			}
		case "old-only":
			if m.Method == "server/discover" {
				fail(m.ID, CodeUnsupportedVersion, "Unsupported protocol version",
					map[string]any{"supported": []string{"2025-11-25"}, "requested": meta["io.modelcontextprotocol/protocolVersion"]})
				continue
			}
		default:
			if m.Method == "server/discover" {
				if mode == "legacy" {
					fail(m.ID, CodeMethodNotFound, "Method not found", nil)
				}
				continue // silent: no answer at all
			}
		}

		switch m.Method {
		case "server/discover":
			reply(m.ID, map[string]any{
				"supportedVersions": []string{"2026-07-28"},
				"capabilities":      map[string]any{"tools": map[string]any{"listChanged": true}},
				"instructions":      "use echo",
				"resultType":        "complete",
				"_meta":             map[string]any{"io.modelcontextprotocol/serverInfo": map[string]any{"name": "fake", "version": "1.0"}},
			})
		case "initialize":
			v, _ := m.Params["protocolVersion"].(string)
			if v == "" {
				v = "2025-06-18"
			}
			reply(m.ID, map[string]any{
				"protocolVersion": v,
				"capabilities":    map[string]any{"tools": map[string]any{"listChanged": true}},
				"serverInfo":      map[string]any{"name": "fake-" + mode, "version": "0.9"},
			})
		case "subscriptions/listen":
			listenID = m.ID
			write(map[string]any{"jsonrpc": "2.0", "method": "notifications/subscriptions/acknowledged",
				"params": map[string]any{"_meta": map[string]any{"io.modelcontextprotocol/subscriptionId": listenID}}})
		case "tools/list":
			if mode != "modern" && !initialized {
				fail(m.ID, CodeInvalidRequest, "not initialized", nil)
				continue
			}
			ro := true
			tools := []Tool{
				{Name: "echo", Description: "Echo a message", InputSchema: map[string]any{"type": "object"},
					Annotations: &ToolAnnotations{ReadOnlyHint: &ro}},
				{Name: "image", InputSchema: map[string]any{"type": "object"}},
			}
			if m.Params["cursor"] == nil {
				reply(m.ID, map[string]any{"tools": tools, "nextCursor": "page2"})
			} else {
				reply(m.ID, map[string]any{"tools": []Tool{
					{Name: "structured", InputSchema: map[string]any{"type": "object"}},
					{Name: "fails", InputSchema: map[string]any{"type": "object"}},
					{Name: "progress", InputSchema: map[string]any{"type": "object"}},
					{Name: "slow", InputSchema: map[string]any{"type": "object"}},
					{Name: "change", InputSchema: map[string]any{"type": "object"}},
				}})
			}
		case "tools/call":
			name, _ := m.Params["name"].(string)
			args, _ := m.Params["arguments"].(map[string]any)
			text := func(s string) map[string]any {
				return map[string]any{"content": []any{map[string]any{"type": "text", "text": s}}}
			}
			switch name {
			case "echo":
				reply(m.ID, text(fmt.Sprint(args["message"])))
			case "image":
				reply(m.ID, map[string]any{"content": []any{
					map[string]any{"type": "image", "data": "aGVsbG8=", "mimeType": "image/png"},
					map[string]any{"type": "resource_link", "name": "notes", "uri": "file:///notes.md"},
					map[string]any{"type": "resource", "resource": map[string]any{"uri": "mem://a", "text": "inline"}},
					map[string]any{"type": "audio", "data": "AAAA", "mimeType": "audio/wav"},
				}})
			case "structured":
				reply(m.ID, map[string]any{"content": []any{}, "structuredContent": map[string]any{"sum": 3}})
			case "fails":
				r := text("no such thing")
				r["isError"] = true
				reply(m.ID, r)
			case "progress":
				tok := meta["progressToken"]
				for i := 1; i <= 3; i++ {
					write(map[string]any{"jsonrpc": "2.0", "method": "notifications/progress",
						"params": map[string]any{"progressToken": tok, "progress": i, "total": 3, "message": fmt.Sprintf("step %d", i)}})
				}
				reply(m.ID, text("done"))
			case "slow":
				ch := make(chan struct{})
				cmu.Lock()
				cancelled[string(m.ID)] = ch
				cmu.Unlock()
				go func(id json.RawMessage) {
					select {
					case <-ch:
					case <-time.After(10 * time.Second):
						reply(id, text("finally"))
					}
				}(m.ID)
			case "change":
				reply(m.ID, text("changed"))
				n := map[string]any{"jsonrpc": "2.0", "method": "notifications/tools/list_changed"}
				if listenID != nil {
					n["params"] = map[string]any{"_meta": map[string]any{"io.modelcontextprotocol/subscriptionId": listenID}}
				}
				write(n)
			default:
				fail(m.ID, CodeInvalidParams, "unknown tool "+name, nil)
			}
		default:
			fail(m.ID, CodeMethodNotFound, "Method not found: "+m.Method, nil)
		}
	}
	if linger {
		time.Sleep(time.Hour)
	}
}
