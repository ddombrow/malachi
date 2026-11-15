package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

// sandboxExec is macOS's Seatbelt front end. It is deprecated but has no
// per-process replacement, and other coding agents rely on it too; if it
// disappears, Available says so and commands fail closed.
const sandboxExec = "/usr/bin/sandbox-exec"

var availability = sync.OnceValue(func() error {
	out, err := exec.Command(sandboxExec, "-p", "(version 1)(allow default)", "/usr/bin/true").CombinedOutput()
	if err == nil {
		return nil
	}
	msg := strings.TrimSpace(string(out))
	if strings.Contains(msg, "sandbox_apply: Operation not permitted") {
		// Seatbelt does not nest: malachi under malachi, or under another
		// tool's sandbox.
		return errors.New("malachi is already running inside a sandbox, and macOS sandboxes do not nest")
	}
	return fmt.Errorf("sandbox-exec does not work here (%v: %s)", err, msg)
})

// Available reports why commands cannot be sandboxed here, or nil.
func Available() error { return availability() }

// Weakness says what the sandbox cannot do for p on this system though it
// runs, or "" when it does everything it promises.
func Weakness(p Policy) string {
	if LoopbackInForce(p) {
		return "the loopback on macOS is shared with everything you run: commands can reach every service listening on localhost, " +
			"and a server they start on all interfaces can be reached from other machines"
	}
	return ""
}

// LoopbackInForce reports whether commands run under p get loopback
// networking with the network off. On macOS only when the user asked for
// it: the loopback is the host's own, so it is not had by default (see
// Policy.Loopback).
func LoopbackInForce(p Policy) bool {
	return p.Enabled && p.Loopback && p.LoopbackAsked && !p.Network
}

func command(ctx context.Context, p Policy, name string, args ...string) (*exec.Cmd, error) {
	return exec.CommandContext(ctx, sandboxExec, append([]string{"-p", Profile(p), name}, args...)...), nil
}
