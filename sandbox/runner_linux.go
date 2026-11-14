package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"

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

// Weakness says what the sandbox cannot do for p on this system though it
// runs, or "" when it does everything it promises.
func Weakness(p Policy) string {
	notes := []string{abiWeakness(landlockABI())}
	if p.PTY {
		if err := privatePTYAvailable(); err != nil {
			notes = append(notes, fmt.Sprintf("the pty setting has no effect, so commands get no pseudo-terminals: "+
				"this system does not let malachi make a private set of them (%v); it takes unprivileged user namespaces", err))
		}
	}
	return strings.Join(slices.DeleteFunc(notes, func(n string) bool { return n == "" }), "; ")
}

func abiWeakness(abi int) string {
	if abi < 1 || abi >= truncateABI {
		return ""
	}
	return fmt.Sprintf("this kernel's Landlock (ABI %d) cannot stop a command truncating a file, so commands can empty any file you can write, "+
		"hidden ones and those outside the writable directories included; they still cannot read hidden files or write contents elsewhere. Linux 6.2 or later closes this",
		abi)
}

func command(ctx context.Context, p Policy, name string, args ...string) (*exec.Cmd, error) {
	ptys := p.PTY && privatePTYAvailable() == nil
	raw, err := json.Marshal(spec{Policy: p, Name: name, Args: args, PrivatePTY: ptys})
	if err != nil {
		return nil, fmt.Errorf("sandbox: %w", err)
	}
	cmd := exec.CommandContext(ctx, selfExe, helperArg, string(raw))
	if ptys {
		cmd.SysProcAttr = privatePTYAttr()
	}
	return cmd, nil
}

// privatePTYAttr starts the helper where it may mount a private /dev/pts: in
// a mount namespace of its own, inside a user namespace of its own, which
// is what lets an unprivileged process mount at all. The user and group
// keep their ids there. Every other id has none, so files owned by anyone
// else, root included, appear to belong to "nobody"; access to them is
// unchanged.
//
// The capability to mount is passed to the helper as an ambient one, since
// a process that is not root loses the rest when it execs. The helper drops
// it before running the command.
//
// Callers that set attributes of their own (a new session) must add to
// these, not replace them.
func privatePTYAttr() *syscall.SysProcAttr {
	uid, gid := os.Getuid(), os.Getgid()
	return &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: uid, HostID: uid, Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: gid, HostID: gid, Size: 1}},
		AmbientCaps: []uintptr{unix.CAP_SYS_ADMIN},
	}
}

// privatePTYAvailable reports why commands cannot be given a private set of
// pseudo-terminals here, or nil. It is found out by trying, once: whether
// an unprivileged process may make a user namespace is decided by sysctls
// and security modules that differ between distributions.
var privatePTYAvailable = sync.OnceValue(func() error {
	cmd := exec.Command(selfExe, probeArg)
	cmd.SysProcAttr = privatePTYAttr()
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	if msg := strings.TrimSpace(string(out)); msg != "" {
		return errors.New(strings.TrimPrefix(msg, "malachi sandbox: "))
	}
	return err
})
