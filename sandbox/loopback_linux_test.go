//go:build linux

package sandbox

import (
	"context"
	"net"
	"os"
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
