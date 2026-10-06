//go:build !darwin

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

func command(context.Context, Policy, string, ...string) (*exec.Cmd, error) {
	return nil, Available()
}
