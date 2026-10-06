package coding

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/ddombrow/malachi/agent"
)

// fileLocks serializes writes and edits to the same path within the process.
var fileLocks sync.Map // path -> *sync.Mutex

func lockFile(path string) func() {
	m, _ := fileLocks.LoadOrStore(path, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// NewWriteTool returns the write tool rooted at cwd.
func NewWriteTool(cwd string, opts ToolOptions) *agent.Tool {
	return &agent.Tool{
		Name:  "write",
		Label: "Write",
		Description: "Write content to a file. Creates the file if it doesn't exist, overwrites if it does. " +
			"Automatically creates parent directories.",
		PromptSnippet:    "Create or overwrite files",
		PromptGuidelines: []string{"Use write only for new files or complete rewrites."},
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":    map[string]any{"type": "string", "description": "Path to the file to write"},
				"content": map[string]any{"type": "string", "description": "Content to write to the file"},
			},
			"required": []string{"path", "content"},
		},
		Execute: func(_ context.Context, _ string, args map[string]any, _ func(agent.ToolResult)) (agent.ToolResult, error) {
			_, path, err := pathArg(args, cwd)
			if err != nil {
				return agent.ToolResult{}, err
			}
			if err := opts.policy().CheckWrite(path); err != nil {
				return agent.ToolResult{}, err
			}
			content, err := strArg(args, "content")
			if err != nil {
				return agent.ToolResult{}, err
			}
			defer lockFile(path)()
			// Creates parent directories; an existing file keeps its mode.
			f, err := opts.policy().OpenWrite(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
			if err != nil {
				return agent.ToolResult{}, err
			}
			_, err = f.WriteString(content)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return agent.ToolResult{}, err
			}
			r := agent.TextResult(fmt.Sprintf("Successfully wrote to %s.", path))
			r.Details = map[string]any{"path": path, "characters": len([]rune(content))}
			return r, nil
		},
	}
}
