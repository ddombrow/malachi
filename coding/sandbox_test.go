package coding

import (
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

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

func TestAddWritableRootIsSessionScopedAndEffective(t *testing.T) {
	s, _, outside, home := sandboxedSession(t, "")
	root, err := s.AddWritableRoot(outside)
	if err != nil {
		t.Fatal(err)
	}
	if want := sandbox.Canonical(outside); root != want {
		t.Fatalf("root = %q, want canonical %q", root, want)
	}
	if err := s.Sandbox().CheckWrite(filepath.Join(root, "new.txt")); err != nil {
		t.Fatalf("new root did not take effect: %v", err)
	}
	if _, err := run(t, tool(t, s, "write"), map[string]any{
		"path": filepath.Join(root, "new.txt"), "content": "session grant",
	}); err != nil {
		t.Fatalf("write using new root: %v", err)
	}
	if _, err := s.AddWritableRoot(home); err == nil {
		t.Fatal("allowed a hidden directory")
	}
	if _, err := s.AddWritableRoot(filepath.Dir(home)); err == nil {
		t.Fatal("allowed a root overlapping the hidden home")
	}
	if _, err := s.AddWritableRoot(string(filepath.Separator)); err == nil {
		t.Fatal("allowed the filesystem root")
	}
}

func TestAddWritableRootRequiresSandboxAndExistingDirectory(t *testing.T) {
	s, _, outside, _ := sandboxedSession(t, "off")
	if _, err := s.AddWritableRoot(outside); err == nil {
		t.Fatal("added a root while sandbox was off")
	}

	s, _, _, _ = sandboxedSession(t, "")
	if _, err := s.AddWritableRoot(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("added a nonexistent directory")
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

// homeSession opens a sandboxed session for a project inside a fake home
// directory, beside a private directory the project may not read. Events
// the session emits are sent on the returned channel.
func homeSession(t *testing.T) (s *Session, project, private string, events <-chan any) {
	t.Helper()
	root := t.TempDir()
	userHome := filepath.Join(root, "user")
	project, private = filepath.Join(userHome, "src", "proj"), filepath.Join(userHome, "private")
	for _, d := range []string{project, private, filepath.Join(userHome, ".ssh"), filepath.Join(root, "outside")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write(t, project, "main.go", "package main\n")
	write(t, private, "notes.txt", "private-words\n")
	t.Setenv("HOME", userHome)
	s, err := Open(Options{Cwd: project, Home: filepath.Join(root, "malachi"), Settings: &Settings{}, Provider: fake.New(), NoSession: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	ch := make(chan any, 16)
	s.Subscribe(func(e any) { ch <- e })
	return s, sandbox.Canonical(project), sandbox.Canonical(private), ch
}

func sandboxChanged(t *testing.T, events <-chan any) SandboxState {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case e := <-events:
			if sc, ok := e.(SandboxChangedEvent); ok {
				return sc.State
			}
		case <-timeout:
			t.Fatal("no sandbox_changed event")
		}
	}
}

func TestAddReadableRoot(t *testing.T) {
	s, project, private, events := homeSession(t)
	r, err := run(t, tool(t, s, "read"), map[string]any{"path": filepath.Join(private, "notes.txt")})
	if err == nil && !strings.Contains(r.Text(), "readable directories") || strings.Contains(r.Text(), "private-words") {
		t.Fatalf("read a private file before allowing it: %v %s", err, r.Text())
	}

	root, err := s.AddReadableRoot(private)
	if err != nil || root != private {
		t.Fatalf("AddReadableRoot = %q, %v", root, err)
	}
	if st := sandboxChanged(t, events); !slices.Contains(st.Readable, private) {
		t.Fatalf("event state lacks the new root: %v", st.Readable)
	}
	r, err = run(t, tool(t, s, "read"), map[string]any{"path": filepath.Join(private, "notes.txt")})
	if err != nil || !strings.Contains(r.Text(), "private-words") {
		t.Fatalf("read after allowing: %v %s", err, r.Text())
	}

	for _, c := range []struct{ name, path string }{
		{"already readable", project},
		{"outside home", filepath.Join(filepath.Dir(filepath.Dir(private)), "outside")},
		{"filesystem root", string(filepath.Separator)},
		{"missing", filepath.Join(private, "missing")},
		{"a file", filepath.Join(private, "notes.txt")},
		{"empty", " "},
	} {
		if _, err := s.AddReadableRoot(c.path); err == nil {
			t.Errorf("%s: allowed %s", c.name, c.path)
		}
	}
	userHome, _ := os.UserHomeDir()
	if _, err := s.AddReadableRoot(filepath.Join(userHome, ".ssh")); err == nil {
		t.Error("allowed a hidden directory")
	}
}

func TestSetNetworkEmitsSandboxChanged(t *testing.T) {
	s, _, _, events := homeSession(t)
	if err := s.SetNetwork(true); err != nil {
		t.Fatal(err)
	}
	if st := sandboxChanged(t, events); !st.Network {
		t.Fatal("event reports the network off")
	}
}

func TestAddReadableRootRequiresTheSandbox(t *testing.T) {
	s, _, outside, _ := sandboxedSession(t, "off")
	if _, err := s.AddReadableRoot(outside); err == nil {
		t.Fatal("added a read root with the sandbox off")
	}
}

func TestSandboxGrants(t *testing.T) {
	s, project, private, _ := homeSession(t)
	if _, err := s.AddReadableRoot(private); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(project, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A default write root that exists, to be counted: on Linux the fake
	// home sits under /tmp, which leaves /tmp and TMPDIR out.
	if err := os.Mkdir(filepath.Join(filepath.Dir(filepath.Dir(project)), ".npm"), 0o755); err != nil {
		t.Fatal(err)
	}
	st := s.Sandbox()
	st.Readable = append(st.Readable, filepath.Join(project, "sub")) // inside a shown root
	g := st.Grants()
	if !slices.Equal(g.Read, []string{private, project}) && !slices.Equal(g.Read, []string{project, private}) {
		t.Fatalf("read %v", g.Read)
	}
	if !slices.Contains(g.Write, project) || g.BuiltinWrite == 0 {
		t.Fatalf("write %v, builtin %d", g.Write, g.BuiltinWrite)
	}
}

// Displays leave out default roots that do not exist, while the sandbox
// still grants them.
func TestSandboxDisplayOmitsMissingRoots(t *testing.T) {
	s, project, _, _ := homeSession(t)
	cargo := filepath.Join(filepath.Dir(filepath.Dir(project)), ".cargo")
	line := " " + sandbox.Canonical(cargo) + "\n" // its own line, not .cargo/credentials
	st := s.Sandbox()
	if !slices.Contains(st.Readable, sandbox.Canonical(cargo)) {
		t.Fatalf("readable %v lacks %s", st.Readable, cargo)
	}
	before := st.Grants().BuiltinRead
	if strings.Contains(st.Describe(), line) {
		t.Fatalf("describe lists the missing %s:\n%s", cargo, st.Describe())
	}
	if err := os.Mkdir(cargo, 0o755); err != nil {
		t.Fatal(err)
	}
	if after := st.Grants().BuiltinRead; after != before+1 {
		t.Fatalf("toolchain count %d after creating %s, want %d", after, cargo, before+1)
	}
	if !strings.Contains(st.Describe(), line) {
		t.Fatalf("describe lacks the existing %s:\n%s", cargo, st.Describe())
	}
}

// A sandbox that runs but cannot enforce everything says so at startup and
// in /sandbox; one that is off or unavailable says that instead.
func TestSandboxNoticeNamesAWeakness(t *testing.T) {
	st := SandboxState{Policy: sandbox.Policy{Enabled: true}, Weakness: "cannot stop truncation"}
	if got := st.Notice(); !strings.Contains(got, "Sandbox limited: cannot stop truncation") {
		t.Fatalf("Notice() = %q", got)
	}
	if got := st.Describe(); !strings.Contains(got, "Sandbox on") || !strings.Contains(got, "cannot stop truncation") {
		t.Fatalf("Describe() = %q", got)
	}
	if got := (SandboxState{Policy: sandbox.Policy{Enabled: true}}).Notice(); got != "" {
		t.Fatalf("a sandbox in full force should say nothing, got %q", got)
	}
}
