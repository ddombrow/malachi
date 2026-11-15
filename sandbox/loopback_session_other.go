//go:build !linux

package sandbox

import (
	"context"
	"errors"
	"os/exec"
)

// LoopbackSession is unavailable on platforms without private network namespaces.
type LoopbackSession struct{}

func NewLoopbackSession(Policy) (*LoopbackSession, error) {
	return nil, errors.ErrUnsupported
}

func (*LoopbackSession) Command(context.Context, Policy, string, ...string) (*exec.Cmd, error) {
	return nil, errors.ErrUnsupported
}

func (*LoopbackSession) Close() error { return nil }

func runSessionHelper() error { return errors.ErrUnsupported }

func runSessionClient(string) error { return errors.ErrUnsupported }
