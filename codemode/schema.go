package codemode

import (
	"sort"
	"strings"

	"github.com/ddombrow/malachi/agent"
)

// detailsSummary lists, one line per tool, the fields a call's details
// hold, so a script can use them without asking describeTool first. Tools
// without an output schema are left out.
func detailsSummary(tools []*agent.Tool) string {
	var lines []string
	for _, t := range tools {
		if f := fields(t.OutputSchema, 2); f != "" {
			lines = append(lines, t.Name+": "+f)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "Each result's details, by tool (? marks a field that may be missing; describeTool(name).details is the full schema with what each field means):\n" +
		strings.Join(lines, "\n")
}

// fields renders an object schema as its field names, `{path, edits,
// truncation?: {truncated, …}}`, descending depth levels into objects and
// arrays of objects. It is "" for a schema that is not an object with
// properties.
func fields(schema map[string]any, depth int) string {
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		return ""
	}
	required := map[string]bool{}
	switch r := schema["required"].(type) {
	case []string:
		for _, n := range r {
			required[n] = true
		}
	case []any:
		for _, n := range r {
			if s, ok := n.(string); ok {
				required[s] = true
			}
		}
	}
	names := make([]string, 0, len(props))
	for n := range props {
		names = append(names, n)
	}
	// Required fields first, each group in name order, so the line reads
	// the same on every request and the prompt cache holds.
	sort.Slice(names, func(i, j int) bool {
		if required[names[i]] != required[names[j]] {
			return required[names[i]]
		}
		return names[i] < names[j]
	})
	parts := make([]string, len(names))
	for i, n := range names {
		part := n
		if !required[n] {
			part += "?"
		}
		if depth > 0 {
			if sub, ok := props[n].(map[string]any); ok {
				if inner := fields(sub, depth-1); inner != "" {
					part += ": " + inner
				} else if items, ok := sub["items"].(map[string]any); ok {
					if inner := fields(items, depth-1); inner != "" {
						part += ": [" + inner + "]"
					}
				}
			}
		}
		parts[i] = part
	}
	return "{" + strings.Join(parts, ", ") + "}"
}
