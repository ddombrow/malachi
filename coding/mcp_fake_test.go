package coding

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// fakeMCPEnv set makes the test binary a minimal MCP server (2026-07-28 only)
// whose tools report what its sandbox lets it do.
const fakeMCPEnv = "MALACHI_FAKE_MCP"

func fakeMCPServer() {
	out := bufio.NewWriter(os.Stdout)
	send := func(v any) {
		b, _ := json.Marshal(v)
		out.Write(append(b, '\n'))
		out.Flush()
	}
	text := func(s string, isError bool) map[string]any {
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": s}}, "isError": isError}
	}
	schema := map[string]any{"type": "object"}
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name      string            `json:"name"`
				Arguments map[string]string `json:"arguments"`
			} `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil || m.ID == nil {
			continue
		}
		var result any
		switch m.Method {
		case "server/discover":
			result = map[string]any{"supportedVersions": []string{"2026-07-28"},
				"capabilities": map[string]any{"tools": map[string]any{}},
				"_meta":        map[string]any{"io.modelcontextprotocol/serverInfo": map[string]any{"name": "fake-coding", "version": "1"}}}
		case "tools/list":
			var tools []any
			for _, n := range []string{"env", "read", "write", "pid", "big"} {
				tools = append(tools, map[string]any{"name": n, "inputSchema": schema})
			}
			result = map[string]any{"tools": tools}
		case "tools/call":
			a := m.Params.Arguments
			switch m.Params.Name {
			case "env":
				v, ok := os.LookupEnv(a["name"])
				if !ok {
					v = "(unset)"
				}
				result = text(v, false)
			case "read":
				b, err := os.ReadFile(a["path"])
				if err != nil {
					result = text(err.Error(), true)
				} else {
					result = text(string(b), false)
				}
			case "write":
				if err := os.WriteFile(a["path"], []byte(a["content"]), 0o600); err != nil {
					result = text(err.Error(), true)
				} else {
					result = text("ok", false)
				}
			case "pid":
				result = text(strconv.Itoa(os.Getpid()), false)
			case "big":
				result = text(strings.Repeat("a line of output\n", 5000), false)
			default:
				result = text(fmt.Sprintf("no tool %s", m.Params.Name), true)
			}
		default:
			send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32601, "message": "not found"}})
			continue
		}
		send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result})
	}
}
