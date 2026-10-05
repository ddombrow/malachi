package coding

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeIn writes a file, creating any parent directories it needs.
func writeIn(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	return write(t, dir, name, content)
}

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"*.go", "main.go", true},
		{"*.go", "cmd/main.go", false},   // no slash: base name only
		{"**/*.go", "cmd/main.go", true}, // ** matches any depth
		{"**/*.go", "main.go", true},     // including zero segments
		{"**/*.go", "cmd/pkg/main.go", true},
		{"src/**/*.ts", "src/a/b.ts", true},
		{"src/**/*.ts", "src/a/b.js", false},
		{"src/*.go", "src/a/b.go", false},
		{"a/**/b", "a/b", true},
		{"a/**/b", "a/x/y/b", true},
		{"**", "anything/at/all", true},
		{"*_test.go", "pkg/tool_test.go", false},
		{"**/*_test.go", "pkg/tool_test.go", true},
	}
	for _, c := range cases {
		if got := matchGlob(c.pattern, c.name); got != c.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}

func TestMatchFilterAnchorsSlashedPatterns(t *testing.T) {
	cases := []struct {
		pattern, rel string
		want         bool
	}{
		{"*.go", "pkg/main.go", true},     // no slash matches at any depth
		{"pkg/*.go", "pkg/main.go", true}, // slash anchors to the root
		{"pkg/*.go", "other/pkg/main.go", false},
		{"", "anything", true}, // no filter matches everything
	}
	for _, c := range cases {
		if got := matchFilter(c.pattern, c.rel); got != c.want {
			t.Errorf("matchFilter(%q, %q) = %v, want %v", c.pattern, c.rel, got, c.want)
		}
	}
}

func TestIgnores(t *testing.T) {
	ig := &ignores{}
	for _, line := range []string{
		"# a comment", "", "  ", "*.log", "build/", "/rooted.txt", "docs/**/draft.md", "!keep.log",
	} {
		if r, ok := parseIgnoreRule(line); ok {
			ig.rules = append(ig.rules, r)
		}
	}
	cases := []struct {
		rel    string
		isDir  bool
		ignore bool
	}{
		{"a.log", false, true},
		{"deep/b.log", false, true},
		{"keep.log", false, false}, // negated by a later rule
		{"build", true, true},
		{"build", false, false}, // dirOnly
		{"build/out.bin", false, false},
		{"rooted.txt", true, true},
		{"sub/rooted.txt", false, false}, // anchored: only at the root
		{"docs/x/draft.md", false, true},
		{"main.go", false, false},
	}
	for _, c := range cases {
		if got := ig.match(c.rel, c.isDir); got != c.ignore {
			t.Errorf("match(%q, dir=%v) = %v, want %v", c.rel, c.isDir, got, c.ignore)
		}
	}
}

func TestGrepFindsAndReportsScope(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "main.go", "package main\n\nfunc main() {\n\thello()\n}\n")
	write(t, dir, "util.go", "package main\n\nfunc hello() {}\n")
	write(t, dir, "main_test.go", "func TestHello(t *testing.T) { hello() }\n")

	r, err := run(t, NewGrepTool(dir), map[string]any{"pattern": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	out := r.Text()
	for _, want := range []string{"main.go:4:", "util.go:3:", "main_test.go:1:"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output:\n%s", want, out)
		}
	}
	// The footer must state the scope, so an empty result is distinguishable
	// from a search that quietly skipped files.
	if !strings.Contains(out, "3 matches in 3 of 3 searched files") {
		t.Errorf("missing scope footer:\n%s", out)
	}
}

func TestGrepNoMatchIsExplicit(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.go", "package a\n")
	r, err := run(t, NewGrepTool(dir), map[string]any{"pattern": "nonexistent"})
	if err != nil {
		t.Fatal(err)
	}
	if out := r.Text(); !strings.Contains(out, "0 matches in 0 of 1 searched file") {
		t.Errorf("negative result did not state the scope searched:\n%s", out)
	}
}

func TestGrepGlobFilterAndCase(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.go", "Hello\n")
	write(t, dir, "a_test.go", "Hello\n")

	r, err := run(t, NewGrepTool(dir), map[string]any{"pattern": "Hello", "glob": "*_test.go"})
	if err != nil {
		t.Fatal(err)
	}
	if out := r.Text(); !strings.Contains(out, "a_test.go:1:") || strings.Contains(out, "a.go:") {
		t.Errorf("glob filter not applied:\n%s", out)
	}

	r, err = run(t, NewGrepTool(dir), map[string]any{"pattern": "hello", "ignore_case": true})
	if err != nil {
		t.Fatal(err)
	}
	if out := r.Text(); !strings.Contains(out, "2 matches") {
		t.Errorf("ignore_case not applied:\n%s", out)
	}
}

func TestGrepLimitStopsAndSaysSo(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.go", strings.Repeat("needle\n", 50))
	r, err := run(t, NewGrepTool(dir), map[string]any{"pattern": "needle", "limit": float64(5)})
	if err != nil {
		t.Fatal(err)
	}
	out := r.Text()
	if n := strings.Count(out, "needle"); n != 5 {
		t.Errorf("got %d matches, want 5", n)
	}
	// Reaching the limit is not the same as having found everything.
	if !strings.Contains(out, "stopped at the limit of 5") {
		t.Errorf("limit not disclosed:\n%s", out)
	}
}

func TestGrepSkipsBinaryAndReportsIt(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "text.go", "needle\n")
	if err := os.WriteFile(filepath.Join(dir, "blob.bin"), []byte("needle\x00needle"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := run(t, NewGrepTool(dir), map[string]any{"pattern": "needle"})
	if err != nil {
		t.Fatal(err)
	}
	out := r.Text()
	if strings.Contains(out, "blob.bin") {
		t.Errorf("binary file was searched:\n%s", out)
	}
	if !strings.Contains(out, "1 binary file(s) skipped") {
		t.Errorf("binary skip not disclosed:\n%s", out)
	}
}

func TestGrepSingleFilePath(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.go", "needle\n")
	write(t, dir, "b.go", "needle\n")
	r, err := run(t, NewGrepTool(dir), map[string]any{"pattern": "needle", "path": "a.go"})
	if err != nil {
		t.Fatal(err)
	}
	if out := r.Text(); strings.Contains(out, "b.go") {
		t.Errorf("searched outside the named file:\n%s", out)
	}
}

func TestGrepClipsVeryLongLines(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "min.js", "needle "+strings.Repeat("x", 5000)+"\n")
	r, err := run(t, NewGrepTool(dir), map[string]any{"pattern": "needle"})
	if err != nil {
		t.Fatal(err)
	}
	out := r.Text()
	if len(out) > MaxGrepLineWidth+200 {
		t.Errorf("line not clipped: %d bytes", len(out))
	}
	if !strings.Contains(out, "…") {
		t.Error("clip should be marked")
	}
}

func TestGrepRejectsBadPattern(t *testing.T) {
	dir := t.TempDir()
	if _, err := run(t, NewGrepTool(dir), map[string]any{"pattern": "([a-z"}); err == nil {
		t.Fatal("expected an error for an uncompilable pattern")
	}
}

func TestGlobFindsAndReports(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "main.go", "package main\n")
	writeIn(t, dir, "pkg/util.go", "package pkg\n")
	writeIn(t, dir, "pkg/deep/nested.go", "package deep\n")

	r, err := run(t, NewGlobTool(dir), map[string]any{"pattern": "**/*.go"})
	if err != nil {
		t.Fatal(err)
	}
	out := r.Text()
	for _, want := range []string{"main.go", "pkg/util.go", "pkg/deep/nested.go"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "3 files") {
		t.Errorf("missing count footer:\n%s", out)
	}
}

func TestGlobSkipsIgnoredDirectories(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, ".gitignore", "node_modules/\n*.log\n")
	write(t, dir, "keep.go", "package keep\n")
	write(t, dir, "debug.log", "noise\n")
	if err := os.MkdirAll(filepath.Join(dir, "node_modules", "dep"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "node_modules/dep/index.js", "module.exports = 1\n")

	r, err := run(t, NewGlobTool(dir), map[string]any{"pattern": "**/*"})
	if err != nil {
		t.Fatal(err)
	}
	out := r.Text()
	if strings.Contains(out, "node_modules") || strings.Contains(out, "debug.log") {
		t.Errorf("ignored paths leaked:\n%s", out)
	}
	if !strings.Contains(out, "keep.go") {
		t.Errorf("kept path missing:\n%s", out)
	}
}

func TestGlobLimitStopsAndSaysSo(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 10; i++ {
		write(t, dir, fmt.Sprintf("f%d.go", i), "package x\n")
	}
	r, err := run(t, NewGlobTool(dir), map[string]any{"pattern": "*.go", "limit": float64(3)})
	if err != nil {
		t.Fatal(err)
	}
	out := r.Text()
	if n := strings.Count(out, ".go\n"); n != 3 {
		t.Errorf("got %d paths, want 3:\n%s", n, out)
	}
	if !strings.Contains(out, "stopped at the limit of 3") {
		t.Errorf("limit not disclosed:\n%s", out)
	}
}

func TestGlobRejectsFilePath(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.go", "package a\n")
	_, err := run(t, NewGlobTool(dir), map[string]any{"pattern": "*", "path": "a.go"})
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("want a not-a-directory error, got %v", err)
	}
}

func TestSearchToolsRejectMissingPattern(t *testing.T) {
	dir := t.TempDir()
	if _, err := run(t, NewGrepTool(dir), map[string]any{}); err == nil {
		t.Error("grep should require a pattern")
	}
	if _, err := run(t, NewGlobTool(dir), map[string]any{}); err == nil {
		t.Error("glob should require a pattern")
	}
}

func TestSummarizeSearchCalls(t *testing.T) {
	got := SummarizeToolCall("grep", map[string]any{"pattern": "func main", "glob": "*.go"})
	if !strings.Contains(got, "func main") || !strings.Contains(got, "[*.go]") {
		t.Errorf("grep summary: %q", got)
	}
	if got := SummarizeToolCall("glob", map[string]any{"pattern": "**/*.go"}); !strings.Contains(got, "**/*.go") {
		t.Errorf("glob summary: %q", got)
	}
}

// A file named explicitly is searched even when .gitignore covers it, and
// without walking its siblings.
func TestGrepNamedFileIgnoresGitignoreAndSkipsTheWalk(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, ".gitignore", "*.log\n")
	write(t, dir, "build.log", "ERROR: linker failed\n")
	write(t, dir, "other.txt", "ERROR: should not be searched\n")
	r, err := run(t, NewGrepTool(dir), map[string]any{"pattern": "ERROR", "path": "build.log"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Text(), "build.log:1: ERROR: linker failed") {
		t.Fatalf("named file not searched:\n%s", r.Text())
	}
	if strings.Contains(r.Text(), "other.txt") || r.Details.(map[string]any)["searched"] != 1 {
		t.Fatalf("searched beyond the named file:\n%s", r.Text())
	}
}
