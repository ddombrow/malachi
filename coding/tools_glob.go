package coding

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/ddombrow/malachi/agent"
)

const (
	// DefaultGlobLimit is how many paths glob returns unless asked otherwise.
	DefaultGlobLimit = 200
	// MaxGlobLimit caps the limit argument.
	MaxGlobLimit = 2000
)

// NewGlobTool returns the glob tool rooted at cwd.
func NewGlobTool(cwd string) *agent.Tool {
	return &agent.Tool{
		Name:  "glob",
		Label: "Glob",
		Description: fmt.Sprintf("Find files by name pattern and return their paths, one per line, relative "+
			"to the working directory. Supports \"**\" to match any number of directories, e.g. "+
			"\"**/*_test.go\" or \"src/**/*.ts\". A pattern with no slash matches file names at any depth. "+
			"Returns at most %d paths; raise limit or narrow the pattern for more.", DefaultGlobLimit),
		PromptSnippet: "Find files by name pattern",
		PromptGuidelines: []string{
			"Use glob to find which files exist and grep to search inside them; glob first when you know the filename shape but not the file.",
			"Prefer a specific pattern such as \"**/*.go\" over listing a directory, and a narrower pattern over raising limit: a smaller result costs less context.",
		},
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{"type": "string", "description": "Filename pattern, e.g. \"**/*.go\""},
				"path":    map[string]any{"type": "string", "description": "Directory to search (default: working directory)"},
				"limit":   map[string]any{"type": "integer", "description": fmt.Sprintf("Maximum paths to return (default %d, max %d)", DefaultGlobLimit, MaxGlobLimit)},
			},
			"required": []string{"pattern"},
		},
		Execute: func(_ context.Context, _ string, args map[string]any, _ func(agent.ToolResult)) (agent.ToolResult, error) {
			return executeGlob(cwd, args)
		},
	}
}

func executeGlob(cwd string, args map[string]any) (agent.ToolResult, error) {
	pattern, err := strArg(args, "pattern")
	if err != nil {
		return agent.ToolResult{}, err
	}
	if strings.TrimSpace(pattern) == "" {
		return agent.ToolResult{}, fmt.Errorf("argument %q must not be empty", "pattern")
	}
	limit, hasLimit, err := optInt(args, "limit")
	if err != nil {
		return agent.ToolResult{}, err
	}
	if !hasLimit {
		limit = DefaultGlobLimit
	}
	if limit < 1 {
		return agent.ToolResult{}, fmt.Errorf("limit must be at least 1")
	}
	limit = min(limit, MaxGlobLimit)

	root, single, err := searchRoot(cwd, args)
	if err != nil {
		return agent.ToolResult{}, err
	}
	if single != "" {
		return agent.ToolResult{}, fmt.Errorf(
			"Path is not a directory: %s. This tool lists names inside a directory.", resolvePath(cwd, single))
	}

	var paths []string
	hitLimit := false
	if err := walkSearch(root, func(rel string, _ fs.DirEntry) error {
		if !matchFilter(pattern, rel) {
			return nil
		}
		if len(paths) >= limit {
			hitLimit = true
			return errStopWalk
		}
		paths = append(paths, rel)
		return nil
	}); err != nil {
		return agent.ToolResult{}, err
	}
	// WalkDir is lexical already; sorting keeps the order stable if the walk
	// ever changes, and makes the output predictable to read.
	sort.Strings(paths)

	var out string
	switch {
	case len(paths) == 0:
		out = fmt.Sprintf("No files matching %q.", pattern)
	default:
		body := TruncateHead(strings.Join(paths, "\n"), MaxOutputLines, MaxOutputBytes)
		out = body.Content
		if body.Truncated {
			out += fmt.Sprintf("\n\n[Output truncated at %s. Narrow the pattern or raise limit.]",
				FormatSize(MaxOutputBytes))
		}
		unit := "files"
		if len(paths) == 1 {
			unit = "file"
		}
		footer := fmt.Sprintf("%d %s", len(paths), unit)
		if hitLimit {
			footer += fmt.Sprintf(" — stopped at the limit of %d, so results may continue", limit)
		}
		out += "\n\n" + footer
	}
	return agent.ToolResult{
		Content: []agent.Content{&agent.TextContent{Text: out}},
		Details: map[string]any{
			"pattern": pattern, "path": root, "count": len(paths), "hit_limit": hitLimit,
		},
	}, nil
}
