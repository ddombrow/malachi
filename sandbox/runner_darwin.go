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

func command(ctx context.Context, p Policy, name string, args ...string) (*exec.Cmd, error) {
	return exec.CommandContext(ctx, sandboxExec, append([]string{"-p", Profile(p), name}, args...)...), nil
}
