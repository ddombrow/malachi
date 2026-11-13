package coding

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/ddombrow/malachi/sandbox"
)

// Notice is what a frontend says at startup about a sandbox that is not
// protecting anything, or not everything it promises, or "" when it is.
func (st SandboxState) Notice() string {
	switch {
	case !st.Enabled:
		return "Sandbox off: commands and file tools can reach everything you can."
	case st.Unavailable != nil:
		return fmt.Sprintf("Sandbox unavailable: %v. Commands will fail rather than run unconfined; run with -sandbox off to allow them anyway.", st.Unavailable)
	case st.Weakness != "":
		return fmt.Sprintf("Sandbox limited: %s.", st.Weakness)
	}
	return ""
}

// Marker is a word or two for a status bar when the sandbox is not fully in
// force, or "".
func (st SandboxState) Marker() string {
	switch {
	case !st.Enabled:
		return "unsandboxed"
	case st.Unavailable != nil:
		return "sandbox unavailable"
	case st.Network:
		return "network on"
	}
	return ""
}

// Describe renders the sandbox in force for a user.
func (st SandboxState) Describe() string {
	if n := st.Notice(); n != "" && !st.Enabled {
		return n + "\n  set sandbox.enabled in settings.json, or drop -sandbox off, to turn it on"
	}
	var b strings.Builder
	switch {
	case st.Unavailable != nil:
		b.WriteString(st.Notice() + "\n")
	default:
		b.WriteString("Sandbox on: commands run confined, and the file tools follow the same rules.\n")
		if st.Weakness != "" {
			b.WriteString(st.Notice() + "\n")
		}
	}
	network := "on"
	if !st.Network {
		network = "off"
	}
	sockets := "only under writable directories (macOS); none (Linux)"
	if st.UnixSockets {
		sockets = "any (local daemons such as Docker are reachable)"
	}
	fmt.Fprintf(&b, "  network: %s\n  unix sockets: %s\n  writable:\n", network, sockets)
	for _, w := range existing(st.Writable) {
		b.WriteString("    " + w + "\n")
	}
	if len(st.Private) == 0 {
		b.WriteString("  readable: everything not hidden\n")
	} else {
		fmt.Fprintf(&b, "  readable in %s: only these and the writable directories\n", strings.Join(st.Private, ", "))
		for _, r := range existing(st.Readable) {
			b.WriteString("    " + r + "\n")
		}
	}
	b.WriteString("  hidden (no reading or writing):\n")
	for _, h := range st.Hidden {
		b.WriteString("    " + h + "\n")
	}
	b.WriteString("  /allow-read and /allow-write add directories for this session; settings.json \"sandbox\" changes the defaults")
	return b.String()
}

// SandboxGrants is the sandbox's roots arranged for display. Read and Write
// are the ones a user looks for: the project, its repository, and roots from
// settings or granted this session. Built-in defaults (toolchains, caches,
// temp directories) are only counted, since every session has them.
type SandboxGrants struct {
	Read, Write               []string
	BuiltinRead, BuiltinWrite int
}

// Grants arranges st's roots for display. It resolves the defaults afresh
// and checks which roots exist, so a frontend computes it when the sandbox
// changes, not every frame.
func (st SandboxState) Grants() SandboxGrants {
	base := sandbox.Build(sandbox.Config{})
	var g SandboxGrants
	g.Read, g.BuiltinRead = splitBuiltin(existing(st.Readable), base.Readable)
	g.Write, g.BuiltinWrite = splitBuiltin(existing(st.Writable), base.Writable)
	return g
}

// existing keeps the roots that exist. The defaults name every common
// toolchain and cache location, granted whether or not it exists so a tool
// installed mid-session works at once; displays leave out the missing ones,
// which would only be noise.
func existing(roots []string) []string {
	return slices.DeleteFunc(slices.Clone(roots), func(r string) bool {
		_, err := os.Stat(r)
		return err != nil
	})
}

// splitBuiltin separates roots into those not in builtin and a count of
// those that are. A shown root inside another shown root (the project inside
// its repository) adds nothing and is left out.
func splitBuiltin(roots, builtin []string) (shown []string, n int) {
	for _, r := range roots {
		if slices.Contains(builtin, r) {
			n++
		} else {
			shown = append(shown, r)
		}
	}
	return slices.DeleteFunc(slices.Clone(shown), func(r string) bool {
		return slices.ContainsFunc(shown, func(o string) bool { return o != r && withinPath(r, o) })
	}), n
}
