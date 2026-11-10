package coding

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ddombrow/malachi/sandbox"
)

func TestNotFoundHint(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "app")
	writeAll(t, cwd, "coding/config.go", "package coding\n")
	writeAll(t, cwd, "coding/sandbox_state.go", "package coding\n")
	writeAll(t, cwd, "coding/sandbox_test.go", "package coding\n")
	writeAll(t, cwd, "sandbox/runner.go", "package sandbox\n")
	writeAll(t, cwd, "sandbox/policy.go", "package sandbox\n")
	writeAll(t, cwd, "tui/view/config.go", "package view\n")

	cases := []struct {
		name, requested, want string
	}{
		// The model wrote the path from the wrong root, dropping "app".
		{"wrong root", filepath.Join(root, "coding", "config.go"), " Did you mean coding/config.go?"},
		{"same name elsewhere", filepath.Join(cwd, "view", "config.go"), " Did you mean one of: tui/view/config.go, coding/config.go?"},
		{"similar name in the directory", filepath.Join(cwd, "coding", "sandbox.go"), " Did you mean one of: coding/sandbox_state.go, coding/sandbox_test.go?"},
		{"nothing close lists the directory", filepath.Join(cwd, "sandbox", "command.go"), " sandbox/ contains: policy.go, runner.go"},
		{"nothing at all", filepath.Join(cwd, "nowhere", "x.go"), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := notFoundHint(cwd, tc.requested, sandbox.Policy{}); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestNotFoundHintCapsTheListing(t *testing.T) {
	cwd := t.TempDir()
	for i := range maxDirListing + 5 {
		writeAll(t, cwd, filepath.Join("d", strings.Repeat("z", i+1)+".txt"), "")
	}
	got := notFoundHint(cwd, filepath.Join(cwd, "d", "q.md"), sandbox.Policy{})
	if !strings.HasSuffix(got, "(and 5 more)") {
		t.Fatalf("got %q", got)
	}
}

func TestReadAndEditSuggestPaths(t *testing.T) {
	cwd := t.TempDir()
	writeAll(t, cwd, "coding/config.go", "package coding\n")
	_, err := run(t, NewReadTool(cwd, ToolOptions{}), map[string]any{"path": "config.go"})
	if err == nil || !strings.Contains(err.Error(), "Did you mean coding/config.go?") {
		t.Fatalf("read: %v", err)
	}
	_, err = run(t, NewEditTool(cwd, ToolOptions{}), map[string]any{"path": "config.go",
		"edits": []any{map[string]any{"oldText": "a", "newText": "b"}}})
	if err == nil || !strings.Contains(err.Error(), "Did you mean coding/config.go?") {
		t.Fatalf("edit: %v", err)
	}
}

// writeAll writes a file, creating its parent directories.
func writeAll(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestNotFoundHintNeverNamesHiddenFiles(t *testing.T) {
	cwd := t.TempDir()
	writeAll(t, cwd, "secret/config.go", "")
	writeAll(t, cwd, "secret/keys.env", "")
	writeAll(t, cwd, "pub/other.go", "")
	pol := sandbox.Build(sandbox.Config{Cwd: cwd, Home: t.TempDir(), Hidden: []string{filepath.Join(cwd, "secret")}})
	if pol.CheckRead(filepath.Join(cwd, "secret", "config.go")) == nil {
		t.Fatal("test policy does not hide secret/")
	}
	for _, requested := range []string{
		filepath.Join(cwd, "config.go"),           // same name, but hidden
		filepath.Join(cwd, "secret", "conf.go"),   // similar name in a hidden directory
		filepath.Join(cwd, "secret", "nothing.x"), // listing a hidden directory
	} {
		if got := notFoundHint(cwd, requested, pol); strings.Contains(got, "secret") || strings.Contains(got, "keys") || strings.Contains(got, "config") {
			t.Errorf("%s: hint %q names a hidden file", requested, got)
		}
	}
}
