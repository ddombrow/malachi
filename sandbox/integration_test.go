package sandbox

import (
	"bufio"
	"context"
	"io"
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

// NETLINK_USERSOCK can relay messages directly between processes, so merely
// allowing the AF_NETLINK family for kernel queries would leave an egress path.
func TestNetworkOffBlocksNetlinkUsersock(t *testing.T) {
	requireBackend(t)
	if runtime.GOOS != "linux" {
		t.Skip("NETLINK_USERSOCK is Linux-specific")
	}
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3")
	}
	serverScript := `import socket
s = socket.socket(16, 2, 2)
s.bind((0, 0))
print(s.getsockname()[0], flush=True)
s.settimeout(3)
try:
    data, _ = s.recvfrom(4096)
    print("received:" + data.decode(), flush=True)
except TimeoutError:
    print("none", flush=True)
`
	server := exec.Command(py, "-c", serverScript)
	stdout, err := server.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	server.Stderr = io.Discard
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if server.ProcessState == nil {
			_ = server.Process.Kill()
			_ = server.Wait()
		}
	}()
	reader := bufio.NewReader(stdout)
	portID, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("netlink listener did not start: %v", err)
	}
	portID = strings.TrimSpace(portID)

	clientScript := `import socket, sys
s = socket.socket(16, 2, 2)
s.bind((0, 0))
s.sendto(b"redteam-marker", (int(sys.argv[1]), 0))
`
	project := t.TempDir()
	p := Build(Config{Cwd: project, NoNet: true})
	command := py + ` -c '` + clientScript + `' ` + portID
	if out, ok := sh(t, p, project, command); ok {
		t.Fatalf("sandboxed command sent over NETLINK_USERSOCK: %s", out)
	}
	result, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("netlink listener did not report result: %v", err)
	}
	if strings.TrimSpace(result) != "none" {
		t.Fatalf("unconfined process received sandbox data: %q", strings.TrimSpace(result))
	}
	if err := server.Wait(); err != nil {
		t.Fatalf("netlink listener: %v", err)
	}
}

// With the network off nothing may carry data out: on Linux every socket
// family but netlink (and Unix, if allowed) is refused by our filter
// (EPERM, not merely unsupported); on macOS the name resolver is closed, so
// hostnames cannot leave as DNS queries.
func TestNetworkOffClosesSideChannels(t *testing.T) {
	requireBackend(t)
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3")
	}
	p, project, _, _, _ := fixture(t)
	p.Network = false
	switch runtime.GOOS {
	case "linux":
		script := py + ` -c "
import errno, socket
for name, fam in [('inet',2),('inet6',10),('packet',17),('can',29),('bluetooth',31),('alg',38),('vsock',40),('unix',1)]:
    try:
        socket.socket(fam, socket.SOCK_DGRAM if fam in (29,31) else socket.SOCK_STREAM if fam != 17 else socket.SOCK_RAW).close()
        print(name, 'open')
    except OSError as e:
        print(name, 'EPERM' if e.errno == errno.EPERM else 'other')
try:
    socket.socket(16, socket.SOCK_RAW, 0).close(); print('netlink open')
except OSError as e:
    print('netlink', e)
"`
		out, _ := sh(t, p, project, script)
		for _, fam := range []string{"inet", "inet6", "packet", "can", "bluetooth", "alg", "vsock", "unix"} {
			if !strings.Contains(out, fam+" EPERM") {
				t.Errorf("%s not refused by the filter:\n%s", fam, out)
			}
		}
		if !strings.Contains(out, "netlink open") {
			t.Errorf("netlink refused:\n%s", out)
		}
	case "darwin":
		connect := py + ` -c "import socket; socket.socket(socket.AF_UNIX).connect('/private/var/run/mDNSResponder')"`
		for _, unix := range []bool{false, true} {
			p.UnixSockets = unix
			if out, ok := sh(t, p, project, connect); ok {
				t.Errorf("unixSockets=%v: reached the resolver with the network off: %s", unix, out)
			}
		}
		p.Network, p.UnixSockets = true, false
		if out, ok := sh(t, p, project, connect); !ok {
			t.Errorf("resolver closed with the network on: %s", out)
		}
	}
}

// Commands read inside a private directory only under the read roots, and
// can still run from a project inside one.
func TestSandboxedPrivateReads(t *testing.T) {
	requireBackend(t)
	p, userHome, project := privateFixture(t, nil)
	if err := os.WriteFile(filepath.Join(userHome, "other", "notes.txt"), []byte("private-words\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(userHome, "other")
	for _, c := range []struct {
		name, script string
		ok           bool
	}{
		{"read project", "cat main.go", true},
		{"pwd in project", "pwd", true},
		{"write project", "echo x > new.txt && cat new.txt", true},
		{"read toolchain", "cat " + filepath.Join(userHome, ".cargo", "bin", "cargo"), true},
		{"read private", "cat " + filepath.Join(other, "notes.txt"), false},
		{"list private", "ls " + other, false},
		{"list home", "ls " + userHome, false},
		{"stat private", "test -e " + filepath.Join(other, "notes.txt"), true},
		{"read via ..", "cat ../../other/notes.txt", false},
		{"read outside home", "cat /etc/hosts >/dev/null", true},
	} {
		out, ok := sh(t, p, project, c.script)
		if ok != c.ok {
			t.Errorf("%s: succeeded=%v, want %v; output: %s", c.name, ok, c.ok, out)
		}
		if strings.Contains(out, "private-words") {
			t.Errorf("%s: private content leaked: %s", c.name, out)
		}
	}

	p.Readable = append(p.Readable, other)
	if out, ok := sh(t, p, project, "cat "+filepath.Join(other, "notes.txt")); !ok || !strings.Contains(out, "private-words") {
		t.Errorf("read root added: %v %s", ok, out)
	}

	// A hidden path stays hidden inside a root that is otherwise readable:
	// the project itself, and a read root.
	p.Hidden = append(p.Hidden, filepath.Join(project, "main.go"), filepath.Join(other, "notes.txt"))
	for _, script := range []string{"cat main.go", "cat " + filepath.Join(other, "notes.txt"), "echo x >> main.go"} {
		if out, ok := sh(t, p, project, script); ok {
			t.Errorf("%s: a hidden path inside a readable root was reachable: %s", script, out)
		}
	}
}
