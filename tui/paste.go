package tui

import "strings"

// pastedLineCount counts non-terminal lines in pasted text. A final newline
// ends the last line; it does not create another pasted line.
func pastedLineCount(text string) int {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	if text == "" {
		return 0
	}
	lines := strings.Count(text, "\n")
	if !strings.HasSuffix(text, "\n") {
		lines++
	}
	return lines
}

// expandPastes swaps visible markers for their original contents when the
// prompt is sent. Pasted bodies stay out of the editor, where the textarea's
// visual-height limit would otherwise truncate them.
func (m *model) expandPastes(text string) string {
	for _, paste := range m.pastes {
		text = strings.Replace(text, paste.marker, paste.content, 1)
	}
	return text
}
