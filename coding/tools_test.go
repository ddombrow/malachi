package coding

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/iotest"
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
	bash := NewBashTool(dir, ToolOptions{})
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
	r, err := run(t, NewBashTool(dir, ToolOptions{}), map[string]any{"command": "sleep 30 & sleep 30", "timeout": 0.3})
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
	r, err := run(t, NewBashTool(t.TempDir(), ToolOptions{}), map[string]any{"command": "seq 1 3000"})
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
	r, err := NewBashTool(t.TempDir(), ToolOptions{}).Execute(ctx, "c", map[string]any{"command": "sleep 30"}, func(agent.ToolResult) {})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(r.Text(), "Command cancelled") {
		t.Fatalf("got %q", r.Text())
	}
}

// Memory stays bounded however much a command prints: the sink keeps a tail,
// and the spill file stops at MaxSpillBytes.
func TestOutputSinkIsBounded(t *testing.T) {
	o := &outputSink{}
	defer func() { o.close(); os.Remove(o.spillPath) }()
	line := []byte(strings.Repeat("x", 99) + "\n")
	chunk := bytes.Repeat(line, 1000) // 100 kB
	for i := 0; i < 1000; i++ {       // 100 MB in all
		if _, err := o.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if o.pending != nil || len(o.tail) > tailBytesKept {
		t.Fatalf("kept %d pending and %d tail bytes; want none and at most %d", len(o.pending), len(o.tail), tailBytesKept)
	}
	if o.spilled != MaxSpillBytes || o.dropped != o.total-MaxSpillBytes {
		t.Fatalf("spilled %d, dropped %d of %d", o.spilled, o.dropped, o.total)
	}
	if o.lines() != 1_000_000 {
		t.Fatalf("lines = %d", o.lines())
	}
	if st, _ := os.Stat(o.spillPath); st.Size() != MaxSpillBytes {
		t.Fatalf("spill file is %d bytes", st.Size())
	}
}

func TestBashReportsTheSpillCap(t *testing.T) {
	r, err := run(t, NewBashTool(t.TempDir(), ToolOptions{}), map[string]any{"command": "yes | head -c 80000000"})
	if err != nil {
		t.Fatal(err)
	}
	path := r.Details.(map[string]any)["full_output_path"].(string)
	defer os.Remove(path)
	if !strings.Contains(r.Text(), "First 64.0MB of 76.3MB saved to: "+path) {
		t.Fatalf("tail: %q", r.Text()[max(0, len(r.Text())-200):])
	}
	if !strings.Contains(r.Text(), "of 40000000.") {
		t.Fatalf("line total should count every line, not the kept tail: %q", r.Text()[max(0, len(r.Text())-200):])
	}
}

func TestBashDefaultTimeout(t *testing.T) {
	r, err := run(t, NewBashTool(t.TempDir(), ToolOptions{BashTimeout: 300 * time.Millisecond}), map[string]any{"command": "sleep 10"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Text(), "Command timed out after 0.3 seconds (the default limit") {
		t.Fatalf("got %q", r.Text())
	}
	// An explicit timeout wins, and is reported without the note.
	r, _ = run(t, NewBashTool(t.TempDir(), ToolOptions{BashTimeout: time.Hour}), map[string]any{"command": "sleep 10", "timeout": 0.2})
	if !strings.HasSuffix(r.Text(), "Command timed out after 0.2 seconds") {
		t.Fatalf("got %q", r.Text())
	}
}

func TestBashTimeoutSetting(t *testing.T) {
	for secs, want := range map[int]time.Duration{0: DefaultBashTimeout, 30: 30 * time.Second, -1: -1} {
		if got := (&Settings{BashTimeoutSeconds: secs}).ToolOptions().bashTimeout(); got != want {
			t.Errorf("bashTimeoutSeconds %d → %v, want %v", secs, got, want)
		}
	}
}

// The window is streamed: a file far larger than memory needs to be is read
// with offset/limit without loading it, and the total still counts every line.
func TestReadStreamsLargeFiles(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "big.txt")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	w := bufio.NewWriter(f)
	for i := 1; i <= 2_000_000; i++ {
		fmt.Fprintf(w, "line %d\n", i)
	}
	w.Flush()
	f.Close()

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	r, err := run(t, NewReadTool(dir), map[string]any{"path": "big.txt", "offset": 1_000_000, "limit": 2})
	if err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	want := "line 1000000\nline 1000001\n\n[1000000 more lines in file. Use offset=1000002 to continue.]"
	if r.Text() != want {
		t.Fatalf("got %q", r.Text())
	}
	if grew := int64(after.HeapAlloc) - int64(before.HeapAlloc); grew > 8<<20 {
		t.Fatalf("heap grew %d bytes reading a 2-line window", grew)
	}
}

func TestScanLinesMatchesSplitting(t *testing.T) {
	cases := []string{"", "a", "a\n", "a\r\nb", "a\rb\r", "a\r\n\r\nb\n", "\n\n", strings.Repeat("é", 70000) + "\nz"}
	for _, in := range cases {
		want := strings.Split(strings.ReplaceAll(strings.ReplaceAll(in, "\r\n", "\n"), "\r", "\n"), "\n")
		var got []string
		// A 1-byte reader splits CRLF pairs and multi-byte runes across reads.
		n, valid, err := scanLines(iotest.OneByteReader(strings.NewReader(in)), 1<<20, func(_ int, line []byte, full int) {
			if full != len(line) {
				t.Errorf("full %d, kept %d", full, len(line))
			}
			got = append(got, string(line))
		})
		if err != nil || !valid || n != len(want) || strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("%q: n=%d valid=%v got %q want %q", in[:min(len(in), 20)], n, valid, got, want)
		}
	}
	// A long line is clipped while streaming, but its length is reported.
	_, _, _ = scanLines(strings.NewReader(strings.Repeat("x", 100)+"\n"), 10, func(i int, line []byte, full int) {
		if i == 0 && (len(line) != 10 || full != 100) {
			t.Errorf("kept %d of %d", len(line), full)
		}
	})
	for _, bad := range []string{"ok\xff", "trailing \xe2\x82"} {
		if _, valid, _ := scanLines(iotest.OneByteReader(strings.NewReader(bad)), 1<<20, func(int, []byte, int) {}); valid {
			t.Errorf("%q reported valid", bad)
		}
	}
}

func TestReadReportsAnOversizedFirstLine(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "wide.txt", strings.Repeat("x", 3*MaxOutputBytes)+"\nshort\n")
	r, err := run(t, NewReadTool(dir), map[string]any{"path": "wide.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(r.Text(), "[Line 1 is 150.0KB, exceeds 50.0KB limit.") {
		t.Fatalf("got %q", r.Text())
	}
}

// Commands do not inherit malachi's API keys: neither the variables its .env
// set nor any provider's apiKeyEnv.
func TestBashDoesNotInheritAPIKeys(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, ".env", "MALACHI_TEST_DOTENV_KEY=from-dotenv\n")
	if err := LoadDotEnv(filepath.Join(dir, ".env")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Unsetenv("MALACHI_TEST_DOTENV_KEY") })
	t.Setenv("MALACHI_TEST_PROVIDER_KEY", "from-shell")
	t.Setenv("MALACHI_TEST_UNRELATED", "kept")

	s := &Settings{Providers: map[string]ProviderConfig{"p": {APIKeyEnv: "MALACHI_TEST_PROVIDER_KEY"}}}
	r, err := run(t, NewBashTool(dir, s.ToolOptions()), map[string]any{
		"command": `echo "[$MALACHI_TEST_DOTENV_KEY][$MALACHI_TEST_PROVIDER_KEY][$MALACHI_TEST_UNRELATED]"`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(r.Text()); got != "[][][kept]" {
		t.Fatalf("command saw %s", got)
	}
}
