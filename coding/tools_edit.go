package coding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/ddombrow/malachi/agent"
)

const utf8BOM = "\uFEFF"

type edit struct {
	oldText, newText string
	replaceAll       bool
}

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
			"To rename or change every occurrence of a text in a file, set edits[].replaceAll instead of using sed or a script.",
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
							"replaceAll": map[string]any{
								"type":        "boolean",
								"description": "Replace every occurrence of oldText instead of requiring it to be unique",
							},
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
		all, _ := obj["replaceAll"].(bool)
		edits = append(edits, edit{o, n, all})
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
//
// An oldText that is not found exactly is retried ignoring whitespace and
// typography differences (see looseIndex); that match is used only if it is
// unique. notes says which edits matched that way, so the model knows its
// copy of the file was off.
func ApplyEdits(content string, edits []edit, path string) (out string, notes []string, err error) {
	type span struct {
		start, end int
		newText    string
	}
	multi := len(edits) > 1
	name := func(i int) string {
		if multi {
			return fmt.Sprintf("edits[%d]", i)
		}
		return "the text"
	}
	spans := make([]span, 0, len(edits))
	for i, e := range edits {
		old := normalizeLF(e.oldText)
		newText := normalizeLF(e.newText)
		if old == "" {
			if multi {
				return "", nil, fmt.Errorf("edits[%d].oldText must not be empty in %s.", i, path)
			}
			return "", nil, fmt.Errorf("oldText must not be empty in %s.", path)
		}
		n := strings.Count(content, old)
		switch {
		case n > 0 && e.replaceAll:
			for at := 0; ; {
				k := strings.Index(content[at:], old)
				if k < 0 {
					break
				}
				spans = append(spans, span{at + k, at + k + len(old), newText})
				at += k + len(old)
			}
			continue
		case n == 1:
			start := strings.Index(content, old)
			spans = append(spans, span{start, start + len(old), newText})
			continue
		case n > 1:
			return "", nil, fmt.Errorf("Found %d occurrences of %s in %s. Each oldText must be unique: add surrounding context, or set replaceAll to change every occurrence.", n, name(i), path)
		}
		if start, end, count := looseIndex(content, old); count == 1 {
			spans = append(spans, span{start, end, newText})
			notes = append(notes, fmt.Sprintf("%s matched only after ignoring whitespace differences", name(i)))
			continue
		} else if count > 1 {
			return "", nil, fmt.Errorf("Could not find %s exactly in %s, and ignoring whitespace it matches %d places. Copy oldText exactly from the file.", name(i), path, count)
		}
		msg := fmt.Sprintf("Could not find %s in %s. The oldText must match exactly including all whitespace and newlines.", name(i), path)
		if !multi {
			msg = fmt.Sprintf("Could not find the exact text in %s. The old text must match exactly including all whitespace and newlines.", path)
		}
		if near := nearestText(content, old); near != "" {
			msg += "\nThe closest text in the file is:\n" + near + "\nIf the file changed since you read it, read it again."
		}
		return "", nil, errors.New(msg)
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	for i := 1; i < len(spans); i++ {
		if spans[i].start < spans[i-1].end {
			return "", nil, fmt.Errorf("Edits must not overlap")
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
	out = b.String()
	if out == content {
		if multi {
			return "", nil, fmt.Errorf("No changes made to %s. The replacements produced identical content.", path)
		}
		return "", nil, fmt.Errorf("No changes made to %s. The replacement produced identical content. This might indicate an issue with special characters or the text not existing as expected.", path)
	}
	return out, notes, nil
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
		return agent.ToolResult{}, fmt.Errorf("Could not edit file: %s. File not found.%s", path, notFoundHint(cwd, path, opts.policy()))
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
	updated, notes, err := ApplyEdits(base, edits, path)
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
	text := fmt.Sprintf("Successfully replaced %d block(s) in %s.", len(edits), path)
	if len(notes) > 0 {
		text += " Note: " + strings.Join(notes, "; ") + "."
	}
	r := agent.TextResult(text)
	r.Details = map[string]any{"path": path, "edits": len(edits), "patch": patch, "first_changed_line": first}
	return r, nil
}
