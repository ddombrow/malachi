package codemode

import (
	"fmt"
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

// Advertise returns copies of tools, for offering to the model next to the
// script tool, whose descriptions end by saying how a script calls each one
// and what its details hold: the model picks a tool by its description, so
// that is where it learns a script could make the call. The script tool
// itself should keep the originals, and Options.Advertised set.
func Advertise(tools []*agent.Tool) []*agent.Tool {
	out := make([]*agent.Tool, len(tools))
	for i, t := range tools {
		c := *t
		note := fmt.Sprintf("In a script: `await tools.%s({...})` gives {text, details, isError}", t.Name)
		if f := fields(t.OutputSchema, 1); f != "" {
			note += ", details " + f
		}
		c.Description = strings.TrimRight(t.Description, " ") + "\n\n" + note + "."
		out[i] = &c
	}
	return out
}

// toolSection is all a model needs to call t from a script when t is not
// offered to it directly: its description and advice, its arguments, and
// what its details hold.
func toolSection(t *agent.Tool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "### tools.%s(args)\n%s", t.Name, strings.TrimSpace(t.Description))
	for _, g := range t.PromptGuidelines {
		b.WriteString("\n- " + g)
	}
	props, _ := t.Parameters["properties"].(map[string]any)
	if len(props) > 0 {
		required := map[string]bool{}
		switch r := t.Parameters["required"].(type) {
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
		sort.Slice(names, func(i, j int) bool {
			if required[names[i]] != required[names[j]] {
				return required[names[i]]
			}
			return names[i] < names[j]
		})
		b.WriteString("\nArguments:")
		for _, n := range names {
			p, _ := props[n].(map[string]any)
			kind, _ := p["type"].(string)
			if !required[n] {
				kind += ", optional"
			}
			fmt.Fprintf(&b, "\n  %s (%s)", n, kind)
			if enum, ok := p["enum"].([]string); ok {
				fmt.Fprintf(&b, " one of %s", strings.Join(enum, ", "))
			}
			if d, _ := p["description"].(string); d != "" {
				b.WriteString(": " + d)
			}
		}
	}
	if f := fields(t.OutputSchema, 1); f != "" {
		b.WriteString("\nDetails: " + f)
	}
	return b.String()
}
