package coding

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/ai/fake"
	"github.com/ddombrow/malachi/sandbox"
)

// sandboxedSession opens a session whose live policy allows writes only in
// the project (everything under temp is writable by default), with a secret
// in its malachi home and an outside directory beside the project.
func sandboxedSession(t *testing.T, mode string) (s *Session, project, outside, home string) {
	t.Helper()
	root := t.TempDir()
	project, outside, home = filepath.Join(root, "project"), filepath.Join(root, "outside"), filepath.Join(root, "home")
	for _, d := range []string{project, outside, home} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write(t, home, ".env", "KEY=secret\n")
	write(t, project, "main.go", "package main // KEY\n")
	// A keyless provider, so Reopen can build one from settings.
	settings := &Settings{DefaultProvider: "local", Providers: map[string]ProviderConfig{
		"local": {BaseURL: "http://127.0.0.1:1", DefaultModel: "m"},
	}}
	s, err := Open(Options{Cwd: project, Home: home, Settings: settings, Provider: fake.New(), NoSession: true, Sandbox: mode})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	s.sbMu.Lock()
	s.sandbox.Writable = []string{sandbox.Canonical(project)}
	s.sbMu.Unlock()
	return s, project, outside, home
}

func tool(t *testing.T, s *Session, name string) *agent.Tool {
	t.Helper()
	for _, tl := range s.tools {
		if tl.Name == name {
			return tl
		}
	}
	t.Fatalf("no %s tool", name)
	return nil
}

func TestFileToolsFollowTheSandbox(t *testing.T) {
	s, project, outside, home := sandboxedSession(t, "")
	secret := filepath.Join(home, ".env")

	if _, err := run(t, tool(t, s, "read"), map[string]any{"path": secret}); err == nil || !strings.Contains(err.Error(), "hidden") {
		t.Errorf("read of the malachi home: %v", err)
	}
	if _, err := run(t, tool(t, s, "write"), map[string]any{"path": filepath.Join(outside, "x"), "content": "x"}); err == nil {
		t.Error("write outside the project allowed")
	}
	if _, err := run(t, tool(t, s, "edit"), map[string]any{"path": secret, "edits": []any{map[string]any{"oldText": "secret", "newText": "x"}}}); err == nil {
		t.Error("edit of a hidden file allowed")
	}
	if _, err := run(t, tool(t, s, "write"), map[string]any{"path": "new/file.txt", "content": "ok"}); err != nil {
		t.Errorf("write inside the project: %v", err)
	}
	// A search from above skips the hidden home and says so.
	r, err := run(t, tool(t, s, "grep"), map[string]any{"pattern": "KEY", "path": filepath.Dir(project)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.Text(), "secret") || !strings.Contains(r.Text(), "main.go") || !strings.Contains(r.Text(), "hidden by the sandbox") {
		t.Errorf("grep: %s", r.Text())
	}
	r, _ = run(t, tool(t, s, "glob"), map[string]any{"pattern": "**/.env", "path": filepath.Dir(project)})
	if !strings.Contains(r.Text(), "No files") {
		t.Errorf("glob found the hidden .env: %s", r.Text())
	}
}

func TestBashFollowsTheSandbox(t *testing.T) {
	if err := sandbox.Available(); err != nil {
		t.Skip(err)
	}
	s, _, outside, home := sandboxedSession(t, "")
	r, err := run(t, tool(t, s, "bash"), map[string]any{"command": "cat " + filepath.Join(home, ".env")})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.Text(), "secret") || !strings.Contains(r.Text(), "This may be the sandbox") {
		t.Errorf("cat of the malachi home: %s", r.Text())
	}
	r, _ = run(t, tool(t, s, "bash"), map[string]any{"command": "echo x > " + filepath.Join(outside, "x")})
	if !strings.Contains(r.Text(), "This may be the sandbox") {
		t.Errorf("write outside: %s", r.Text())
	}
}

func TestSandboxOffReachesEverything(t *testing.T) {
	s, _, outside, home := sandboxedSession(t, "off")
	if s.Sandbox().Enabled {
		t.Fatal("-sandbox off left it enabled")
	}
	if r, err := run(t, tool(t, s, "read"), map[string]any{"path": filepath.Join(home, ".env")}); err != nil || !strings.Contains(r.Text(), "secret") {
		t.Fatalf("read: %v", err)
	}
	r, _ := run(t, tool(t, s, "bash"), map[string]any{"command": "echo x > " + filepath.Join(outside, "x") + " && echo done"})
	if !strings.Contains(r.Text(), "done") {
		t.Fatalf("bash: %s", r.Text())
	}
	// The choice survives /new.
	next, err := s.Reopen("")
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if next.Sandbox().Enabled {
		t.Fatal("Reopen turned the sandbox back on")
	}
}

func TestSandboxOptionIsValidated(t *testing.T) {
	if _, err := Open(Options{Cwd: t.TempDir(), Home: t.TempDir(), Settings: &Settings{}, Provider: fake.New(), NoSession: true, Sandbox: "maybe"}); err == nil {
		t.Fatal("accepted -sandbox maybe")
	}
}

func TestSetNetworkSwitchesCommandsNetwork(t *testing.T) {
	s, _, _, _ := sandboxedSession(t, "")
	if s.Sandbox().Network {
		t.Fatal("network on by default")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	connect := map[string]any{"command": "exec 3<>/dev/tcp/127.0.0.1/" + strconv.Itoa(ln.Addr().(*net.TCPAddr).Port) + " && echo connected"}
	connects := func(s *Session) bool {
		r, _ := run(t, tool(t, s, "bash"), connect)
		return strings.Contains(r.Text(), "connected")
	}
	backend := sandbox.Available() == nil

	if backend && connects(s) {
		t.Fatal("connected with the network off by default")
	}
	if err := s.SetNetwork(true); err != nil {
		t.Fatal(err)
	}
	if !s.Sandbox().Network || backend && !connects(s) {
		t.Fatal("could not connect after /network on")
	}
	// The switch survives /new.
	next, err := s.Reopen("")
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if !next.Sandbox().Network {
		t.Fatal("Reopen turned the network back off")
	}
	if err := next.SetNetwork(false); err != nil {
		t.Fatal(err)
	}
	if next.Sandbox().Network || backend && connects(next) {
		t.Fatal("connected after /network off")
	}

	off, _, _, _ := sandboxedSession(t, "off")
	if off.SetNetwork(true) == nil {
		t.Fatal("SetNetwork accepted with the sandbox off")
	}
}
