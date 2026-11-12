package tui

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/ddombrow/malachi/codemode"
)

// scriptDetails reads a script tool result's details: the typed value while
// the session runs, a decoded map once it has been saved and reloaded.
func scriptDetails(v any) (codemode.Details, bool) {
	switch d := v.(type) {
	case codemode.Details:
		return d, true
	case *codemode.Details:
		return *d, d != nil
	case map[string]any:
		b, err := json.Marshal(d)
		if err != nil {
			return codemode.Details{}, false
		}
		var out codemode.Details
		if json.Unmarshal(b, &out) != nil || out.Calls == nil {
			return codemode.Details{}, false
		}
		return out, true
	}
	return codemode.Details{}, false
}

// scriptTally summarizes a script's calls: how many of each tool, most
// used first, and how many failed.
func scriptTally(d codemode.Details) string {
	counts := map[string]int{}
	failed := 0
	for _, c := range d.Calls {
		counts[c.Name]++
		if c.Status == codemode.StatusError {
			failed++
		}
	}
	names := make([]string, 0, len(counts))
	for n := range counts {
		names = append(names, n)
	}
	slices.SortFunc(names, func(a, b string) int {
		if counts[a] != counts[b] {
			return counts[b] - counts[a]
		}
		return strings.Compare(a, b)
	})
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = n
		if counts[n] > 1 {
			parts[i] += fmt.Sprintf(" ×%d", counts[n])
		}
	}
	unit := "calls"
	if len(d.Calls) == 1 {
		unit = "call"
	}
	s := fmt.Sprintf("%d %s: %s", len(d.Calls), unit, strings.Join(parts, ", "))
	if failed > 0 {
		s += fmt.Sprintf(" · %d failed", failed)
	}
	return s
}

// scriptCalls lists a running script's latest calls, one per line, each
// marked running (with spin), done or failed.
func (r *renderer) scriptCalls(d codemode.Details, spin string, keep int) string {
	calls := d.Calls
	var lines []string
	if len(calls) > keep {
		lines = append(lines, r.st.dim.Render(fmt.Sprintf("… %d earlier calls", len(calls)-keep)))
		calls = calls[len(calls)-keep:]
	}
	for _, c := range calls {
		args, _ := c.Args.(map[string]any)
		text := truncateWidth(r.summary(c.Name, args), r.width-8)
		switch c.Status {
		case codemode.StatusRunning:
			lines = append(lines, r.st.toolRun.Render(spin)+" "+text)
		case codemode.StatusError:
			lines = append(lines, r.st.toolErr.Render("✗")+" "+r.st.dim.Render(text))
		default:
			lines = append(lines, r.st.toolOK.Render("✓")+" "+r.st.dim.Render(text))
		}
	}
	return strings.Join(lines, "\n")
}
