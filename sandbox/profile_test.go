package sandbox

import (
	"strings"
	"testing"
)

func TestProfile(t *testing.T) {
	p := Policy{Enabled: true, Network: false, Writable: []string{"/w"}, Hidden: []string{`/h "q" \b`}}
	prof := Profile(p)
	for _, want := range []string{
		"(deny default)",
		"(allow process-info* (target self))",
		"(allow system-debug mach-priv-task-port (target same-sandbox))",
		`(global-name "com.apple.system.opendirectoryd.libinfo")`,
		`(subpath "/w")`,
		`(deny file-read* file-read-data file-read-xattr file-write*` + "\n" + `  (subpath "/h \"q\" \\b"))`,
		"(deny network* (remote ip))",
	} {
		if !strings.Contains(prof, want) {
			t.Errorf("profile lacks %s:\n%s", want, prof)
		}
	}
	// Nothing is allowed wholesale, and no service that would act for the
	// command outside the sandbox is reachable.
	for _, bad := range []string{"(allow default)", "(allow mach-lookup)", "(allow mach*", "launchservices", "pasteboard", "pboard", "SecurityServer", "appleevent", "iokit"} {
		if strings.Contains(prof, bad) {
			t.Errorf("profile has %s:\n%s", bad, prof)
		}
	}
	// Services that can carry data out are open only with the network.
	for _, svc := range networkServices {
		if strings.Contains(prof, svc) {
			t.Errorf("%s reachable with the network off", svc)
		}
		on := p
		on.Network = true
		if !strings.Contains(Profile(on), `(global-name "`+svc+`")`) {
			t.Errorf("%s unreachable with the network on", svc)
		}
	}
	// The user's extra services are allowed by exact name, quoted.
	extra := p
	extra.Services = []string{"com.apple.testmanagerd", `odd"name`}
	for _, want := range []string{
		`(global-name "com.apple.testmanagerd") (xpc-service-name "com.apple.testmanagerd")`,
		`(global-name "odd\"name")`,
	} {
		if strings.Contains(prof, want) || !strings.Contains(Profile(extra), want) {
			t.Errorf("%s: want it only when the user lists the service:\n%s", want, Profile(extra))
		}
	}
	// Devices are closed, and the GPU setting opens graphics drivers by name.
	gpu := p
	gpu.GPU = true
	for _, want := range []string{`(iokit-user-client-class "AGXDeviceUserClient")`, `(xpc-service-name "com.apple.MTLCompilerService")`} {
		if strings.Contains(prof, want) || !strings.Contains(Profile(gpu), want) {
			t.Errorf("%s: want it only with the GPU allowed", want)
		}
	}
	if strings.Contains(Profile(gpu), "(allow iokit-open)") || strings.Contains(Profile(gpu), "windowserver") {
		t.Errorf("the GPU setting opens more than graphics drivers:\n%s", Profile(gpu))
	}
	// Preferences are written by a daemon outside the sandbox, so only
	// Java's own domain is writable.
	if strings.Count(prof, "user-preference-write") != 1 ||
		!strings.Contains(prof, `(allow user-preference-write (preference-domain "com.apple.java.util.prefs"))`) {
		t.Errorf("preference writes are not limited to Java's domain:\n%s", prof)
	}
	// No terminal device opens unless the user asks for pseudo-terminals.
	withPTY := p
	withPTY.PTY = true
	if strings.Contains(prof, "pseudo-tty") || !strings.Contains(prof, `(deny file-read-data file-write-data file-ioctl (literal "/dev/ptmx") (regex #"^/dev/tty")`) ||
		!strings.Contains(Profile(withPTY), "(allow pseudo-tty)") || strings.Contains(Profile(withPTY), "(deny file-read-data file-write-data file-ioctl") {
		t.Errorf("terminal devices do not follow the pty setting:\n%s", prof)
	}
	// A command registers no service except the one a Chromium browser
	// names after its own pid.
	if strings.Count(prof, "mach-register") != 1 ||
		!strings.Contains(prof, `(global-name-regex #"\.MachPortRendezvousServer\.[0-9]+$")`) {
		t.Errorf("service registration is not limited to the browser rendezvous name:\n%s", prof)
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

	// With the network off the resolver is closed, even when Unix sockets
	// are otherwise allowed: it would carry hostnames out as DNS queries.
	const resolver = `(literal "/private/var/run/mDNSResponder")`
	for _, unix := range []bool{false, true} {
		off := Policy{Enabled: true, UnixSockets: unix, Writable: []string{"/w"}}
		prof := Profile(off)
		if strings.Contains(prof, "(allow network-outbound (remote unix-socket "+resolver+"))") {
			t.Errorf("unixSockets=%v: resolver allowed with the network off", unix)
		}
		if unix && !strings.Contains(prof, "(deny network-outbound (remote unix-socket "+resolver+"))") {
			t.Error("resolver open with the network off and Unix sockets allowed")
		}
	}
}

func TestProfilePrivate(t *testing.T) {
	p := Policy{Enabled: true, Network: true, Writable: []string{"/home/u/proj"},
		Private: []string{"/home/u"}, Readable: []string{"/home/u/.cargo"}, Hidden: []string{"/home/u/.ssh"}}
	prof := Profile(p)
	deny := strings.Index(prof, "(deny file-read-data file-read-xattr\n  (subpath \"/home/u\"))")
	allow := strings.Index(prof, "(allow file-read-data file-read-xattr")
	hidden := strings.Index(prof, "(deny file-read* file-read-data file-read-xattr file-write*")
	if deny < 0 || allow < 0 || hidden < 0 {
		t.Fatalf("profile lacks a read rule:\n%s", prof)
	}
	// The last match wins: deny private, re-allow roots, deny hidden.
	if deny >= allow || allow >= hidden {
		t.Errorf("read rules out of order:\n%s", prof)
	}
	for _, root := range []string{`(subpath "/home/u/.cargo")`, `(subpath "/home/u/proj")`} {
		if !strings.Contains(prof[allow:hidden], root) {
			t.Errorf("read allow lacks %s:\n%s", root, prof)
		}
	}
	// Metadata stays readable, so walking up through the home works.
	if strings.Contains(prof, "file-read-metadata") {
		t.Error("private metadata denied")
	}

	// With no roots at all, an allow with no filters would allow everything.
	bare := Policy{Enabled: true, Private: []string{"/home/u"}}
	if strings.Contains(Profile(bare), "(allow file-read-data file-read-xattr)") {
		t.Errorf("unfiltered read allow:\n%s", Profile(bare))
	}
	if strings.Contains(Profile(Policy{Enabled: true}), "file-read-data file-read-xattr") {
		t.Error("read rules without private directories")
	}
}
