package sandbox

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/landlock-lsm/go-landlock/landlock"
	ll "github.com/landlock-lsm/go-landlock/landlock/syscall"
	"golang.org/x/sys/unix"
)

// devices commands write to as a matter of course.
var devices = []string{"/dev/null", "/dev/zero", "/dev/full", "/dev/random", "/dev/urandom", "/dev/tty"}

// gpuDevicePatterns are the device nodes a command may open with Policy.GPU:
// the ones a program renders and computes through, and none that drive a
// display.
//
// /dev/dri/renderD* are the kernel's render nodes, which every open driver
// (Intel, AMD, Nouveau and the rest of Mesa) offers for exactly this: they
// carry rendering and compute and refuse mode setting. The card* nodes
// beside them, which set modes and are what a display server holds, stay
// closed. Rendering on an AMD GPU through its render node is tested here.
//
// The rest are the vendors' own, as their documentation lists them, and are
// untested here: NVIDIA's proprietary driver (the control device, one device
// per GPU, unified memory for CUDA, and its capability files; not
// nvidia-modeset, which is the display's), AMD's compute interface (ROCm),
// and the GPU of the Windows Subsystem for Linux.
var gpuDevicePatterns = []string{
	"/dev/dri/renderD*",
	"/dev/nvidiactl", "/dev/nvidia[0-9]*", "/dev/nvidia-uvm", "/dev/nvidia-uvm-tools", "/dev/nvidia-caps/*",
	"/dev/kfd",
	"/dev/dxg",
}

// gpuDevices are the GPU device nodes present on this system.
func gpuDevices() []string {
	var out []string
	for _, pattern := range gpuDevicePatterns {
		matches, _ := filepath.Glob(pattern)
		out = append(out, matches...)
	}
	return out
}

// runHelper confines this process to the spec's policy and execs its
// command. It returns only on failure.
func runHelper(raw string) error {
	var s spec
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return fmt.Errorf("bad spec: %w", err)
	}
	if err := Available(); err != nil {
		return err
	}
	path, err := exec.LookPath(s.Name)
	if err != nil {
		return err
	}
	if s.PrivatePTY {
		// Capabilities belong to a thread: the one that holds them here must
		// be the one that drops them and then execs.
		runtime.LockOSThread()
		if err := privatePTY(); err != nil {
			return fmt.Errorf("pseudo-terminals: %w", err)
		}
		if err := dropCapabilities(); err != nil {
			return fmt.Errorf("dropping capabilities: %w", err)
		}
	}
	// Whether Landlock can tell a command's own Unix sockets from the
	// user's daemons is decided once, since the file rules and the socket
	// filter must agree: the filter may let Unix sockets be made only if
	// Landlock is keeping them in.
	own := ownUnixSockets(s.Policy, landlockABI())
	if err := restrictFS(s.Policy, own, s.PrivatePTY); err != nil {
		return fmt.Errorf("landlock: %w", err)
	}
	filtered := s.Policy
	filtered.UnixSockets = filtered.UnixSockets || own
	if err := denySockets(filtered); err != nil {
		return fmt.Errorf("seccomp: %w", err)
	}
	return syscall.Exec(path, append([]string{s.Name}, s.Args...), os.Environ())
}

// restrictFS applies p with Landlock: read everything but hidden paths and
// private directories outside the readable roots, write only under writable
// roots (minus hidden paths) and a few devices. Directories split around a
// hidden path stay listable, so "ls ~" works when the home directory is
// readable; on Linux that shows the names inside hidden directories, not
// their contents. Neither a private directory nor one holding it is
// listable (see readGrants).
//
// With own set (see ownUnixSockets), Unix sockets are confined here too:
// connecting to a socket by path is refused unless the socket was made
// inside this sandbox, and so is connecting to an abstract one. No path is
// granted the right, so the user's daemons stay out of reach wherever their
// sockets are.
//
// With ptys set, /dev/pts is a set of pseudo-terminals of the command's own
// (see privatePTY) and is opened to it, device controls included: a terminal
// is driven by them.
func restrictFS(p Policy, own, ptys bool) error {
	readable, listable := readGrants(p)
	roDirs, roFiles := partition(readable)
	var writable []string
	for _, w := range p.Writable {
		g, _ := carve(w, p.Hidden)
		writable = append(writable, g...)
	}
	rwDirs, rwFiles := partition(writable)

	// Paths can vanish between listing them and Landlock opening them (temp
	// directories churn constantly), so a missing one is skipped: it can no
	// longer be accessed anyway.
	rules := []landlock.Rule{
		landlock.RODirs(roDirs...).IgnoreIfMissing(),
		landlock.ROFiles(roFiles...).IgnoreIfMissing(),
		landlock.PathAccess(ll.AccessFSReadDir, listable...).IgnoreIfMissing(),
		landlock.RWDirs(rwDirs...).WithRefer().IgnoreIfMissing(),
		landlock.RWFiles(rwFiles...).IgnoreIfMissing(),
		landlock.RWFiles(devices...).IgnoreIfMissing(),
	}
	if p.GPU {
		rules = append(rules, landlock.RWFiles(gpuDevices()...).WithIoctlDev().IgnoreIfMissing())
	}
	if ptys {
		rules = append(rules,
			landlock.RWDirs("/dev/pts").WithIoctlDev(),
			landlock.RWFiles("/dev/ptmx").WithIoctlDev())
	}
	if own {
		// Not best effort: the socket filter is about to allow Unix
		// sockets on the strength of these two.
		if err := landlock.V9.RestrictPaths(rules...); err != nil {
			return err
		}
		return landlock.MustConfig(landlock.ScopedSet(ll.ScopeAbstractUnixSocket)).RestrictScoped()
	}
	// Best effort over the ABIs this kernel lacks, never over Landlock
	// itself: runHelper has already required ABI 1 or later.
	return landlock.V5.BestEffort().RestrictPaths(rules...)
}

// unixSocketABI is the Landlock ABI from which connecting to a Unix socket
// by path can be restricted to sockets made inside the sandbox.
const unixSocketABI = 9

// ownUnixSockets reports whether commands under p get Unix sockets of their
// own: they may make and use them among their own processes, and still not
// reach one made outside the sandbox. It takes a kernel whose Landlock can
// tell the two apart; on an older one, commands get no Unix sockets at all.
//
// Programs need them more than it seems: Python's multiprocessing starts its
// workers through one (so every process pool does), and so do language
// servers, test runners and headless browsers talking to their children.
func ownUnixSockets(p Policy, abi int) bool {
	return !p.UnixSockets && abi >= unixSocketABI
}

// privatePTY replaces /dev/pts, in this process's own mount namespace, with
// a new and empty set of pseudo-terminals. A terminal the command makes is
// then the only kind there is to open: the user's other terminal windows,
// which are the same kind of device and which no file rule could tell apart
// from the command's own, are simply not there. /dev/ptmx hands out
// terminals from the set mounted beside it, so it needs nothing done.
//
// It needs to be allowed to mount, which the caller arranges by starting
// this process in a user namespace (see privatePTYAttr).
func privatePTY() error {
	// Nothing mounted here may spread to the namespace this one was copied
	// from.
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("making mounts private: %w", err)
	}
	if err := unix.Mount("devpts", "/dev/pts", "devpts", unix.MS_NOSUID|unix.MS_NOEXEC, "newinstance,ptmxmode=0666,mode=0620"); err != nil {
		return fmt.Errorf("mounting /dev/pts: %w", err)
	}
	return nil
}

// dropCapabilities gives up, on this thread, every capability the user
// namespace came with, so that none is left for the command: the right to
// mount was wanted for privatePTY and for nothing after it.
func dropCapabilities() error {
	if err := unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0); err != nil {
		return err
	}
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var none [2]unix.CapUserData
	return unix.Capset(&header, &none[0])
}
