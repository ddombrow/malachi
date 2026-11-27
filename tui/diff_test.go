package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

const gitPatch = `diff --git a/main.go b/main.go
index 1111111..2222222 100644
--- a/main.go
+++ b/main.go
@@ -10,4 +10,4 @@ func main() {
 	a := 1
-	fmt.Println("hello", a)
+	fmt.Println("goodbye", a)
-- not a header, a removed line
 }
@@ -40 +40,2 @@
 x
+y
diff --git a/old.txt b/new.txt
similarity index 90%
rename from old.txt
rename to new.txt
diff --git a/added.md b/added.md
new file mode 100644
--- /dev/null
+++ b/added.md
@@ -0,0 +1 @@
+# title
\ No newline at end of file
diff --git a/logo.png b/logo.png
Binary files a/logo.png and b/logo.png differ
`

func TestParsePatch(t *testing.T) {
	files := parsePatch(gitPatch)
	if len(files) != 4 {
		t.Fatalf("files: %+v", files)
	}
	m := files[0]
	if m.oldPath != "main.go" || m.newPath != "main.go" || len(m.hunks) != 2 {
		t.Fatalf("main.go: %+v", m)
	}
	h := m.hunks[0]
	if h.section != "func main() {" || len(h.lines) != 5 {
		t.Fatalf("hunk: %+v", h)
	}
	// The counts, not the "--" prefix, decide that line is a removal.
	if l := h.lines[3]; l.kind != '-' || l.text != "- not a header, a removed line" || l.old != 12 {
		t.Fatalf("removed line: %+v", l)
	}
	if l := h.lines[4]; l.kind != ' ' || l.old != 13 || l.new != 12 {
		t.Fatalf("context numbers: %+v", l)
	}
	if h := m.hunks[1]; h.oldLines != 1 || h.newLines != 2 || h.lines[1].new != 41 {
		t.Fatalf("short header: %+v", h)
	}
	if f := files[1]; f.oldPath != "old.txt" || f.newPath != "new.txt" || fileHeading(&f) != "old.txt → new.txt" {
		t.Fatalf("rename: %+v", f)
	}
	if f := files[2]; fileHeading(&f) != "added.md (new)" || len(f.hunks[0].lines) != 1 {
		t.Fatalf("new file: %+v", f)
	}
	if f := files[3]; !f.binary {
		t.Fatalf("binary: %+v", f)
	}
}

func TestParseEditPatch(t *testing.T) {
	files := parsePatch("--- a/f.go\n+++ a/f.go\n@@ -1,2 +1,2 @@\n x\n-y\n+z\n")
	// Without a diff --git header, a/ is part of the name.
	if len(files) != 1 || files[0].path() != "a/f.go" || len(files[0].hunks[0].lines) != 3 {
		t.Fatalf("%+v", files)
	}
}

func TestDiffView(t *testing.T) {
	r := newRenderer(60, true, "", "")
	out, more := r.diffView(gitPatch, diffOpts{width: 60, files: true})
	if more != 0 {
		t.Fatalf("more = %d", more)
	}
	plain := ansi.Strip(out)
	for _, want := range []string{
		"main.go",
		"@@ -10,4 +10,4 @@ func main() {",
		`11 -     fmt.Println("hello", a)`, // tabs expanded
		`11 +     fmt.Println("goodbye", a)`,
		"12 - - not a header",
		"old.txt → new.txt",
		" 1 + # title",
		"binary file",
	} {
		if !strings.Contains(plain, want) {
			t.Errorf("missing %q in:\n%s", want, plain)
		}
	}
	for _, row := range strings.Split(out, "\n") {
		if w := ansi.StringWidth(row); w > 60 {
			t.Errorf("row %d wide: %q", w, ansi.Strip(row))
		}
	}
}

func TestDiffMarksChangedWords(t *testing.T) {
	r := newRenderer(60, true, "", "")
	h := parsePatch(gitPatch)[0].hunks[0]
	cells := r.hunkCells(&h, "main.go")
	changed := func(cs []cell, bg *mdStyle) string {
		var b strings.Builder
		for _, c := range cs {
			if c.st != nil && c.st.bg == bg.bg {
				b.WriteString(c.g)
			}
		}
		return b.String()
	}
	if got := changed(cells[1], &r.ds.delEmph); got != "hello" {
		t.Errorf("removed words = %q", got)
	}
	if got := changed(cells[2], &r.ds.addEmph); got != "goodbye" {
		t.Errorf("added words = %q", got)
	}
	// A line with nothing in common gets no marks.
	h = parsePatch("@@ -1 +1 @@\n-alpha beta\n+gamma delta\n")[0].hunks[0]
	cells = r.hunkCells(&h, "f.txt")
	if got := changed(cells[0], &r.ds.delEmph) + changed(cells[1], &r.ds.addEmph); got != "" {
		t.Errorf("unrelated lines marked: %q", got)
	}
}

func TestDiffViewLimits(t *testing.T) {
	r := newRenderer(40, true, "", "")
	patch := "@@ -1,3 +1,3 @@\n-" + strings.Repeat("x", 100) + "\n+y\n a\n b\n"
	out, more := r.diffView(patch, diffOpts{width: 30, maxRows: 2})
	rows := strings.Split(ansi.Strip(out), "\n")
	if len(rows) != 2 || more != 3 {
		t.Fatalf("rows %d, more %d:\n%s", len(rows), more, out)
	}
	if !strings.HasSuffix(strings.TrimRight(rows[1], " "), "…") || ansi.StringWidth(rows[1]) != 30 {
		t.Fatalf("cut row: %q", rows[1])
	}
	out, _ = r.diffView(patch, diffOpts{width: 30, wrap: true})
	if rows := strings.Split(ansi.Strip(out), "\n"); len(rows) != 1+4+3 { // header, 100 x's in rows of 26, y, a, b
		t.Fatalf("wrapped rows %d:\n%s", len(rows), ansi.Strip(out))
	}
}
