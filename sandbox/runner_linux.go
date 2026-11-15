package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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
	if _, err := nativeArch(); err != nil {
		return err
	}
	return nested()
})

// nested reports that this process is already confined in a way that keeps
// a sandbox from being built inside it, or nil.
//
// Granting everything but the hidden and private paths takes listing the
// directories around them, / first of all. A malachi sandbox does not let
// / be listed (the right would reach every directory under it, the private
// ones included), so malachi run by a sandboxed command, as when it works
// on itself and runs its own tests, cannot work out what to grant. It says
// so here, and commands fail, as they do on macOS, whose sandboxes do not
// nest either. Left to try, each command's helper granted nothing and
// failed to start with a bare "permission denied".
func nested() error {
	if _, err := os.ReadDir("/"); errors.Is(err, fs.ErrPermission) {
		return errors.New("malachi is already running inside a sandbox that does not let / be listed (its own, most likely), and a sandbox cannot be built inside it")
	}
	return nil
}

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
				"this system does not let malachi make a private set of them (%v); it takes unprivileged user namespaces%s", err, userNSHint()))
		}
	}
	if p.Loopback && p.LoopbackAsked && !p.Network {
		if err := privateLoopbackAvailable(); err != nil {
			notes = append(notes, fmt.Sprintf("the loopback setting has no effect, so commands have no network at all: "+
				"this system does not let malachi give them a private network namespace (%v); it takes unprivileged user namespaces%s", err, userNSHint()))
		}
	}
	return strings.Join(slices.DeleteFunc(notes, func(n string) bool { return n == "" }), "; ")
}

// userNSHint names the usual reason unprivileged user namespaces are
// useless here, Ubuntu's AppArmor restriction (23.10 and later), which lets
// one be made but takes away its capabilities, or "".
func userNSHint() string {
	b, err := os.ReadFile("/proc/sys/kernel/apparmor_restrict_unprivileged_userns")
	if err != nil || strings.TrimSpace(string(b)) != "1" {
		return ""
	}
	return ", which AppArmor restricts here (kernel.apparmor_restrict_unprivileged_userns=1)"
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
	// Without a private namespace the setting is dropped, not failed on:
	// the command then has no IP networking at all, which confines it more
	// than was asked. Weakness says so.
	loopback := LoopbackInForce(p)
	p.Loopback = loopback
	ptys := p.PTY && privatePTYAvailable() == nil
	raw, err := json.Marshal(spec{Policy: p, Name: name, Args: args, PrivatePTY: ptys, PrivateLoopback: loopback})
	if err != nil {
		return nil, fmt.Errorf("sandbox: %w", err)
	}
	cmd := exec.CommandContext(ctx, selfExe, helperArg, string(raw))
	if ptys || loopback {
		cmd.SysProcAttr = privateAttr(ptys, loopback)
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
func privatePTYAttr() *syscall.SysProcAttr { return privateAttr(true, false) }

// privateLoopbackAttr starts the helper in a user and network namespace. The
// namespace has no external interfaces or routes; the helper brings only lo up.
func privateLoopbackAttr() *syscall.SysProcAttr { return privateAttr(false, true) }

// privateAttr combines the user namespace needed for capabilities with the
// requested private namespaces. The helper drops all capabilities before exec.
func privateAttr(pty, loopback bool) *syscall.SysProcAttr {
	flags := uintptr(syscall.CLONE_NEWUSER)
	var caps []uintptr
	if pty {
		flags |= syscall.CLONE_NEWNS
		caps = append(caps, unix.CAP_SYS_ADMIN)
	}
	if loopback {
		flags |= syscall.CLONE_NEWNET
		caps = append(caps, unix.CAP_NET_ADMIN)
	}
	uid, gid := os.Getuid(), os.Getgid()
	return &syscall.SysProcAttr{
		Cloneflags:  flags,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: uid, HostID: uid, Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: gid, HostID: gid, Size: 1}},
		AmbientCaps: caps,
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

// privateLoopbackAvailable reports why commands cannot be given a private
// network namespace with its loopback up, or nil. Found out by trying, once,
// like privatePTYAvailable. Without one, the loopback setting falls back to
// no network, never to the host's.
var privateLoopbackAvailable = sync.OnceValue(func() error {
	cmd := exec.Command(selfExe, probeLoopbackArg)
	cmd.SysProcAttr = privateLoopbackAttr()
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	if msg := strings.TrimSpace(string(out)); msg != "" {
		return errors.New(strings.TrimPrefix(msg, "malachi sandbox: "))
	}
	return err
})

// LoopbackInForce reports whether commands run under p get a private
// loopback: the setting is on, the network is off, and this system can make
// them a network namespace of their own.
func LoopbackInForce(p Policy) bool {
	return p.Enabled && p.Loopback && !p.Network && privateLoopbackAvailable() == nil
}
