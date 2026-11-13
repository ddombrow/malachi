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
	_, err := nativeArch()
	return err
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

// truncateABI is the Landlock ABI (Linux 6.2) from which truncating a file
// counts as writing it.
const truncateABI = 3

// Weakness says what the sandbox cannot enforce on this system though it
// runs, or "" when it enforces everything it promises.
func Weakness() string { return abiWeakness(landlockABI()) }

func abiWeakness(abi int) string {
	if abi < 1 || abi >= truncateABI {
		return ""
	}
	return fmt.Sprintf("this kernel's Landlock (ABI %d) cannot stop a command truncating a file, so commands can empty any file you can write, "+
		"hidden ones and those outside the writable directories included; they still cannot read hidden files or write contents elsewhere. Linux 6.2 or later closes this",
		abi)
}

func command(ctx context.Context, p Policy, name string, args ...string) (*exec.Cmd, error) {
	raw, err := json.Marshal(spec{Policy: p, Name: name, Args: args})
	if err != nil {
		return nil, fmt.Errorf("sandbox: %w", err)
	}
	return exec.CommandContext(ctx, selfExe, helperArg, string(raw)), nil
}
