package coding

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/ddombrow/malachi/agent"
)

// MaxImageBytes is the largest image read returns inline.
const MaxImageBytes = 5 * 1024 * 1024

var imageTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

// NewReadTool returns the read tool rooted at cwd.
func NewReadTool(cwd string) *agent.Tool {
	return &agent.Tool{
		Name:  "read",
		Label: "Read",
		Description: fmt.Sprintf("Read the contents of a file. Supports text files and images (jpg, png, gif, webp). "+
			"Images are sent to vision-capable models as attachments. For text files, output is truncated to "+
			"%d lines or %dKB (whichever is hit first). Use offset/limit for large files. When you need the "+
			"full file, continue with offset until complete.", MaxOutputLines, MaxOutputBytes/1024),
		PromptSnippet:    "Read file contents",
		PromptGuidelines: []string{"Use read to examine files instead of cat or sed."},
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":   map[string]any{"type": "string", "description": "Path to the file to read"},
				"offset": map[string]any{"type": "integer", "description": "Line number to start reading from"},
				"limit":  map[string]any{"type": "integer", "description": "Maximum number of lines to read"},
			},
			"required": []string{"path"},
		},
		Execute: func(_ context.Context, _ string, args map[string]any, _ func(agent.ToolResult)) (agent.ToolResult, error) {
			return executeRead(cwd, args)
		},
	}
}

func executeRead(cwd string, args map[string]any) (agent.ToolResult, error) {
	raw, path, err := pathArg(args, cwd)
	if err != nil {
		return agent.ToolResult{}, err
	}
	offset, _, err := optInt(args, "offset")
	if err != nil {
		return agent.ToolResult{}, err
	}
	limit, hasLimit, err := optInt(args, "limit")
	if err != nil {
		return agent.ToolResult{}, err
	}
	if offset < 0 {
		return agent.ToolResult{}, fmt.Errorf("offset must be at least 0")
	}
	if hasLimit && limit < 1 {
		return agent.ToolResult{}, fmt.Errorf("limit must be at least 1")
	}

	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return agent.ToolResult{}, fmt.Errorf("File not found: %s", path)
		}
		return agent.ToolResult{}, err
	}
	if info.IsDir() {
		return agent.ToolResult{}, fmt.Errorf("Path is a directory: %s. Use bash with ls to list it", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return agent.ToolResult{}, err
	}

	if mime := http.DetectContentType(data); imageTypes[mime] {
		if len(data) > MaxImageBytes {
			return agent.ToolResult{
				Content: []agent.Content{&agent.TextContent{Text: fmt.Sprintf(
					"Read image file [%s]\n[Image omitted: %s exceeds the %s inline limit.]",
					mime, FormatSize(len(data)), FormatSize(MaxImageBytes))}},
				Details: map[string]any{"path": path, "mime_type": mime, "bytes": len(data)},
			}, nil
		}
		return agent.ToolResult{
			Content: []agent.Content{
				&agent.TextContent{Text: fmt.Sprintf("Read image file [%s]", mime)},
				&agent.ImageContent{Data: base64.StdEncoding.EncodeToString(data), MimeType: mime},
			},
			Details: map[string]any{"path": path, "mime_type": mime, "bytes": len(data)},
		}, nil
	}
	if !utf8.Valid(data) {
		return agent.ToolResult{}, fmt.Errorf("Cannot read %s: file is not UTF-8 text or a supported image", path)
	}

	text := strings.ReplaceAll(strings.ReplaceAll(string(data), "\r\n", "\n"), "\r", "\n")
	all := strings.Split(text, "\n")
	start := 0
	if offset > 0 {
		start = offset - 1
	}
	if start >= len(all) {
		return agent.ToolResult{}, fmt.Errorf("Offset %d is beyond end of file (%d lines total)", offset, len(all))
	}
	end := len(all)
	if hasLimit {
		end = min(start+limit, len(all))
	}
	selected := strings.Join(all[start:end], "\n")

	t := TruncateHead(selected, MaxOutputLines, MaxOutputBytes)
	startDisplay := start + 1
	var out string
	switch {
	case t.FirstLineTooBig:
		out = fmt.Sprintf("[Line %d is %s, exceeds %s limit. Use bash: sed -n '%dp' %s | head -c %d]",
			startDisplay, FormatSize(len(all[start])), FormatSize(MaxOutputBytes), startDisplay, raw, MaxOutputBytes)
	case t.Truncated:
		endDisplay := startDisplay + t.OutputLines - 1
		limitNote := ""
		if t.TruncatedBy == "bytes" {
			limitNote = fmt.Sprintf(" (%s limit)", FormatSize(MaxOutputBytes))
		}
		out = fmt.Sprintf("%s\n\n[Showing lines %d-%d of %d%s. Use offset=%d to continue.]",
			t.Content, startDisplay, endDisplay, len(all), limitNote, endDisplay+1)
	case hasLimit && end < len(all):
		out = fmt.Sprintf("%s\n\n[%d more lines in file. Use offset=%d to continue.]", t.Content, len(all)-end, end+1)
	default:
		out = t.Content
	}
	return agent.ToolResult{
		Content: []agent.Content{&agent.TextContent{Text: out}},
		Details: map[string]any{"path": path, "truncation": t},
	}, nil
}
