package sandbox

import (
	"slices"
	"strings"
)

// baseServices are the system services every command may look up: the ones
// libSystem itself uses for user and group lookups, per-user temporary and
// cache directories, logging, notifications and preferences, plus
// file-change events for watchers. Without the membership and dirhelper
// services the Swift compiler cannot write its module cache. xcodebuild
// reads LaunchServices' database of file types to recognise a project, and
// holds off sleep while it builds; reading that database opens nothing
// (launchservicesd does, and is not here). Anything else
// (LaunchServices, the pasteboard, the keychain daemon, AppleEvents, ...)
// would act outside the sandbox for the command, and stays closed.
var baseServices = []string{
	"com.apple.system.opendirectoryd.libinfo",
	"com.apple.system.opendirectoryd.membership",
	"com.apple.bsd.dirhelper",
	"com.apple.system.notification_center",
	"com.apple.system.logger",
	"com.apple.logd",
	"com.apple.cfprefsd.daemon",
	"com.apple.cfprefsd.agent",
	"com.apple.FSEvents",
	"com.apple.lsd.mapdb",
	"com.apple.PowerManagement.control",
}

// networkServices are looked up only with the network on: name resolution,
// the network configuration, and certificate trust. Each of them can carry
// data out (a hostname as a DNS query, a certificate's issuer URL as a
// fetch), so they are closed with the network.
var networkServices = []string{
	"com.apple.dnssd.service",
	"com.apple.SystemConfiguration.configd",
	"com.apple.SystemConfiguration.DNSConfiguration",
	"com.apple.trustd.agent",
}

// gpuDrivers are the IOKit clients a command may open with Policy.GPU: the
// graphics drivers and the surfaces they draw to, by name, so the rest of
// IOKit (input devices, USB, power, displays) stays closed. The first two
// are what Apple silicon needs, found by testing rendering and compute off
// screen on an M4. The rest are the Intel and AMD drivers of older Macs, as
// Chromium's GPU process allows them; they are untested here.
var gpuDrivers = []string{
	`(iokit-user-client-class "AGXDeviceUserClient")`,
	`(iokit-user-client-class "IOSurfaceRootUserClient")`,
	`(iokit-connection "IOAccelerator")`,
	`(iokit-user-client-class "IOAccelerationUserClient")`,
	`(iokit-user-client-class "IOSurfaceSendRight")`,
	`(iokit-user-client-class "AGPMClient")`,
	`(iokit-user-client-class "AppleGraphicsControlClient")`,
	`(iokit-user-client-class "AppleGraphicsPolicyClient")`,
	`(iokit-user-client-class "AppleMGPUPowerControlClient")`,
	`(iokit-user-client-class "AppleSNBFBUserClient")`,
}

// gpuServices compile shaders for a command with Policy.GPU: Metal's
// compiler, and OpenGL's on Intel Macs (untested here).
var gpuServices = []string{
	`(xpc-service-name "com.apple.MTLCompilerService")`,
	`(global-name "com.apple.cvmsServ")`,
}

// Profile renders p as a Seatbelt (SBPL) profile. It denies by default and
// allows what commands need: running and signalling processes, reading
// system information, local IPC, a short list of system services, then
// files and sockets by the policy. What it leaves out is the point: a
// command cannot ask another program to act for it (open a URL, read the
// clipboard, send an AppleEvent) or talk to devices (IOKit; Policy.GPU opens
// the graphics drivers alone). A debugger
// works on what the command itself started, and on nothing else: attaching
// to a process outside the sandbox would be running code there.
//
// Preferences are read-only, since the preferences daemon would write any
// program's settings for the command, except Java's own store: without it
// anything using java.util.prefs (jshell, for one) fails.
//
// In SBPL the last matching rule wins, so the order of the file rules
// matters: allow all reads, allow writes under writable roots, deny reading
// private directories, re-allow readable and writable roots, then deny
// hidden paths, which therefore beat everything.
//
// Reading a private directory's contents is denied, not its metadata: like
// Landlock, which cannot restrict stat, a command can tell that a path
// exists but not list or read it. Walking up through the home directory to a
// project (getcwd, git looking for .git) keeps working. Unix-socket connects
// are allowed only to an allow-list. Seatbelt matches resolved paths, which
// is why Policy paths are canonical.
// rendezvous lets a Chromium-based browser (headless Chrome, Electron)
// start: the browser process registers a service named after its own pid,
// and its helper processes look that name up to get their ports. Seatbelt
// cannot say "this process's pid", so the rule also covers the same name
// registered by a browser outside the sandbox, which answers only its own
// children. The pattern is anchored at the end only: a leading character
// class made Seatbelt match every name.
const rendezvous = `(allow mach-register mach-lookup
  (global-name-regex #"\.MachPortRendezvousServer\.[0-9]+$"))
`

func Profile(p Policy) string {
	var b strings.Builder
	b.WriteString(`(version 1)
(deny default)
(allow process-exec* process-fork signal)
(allow process-info* (target self))
(allow system-debug mach-priv-task-port (target same-sandbox))
(allow sysctl-read)
(allow ipc-posix* ipc-sysv*)
(allow user-preference-read)
(allow user-preference-write (preference-domain "com.apple.java.util.prefs"))
(allow system-socket)
(allow mach-lookup`)
	services := baseServices
	if p.Network {
		services = slices.Concat(services, networkServices)
	}
	for _, s := range services {
		b.WriteString("\n  (global-name " + sbplString(s) + ")")
	}
	b.WriteString(")\n")
	b.WriteString(rendezvous)
	if len(p.Services) > 0 {
		// A service is registered under a global name or as an XPC
		// service; the user gives the name, and both kinds are allowed.
		b.WriteString("(allow mach-lookup")
		for _, s := range p.Services {
			b.WriteString("\n  (global-name " + sbplString(s) + ") (xpc-service-name " + sbplString(s) + ")")
		}
		b.WriteString(")\n")
	}
	if p.GPU {
		b.WriteString("(allow iokit-open\n  " + strings.Join(gpuDrivers, "\n  ") + ")\n")
		b.WriteString("(allow mach-lookup\n  " + strings.Join(gpuServices, "\n  ") + ")\n")
	}
	b.WriteString("(allow file-read* file-ioctl)\n(allow file-write*")
	for _, w := range p.Writable {
		b.WriteString("\n  (subpath " + sbplString(w) + ")")
	}
	// Devices commands write to as a matter of course.
	b.WriteString(`
  (literal "/dev/null") (literal "/dev/zero") (literal "/dev/stdout") (literal "/dev/stderr")
  (literal "/dev/dtracehelper") (regex #"^/dev/fd/"))
`)
	// Terminals. A pseudo-terminal a command makes for its children
	// (script, tmux, expect) is a /dev/ttysNNN like the user's other
	// terminal windows, and Seatbelt cannot tell them apart (found by
	// testing): what opens one opens the others, where a command could
	// read what is typed and write what is shown. So no terminal device
	// opens unless the user asks for pseudo-terminals. Reads are named
	// because the allow above covers them.
	if p.PTY {
		b.WriteString("(allow pseudo-tty)\n" + `(allow file-write* (literal "/dev/ptmx") (regex #"^/dev/tty"))` + "\n")
	} else {
		b.WriteString(`(deny file-read-data file-write-data file-ioctl (literal "/dev/ptmx") (regex #"^/dev/tty") (regex #"^/dev/pty"))` + "\n")
	}
	if len(p.Private) > 0 {
		b.WriteString("(deny file-read-data file-read-xattr")
		for _, d := range p.Private {
			b.WriteString("\n  (subpath " + sbplString(d) + ")")
		}
		b.WriteString(")\n")
		// An allow with no filters would allow everything.
		if roots := slices.Concat(p.Readable, p.Writable); len(roots) > 0 {
			b.WriteString("(allow file-read-data file-read-xattr")
			for _, r := range roots {
				b.WriteString("\n  (subpath " + sbplString(r) + ")")
			}
			b.WriteString(")\n")
		}
	}
	if len(p.Hidden) > 0 {
		// The two operations are named again because an allow that names an
		// operation beats a later deny that only covers it by wildcard: with
		// file-read* alone, a hidden path inside a readable root stays readable.
		b.WriteString("(deny file-read* file-read-data file-read-xattr file-write*")
		for _, h := range p.Hidden {
			b.WriteString("\n  (subpath " + sbplString(h) + ")")
		}
		b.WriteString(")\n")
	}
	// Sockets: everything, less what the policy closes below.
	b.WriteString("(allow network*)\n")
	if !p.Network {
		b.WriteString("(deny network* (remote ip))\n")
		// The rule above is matched by the far end, so it does not cover
		// listening: a command could bind every interface and be connected
		// to from another machine. Seatbelt cannot tell a loopback address
		// from any other when binding (found by testing), so with the
		// network off a command listens on no IP address at all, as on
		// Linux. Unix sockets are not matched by (local ip).
		b.WriteString("(deny network-bind network-inbound (local ip))\n")
		if p.UnixSockets {
			// Unix sockets are otherwise unrestricted; the resolver must
			// still be closed (see below).
			b.WriteString(`(deny network-outbound (remote unix-socket (literal "/private/var/run/mDNSResponder")))` + "\n")
		}
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
		if p.Network {
			// Name resolution. With the network off it is not needed, and
			// would leave a channel out: the resolver forwards the
			// command's chosen hostnames as DNS queries.
			allow(`(literal "/private/var/run/mDNSResponder")`)
		}
		allow(`(literal "/private/var/run/syslog")`)
	}
	return b.String()
}

// sbplString quotes s as an SBPL string literal.
func sbplString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}
