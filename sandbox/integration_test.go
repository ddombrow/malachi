package sandbox

import (
	"bufio"
	"context"
	"fmt"
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
		{"dev stderr", "echo x > /dev/stderr && echo y > /dev/stdout && echo z > /dev/fd/2", true},
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

// A directory name is data wherever the policy is written out: one made of
// the characters a profile is written in confines commands like any other.
func TestSandboxedOddProjectName(t *testing.T) {
	requireBackend(t)
	root := Canonical(t.TempDir())
	project := filepath.Join(root, "a\"b\\c) (allow default) ;#\nd")
	outside, home := filepath.Join(root, "outside"), filepath.Join(root, "malachi")
	for _, d := range []string{project, outside, home} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "key"), []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := projectOnly(Build(Config{Cwd: project, Home: home}), project)
	for _, c := range []struct {
		name, script string
		ok           bool
	}{
		{"write inside", "echo hi > in.txt && cat in.txt", true},
		{"write outside", "echo hi > ../outside/x", false},
		{"read hidden", "cat ../malachi/key", false},
	} {
		out, ok := sh(t, p, project, c.script)
		if ok != c.ok || strings.Contains(out, "secret") {
			t.Errorf("%s: succeeded=%v, want %v; output: %s", c.name, ok, c.ok, out)
		}
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

// Network off also means a command serves nothing: it cannot listen on an
// IP address, where another machine could connect to it and be handed
// whatever the command can read. Unix sockets are not the network and keep
// working.
func TestNetworkOffRefusesListening(t *testing.T) {
	requireBackend(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p, project, _, _, _ := fixture(t)
	p.Readable = append(p.Readable, filepath.Dir(self))
	specs := []string{"tcp4 0.0.0.0:0", "tcp4 127.0.0.1:0", "udp4 0.0.0.0:0", "udp4 127.0.0.1:0", "tcp6 [::]:0", "tcp6 [::1]:0", "udp6 [::]:0"}
	for _, network := range []bool{true, false} {
		p.Network = network
		for _, spec := range specs {
			out, ok := sh(t, p, project, listenEnv+"='"+spec+"' "+self)
			if strings.Contains(spec, "6") && network && !ok {
				continue // no IPv6 on this machine
			}
			if ok != network {
				t.Errorf("network=%v: listen %s succeeded=%v: %s", network, spec, ok, out)
			}
		}
	}
	// Linux cannot tell one unix socket from another, so it has them only
	// with the setting on.
	p.Network, p.UnixSockets = false, true
	// A relative name: the fixture's path is longer than a socket name may be.
	if out, ok := sh(t, p, project, listenEnv+"='unix s' "+self); !ok {
		t.Errorf("network off: a unix socket in the project was refused: %s", out)
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

	// A file the sandbox refuses does not become readable under a new name
	// in the project: not by a hard link, and not by a copy-on-write clone.
	key := filepath.Join(userHome, ".ssh", "id_ed25519")
	clone := "cp -c"
	if runtime.GOOS == "linux" {
		clone = "cp --reflink=always"
	}
	for i, src := range []string{filepath.Join(other, "notes.txt"), key} {
		for j, script := range []string{
			fmt.Sprintf("ln %s link%d && cat link%d", src, i, i),
			fmt.Sprintf("%s %s clone%d && cat clone%d", clone, src, i, i),
		} {
			out, ok := sh(t, p, project, script)
			if ok || strings.Contains(out, "private-words") || strings.Contains(out, "content") {
				t.Errorf("%d/%d %s: a refused file was reachable under a new name: %v %s", i, j, script, ok, out)
			}
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

// A command may register one kind of service name, the one a Chromium
// browser gives its helper processes, and nothing that looks like it.
func TestSandboxedServiceRegistration(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Seatbelt only")
	}
	requireBackend(t)
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3")
	}
	p, project, _, _, _ := fixture(t)
	register := py + ` -c "
import ctypes, sys
lib = ctypes.CDLL('/usr/lib/libSystem.B.dylib')
bootstrap = ctypes.c_uint.in_dll(lib, 'bootstrap_port')
for name in sys.argv[1:]:
    port = ctypes.c_uint()
    print(name, 'registered' if lib.bootstrap_check_in(bootstrap, name.encode(), ctypes.byref(port)) == 0 else 'refused')
" `
	own := fmt.Sprintf("malachi.test.MachPortRendezvousServer.%d", os.Getpid())
	refused := []string{"malachi.test.other", own + "x", own + ".more", "com.apple.pasteboard.1"}
	out, ok := sh(t, p, project, register+own+" "+strings.Join(refused, " "))
	if !ok {
		t.Fatalf("registration probe failed: %s", out)
	}
	out = strings.TrimSpace(out) + "\n"
	if !strings.Contains(out, own+" registered\n") {
		t.Errorf("the rendezvous name was refused:\n%s", out)
	}
	for _, name := range refused {
		if !strings.Contains(out, name+" refused\n") {
			t.Errorf("%s was not refused:\n%s", name, out)
		}
	}
}

// On macOS a command reaches only the system services on the allow-list:
// not the ones that would act for it outside the sandbox (opening a URL,
// reading the clipboard, the keychain daemon), and the ones that can carry
// data out only with the network on.
func TestSandboxedServices(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Seatbelt only")
	}
	requireBackend(t)
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3")
	}
	p, project, _, _, _ := fixture(t)
	// bootstrap_look_up is what every client library does first; it
	// returns 0 when the sandbox lets the name through.
	lookup := py + ` -c "
import ctypes, sys
lib = ctypes.CDLL('/usr/lib/libSystem.B.dylib')
bootstrap = ctypes.c_uint.in_dll(lib, 'bootstrap_port')
for name in sys.argv[1:]:
    port = ctypes.c_uint()
    print(name, 'open' if lib.bootstrap_look_up(bootstrap, name.encode(), ctypes.byref(port)) == 0 else 'closed')
" `
	const (
		users      = "com.apple.system.opendirectoryd.libinfo"
		resolver   = "com.apple.dnssd.service"
		launch     = "com.apple.coreservices.launchservicesd"
		pasteboard = "com.apple.pasteboard.1"
		keychain   = "com.apple.SecurityServer"
	)
	names := strings.Join([]string{users, resolver, launch, pasteboard, keychain}, " ")
	for _, c := range []struct {
		network bool
		want    map[string]string
	}{
		{false, map[string]string{users: "open", resolver: "closed", launch: "closed", pasteboard: "closed", keychain: "closed"}},
		{true, map[string]string{users: "open", resolver: "open", launch: "closed", pasteboard: "closed", keychain: "closed"}},
	} {
		p.Network = c.network
		out, ok := sh(t, p, project, lookup+names)
		if !ok {
			t.Fatalf("network=%v: lookup failed: %s", c.network, out)
		}
		for name, want := range c.want {
			if !strings.Contains(out, name+" "+want) {
				t.Errorf("network=%v: %s is not %s:\n%s", c.network, name, want, out)
			}
		}
	}
	// A service the user lists opens, and only that one.
	p.Network, p.Services = false, []string{pasteboard}
	if out, _ := sh(t, p, project, lookup+names); !strings.Contains(out, pasteboard+" open") || !strings.Contains(out, launch+" closed") {
		t.Errorf("listing a service did not open it alone:\n%s", out)
	}
	// The services exist: without the sandbox the same lookups succeed, so
	// "closed" above is the sandbox and not a missing service.
	out, ok := sh(t, Policy{}, project, lookup+names)
	if !ok || strings.Contains(out, "closed") {
		t.Skipf("services missing here, nothing to compare against: %s", out)
	}
}

// On macOS a debugger in a command works on what the command started, and
// cannot attach to a process outside the sandbox, which would be running
// code there.
func TestSandboxedDebugger(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Seatbelt only")
	}
	requireBackend(t)
	lldb, err := exec.LookPath("lldb")
	if err != nil {
		t.Skip("no lldb")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p, project, _, _, _ := fixture(t)
	// The test binary running no tests: a program that exits at once.
	launch := lldb + " -b -o run -o quit -- " + self + " -test.run='^$' 2>&1"
	const exited = "exited with status = 0"
	if out, _ := sh(t, Policy{}, project, launch); !strings.Contains(out, exited) {
		t.Skipf("debugging does not work here even without the sandbox: %s", out)
	}
	if out, _ := sh(t, p, project, launch); !strings.Contains(out, exited) {
		t.Errorf("a command could not debug a process it started: %s", out)
	}

	outside := exec.Command(self)
	outside.Env = append(os.Environ(), sleepEnv+"=1")
	if err := outside.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		outside.Process.Kill()
		outside.Wait()
	}()
	attach := lldb + " -b -p " + strconv.Itoa(outside.Process.Pid) + " -o 'process status' -o detach -o quit 2>&1"
	if out, _ := sh(t, p, project, attach); strings.Contains(out, "stopped") {
		t.Errorf("a command attached to a process outside the sandbox: %s", out)
	}
	if out, _ := sh(t, Policy{}, project, attach); !strings.Contains(out, "stopped") {
		t.Logf("attaching fails here without the sandbox too, so the refusal above shows little: %s", out)
	}
}

// On macOS devices are closed to commands, and the GPU setting opens the
// graphics drivers and surfaces without opening the others.
func TestSandboxedGPU(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Seatbelt only")
	}
	requireBackend(t)
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3")
	}
	p, project, _, _, _ := fixture(t)
	// IOServiceOpen on the first service of each class: IOSurfaceRoot is
	// what every GPU client draws to and exists on every Mac, virtual ones
	// included; IOHIDSystem is the keyboard and mouse.
	open := py + ` -c "
import ctypes, sys
iokit = ctypes.CDLL('/System/Library/Frameworks/IOKit.framework/IOKit')
libc = ctypes.CDLL('/usr/lib/libSystem.B.dylib')
iokit.IOServiceMatching.restype = ctypes.c_void_p
iokit.IOServiceGetMatchingService.argtypes = [ctypes.c_uint, ctypes.c_void_p]
task = ctypes.c_uint.in_dll(libc, 'mach_task_self_')
for name in sys.argv[1:]:
    service = iokit.IOServiceGetMatchingService(0, iokit.IOServiceMatching(name.encode()))
    conn = ctypes.c_uint()
    if not service:
        print(name, 'missing')
    else:
        print(name, 'open' if iokit.IOServiceOpen(service, task, 0, ctypes.byref(conn)) == 0 else 'closed')
" IOSurfaceRoot IOHIDSystem`
	free, ok := sh(t, Policy{}, project, open)
	if !ok || !strings.Contains(free, "IOSurfaceRoot open") {
		t.Skipf("no surfaces to open here even without the sandbox: %s", free)
	}
	if out, _ := sh(t, p, project, open); !strings.Contains(out, "IOSurfaceRoot closed") {
		t.Errorf("devices open by default: %s", out)
	}
	p.GPU = true
	out, _ := sh(t, p, project, open)
	if !strings.Contains(out, "IOSurfaceRoot open") {
		t.Errorf("surfaces closed with the GPU allowed: %s", out)
	}
	if strings.Contains(out, "IOHIDSystem open") {
		t.Errorf("the GPU setting opened input devices: %s", out)
	}
}

// On macOS a command can open a pseudo-terminal and run a child on it, as
// script, tmux and expect do.
func TestSandboxedPseudoTerminal(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Seatbelt only")
	}
	requireBackend(t)
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3")
	}
	p, project, _, _, _ := fixture(t)
	p.PTY = true
	script := py + ` -c "
import os, pty
pid, fd = pty.fork()
if pid == 0:
    os.execvp('sh', ['sh', '-c', 'test -t 1 && echo on-a-terminal'])
out = b''
try:
    while True:
        chunk = os.read(fd, 1024)
        if not chunk:
            break
        out += chunk
except OSError:
    pass
print(out.decode().strip())
"`
	if out, ok := sh(t, p, project, script); !ok || !strings.Contains(out, "on-a-terminal") {
		t.Errorf("no pseudo-terminal in the sandbox: %s", out)
	}
	// Off, which is the default, a command gets none.
	p.PTY = false
	if out, ok := sh(t, p, project, script); ok && strings.Contains(out, "on-a-terminal") {
		t.Errorf("a pseudo-terminal without the setting: %s", out)
	}
}

// A terminal window other than malachi's is closed to a command: it can
// neither read what is typed there nor write what is shown.
func TestOtherTerminalsAreClosed(t *testing.T) {
	requireBackend(t)
	master, name := openTestPTY(t)
	if _, err := master.Write([]byte("typed-elsewhere\n")); err != nil {
		t.Fatal(err)
	}
	p, project, _, _, _ := fixture(t)
	if out, ok := sh(t, p, project, "head -n 1 < "+name); ok || strings.Contains(out, "typed-elsewhere") {
		t.Errorf("read another terminal's input: %v %s", ok, out)
	}
	if out, ok := sh(t, p, project, "echo drawn > "+name+" && echo WROTE"); ok || strings.Contains(out, "WROTE") {
		t.Errorf("wrote to another terminal: %v %s", ok, out)
	}
}
