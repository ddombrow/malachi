package coding

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ddombrow/malachi/sandbox"
)

// These run the reference servers when MCP_TEST_SERVERS names a directory
// they were installed into (never through a launcher):
//
//	npm install --prefix $DIR @modelcontextprotocol/server-filesystem @modelcontextprotocol/server-memory
//	UV_TOOL_DIR=$DIR/uv-tools UV_TOOL_BIN_DIR=$DIR/uv-bin uv tool install mcp-server-time
func realServers(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("MCP_TEST_SERVERS")
	if dir == "" {
		t.Skip("MCP_TEST_SERVERS not set")
	}
	requireSandbox(t)
	return dir
}

// mcp-server-time, a Python server installed with uv, runs under the
// sandbox with the user's real home private: its interpreter and packages
// must be readable by default.
func TestRealTimeServer(t *testing.T) {
	dir := realServers(t)
	cwd := t.TempDir()
	pol := sandbox.Build(sandbox.Config{Cwd: cwd, Home: t.TempDir()})
	policy := func() sandbox.Policy { return pol }
	m := newTestManager(t, cwd, map[string]MCPServerConfig{
		"time": {Command: filepath.Join(dir, "uv-bin", "mcp-server-time")},
	}, policy, ToolOptions{Sandbox: policy})
	got, isErr := callText(t, m, "time", "get_current_time", map[string]any{"timezone": "UTC"})
	if isErr || !strings.Contains(got, "UTC") {
		t.Fatalf("get_current_time: %q (state %+v)", got, m.State())
	}
}

// server-filesystem is told to serve two directories, one of them private;
// the sandbox, not the server, decides, so the private one stays out of
// reach.
func TestRealFilesystemServerStaysInItsRoots(t *testing.T) {
	dir := realServers(t)
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	cwd := t.TempDir()
	allowed := filepath.Join(cwd, "allowed")
	private := filepath.Join(userHome, "private")
	for _, d := range []string{allowed, private} {
		os.MkdirAll(d, 0o755)
		os.WriteFile(filepath.Join(d, "f.txt"), []byte("contents of "+filepath.Base(d)), 0o600)
	}
	pol := sandbox.Build(sandbox.Config{Cwd: cwd, Home: t.TempDir()})
	policy := func() sandbox.Policy { return pol }
	m := newTestManager(t, cwd, map[string]MCPServerConfig{
		"fs": {Command: filepath.Join(dir, "node_modules", ".bin", "mcp-server-filesystem"), Args: []string{allowed, private}},
	}, policy, ToolOptions{Sandbox: policy})

	tools, err := m.Tools(t.Context(), "fs")
	if err != nil {
		// Refusing to start because it cannot see the private directory is
		// a pass too: either way it never reads it.
		t.Logf("server did not start: %v", err)
		if !strings.Contains(err.Error(), "private") {
			t.Fatalf("unexpected failure: %v", err)
		}
		return
	}
	var names []string
	for _, tl := range tools {
		names = append(names, tl.Name)
	}
	t.Logf("tools: %v", names)
	read := "read_text_file"
	if !strings.Contains(strings.Join(names, ","), read) {
		read = "read_file"
	}
	if got, isErr := callText(t, m, "fs", read, map[string]any{"path": filepath.Join(allowed, "f.txt")}); isErr || !strings.Contains(got, "contents of allowed") {
		t.Errorf("allowed: %q", got)
	}
	if got, _ := callText(t, m, "fs", read, map[string]any{"path": filepath.Join(private, "f.txt")}); strings.Contains(got, "contents of private") {
		t.Errorf("the server read a private file the sandbox should refuse: %q", got)
	}
	if got, isErr := callText(t, m, "fs", "write_file", map[string]any{"path": filepath.Join(allowed, "new.txt"), "content": "hi"}); isErr {
		t.Errorf("write in the project: %q", got)
	}
}

// server-memory keeps its graph in one file; it works with that file's
// directory as its writable root, and not without.
func TestRealMemoryServerNeedsItsWritableRoot(t *testing.T) {
	dir := realServers(t)
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	cwd := t.TempDir()
	memDir := filepath.Join(userHome, "mem")
	os.MkdirAll(memDir, 0o755)
	pol := sandbox.Build(sandbox.Config{Cwd: cwd, Home: t.TempDir()})
	policy := func() sandbox.Policy { return pol }
	server := MCPServerConfig{
		Command: filepath.Join(dir, "node_modules", ".bin", "mcp-server-memory"),
		Env:     map[string]string{"MEMORY_FILE_PATH": filepath.Join(memDir, "memory.jsonl")},
	}
	granted := server
	granted.Writable = []string{"~/mem"}
	m := newTestManager(t, cwd, map[string]MCPServerConfig{"granted": granted, "ungranted": server}, policy, ToolOptions{Sandbox: policy})

	entity := map[string]any{"entities": []any{map[string]any{"name": "malachi", "entityType": "project", "observations": []any{"sandboxed"}}}}
	if got, isErr := callText(t, m, "granted", "create_entities", entity); isErr {
		t.Fatalf("with its writable root: %q", got)
	}
	if got, isErr := callText(t, m, "granted", "read_graph", nil); isErr || !strings.Contains(got, "sandboxed") {
		t.Errorf("read_graph: %q", got)
	}
	if b, err := os.ReadFile(filepath.Join(memDir, "memory.jsonl")); err != nil || !strings.Contains(string(b), "malachi") {
		t.Errorf("memory file: %q %v", b, err)
	}
	os.Remove(filepath.Join(memDir, "memory.jsonl"))
	got, isErr := callText(t, m, "ungranted", "create_entities", entity)
	if !isErr {
		t.Errorf("without a writable root the write succeeded: %q", got)
	}
	t.Logf("ungranted: %q", got)
}
