package sandbox

import "strings"

// Profile renders p as a Seatbelt (SBPL) profile. In SBPL the last matching
// rule wins, so the order matters: allow everything, deny all writes,
// re-allow writable roots, then deny hidden paths, which therefore beat
// writable roots. Unix-socket connects are allowed only to an allow-list. Seatbelt matches resolved paths, which is why Policy paths
// are canonical.
func Profile(p Policy) string {
	var b strings.Builder
	b.WriteString("(version 1)\n(allow default)\n(deny file-write*)\n(allow file-write*")
	for _, w := range p.Writable {
		b.WriteString("\n  (subpath " + sbplString(w) + ")")
	}
	// Devices commands write to as a matter of course.
	b.WriteString(`
  (literal "/dev/null") (literal "/dev/zero") (literal "/dev/stdout") (literal "/dev/stderr")
  (literal "/dev/dtracehelper") (regex #"^/dev/tty") (regex #"^/dev/fd/"))
`)
	if len(p.Hidden) > 0 {
		b.WriteString("(deny file-read* file-write*")
		for _, h := range p.Hidden {
			b.WriteString("\n  (subpath " + sbplString(h) + ")")
		}
		b.WriteString(")\n")
	}
	if !p.Network {
		b.WriteString("(deny network* (remote ip))\n")
	}
	if !p.UnixSockets {
		// Local daemons (Docker, ssh-agent, ...) would act outside the
		// sandbox for the command. Sockets the command could have made
		// itself, under writable roots, stay usable, as do name resolution
		// and logging.
		// One rule per path: several filters inside one (remote unix-socket)
		// do not all apply (found by testing).
		b.WriteString("(deny network-outbound (remote unix-socket))\n")
		allow := func(filter string) {
			b.WriteString("(allow network-outbound (remote unix-socket " + filter + "))\n")
		}
		for _, w := range p.Writable {
			allow("(subpath " + sbplString(w) + ")")
		}
		allow(`(literal "/private/var/run/mDNSResponder")`)
		allow(`(literal "/private/var/run/syslog")`)
	}
	return b.String()
}

// sbplString quotes s as an SBPL string literal.
func sbplString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}
