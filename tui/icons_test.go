package tui

import (
	"testing"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
	"golang.org/x/text/width"
)

// Every icon is as wide in every terminal as lipgloss measures it: plain
// narrow glyphs, or one code point that is wide on its own. A variation
// selector makes the width the terminal's choice, and a row one column off
// pulls the sidebar's border out of line.
func TestIconsHaveOneWidth(t *testing.T) {
	for set, icons := range iconSets {
		for kind, glyph := range icons {
			for _, r := range glyph {
				if r == '️' || r == '︎' || r == '‍' {
					t.Errorf("%s/%s %q: a variation selector or joiner leaves its width to the terminal", set, kind, glyph)
				}
			}
			if utf8.RuneCountInString(glyph) == 1 {
				r, _ := utf8.DecodeRuneInString(glyph)
				wide := width.LookupRune(r).Kind() == width.EastAsianWide
				if lw := lipgloss.Width(glyph); (lw == 2) != wide {
					t.Errorf("%s/%s %q: lipgloss measures %d cells, East Asian Width wide=%v", set, kind, glyph, lw, wide)
				}
			}
		}
	}
}
