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
	if strings.Contains(Profile(p), "network") {
		t.Error("network denied with the network on")
	}
}
