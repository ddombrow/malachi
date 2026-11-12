package tui

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// Icon kinds: one per kind of transcript item.
const (
	iconUser      = "user"
	iconReply     = "reply"
	iconThinking  = "thinking"
	iconRead      = "read"
	iconGrep      = "grep"
	iconGlob      = "glob"
	iconEdit      = "edit"
	iconWrite     = "write"
	iconBash      = "bash"
	iconFetch     = "fetch"
	iconScript    = "script"
	iconTool      = "tool" // any other tool
	iconError     = "error"
	iconCancelled = "cancelled"
	iconTimeout   = "timeout"
	iconCompacted = "compacted"
	// iconTrimmed marks mechanical tool-output trimming, which is not
	// compaction and now reads differently to say so.
	iconTrimmed     = "trimmed"
	iconInterrupted = "interrupted"
)

// Icon sets, chosen with "icons" in settings.json. Emoji icons are single
// code points that are wide on their own (East Asian Width W), so every
// terminal draws them two cells, as lipgloss measures them. An emoji that
// needs a U+FE0F variation selector to be wide (🗂️, ✏️, ✂️) is not: lipgloss
// counts two cells, a terminal ignoring the selector draws one, and the row
// comes out a column short, pulling the sidebar's border out of line.
var iconSets = map[string]map[string]string{
	"emoji": {
		iconUser:        "❯",
		iconReply:       "💬",
		iconThinking:    "💭",
		iconRead:        "👀",
		iconGrep:        "🔍",
		iconGlob:        "📂",
		iconEdit:        "🔨",
		iconWrite:       "📝",
		iconBash:        ">_",
		iconFetch:       "🌐",
		iconScript:      "📜",
		iconTool:        "🔧",
		iconError:       "❌",
		iconCancelled:   "🛑",
		iconTimeout:     "⌛",
		iconCompacted:   "📦",
		iconTrimmed:     "🧹",
		iconInterrupted: "🛑",
	},
	// "dots" mimics Claude Code: one glyph, colored by status.
	"dots": {
		iconUser:        "❯",
		iconReply:       "⏺",
		iconThinking:    "∴",
		iconRead:        "⏺",
		iconGrep:        "⏺",
		iconGlob:        "⏺",
		iconEdit:        "⏺",
		iconWrite:       "⏺",
		iconBash:        "⏺",
		iconFetch:       "⏺",
		iconScript:      "⏺",
		iconTool:        "⏺",
		iconError:       "⏺",
		iconCancelled:   "⏺",
		iconTimeout:     "⏺",
		iconCompacted:   "⏺",
		iconTrimmed:     "⏺",
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
	case "read", "edit", "write", "bash", "grep", "glob", "fetch", "script":
		return name
	}
	return iconTool
}

// gutter prefixes the first line of body with the icon and indents the rest
// to align under the first line's text. The icon is colored with style
// (which matters for single-glyph sets; emoji keep their own colors).
const iconGap = "  "

// iconGapFor is the gap after an icon: the same for every kind, now that
// every icon is as wide as it measures.
func iconGapFor(string) string { return iconGap }

func (r *renderer) gutter(kind string, style lipgloss.Style, body string) string {
	glyph := r.icons[kind]
	gap := iconGapFor(kind)
	pad := strings.Repeat(" ", lipgloss.Width(glyph)+lipgloss.Width(gap))
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		if i == 0 {
			lines[i] = style.Render(glyph) + gap + l
		} else if l != "" {
			lines[i] = pad + l
		}
	}
	return strings.Join(lines, "\n")
}

// gutterWidth is how far gutter() indents continuation lines for kind.
func (r *renderer) gutterWidth(kind string) int {
	return lipgloss.Width(r.icons[kind]) + lipgloss.Width(iconGapFor(kind))
}
