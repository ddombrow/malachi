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

// Started closes the descriptors cmd was made to hand its process, once it
// has been started or will not be. Call it after cmd.Start, whatever that
// returned, for a command from Command or LoopbackSession.Command: until
// then the far end cannot tell the process has gone.
func Started(cmd *exec.Cmd) {
	for _, f := range cmd.ExtraFiles {
		if f != nil {
			_ = f.Close()
		}
	}
}
