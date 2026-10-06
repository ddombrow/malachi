package sandbox

import (
	"context"
	"os/exec"
)

// Command returns a command running name with args under p. With p disabled
// it is a plain exec.CommandContext. With p enabled and no backend here, it
// fails rather than run unconfined: the user asked for a sandbox.
func Command(ctx context.Context, p Policy, name string, args ...string) (*exec.Cmd, error) {
	if !p.Enabled {
		return exec.CommandContext(ctx, name, args...), nil
	}
	if err := Available(); err != nil {
		return nil, err
	}
	return command(ctx, p, name, args...)
}
