//go:build darwin

package sandbox

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// macOS's loopback is the host's own, so the setting takes effect only when
// the user asked for it, and the sandbox then says what it opens.
func TestLoopbackOnlyWhenAskedOnMacOS(t *testing.T) {
	off := Policy{Enabled: true}
	byDefault := Policy{Enabled: true, Loopback: true}
	asked := Policy{Enabled: true, Loopback: true, LoopbackAsked: true}
	if Profile(byDefault) != Profile(off) {
		t.Error("a loopback nobody asked for changed the profile")
	}
	if LoopbackInForce(byDefault) || Weakness(byDefault) != "" {
		t.Errorf("a loopback nobody asked for: in force %v, weakness %q", LoopbackInForce(byDefault), Weakness(byDefault))
	}
	if Profile(asked) == Profile(off) || !LoopbackInForce(asked) {
		t.Error("an asked-for loopback is not in force")
	}
	if w := Weakness(asked); !strings.Contains(w, "every service listening on localhost") || !strings.Contains(w, "other machines") {
		t.Errorf("Weakness = %q, want it to say what the shared loopback opens", w)
	}
	withNetwork := Policy{Enabled: true, Network: true, Loopback: true, LoopbackAsked: true}
	if LoopbackInForce(withNetwork) || Weakness(withNetwork) != "" {
		t.Error("loopback reported with the network on, where it means nothing")
	}
}

// Asked for, the loopback reaches the host's localhost services and nothing
// beyond it.
func TestLoopbackReachesLocalhostOnlyOnMacOS(t *testing.T) {
	requireBackend(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p, project, _, _, _ := fixture(t)
	p.Readable = append(p.Readable, filepath.Dir(self))
	p.Network = false
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	dial := func(addr string) (string, bool) {
		return sh(t, p, project, dialEnv+"='tcp "+addr+"' "+self)
	}
	host := l.Addr().String()
	if out, ok := dial(host); ok {
		t.Fatalf("the network off, a host service was reached: %s", out)
	}
	p.Loopback, p.LoopbackAsked = true, true
	if out, ok := dial(host); !ok {
		t.Errorf("loopback: a localhost service was refused: %s", out)
	}
	// Refused by the sandbox at once, whether or not anything listens there.
	if out, _ := dial("192.0.2.1:80"); !strings.Contains(out, "operation not permitted") {
		t.Errorf("loopback: an address beyond localhost was not refused by the sandbox: %s", out)
	}
}
