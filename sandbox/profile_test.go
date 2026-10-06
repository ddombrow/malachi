package sandbox

import (
	"strings"
	"testing"
)

func TestProfile(t *testing.T) {
	p := Policy{Enabled: true, Network: false, Writable: []string{"/w"}, Hidden: []string{`/h "q" \b`}}
	prof := Profile(p)
	for _, want := range []string{
		"(allow default)",
		"(deny file-write*)",
		`(subpath "/w")`,
		`(deny file-read* file-write*` + "\n" + `  (subpath "/h \"q\" \\b"))`,
		"(deny network* (remote ip))",
	} {
		if !strings.Contains(prof, want) {
			t.Errorf("profile lacks %s:\n%s", want, prof)
		}
	}
	// Hidden must come after the writable allows: the last match wins.
	if strings.Index(prof, "(deny file-read*") < strings.Index(prof, `(subpath "/w")`) {
		t.Error("hidden paths precede writable roots")
	}
	p.Network = true
	if strings.Contains(Profile(p), "(remote ip)") {
		t.Error("network denied with the network on")
	}
	// Unix-socket connects are limited to an allow-list unless allowed.
	if !strings.Contains(Profile(p), "(deny network-outbound (remote unix-socket))") {
		t.Error("Unix sockets unrestricted by default")
	}
	// One allow per path: Seatbelt does not apply several in one rule.
	if !strings.Contains(Profile(p), `(allow network-outbound (remote unix-socket (subpath "/w")))`) ||
		!strings.Contains(Profile(p), `(allow network-outbound (remote unix-socket (literal "/private/var/run/mDNSResponder")))`) {
		t.Errorf("socket allow-list not one rule per path:\n%s", Profile(p))
	}
	p.UnixSockets = true
	if strings.Contains(Profile(p), "unix-socket") {
		t.Error("Unix sockets restricted when allowed")
	}
}
