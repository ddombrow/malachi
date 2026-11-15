//go:build !darwin && !linux

package sandbox

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
)

// Available reports why commands cannot be sandboxed here, or nil.
func Available() error {
	return errors.New("no sandbox backend for " + runtime.GOOS)
}

// LoopbackInForce reports whether commands run under p get a private
// loopback; there are no sandboxed commands here.
func LoopbackInForce(Policy) bool { return false }

// LoopbackMissing reports why commands run under p get no loopback though
// the setting gives them one, or nil. Commands do not run here at all.
func LoopbackMissing(Policy) error { return nil }

// Weakness says what the sandbox cannot do for p on this system though it
// runs, or "" when it does everything it promises.
func Weakness(Policy) string { return "" }

func command(context.Context, Policy, string, ...string) (*exec.Cmd, error) {
	return nil, Available()
}
