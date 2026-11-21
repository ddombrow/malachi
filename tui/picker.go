package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// pickerMaxRows is how many choices a picker shows at once on a roomy
// terminal, the same height the input box may grow to.
const pickerMaxRows = inputMaxHeight

// pickerItem is one choice. Label is what is shown and filtered on; value
// is what choosing it hands back.
type pickerItem struct {
	label, detail, value string
}

// picker takes the input box's place while a bare command such as /thinking
// asks for a choice. Typing filters the list, the arrows move, enter chooses
// and esc puts the input box back. The typed form (/thinking high) still
// works without it.
type picker struct {
	title   string
	items   []pickerItem
	current string // the value in effect now, marked in the list
	filter  string
	cursor  int // index into shown()
	top     int // first row of shown() on screen
	loading bool
	note    string // shown under the title, e.g. why a live list is missing
	choose  func(value string) tea.Cmd
}

func newPicker(title, current string, items []pickerItem, choose func(string) tea.Cmd) *picker {
	p := &picker{title: title, current: current, items: items, choose: choose}
	p.focusCurrent()
	return p
}

// setItems fills a picker whose items arrived after it opened.
func (p *picker) setItems(items []pickerItem) {
	p.items, p.loading = items, false
	p.focusCurrent()
}

// focusCurrent puts the cursor on the choice in effect, so enter on an
// untouched picker changes nothing.
func (p *picker) focusCurrent() {
	p.cursor, p.top = 0, 0
	for i, it := range p.shown() {
		if it.value == p.current {
			p.cursor = i
			return
		}
	}
}

// shown is the items that match the filter, in their original order.
func (p *picker) shown() []pickerItem {
	if p.filter == "" {
		return p.items
	}
	f := strings.ToLower(p.filter)
	var out []pickerItem
	for _, it := range p.items {
		if strings.Contains(strings.ToLower(it.label), f) {
			out = append(out, it)
		}
	}
	return out
}

// key handles a key press while the picker is open. done reports that the
// picker should close; cmd is the choice's effect, if one was made.
func (p *picker) key(msg tea.KeyPressMsg) (cmd tea.Cmd, done bool) {
	n := len(p.shown())
	switch msg.String() {
	case "esc", "ctrl+c":
		return nil, true
	case "enter":
		if n == 0 {
			return nil, false
		}
		return p.choose(p.shown()[p.cursor].value), true
	case "up", "ctrl+p", "shift+tab":
		if n > 0 {
			p.cursor = (p.cursor - 1 + n) % n
		}
	case "down", "ctrl+n", "tab":
		if n > 0 {
			p.cursor = (p.cursor + 1) % n
		}
	case "pgup":
		p.cursor = max(0, p.cursor-pickerMaxRows)
	case "pgdown":
		p.cursor = max(0, min(n-1, p.cursor+pickerMaxRows))
	case "home":
		p.cursor = 0
	case "end":
		p.cursor = max(0, n-1)
	case "backspace":
		if p.filter != "" {
			r := []rune(p.filter)
			p.filter = string(r[:len(r)-1])
			p.cursor, p.top = 0, 0
		}
	case "ctrl+u":
		p.filter, p.cursor, p.top = "", 0, 0
	default:
		if msg.Text != "" {
			p.filter += msg.Text
			p.cursor, p.top = 0, 0
		}
	}
	return nil, false
}

// height is how many rows view will use given at most avail.
func (p *picker) height(avail int) int {
	return max(1, min(avail, 1+p.listRows()))
}

func (p *picker) listRows() int {
	if p.loading || len(p.shown()) == 0 {
		return 1
	}
	return min(pickerMaxRows, len(p.shown()))
}

// view renders the header and as many choices as fit in rows, scrolled to
// keep the cursor on screen. spin is the spinner frame shown while loading.
func (p *picker) view(st styles, spin string, width, rows int) string {
	rows = p.height(rows)
	head := st.accent.Render(p.title)
	if p.filter != "" {
		head += " " + p.filter
	}
	hint := "type to filter · ↑↓ move · enter choose · esc cancel"
	if p.note != "" {
		hint = p.note
	}
	shown := p.shown()
	// With a single row, the choice under the cursor matters more than the
	// hint: show it beside the title.
	if rows == 1 && !p.loading && len(shown) > 0 {
		return truncateWidth(head+" "+st.accent.Render("› "+shown[p.cursor].label), width)
	}
	head = truncateWidth(head+"  "+st.dim.Render(hint), width)
	lines := []string{head}
	switch {
	case p.loading:
		lines = append(lines, "  "+st.toolRun.Render(spin)+st.dim.Render(" loading…"))
	case len(shown) == 0:
		lines = append(lines, st.dim.Render("  no match"))
	default:
		list := rows - 1
		if p.cursor < p.top {
			p.top = p.cursor
		}
		if p.cursor >= p.top+list {
			p.top = p.cursor - list + 1
		}
		p.top = max(0, min(p.top, len(shown)-list))
		labelW := 0
		for _, it := range shown {
			labelW = max(labelW, lipgloss.Width(it.label))
		}
		for i := p.top; i < min(len(shown), p.top+list); i++ {
			it := shown[i]
			mark := "  "
			if it.value == p.current {
				mark = "● "
			}
			line := mark + it.label
			if it.detail != "" {
				line += strings.Repeat(" ", labelW-lipgloss.Width(it.label)+2) + it.detail
			}
			line = truncateWidth(line, width-2)
			if i == p.cursor {
				line = st.accent.Render("› " + line)
			} else {
				line = "  " + st.dim.Render(line)
			}
			lines = append(lines, line)
		}
	}
	return strings.Join(lines[:min(len(lines), rows)], "\n")
}
