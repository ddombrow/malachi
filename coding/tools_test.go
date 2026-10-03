package coding

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ddombrow/malachi/agent"
)

func run(t *testing.T, tool *agent.Tool, args map[string]any) (agent.ToolResult, error) {
	t.Helper()
	return tool.Execute(context.Background(), "call", args, func(agent.ToolResult) {})
}

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadSlicesAndHints(t *testing.T) {
	dir := t.TempDir()
	var lines []string
	for i := 1; i <= 10; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	write(t, dir, "f.txt", strings.Join(lines, "\n"))
	read := NewReadTool(dir)

	r, err := run(t, read, map[string]any{"path": "f.txt", "offset": float64(3), "limit": float64(2)})
	if err != nil {
		t.Fatal(err)
	}
	if want := "line 3\nline 4\n\n[6 more lines in file. Use offset=5 to continue.]"; r.Text() != want {
		t.Fatalf("got %q", r.Text())
	}
	if _, err := run(t, read, map[string]any{"path": "f.txt", "offset": float64(50)}); err == nil || !strings.Contains(err.Error(), "beyond end of file") {
		t.Fatalf("want offset error, got %v", err)
	}
	if _, err := run(t, read, map[string]any{"path": "missing.txt"}); err == nil {
		t.Fatal("want not found error")
	}
	if _, err := run(t, read, map[string]any{"path": "."}); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("want directory error, got %v", err)
	}
}

func TestReadTruncatesLongFiles(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	for i := 1; i <= 2500; i++ {
		fmt.Fprintf(&b, "%d\n", i)
	}
	write(t, dir, "big.txt", b.String())
	r, err := run(t, NewReadTool(dir), map[string]any{"path": "big.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(r.Text(), "[Showing lines 1-2000 of 2501. Use offset=2001 to continue.]") {
		t.Fatalf("tail: %q", r.Text()[len(r.Text())-80:])
	}
}

func TestReadImage(t *testing.T) {
	dir := t.TempDir()
	png := "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"
	write(t, dir, "a.png", png)
	r, err := run(t, NewReadTool(dir), map[string]any{"path": "a.png"})
	if err != nil {
		t.Fatal(err)
	}
	if img, ok := r.Content[1].(*agent.ImageContent); !ok || img.MimeType != "image/png" {
		t.Fatalf("content: %#v", r.Content)
	}
}

func TestWriteCreatesParents(t *testing.T) {
	dir := t.TempDir()
	if _, err := run(t, NewWriteTool(dir), map[string]any{"path": "a/b/c.txt", "content": "hi"}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "a/b/c.txt"))
	if string(got) != "hi" {
		t.Fatalf("got %q", got)
	}
}

func TestEdit(t *testing.T) {
	cases := []struct {
		name, content string
		args          map[string]any
		want, errSub  string
	}{
		{
			name:    "multiple disjoint edits",
			content: "a\nb\nc\nd\n",
			args:    map[string]any{"edits": []any{map[string]any{"oldText": "a", "newText": "A"}, map[string]any{"oldText": "d", "newText": "D"}}},
			want:    "A\nb\nc\nD\n",
		},
		{
			name:    "legacy top-level args",
			content: "hello world",
			args:    map[string]any{"oldText": "world", "newText": "there"},
			want:    "hello there",
		},
		{
			name:    "json string edits",
			content: "x=1",
			args:    map[string]any{"edits": `[{"oldText":"1","newText":"2"}]`},
			want:    "x=2",
		},
		{
			name:    "preserves CRLF and BOM",
			content: "\uFEFFone\r\ntwo\r\n",
			args:    map[string]any{"edits": []any{map[string]any{"oldText": "one\ntwo", "newText": "1\n2"}}},
			want:    "\uFEFF1\r\n2\r\n",
		},
		{
			name:    "not found",
			content: "abc",
			args:    map[string]any{"edits": []any{map[string]any{"oldText": "zzz", "newText": "y"}}},
			errSub:  "Could not find the exact text",
		},
		{
			name:    "duplicate",
			content: "aa",
			args:    map[string]any{"edits": []any{map[string]any{"oldText": "a", "newText": "b"}}},
			errSub:  "Found 2 occurrences",
		},
		{
			name:    "overlap",
			content: "abcdef",
			args:    map[string]any{"edits": []any{map[string]any{"oldText": "abc", "newText": "x"}, map[string]any{"oldText": "cde", "newText": "y"}}},
			errSub:  "must not overlap",
		},
		{
			name:    "no change",
			content: "same",
			args:    map[string]any{"edits": []any{map[string]any{"oldText": "same", "newText": "same"}}},
			errSub:  "No changes made",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			p := write(t, dir, "f.txt", tc.content)
			tc.args["path"] = "f.txt"
			r, err := run(t, NewEditTool(dir), tc.args)
			got, _ := os.ReadFile(p)
			if tc.errSub != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errSub) {
					t.Fatalf("want error containing %q, got %v", tc.errSub, err)
				}
				if string(got) != tc.content {
					t.Fatal("file must be unchanged on error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
			if r.Details.(map[string]any)["patch"] == "" {
				t.Fatal("missing patch")
			}
		})
	}
}

func TestUnifiedDiff(t *testing.T) {
	patch, first := UnifiedDiff("f", "a\nb\nc\nd\ne\nf\ng\nh\n", "a\nb\nc\nD\ne\nf\ng\nh\n")
	want := "--- f\n+++ f\n@@ -1,7 +1,7 @@\n a\n b\n c\n-d\n+D\n e\n f\n g\n"
	if patch != want || first != 4 {
		t.Fatalf("first=%d patch:\n%s", first, patch)
	}
}

func TestBash(t *testing.T) {
	dir := t.TempDir()
	bash := NewBashTool(dir)
	r, err := run(t, bash, map[string]any{"command": "echo out; echo err >&2; pwd"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Text(), "out\nerr\n") || !strings.Contains(r.Text(), filepath.Base(dir)) {
		t.Fatalf("got %q", r.Text())
	}
	r, _ = run(t, bash, map[string]any{"command": "exit 3"})
	if !strings.HasSuffix(r.Text(), "Command exited with code 3") {
		t.Fatalf("got %q", r.Text())
	}
}

func TestBashTimeoutKillsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	start := time.Now()
	// The background child would keep the pipe open if only the shell died.
	r, err := run(t, NewBashTool(dir), map[string]any{"command": "sleep 30 & sleep 30", "timeout": 0.3})
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("timeout took %v", time.Since(start))
	}
	if !strings.Contains(r.Text(), "Command timed out after 0.3 seconds") {
		t.Fatalf("got %q", r.Text())
	}
}

func TestBashTruncatesAndSpills(t *testing.T) {
	r, err := run(t, NewBashTool(t.TempDir()), map[string]any{"command": "seq 1 3000"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Text(), "[Showing lines 1001-3000 of 3000. Full output: ") {
		t.Fatalf("tail: %q", r.Text()[len(r.Text())-120:])
	}
	path := r.Details.(map[string]any)["full_output_path"].(string)
	defer os.Remove(path)
	if data, _ := os.ReadFile(path); !strings.HasPrefix(string(data), "1\n2\n") {
		t.Fatal("spill file missing full output")
	}
}

func TestBashCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	r, err := NewBashTool(t.TempDir()).Execute(ctx, "c", map[string]any{"command": "sleep 30"}, func(agent.ToolResult) {})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(r.Text(), "Command cancelled") {
		t.Fatalf("got %q", r.Text())
	}
}
