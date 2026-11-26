package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

const mdDoc = "# Title\n\nSome **bold** and *italic* text with `code`, a [link](http://x.y) and https://auto.link, long enough to wrap.\n\n" +
	"- one\n- **two**\n  - nested `x`\n- [x] done\n\n3. three\n4. four\n\n> a quote long enough to wrap across lines in the view\n\n" +
	"```go\nfunc main() {\n\tfmt.Println(\"hi\")\n\n\t// done\n}\n```\n\n```diff\n-old\n+new\n```\n\n```\nbare fence\n```\n\n" +
	"| name | value | note |\n|:-----|------:|------|\n| a | 1 | short |\n| bbbb | 22 | a longer note that needs wrapping in its cell |\n\n---\n\nend ~~gone~~."

// A reply drawn token by token must end up exactly as the finished message
// is drawn, or it jumps when it lands.
func TestMarkdownStreamMatchesWhole(t *testing.T) {
	r := newRenderer(50, true, "", "")
	want := r.markdown(mdDoc)
	for i := 1; i < len(mdDoc); i++ {
		r.markdownStream(mdDoc[:i])
	}
	if got := r.markdownStream(mdDoc); got != want {
		t.Fatalf("streamed:\n%s\n\nwhole:\n%s", got, want)
	}
	if !strings.HasPrefix(mdDoc, r.stream.src) || len(r.stream.src) < len(mdDoc)/2 {
		t.Fatalf("finished blocks were not cached: %q", r.stream.src)
	}
}

// A code block streamed line by line is lexed a line at a time, which must
// color it as lexing it whole does.
func TestMarkdownStreamCode(t *testing.T) {
	src := "```go\n// c\nfunc f(x int) string {\n\treturn fmt.Sprint(x)\n}\n```"
	r := newRenderer(60, true, "", "")
	want := r.markdown(src)
	for i := 1; i < len(src); i++ {
		r.markdownStream(src[:i])
	}
	if got := r.markdownStream(src); got != want {
		t.Fatalf("streamed:\n%q\nwhole:\n%q", got, want)
	}
}

// A new reply must not reuse the last one's blocks.
func TestMarkdownStreamResets(t *testing.T) {
	r := newRenderer(50, true, "", "")
	r.markdownStream("first\n\nsecond\n\nthird")
	if got := ansi.Strip(r.markdownStream("other")); got != "other" {
		t.Fatalf("got %q", got)
	}
}

func TestMarkdownFitsWidth(t *testing.T) {
	r := newRenderer(50, true, "", "")
	out := r.markdown(mdDoc)
	panel := strings.TrimSuffix(r.md.codeBg.render("x"), "x\x1b[m") // its opening escape
	for _, l := range strings.Split(out, "\n") {
		if w := ansi.StringWidth(l); w > r.mdWidth() {
			t.Errorf("line %d wide (max %d): %q", w, r.mdWidth(), ansi.Strip(l))
		}
		// A code block's panel is padded out with tinted spaces.
		if strings.HasSuffix(ansi.Strip(l), " ") && !strings.HasPrefix(l, panel) {
			t.Errorf("trailing blank: %q", ansi.Strip(l))
		}
	}
	plain := ansi.Strip(out)
	if !strings.Contains(plain, "│ a    │     1 │ short                      │\n├──────┼───────┼") {
		t.Errorf("no rule between body rows:\n%s", plain)
	}
	for _, want := range []string{"Title", "• one", "  ◦ nested x", "[x] done", "3. three", "│ a quote", "      fmt.Println", "│ name │ value │ note", "(http://x.y)"} {
		if !strings.Contains(plain, want) {
			t.Errorf("missing %q in:\n%s", want, plain)
		}
	}
}

func TestCodeBlockHighlights(t *testing.T) {
	r := newRenderer(60, true, "", "")
	plain := r.markdown("```nosuchlang\nreturn 1\n```")
	goCode := r.markdown("```go\nreturn 1\n```")
	if ansi.Strip(goCode) != ansi.Strip(plain) {
		t.Fatalf("text differs: %q vs %q", ansi.Strip(goCode), ansi.Strip(plain))
	}
	if !strings.Contains(goCode, r.md.syn.keyword.on(r.md.codeBg).render("return")) {
		t.Fatalf("keyword not colored: %q", goCode)
	}
	if strings.Contains(plain, r.md.syn.keyword.on(r.md.codeBg).render("return")) {
		t.Fatalf("unknown language colored: %q", plain)
	}
}
