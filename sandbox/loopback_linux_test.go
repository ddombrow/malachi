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
	"testing"
)

// A command with the loopback setting serves and connects to itself, but
// the host's own localhost services are out of its reach.
func TestPrivateLoopbackIsolatedFromHost(t *testing.T) {
	requireBackend(t)
	if err := privateLoopbackAvailable(); err != nil {
		t.Skipf("no private network namespace here: %v", err)
	}
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

// Where no private namespace can be made, the setting is reported as having
// no effect, rather than commands failing.
func TestWeaknessMentionsLoopbackOnlyWhenUnavailable(t *testing.T) {
	got := Weakness(Policy{Enabled: true, Loopback: true})
	if mentions := strings.Contains(got, "loopback"); mentions != (privateLoopbackAvailable() != nil) {
		t.Errorf("Weakness = %q with the namespace available: %v", got, privateLoopbackAvailable() == nil)
	}
	if got := Weakness(Policy{Enabled: true, Network: true, Loopback: true}); strings.Contains(got, "loopback") {
		t.Errorf("Weakness with the network on = %q", got)
	}
}

// Both settings at once, for a server run on a terminal of its own (under
// tmux, say): the command gets its own pseudo-terminals and its own
// loopback, and no capability from either namespace survives into it.
func TestPrivateLoopbackWithPseudoTerminals(t *testing.T) {
	requireBackend(t)
	requirePTYBackend(t)
	if err := privateLoopbackAvailable(); err != nil {
		t.Skipf("no private network namespace here: %v", err)
	}
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
