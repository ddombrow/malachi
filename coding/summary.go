package coding

import (
	"fmt"
	"strings"
)

// SummarizeToolCall renders a one-line, human-readable form of a tool call
// for frontends, e.g. "read main.go:10-50" or "$ go test ./...".
func SummarizeToolCall(name string, args map[string]any) string {
	str := func(k string) string { s, _ := args[k].(string); return s }
	num := func(k string) (int, bool) {
		f, ok := args[k].(float64)
		return int(f), ok
	}
	switch name {
	case "read":
		s := "read " + str("path")
		off, hasOff := num("offset")
		lim, hasLim := num("limit")
		switch {
		case hasOff && hasLim:
			s += fmt.Sprintf(":%d-%d", off, off+lim-1)
		case hasOff:
			s += fmt.Sprintf(":%d-", off)
		case hasLim:
			s += fmt.Sprintf(":1-%d", lim)
		}
		return s
	case "bash":
		cmd := strings.TrimSpace(str("command"))
		if i := strings.IndexByte(cmd, '\n'); i >= 0 {
			cmd = cmd[:i] + " …"
		}
		return "$ " + cmd
	case "edit":
		n := 0
		if list, ok := args["edits"].([]any); ok {
			n = len(list)
		}
		if _, ok := args["oldText"]; ok {
			n++
		}
		return fmt.Sprintf("edit %s (%d change%s)", str("path"), n, map[bool]string{true: "", false: "s"}[n == 1])
	case "write":
		lines := strings.Count(str("content"), "\n")
		if c := str("content"); c != "" && !strings.HasSuffix(c, "\n") {
			lines++
		}
		return fmt.Sprintf("write %s (%d lines)", str("path"), lines)
	}
	var parts []string
	for k, v := range args {
		parts = append(parts, fmt.Sprintf("%s=%v", k, v))
	}
	return name + " " + strings.Join(parts, " ")
}
