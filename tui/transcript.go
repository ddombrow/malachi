package tui

import "strings"

// block is one transcript item. It keeps a render function rather than
// fixed text so the whole transcript re-wraps when the terminal is resized
// or the theme changes.
type block struct {
	render func(r *renderer) string
}

// transcript is the conversation shown in the viewport. Rendering is cached
// per renderer, and appending only renders the new blocks, so long sessions
// stay cheap to redraw.
type transcript struct {
	blocks []block

	cacheFor *renderer
	cacheN   int
	cache    strings.Builder
}

func (t *transcript) add(f func(r *renderer) string) {
	t.blocks = append(t.blocks, block{render: f})
}

func (t *transcript) reset() {
	t.blocks = nil
	t.cacheFor, t.cacheN = nil, 0
	t.cache.Reset()
}

// text renders the transcript with r, reusing previous work when r has not
// changed since the last call.
func (t *transcript) text(r *renderer) string {
	if t.cacheFor != r {
		t.cacheFor, t.cacheN = r, 0
		t.cache.Reset()
	}
	for ; t.cacheN < len(t.blocks); t.cacheN++ {
		out := t.blocks[t.cacheN].render(r)
		if strings.TrimSpace(out) == "" {
			continue // e.g. an assistant turn that was only tool calls
		}
		// Blocks begin with a newline (item()); one more makes the blank
		// line that separates items.
		if t.cache.Len() > 0 {
			t.cache.WriteByte('\n')
		}
		t.cache.WriteString(out)
	}
	return t.cache.String()
}
