package sandbox

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// requireBackend skips without a backend, unless MALACHI_REQUIRE_SANDBOX is
// set (CI on platforms that have one), where a missing backend fails.
func requireBackend(t *testing.T) {
	t.Helper()
	if err := Available(); err != nil {
		if os.Getenv("MALACHI_REQUIRE_SANDBOX") != "" {
			t.Fatalf("sandbox required but unavailable: %v", err)
		}
		t.Skipf("no sandbox here: %v", err)
	}
}

// sh runs script with bash under p in dir and returns its combined output
// and whether it succeeded.
func sh(t *testing.T, p Policy, dir, script string) (string, bool) {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd, err := Command(ctx, p, bash, "-c", script)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err == nil
}

func TestSandboxedCommands(t *testing.T) {
	requireBackend(t)
	p, project, outside, home, _ := fixture(t)
	p = projectOnly(p, project)
	if err := os.WriteFile(filepath.Join(home, ".env"), []byte("KEY=secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, ".env"), filepath.Join(project, "env-link")); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name, script string
		ok           bool
	}{
		{"write inside", "echo hi > inside.txt && cat inside.txt", true},
		{"mkdir inside", "mkdir -p a/b && touch a/b/c", true},
		{"write outside", "echo hi > " + filepath.Join(outside, "x"), false},
		{"write via ..", "echo hi > ../outside/y", false},
		{"read hidden", "cat " + filepath.Join(home, ".env"), false},
		// Landlock grants subtrees, so a directory split around a hidden
		// path stays listable on Linux: names show, contents do not.
		{"list hidden", "ls " + home, runtime.GOOS == "linux"},
		{"read hidden via link", "cat env-link", false},
		{"read elsewhere", "cat /etc/hosts >/dev/null", true},
		{"dev null", "echo x > /dev/null", true},
	} {
		out, ok := sh(t, p, project, c.script)
		if ok != c.ok {
			t.Errorf("%s: succeeded=%v, want %v; output: %s", c.name, ok, c.ok, out)
		}
		if strings.Contains(out, "secret") {
			t.Errorf("%s: hidden content leaked: %s", c.name, out)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "x")); err == nil {
		t.Error("a file appeared outside the writable roots")
	}
}

// Network off blocks IP connections; checked against a local listener so the
// test never reaches the internet.
func TestSandboxedNetwork(t *testing.T) {
	requireBackend(t)
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
	port := ln.Addr().(*net.TCPAddr).Port
	connect := "exec 3<>/dev/tcp/127.0.0.1/" + strconv.Itoa(port)

	p, project, _, _, _ := fixture(t)
	if out, ok := sh(t, p, project, connect); !ok {
		t.Fatalf("network on: connect failed: %s", out)
	}
	p.Network = false
	if out, ok := sh(t, p, project, connect); ok {
		t.Fatalf("network off: connect succeeded: %s", out)
	}
}

// A real build works under the default policy: the Go build cache and
// module cache are writable, and their atomic renames across directories
// are allowed.
func TestSandboxedGoBuild(t *testing.T) {
	requireBackend(t)
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go toolchain")
	}
	project := t.TempDir()
	t.Setenv("GOCACHE", t.TempDir()) // a cold cache, so the build writes to it
	// As in real use, malachi's home is not under a writable root (the temp
	// directory here); see carve.
	p := Build(Config{Cwd: project, Home: "/nonexistent/malachi-home"})
	script := "export GOTOOLCHAIN=local GOFLAGS=-mod=mod && " + gobin + ` mod init example.com/x && printf 'package main\nfunc main() { println("built") }\n' > main.go && ` + gobin + " run ."
	if out, ok := sh(t, p, project, script); !ok || !strings.Contains(out, "built") {
		t.Fatalf("go run under the sandbox failed: %s", out)
	}
}

// Commands cannot reach local daemons over Unix sockets (Docker, ssh-agent):
// they would act outside the sandbox. Checked against a listener of our own
// outside the writable roots.
func TestSandboxedUnixSockets(t *testing.T) {
	requireBackend(t)
	dir, err := os.MkdirTemp("", "us") // short: socket paths are limited to ~104 bytes
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(Canonical(dir), "s.sock")
	ln, err := net.Listen("unix", sock)
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
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3")
	}
	connect := py + ` -c "import socket; socket.socket(socket.AF_UNIX).connect('` + sock + `')"`

	p, project, _, _, _ := fixture(t)
	p = projectOnly(p, project) // the listener is outside
	if out, ok := sh(t, p, project, connect); ok {
		t.Fatalf("connected to an outside Unix socket: %s", out)
	}
	// On macOS a socket under a writable root (one the command could have
	// made) works; on Linux commands cannot create Unix sockets at all.
	inside := p
	inside.Writable = append(slices.Clone(p.Writable), Canonical(dir))
	if out, ok := sh(t, inside, project, connect); ok != (runtime.GOOS == "darwin") {
		t.Fatalf("socket under a writable root: connected=%v on %s: %s", ok, runtime.GOOS, out)
	}
	p.UnixSockets = true
	if out, ok := sh(t, p, project, connect); !ok {
		t.Fatalf("Unix sockets allowed but connect failed: %s", out)
	}
}
