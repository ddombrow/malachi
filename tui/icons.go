package tui

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// Icon kinds: one per kind of scrollback item.
const (
	iconUser        = "user"
	iconReply       = "reply"
	iconThinking    = "thinking"
	iconRead        = "read"
	iconEdit        = "edit"
	iconWrite       = "write"
	iconBash        = "bash"
	iconTool        = "tool" // any other tool
	iconError       = "error"
	iconCancelled   = "cancelled"
	iconTimeout     = "timeout"
	iconCompacted   = "compacted"
	iconInterrupted = "interrupted"
)

// Icon sets, chosen with "icons" in settings.json. Emoji are all
// default-presentation (no U+FE0F variation selector) so terminals agree they
// are two cells wide and gutters line up.
var iconSets = map[string]map[string]string{
	"emoji": {
		iconUser:        "❯",
		iconReply:       "💬",
		iconThinking:    "💭",
		iconRead:        "📖",
		iconEdit:        "📝",
		iconWrite:       "📄",
		iconBash:        "💻",
		iconTool:        "🔧",
		iconError:       "❌",
		iconCancelled:   "🛑",
		iconTimeout:     "⌛",
		iconCompacted:   "📦",
		iconInterrupted: "🛑",
	},
	// "dots" mimics Claude Code: one glyph, colored by status.
	"dots": {
		iconUser:        "❯",
		iconReply:       "⏺",
		iconThinking:    "∴",
		iconRead:        "⏺",
		iconEdit:        "⏺",
		iconWrite:       "⏺",
		iconBash:        "⏺",
		iconTool:        "⏺",
		iconError:       "⏺",
		iconCancelled:   "⏺",
		iconTimeout:     "⏺",
		iconCompacted:   "⏺",
		iconInterrupted: "⏺",
	},
}

func iconSet(name string) map[string]string {
	if set, ok := iconSets[name]; ok {
		return set
	}
	return iconSets["emoji"]
}

// toolIcon picks the icon kind for a tool name.
func toolIcon(name string) string {
	switch name {
	case "read", "edit", "write", "bash":
		return name
	}
	return iconTool
}

// gutter prefixes the first line of body with the icon and indents the rest
// to align under the first line's text. The icon is colored with style
// (which matters for single-glyph sets; emoji keep their own colors).
func (r *renderer) gutter(kind string, style lipgloss.Style, body string) string {
	glyph := r.icons[kind]
	pad := strings.Repeat(" ", lipgloss.Width(glyph)+1)
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		if i == 0 {
			lines[i] = style.Render(glyph) + " " + l
		} else if l != "" {
			lines[i] = pad + l
		}
	}
	return strings.Join(lines, "\n")
}

// gutterWidth is how far gutter() indents continuation lines for kind.
func (r *renderer) gutterWidth(kind string) int {
	return lipgloss.Width(r.icons[kind]) + 1
}
