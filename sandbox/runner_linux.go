package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sync"

	"golang.org/x/sys/unix"
)

// Landlock confines only the calling process, so a command is started as
// malachi itself (/proc/self/exe, which works even if the binary was
// replaced since) running the helper, which confines itself and then execs
// the command.
const selfExe = "/proc/self/exe"

var availability = sync.OnceValue(func() error {
	if v := landlockABI(); v < 1 {
		return errors.New("the kernel has no Landlock support (needs Linux 5.13 or later with Landlock enabled)")
	}
	return nil
})

// landlockABI returns the kernel's Landlock ABI version, or 0 without it.
func landlockABI() int {
	v, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if errno != 0 {
		return 0
	}
	return int(v)
}

// Available reports why commands cannot be sandboxed here, or nil.
func Available() error { return availability() }

func command(ctx context.Context, p Policy, name string, args ...string) (*exec.Cmd, error) {
	raw, err := json.Marshal(spec{Policy: p, Name: name, Args: args})
	if err != nil {
		return nil, fmt.Errorf("sandbox: %w", err)
	}
	return exec.CommandContext(ctx, selfExe, helperArg, string(raw)), nil
}
