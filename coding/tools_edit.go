package coding

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/ddombrow/malachi/agent"
)

const utf8BOM = "\uFEFF"

type edit struct{ oldText, newText string }

// NewEditTool returns the edit tool rooted at cwd.
func NewEditTool(cwd string, opts ToolOptions) *agent.Tool {
	return &agent.Tool{
		Name:  "edit",
		Label: "Edit",
		Description: "Edit a single file using exact text replacement. Every edits[].oldText must match " +
			"a unique, non-overlapping region of the original file. If two changes affect the " +
			"same block or nearby lines, merge them into one edit instead of emitting overlapping " +
			"edits. Do not include large unchanged regions just to connect distant changes.",
		PromptSnippet: "Make precise file edits with exact text replacement, including multiple disjoint edits in one call",
		PromptGuidelines: []string{
			"Use edit for precise changes (edits[].oldText must match exactly)",
			"When changing multiple separate locations in one file, use one edit call with multiple entries in edits[] instead of multiple edit calls",
			"Each edits[].oldText is matched against the original file, not after earlier edits are applied. Do not emit overlapping or nested edits. Merge nearby changes into one edit.",
			"Keep edits[].oldText as small as possible while still being unique in the file. Do not pad with large unchanged regions.",
		},
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string", "description": "Path to the file to edit"},
				"edits": map[string]any{
					"type":        "array",
					"description": "One or more targeted replacements.",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"oldText": map[string]any{"type": "string"},
							"newText": map[string]any{"type": "string"},
						},
						"required":             []string{"oldText", "newText"},
						"additionalProperties": false,
					},
				},
			},
			"required":             []string{"path", "edits"},
			"additionalProperties": false,
		},
		Execute: func(_ context.Context, _ string, args map[string]any, _ func(agent.ToolResult)) (agent.ToolResult, error) {
			return executeEdit(cwd, opts, args)
		},
	}
}

// parseEdits accepts the canonical edits list, a JSON-string edits value,
// and legacy top-level oldText/newText.
func parseEdits(args map[string]any) ([]edit, error) {
	value := args["edits"]
	if s, ok := value.(string); ok {
		var parsed []any
		if json.Unmarshal([]byte(s), &parsed) == nil {
			value = parsed
		}
	}
	list, _ := value.([]any)
	if o, ok := args["oldText"].(string); ok {
		if n, ok := args["newText"].(string); ok {
			list = append(list, map[string]any{"oldText": o, "newText": n})
		}
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("Edit tool input is invalid. edits must contain at least one replacement.")
	}
	edits := make([]edit, 0, len(list))
	for i, item := range list {
		obj, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("edits[%d] must be an object", i)
		}
		o, ok1 := obj["oldText"].(string)
		n, ok2 := obj["newText"].(string)
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("edits[%d].oldText and edits[%d].newText must be strings", i, i)
		}
		edits = append(edits, edit{o, n})
	}
	return edits, nil
}

func normalizeLF(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
}

func detectLineEnding(s string) string {
	crlf, lf := strings.Index(s, "\r\n"), strings.Index(s, "\n")
	if lf == -1 || crlf == -1 || crlf > lf {
		return "\n"
	}
	return "\r\n"
}

// ApplyEdits applies exact, unique, non-overlapping replacements to
// LF-normalized content. All edits are validated before any is applied.
func ApplyEdits(content string, edits []edit, path string) (string, error) {
	type span struct {
		start, end int
		newText    string
	}
	multi := len(edits) > 1
	spans := make([]span, 0, len(edits))
	for i, e := range edits {
		old := normalizeLF(e.oldText)
		if old == "" {
			if multi {
				return "", fmt.Errorf("edits[%d].oldText must not be empty in %s.", i, path)
			}
			return "", fmt.Errorf("oldText must not be empty in %s.", path)
		}
		switch n := strings.Count(content, old); {
		case n == 0 && multi:
			return "", fmt.Errorf("Could not find edits[%d] in %s. The oldText must match exactly including all whitespace and newlines.", i, path)
		case n == 0:
			return "", fmt.Errorf("Could not find the exact text in %s. The old text must match exactly including all whitespace and newlines.", path)
		case n > 1 && multi:
			return "", fmt.Errorf("Found %d occurrences of edits[%d] in %s. Each oldText must be unique. Please provide more context to make it unique.", n, i, path)
		case n > 1:
			return "", fmt.Errorf("Found %d occurrences of the text in %s. The text must be unique. Please provide more context to make it unique.", n, path)
		}
		start := strings.Index(content, old)
		spans = append(spans, span{start, start + len(old), normalizeLF(e.newText)})
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	for i := 1; i < len(spans); i++ {
		if spans[i].start < spans[i-1].end {
			return "", fmt.Errorf("Edits must not overlap")
		}
	}
	var b strings.Builder
	prev := 0
	for _, s := range spans {
		b.WriteString(content[prev:s.start])
		b.WriteString(s.newText)
		prev = s.end
	}
	b.WriteString(content[prev:])
	out := b.String()
	if out == content {
		if multi {
			return "", fmt.Errorf("No changes made to %s. The replacements produced identical content.", path)
		}
		return "", fmt.Errorf("No changes made to %s. The replacement produced identical content. This might indicate an issue with special characters or the text not existing as expected.", path)
	}
	return out, nil
}

func executeEdit(cwd string, opts ToolOptions, args map[string]any) (agent.ToolResult, error) {
	_, path, err := pathArg(args, cwd)
	if err != nil {
		return agent.ToolResult{}, err
	}
	if err := opts.policy().CheckWrite(path); err != nil {
		return agent.ToolResult{}, err
	}
	edits, err := parseEdits(args)
	if err != nil {
		return agent.ToolResult{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return agent.ToolResult{}, fmt.Errorf("Could not edit file: %s. File not found.", path)
	}
	if info.IsDir() {
		return agent.ToolResult{}, fmt.Errorf("Could not edit file: %s. Path is a directory.", path)
	}

	defer lockFile(path)()
	// Read and write through one descriptor that OpenWrite has checked, so
	// nothing can swap the file in between.
	f, err := opts.policy().OpenWrite(path, os.O_RDWR, 0)
	if err != nil {
		return agent.ToolResult{}, err
	}
	defer f.Close()
	raw, err := io.ReadAll(f)
	if err != nil {
		return agent.ToolResult{}, err
	}
	content := string(raw)
	bom := ""
	if strings.HasPrefix(content, utf8BOM) {
		bom, content = utf8BOM, content[len(utf8BOM):]
	}
	ending := detectLineEnding(content)
	base := normalizeLF(content)
	updated, err := ApplyEdits(base, edits, path)
	if err != nil {
		return agent.ToolResult{}, err
	}
	final := updated
	if ending == "\r\n" {
		final = strings.ReplaceAll(updated, "\n", "\r\n")
	}
	if err := f.Truncate(0); err != nil {
		return agent.ToolResult{}, err
	}
	if _, err := f.WriteAt([]byte(bom+final), 0); err != nil {
		return agent.ToolResult{}, err
	}
	if err := f.Close(); err != nil {
		return agent.ToolResult{}, err
	}
	patch, first := UnifiedDiff(path, base, updated)
	r := agent.TextResult(fmt.Sprintf("Successfully replaced %d block(s) in %s.", len(edits), path))
	r.Details = map[string]any{"path": path, "edits": len(edits), "patch": patch, "first_changed_line": first}
	return r, nil
}
