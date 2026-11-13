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

// Weakness says what the sandbox cannot enforce on this system though it
// runs, or "" when it enforces everything it promises.
func Weakness() string { return "" }

func command(context.Context, Policy, string, ...string) (*exec.Cmd, error) {
	return nil, Available()
}
