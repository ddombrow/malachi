//go:build darwin

package sandbox

import (
	"strings"
	"testing"
)

// macOS has no loopback private to a command, so the setting changes
// nothing in the profile, and the sandbox says it has no effect.
func TestLoopbackHasNoEffectOnMacOS(t *testing.T) {
	off := Policy{Enabled: true}
	on := Policy{Enabled: true, Loopback: true}
	if Profile(on) != Profile(off) {
		t.Error("the loopback setting changed the profile")
	}
	if LoopbackInForce(on) {
		t.Error("loopback reported in force")
	}
	if w := Weakness(on); !strings.Contains(w, "loopback setting has no effect") {
		t.Errorf("Weakness = %q, want it to say loopback has no effect", w)
	}
	if w := Weakness(Policy{Enabled: true, Network: true, Loopback: true}); w != "" {
		t.Errorf("Weakness with the network on = %q", w)
	}
}
