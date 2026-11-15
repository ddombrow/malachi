//go:build linux

package sandbox

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A command with the loopback setting serves and connects to itself, but
// the host's own localhost services are out of its reach.
func TestPrivateLoopbackIsolatedFromHost(t *testing.T) {
	requireBackend(t)
	requireLoopbackBackend(t)
	host, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()

	script := `import socket, sys
s = socket.socket()
s.bind(("127.0.0.1", 0))
s.listen()
c = socket.create_connection(s.getsockname(), 2)
a, _ = s.accept()
c.close(); a.close(); s.close()
try:
    socket.create_connection(("127.0.0.1", int(sys.argv[1])), 0.5).close()
    sys.exit("reached a service on the host's loopback")
except OSError:
    print("ok")
`
	project, home := t.TempDir(), t.TempDir()
	path := filepath.Join(project, "loopback.py")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	p := Build(Config{Cwd: project, Home: home, NoNet: true, Loopback: true})
	cmd, err := command(context.Background(), p, "python3", path, strconv.Itoa(host.Addr().(*net.TCPAddr).Port))
	if err != nil {
		t.Fatal(err)
	}
	if out, err := cmd.CombinedOutput(); err != nil || string(out) != "ok\n" {
		t.Fatalf("loopback command: %v\n%s", err, out)
	}
}

// Commands in a session share one private loopback namespace. This is what lets
// a background server started by one command be reached by a later command.
func TestLoopbackSessionSharesLoopback(t *testing.T) {
	requireBackend(t)
	requireLoopbackBackend(t)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3")
	}
	project, home := t.TempDir(), t.TempDir()
	portFile := filepath.Join(project, "port")
	stopFile := filepath.Join(project, "stopped")
	p := Build(Config{Cwd: project, Home: home, NoNet: true, Loopback: true})
	session, err := NewLoopbackSession(p)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	server := `import signal, socket, sys
s = socket.socket()
s.bind(("127.0.0.1", 0))
s.listen()
open(sys.argv[1], "w").write(str(s.getsockname()[1]))
signal.signal(signal.SIGTERM, lambda *_: open(sys.argv[2], "w").close())
while True:
    c, _ = s.accept()
    c.sendall(b"shared")
    c.close()
`
	start, err := session.Command(context.Background(), p, "sh", "-c", "python3 -c '"+server+"' "+portFile+" "+stopFile+" >/dev/null 2>&1 &")
	if err != nil {
		t.Fatal(err)
	}
	start.Dir = project
	if out, err := start.CombinedOutput(); err != nil {
		t.Fatalf("start server: %v\n%s", err, out)
	}
	var port string
	for i := 0; i < 100; i++ {
		if b, err := os.ReadFile(portFile); err == nil {
			port = string(b)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if port == "" {
		t.Fatal("server did not publish its port")
	}
	client, err := session.Command(context.Background(), p, "python3", "-c", `import socket,sys
s=socket.create_connection(("127.0.0.1", int(sys.argv[1])), 2)
print(s.recv(32).decode())`, port)
	if err != nil {
		t.Fatal(err)
	}
	client.Dir = project
	if out, err := client.CombinedOutput(); err != nil || string(out) != "shared\n" {
		t.Fatalf("connect from later command: %v\n%s", err, out)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		if _, err := os.Stat(stopFile); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("session close did not stop its background server")
}

// The session helper is the subreaper for its commands, so a process one of
// them left behind in a session of its own (a daemon, a job's server once
// its shell is gone) is the helper's to reap when it ends, or it stays a
// zombie that kill(pid, 0) still finds.
func TestLoopbackSessionReapsOrphans(t *testing.T) {
	requireBackend(t)
	requireLoopbackBackend(t)
	project, home := t.TempDir(), t.TempDir()
	p := Build(Config{Cwd: project, Home: home, NoNet: true, Loopback: true})
	session, err := NewLoopbackSession(p)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	cmd, err := session.Command(context.Background(), p, "sh", "-c", "setsid -f sh -c 'sleep 0.2 & echo $!' ; sleep 0.1")
	if err != nil {
		t.Fatal(err)
	}
	cmd.Dir = project
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	pid := strings.TrimSpace(string(out))
	if pid == "" {
		t.Fatal("no pid printed")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		stat, err := os.ReadFile("/proc/" + pid + "/stat")
		if err != nil {
			return // gone, reaped
		}
		if time.Now().After(deadline) {
			t.Fatalf("orphan %s was never reaped: %s", pid, stat)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The session helper starts commands under whatever policy it is sent, so a
// command must have no way to send it one: no socket with a path to connect
// to, and no descriptor of the helper's inherited.
func TestLoopbackSessionIsUnreachableFromCommands(t *testing.T) {
	requireBackend(t)
	requireLoopbackBackend(t)
	project, home := t.TempDir(), t.TempDir()
	p := Build(Config{Cwd: project, Home: home, NoNet: true, Loopback: true})
	session, err := NewLoopbackSession(p)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	cmd, err := session.Command(context.Background(), p, "sh", "-c", `for f in /proc/$$/fd/*; do readlink "$f"; done; true`)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Dir = project
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Contains(string(out), "socket:") {
		t.Fatalf("a command in the session holds a socket:\n%s", out)
	}
}

// A command in the session ends, to whoever waits on it, the way it did:
// by its exit code, or by the signal that killed it.
func TestLoopbackSessionPassesOnHowCommandsEnd(t *testing.T) {
	requireBackend(t)
	requireLoopbackBackend(t)
	project, home := t.TempDir(), t.TempDir()
	p := Build(Config{Cwd: project, Home: home, NoNet: true, Loopback: true})
	session, err := NewLoopbackSession(p)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	end := func(script string) *os.ProcessState {
		t.Helper()
		cmd, err := session.Command(context.Background(), p, "sh", "-c", script)
		if err != nil {
			t.Fatal(err)
		}
		cmd.Dir = project
		_ = cmd.Run()
		return cmd.ProcessState
	}
	if st := end("exit 7"); st.ExitCode() != 7 {
		t.Fatalf("exit 7 ended as %v", st)
	}
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL, syscall.SIGINT} {
		st := end("kill -" + strconv.Itoa(int(sig)) + " $$")
		if ws := st.Sys().(syscall.WaitStatus); !ws.Signaled() || ws.Signal() != sig {
			t.Fatalf("killed by %v, ended as %v", sig, st)
		}
	}
}

// Where no private namespace can be made, the setting is reported as having
// no effect, rather than commands failing.
func TestWeaknessMentionsLoopbackOnlyWhenUnavailable(t *testing.T) {
	got := Weakness(Policy{Enabled: true, Loopback: true, LoopbackAsked: true})
	if mentions := strings.Contains(got, "loopback"); mentions != (privateLoopbackAvailable() != nil) {
		t.Errorf("Weakness = %q with the namespace available: %v", got, privateLoopbackAvailable() == nil)
	}
	if got := Weakness(Policy{Enabled: true, Network: true, Loopback: true, LoopbackAsked: true}); strings.Contains(got, "loopback") {
		t.Errorf("Weakness with the network on = %q", got)
	}
	// On by default and not asked for, its absence is not announced.
	if got := Weakness(Policy{Enabled: true, Loopback: true}); strings.Contains(got, "loopback") {
		t.Errorf("Weakness for a loopback nobody asked for = %q", got)
	}
}

// Unlike Weakness, LoopbackMissing answers for a loopback on by default too:
// it is what a failed command's hint uses to say why it had none.
func TestLoopbackMissingSaysWhy(t *testing.T) {
	for _, p := range []Policy{{Enabled: true, Loopback: true}, {Enabled: true, Loopback: true, LoopbackAsked: true}} {
		if err := LoopbackMissing(p); (err != nil) != (privateLoopbackAvailable() != nil) {
			t.Errorf("LoopbackMissing(%+v) = %v with the namespace available: %v", p, err, privateLoopbackAvailable() == nil)
		}
	}
	for _, p := range []Policy{{Enabled: true}, {Enabled: true, Network: true, Loopback: true}, {Loopback: true}} {
		if err := LoopbackMissing(p); err != nil {
			t.Errorf("LoopbackMissing(%+v) = %v, though no loopback was to be had", p, err)
		}
	}
}

// Both settings at once, for a server run on a terminal of its own (under
// tmux, say): the command gets its own pseudo-terminals and its own
// loopback, and no capability from either namespace survives into it.
func TestPrivateLoopbackWithPseudoTerminals(t *testing.T) {
	requireBackend(t)
	requirePTYBackend(t)
	requireLoopbackBackend(t)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3")
	}
	host, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()

	// The server answers on loopback from a child whose output goes to a
	// pseudo-terminal, as one started in a terminal multiplexer would.
	script := `import os, pty, socket, sys
s = socket.socket()
s.bind(("127.0.0.1", 0))
s.listen()
port = s.getsockname()[1]
pid, fd = pty.fork()
if pid == 0:
    a, _ = s.accept()
    a.sendall(b"served")
    os.write(1, b"on a terminal: %r" % os.isatty(1))
    os._exit(0)
s.close()
c = socket.create_connection(("127.0.0.1", port), 2)
got = c.recv(16)
c.close()
child = os.read(fd, 64)
os.waitpid(pid, 0)
assert got == b"served", got
assert b"on a terminal: True" in child, child
try:
    socket.create_connection(("127.0.0.1", int(sys.argv[1])), 0.5).close()
    sys.exit("reached a service on the host's loopback")
except OSError:
    pass
for line in open("/proc/self/status"):
    if line.startswith(("CapInh", "CapPrm", "CapEff", "CapAmb")) and not line.rstrip().endswith("0" * 16):
        sys.exit("kept a capability: " + line)
print("ok")
`
	p, project, _, _, _ := fixture(t)
	p.Network, p.Loopback, p.PTY = false, true, true
	path := filepath.Join(project, "both.py")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(host.Addr().(*net.TCPAddr).Port)
	if out, ok := sh(t, p, project, "python3 "+path+" "+port); !ok || out != "ok" {
		t.Fatalf("server on a terminal and loopback: %s", out)
	}
	// And the namespaces are still the ones the command was given: its
	// terminals are its own, and so is its network.
	if out, _ := sh(t, p, project, "ls /dev/pts"); out != "ptmx" {
		t.Errorf("/dev/pts should start empty but for ptmx, got: %s", out)
	}
	// /proc/net follows the reader's network namespace; /sys/class/net
	// would show the one sysfs was mounted in.
	if out, _ := sh(t, p, project, "awk 'NR > 2 { print $1 }' /proc/net/dev"); out != "lo:" {
		t.Errorf("interfaces other than loopback: %s", out)
	}
}
