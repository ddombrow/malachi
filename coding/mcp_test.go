package coding

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/ai/fake"
	"github.com/ddombrow/malachi/sandbox"
)

func TestServerCommandRules(t *testing.T) {
	bin := t.TempDir()
	exe := func(name string) string {
		p := filepath.Join(bin, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	server := exe("my-server")
	for _, name := range []string{"npx", "uvx", "pipx", "pnpm", "bash", "python3.12", "node", "env", "deno", "docker"} {
		exe(name)
	}
	if err := os.Symlink(filepath.Join(bin, "npx"), filepath.Join(bin, "innocent")); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(bin, "plain")
	os.WriteFile(plain, []byte("x"), 0o644)

	cases := []struct {
		name string
		c    MCPServerConfig
		want string // "" means accepted
	}{
		{"installed", MCPServerConfig{Command: server, Args: []string{"--data", "x"}}, ""},
		{"python script with its own -c", MCPServerConfig{Command: filepath.Join(bin, "python3.12"), Args: []string{"server.py", "-c", "conf"}}, ""},
		{"npx", MCPServerConfig{Command: filepath.Join(bin, "npx"), Args: []string{"-y", "pkg"}}, "fetches and runs packages"},
		{"npx by name", MCPServerConfig{Command: "npx", Args: []string{"-y", "pkg"}}, "fetches and runs packages"},
		{"uvx", MCPServerConfig{Command: filepath.Join(bin, "uvx")}, "fetches and runs packages"},
		{"pipx run", MCPServerConfig{Command: filepath.Join(bin, "pipx"), Args: []string{"run", "pkg"}}, "pipx run fetches"},
		{"pnpm dlx", MCPServerConfig{Command: filepath.Join(bin, "pnpm"), Args: []string{"dlx", "pkg"}}, "pnpm dlx fetches"},
		{"docker run", MCPServerConfig{Command: filepath.Join(bin, "docker"), Args: []string{"run", "img"}}, "docker run fetches"},
		{"symlink to npx", MCPServerConfig{Command: filepath.Join(bin, "innocent")}, "npx fetches"},
		{"bash -c", MCPServerConfig{Command: filepath.Join(bin, "bash"), Args: []string{"-c", "npx -y pkg"}}, "runs inline code"},
		{"bash -lc", MCPServerConfig{Command: filepath.Join(bin, "bash"), Args: []string{"-lc", "npx pkg"}}, "runs inline code"},
		{"python -c", MCPServerConfig{Command: filepath.Join(bin, "python3.12"), Args: []string{"-c", "import x"}}, "runs inline code"},
		{"node -e", MCPServerConfig{Command: filepath.Join(bin, "node"), Args: []string{"-e", "x"}}, "runs inline code"},
		{"env wrapper", MCPServerConfig{Command: filepath.Join(bin, "env"), Args: []string{"npx", "pkg"}}, "runs another command"},
		{"deno remote", MCPServerConfig{Command: filepath.Join(bin, "deno"), Args: []string{"run", "-A", "https://x/mod.ts"}}, "fetches code"},
		{"remote", MCPServerConfig{URL: "https://mcp.example.com"}, "only local servers"},
		{"http type", MCPServerConfig{Type: "http", Command: server}, "only local servers"},
		{"missing", MCPServerConfig{Command: filepath.Join(bin, "nope")}, "no such file"},
		{"not on PATH", MCPServerConfig{Command: "surely-not-installed-xyz"}, "not installed"},
		{"not executable", MCPServerConfig{Command: plain}, "not executable"},
		{"no command", MCPServerConfig{}, "no command"},
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, c := range cases {
		_, err := checkServer("s", c.c, bin)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: refused: %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: got %v, want %q", c.name, err, c.want)
		}
	}
	if _, err := checkServer("bad name!", MCPServerConfig{Command: server}, bin); err == nil {
		t.Error("bad server name accepted")
	}
	if _, err := checkServer("s", MCPServerConfig{Command: filepath.Join(bin, "npx")}, bin); !strings.Contains(err.Error(), "npm i -g") {
		t.Errorf("no install hint: %v", err)
	}
}

func TestServerEnv(t *testing.T) {
	t.Setenv("MY_TOKEN", "tok")
	t.Setenv("PROVIDER_KEY", "sk-secret")
	opts := ToolOptions{HideEnv: []string{"PROVIDER_KEY"}}
	env, err := serverEnv(MCPServerConfig{Env: map[string]string{"TOKEN": "${MY_TOKEN}", "MODE": "x"}}, opts)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "TOKEN=tok") || !strings.Contains(joined, "MODE=x") || strings.Contains(joined, "sk-secret") {
		t.Errorf("env %q", env)
	}
	if strings.Contains(joined, "MY_TOKEN=") {
		t.Error("a variable the entry did not name was passed")
	}
	for _, c := range []map[string]string{{"PROVIDER_KEY": "x"}, {"K": "$PROVIDER_KEY"}} {
		if _, err := serverEnv(MCPServerConfig{Env: c}, opts); err == nil || !strings.Contains(err.Error(), "never passed") {
			t.Errorf("%v: %v", c, err)
		}
	}
}

// fakeServerConfig runs this test binary as an MCP server.
func fakeServerConfig(env map[string]string) MCPServerConfig {
	e := map[string]string{fakeMCPEnv: "1"}
	for k, v := range env {
		e[k] = v
	}
	return MCPServerConfig{Command: os.Args[0], Env: e}
}

func newTestManager(t *testing.T, cwd string, servers map[string]MCPServerConfig, policy func() sandbox.Policy, opts ToolOptions) *MCPManager {
	t.Helper()
	m := newMCPManager(MCPConfig{Servers: servers}, nil, cwd, opts, policy, NewDiagnostics(t.TempDir()))
	t.Cleanup(m.Close)
	return m
}

func callText(t *testing.T, m *MCPManager, server, tool string, args map[string]any) (string, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	res, err := m.Call(ctx, server, tool, args, nil)
	var failed *agent.ResultError
	switch {
	case errors.As(err, &failed):
		return failed.Result.Text(), true
	case err != nil:
		t.Fatalf("%s.%s: %v", server, tool, err)
	}
	return res.Text(), false
}

func requireSandbox(t *testing.T) {
	t.Helper()
	if err := sandbox.Available(); err != nil {
		t.Skipf("no sandbox here: %v", err)
	}
}

// A server runs under the session's sandbox plus its own roots, which reach
// only its process: bash, under the session's policy, cannot use them.
func TestMCPServerRunsUnderTheSandboxWithItsRoots(t *testing.T) {
	requireSandbox(t)
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	cwd, malachiHome := t.TempDir(), t.TempDir()
	mk := func(rel, body string) string {
		p := filepath.Join(userHome, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	data := mk("server-data/notes.txt", "server notes")
	secret := mk("secret/creds.txt", "private")
	os.MkdirAll(filepath.Join(userHome, "server-out"), 0o755)
	pol := sandbox.Build(sandbox.Config{Cwd: cwd, Home: malachiHome})
	policy := func() sandbox.Policy { return pol }
	opts := ToolOptions{Sandbox: policy, HideEnv: []string{"PROVIDER_KEY"}}
	t.Setenv("PROVIDER_KEY", "sk-secret")

	cfg := fakeServerConfig(map[string]string{"SERVER_TOKEN": "tok-123"})
	cfg.Readable = []string{"~/server-data"}
	cfg.Writable = []string{"~/server-out"}
	m := newTestManager(t, cwd, map[string]MCPServerConfig{"fake": cfg}, policy, opts)

	if got, isErr := callText(t, m, "fake", "read", map[string]any{"path": data}); isErr || got != "server notes" {
		t.Errorf("readable root: %q", got)
	}
	if got, isErr := callText(t, m, "fake", "read", map[string]any{"path": secret}); !isErr {
		t.Errorf("read outside its roots: %q", got)
	}
	if got, isErr := callText(t, m, "fake", "write", map[string]any{"path": filepath.Join(userHome, "server-out", "x"), "content": "y"}); isErr {
		t.Errorf("writable root: %q", got)
	}
	if got, isErr := callText(t, m, "fake", "write", map[string]any{"path": filepath.Join(userHome, "server-data", "y"), "content": "y"}); !isErr {
		t.Errorf("write to a readable root: %q", got)
	}
	if got, _ := callText(t, m, "fake", "env", map[string]any{"name": "SERVER_TOKEN"}); got != "tok-123" {
		t.Errorf("configured variable: %q", got)
	}
	if got, _ := callText(t, m, "fake", "env", map[string]any{"name": "PROVIDER_KEY"}); got != "(unset)" {
		t.Errorf("malachi's key reached the server: %q", got)
	}

	// The server's roots are its own.
	bash := NewBashTool(cwd, opts)
	res, _ := bash.Execute(context.Background(), "b", map[string]any{"command": "cat " + data}, func(agent.ToolResult) {})
	if strings.Contains(res.Text(), "server notes") {
		t.Errorf("bash read the server's root: %q", res.Text())
	}
}

// A sandboxed command cannot read a server's environment, where its tokens
// are: on Linux, Landlock refuses /proc/<pid>/environ across domains; on
// macOS, the profile denies inspecting other processes.
func TestBashCannotReadAServersEnvironment(t *testing.T) {
	requireSandbox(t)
	cwd, malachiHome := t.TempDir(), t.TempDir()
	pol := sandbox.Build(sandbox.Config{Cwd: cwd, Home: malachiHome})
	policy := func() sandbox.Policy { return pol }
	opts := ToolOptions{Sandbox: policy}
	m := newTestManager(t, cwd, map[string]MCPServerConfig{"fake": fakeServerConfig(map[string]string{"SERVER_TOKEN": "tok-secret-456"})}, policy, opts)
	pid, _ := callText(t, m, "fake", "pid", nil)

	var command string
	switch runtime.GOOS {
	case "linux":
		command = "cat /proc/" + pid + "/environ; tr '\\0' '\\n' < /proc/" + pid + "/environ"
	case "darwin":
		command = "ps eww -p " + pid
	default:
		t.Skip("no check for " + runtime.GOOS)
	}
	// Unsandboxed, the token is there to find, so the check means
	// something.
	out, _ := exec.Command("sh", "-c", command).CombinedOutput()
	if !strings.Contains(string(out), "tok-secret-456") {
		t.Logf("unsandboxed, the token was not visible either, so this proves less: %q", out)
	}
	res, _ := NewBashTool(cwd, opts).Execute(context.Background(), "b", map[string]any{"command": command}, func(agent.ToolResult) {})
	t.Logf("sandboxed: %q", res.Text())
	if strings.Contains(res.Text(), "tok-secret-456") {
		t.Fatalf("bash read the server's token:\n%s", res.Text())
	}
}

// Changing the policy restarts a server on its next use; until then it is
// marked stale.
func TestMCPRestartsWhenThePolicyChanges(t *testing.T) {
	cwd := t.TempDir()
	pol := sandbox.Policy{} // off: no backend needed
	policy := func() sandbox.Policy { return pol }
	m := newTestManager(t, cwd, map[string]MCPServerConfig{"fake": fakeServerConfig(nil)}, policy, ToolOptions{Sandbox: policy})
	first, _ := callText(t, m, "fake", "pid", nil)
	if again, _ := callText(t, m, "fake", "pid", nil); again != first {
		t.Fatalf("restarted without a change: %s then %s", first, again)
	}
	pol.Network = true
	if st := m.State()[0]; !st.Stale || st.Status != MCPReady {
		t.Errorf("after the change: %+v", st)
	}
	second, _ := callText(t, m, "fake", "pid", nil)
	if second == first {
		t.Fatal("not restarted after the policy changed")
	}
	if st := m.State()[0]; st.Stale || st.Restarts != 1 {
		t.Errorf("after the restart: %+v", st)
	}
}

func TestMCPStates(t *testing.T) {
	cwd := t.TempDir()
	policy := func() sandbox.Policy { return sandbox.Policy{} }
	failing := fakeServerConfig(nil)
	failing.Command, failing.Args = "/bin/sh", []string{filepath.Join(cwd, "fail.sh")}
	os.WriteFile(filepath.Join(cwd, "fail.sh"), []byte("echo 'missing API_TOKEN' >&2; exit 2\n"), 0o755)
	m := newTestManager(t, cwd, map[string]MCPServerConfig{
		"ok":       fakeServerConfig(nil),
		"launcher": {Command: "npx", Args: []string{"-y", "x"}},
		"off":      {Command: os.Args[0], Disabled: true},
		"failing":  failing,
	}, policy, ToolOptions{})
	m.startAll()

	deadline := time.Now().Add(time.Minute)
	want := map[string]string{"ok": MCPReady, "launcher": MCPInvalid, "off": MCPDisabled, "failing": MCPFailed}
	for {
		got := map[string]MCPServerState{}
		for _, st := range m.State() {
			got[st.Name] = st
		}
		done := true
		for name, status := range want {
			if got[name].Status != status {
				done = false
			}
		}
		if done {
			if !strings.Contains(got["failing"].Error, "missing API_TOKEN") || !strings.Contains(got["launcher"].Error, "npx") {
				t.Errorf("errors: %q / %q", got["failing"].Error, got["launcher"].Error)
			}
			if len(got["ok"].Tools) != 5 || got["ok"].Server != "fake-coding" || got["ok"].Version != "2026-07-28" {
				t.Errorf("ok: %+v", got["ok"])
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("states %+v", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A tool allowlist hides the rest of a server's tools.
func TestMCPToolAllowlist(t *testing.T) {
	cfg := fakeServerConfig(nil)
	cfg.Tools = []string{"pid"}
	m := newTestManager(t, t.TempDir(), map[string]MCPServerConfig{"fake": cfg}, func() sandbox.Policy { return sandbox.Policy{} }, ToolOptions{})
	tools, err := m.Tools(context.Background(), "fake")
	if err != nil || len(tools) != 1 || tools[0].Name != "pid" {
		t.Fatalf("%v %+v", err, tools)
	}
	if _, err := m.Call(context.Background(), "fake", "env", map[string]any{"name": "X"}, nil); err == nil {
		t.Error("a tool outside the allowlist ran")
	}
}

// A session reads mcp.json from its home; /new and resume hand running
// servers to the next session instead of restarting them.
func TestSessionMCPCarriesAcrossReopen(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	cfg := `{"mcpServers": {"fake": {"command": "` + os.Args[0] + `", "env": {"` + fakeMCPEnv + `": "1"}}}}`
	if err := os.WriteFile(filepath.Join(home, MCPConfigFile), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	// Reopen builds the configured provider, which needs a key to exist;
	// it is never called.
	t.Setenv("OPENCODE_API_KEY", "test")
	off := false
	settings := &Settings{Sandbox: SandboxSettings{Enabled: &off}}
	s, err := Open(Options{Cwd: cwd, Home: home, Settings: settings, Provider: fake.New(), NoSession: true})
	if err != nil {
		t.Fatal(err)
	}
	first, _ := callText(t, s.MCP(), "fake", "pid", nil)
	next, err := s.Reopen("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(next.Close)
	if again, _ := callText(t, next.MCP(), "fake", "pid", nil); again != first {
		t.Errorf("server restarted across Reopen: %s then %s", first, again)
	}
}
