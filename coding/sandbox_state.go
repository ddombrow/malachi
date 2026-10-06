package coding

import (
	"fmt"
	"strings"
)

// Notice is what a frontend says at startup about a sandbox that is not
// protecting anything, or "" when it is.
func (st SandboxState) Notice() string {
	switch {
	case !st.Enabled:
		return "Sandbox off: commands and file tools can reach everything you can."
	case st.Unavailable != nil:
		return fmt.Sprintf("Sandbox unavailable: %v. Commands will fail rather than run unconfined; run with -sandbox off to allow them anyway.", st.Unavailable)
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
	case !st.Network:
		return "offline"
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
	for _, w := range st.Writable {
		b.WriteString("    " + w + "\n")
	}
	b.WriteString("  hidden (no reading or writing):\n")
	for _, h := range st.Hidden {
		b.WriteString("    " + h + "\n")
	}
	b.WriteString("  change these in settings.json under \"sandbox\"")
	return b.String()
}
