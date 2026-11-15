package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
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
// /dev/kfd is AMD's compute interface, which ROCm drives. It is tested as
// far as the kernel: it opens and answers. ROCm itself is untested.
//
// The rest are listed from the vendors' documentation and are untested
// here: NVIDIA's proprietary driver (the control device, one device per
// GPU, unified memory for CUDA, and its capability files; not
// nvidia-modeset, which is the display's) and the GPU of the Windows
// Subsystem for Linux.
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
	if s.PrivatePTY || s.PrivateLoopback {
		// Capabilities belong to a thread: the one that holds them here must
		// be the one that uses them and drops them before exec.
		runtime.LockOSThread()
		if err := mapSelf(s.IDs); err != nil {
			return err
		}
		if s.PrivatePTY {
			if err := privatePTY(); err != nil {
				return fmt.Errorf("pseudo-terminals: %w", err)
			}
		}
		if s.PrivateLoopback {
			if err := privateLoopback(); err != nil {
				return fmt.Errorf("loopback networking: %w", err)
			}
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
	// IP sockets with the network off are safe only in a namespace with
	// nothing but loopback in it.
	filtered.Loopback = filtered.Loopback && (s.PrivateLoopback || s.LoopbackSession)
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
	roDirs, roFiles := pin(readable)
	listable, _ = pin(listable)
	var writable []string
	for _, w := range p.Writable {
		g, _ := carve(w, p.Hidden)
		writable = append(writable, g...)
	}
	rwDirs, rwFiles := pin(writable)

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

// pin opens each path as the policy named it, following no symlink anywhere
// along it, and returns names for what it opened (through /proc/self/fd) for
// Landlock to put its rules on, directories apart from the rest. A path
// that has a symlink in it, or is gone, or is a pipe or socket (which
// Landlock takes no rule for, and does not confine), is left out.
//
// A rule lands on whatever a path leads to when Landlock opens it, and the
// policy's paths were resolved when the session started. A command can
// change what one leads to afterwards if it lies inside a directory the
// command may write: the project's .git is such a root, and moving it aside
// and leaving a link to ~/.ssh in its place got the next command a rule on
// ~/.ssh, hidden or not. Nothing in a policy's path is a link when the
// policy is built, so one that is a link now is not the place that was
// meant, and gets no rule. Opening here and handing Landlock the opened
// file also leaves no moment between the check and the rule.
//
// The files stay open until exec closes them.
func pin(paths []string) (dirs, files []string) {
	for _, path := range paths {
		fd, err := openNoSymlinks(path)
		if err != nil {
			continue
		}
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			unix.Close(fd)
			continue
		}
		name := "/proc/self/fd/" + strconv.Itoa(fd)
		switch st.Mode & unix.S_IFMT {
		case unix.S_IFDIR:
			dirs = append(dirs, name)
		case unix.S_IFIFO, unix.S_IFSOCK, unix.S_IFLNK:
			unix.Close(fd)
		default:
			files = append(files, name)
		}
	}
	return dirs, files
}

// openNoSymlinks opens path for reference only (O_PATH), refusing a symlink
// in any part of it.
func openNoSymlinks(path string) (int, error) {
	fd, err := unix.Openat2(unix.AT_FDCWD, path, &unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_NO_SYMLINKS,
	})
	if !errors.Is(err, unix.ENOSYS) {
		return fd, err
	}
	// openat2 is filtered out in some containers. Without it, the last part
	// is still refused if it is a link, and the rest is compared with what
	// the path resolves to now.
	if Canonical(path) != path {
		return -1, unix.ELOOP
	}
	return unix.Open(path, unix.O_PATH|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
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

// mapSelf maps the user and group in ids ("uid:gid") to themselves in the
// user namespace this process was started in (see privateAttr), which no
// one else may do for it: malachi is not dumpable (ProtectProcess), so its
// children's /proc files are root's until they exec, and the mapping Go
// would write from the parent is refused. Started afresh, this process is
// dumpable, and the capabilities it was passed let it write its own. Only
// its own ids, and with setgroups denied, as an unprivileged mapping must.
func mapSelf(ids string) error {
	uid, gid, ok := strings.Cut(ids, ":")
	if _, err := strconv.Atoi(uid); !ok || err != nil {
		return fmt.Errorf("user namespace: bad ids %q", ids)
	}
	if _, err := strconv.Atoi(gid); err != nil {
		return fmt.Errorf("user namespace: bad ids %q", ids)
	}
	for _, w := range []struct{ file, data string }{
		{"/proc/self/setgroups", "deny"},
		{"/proc/self/uid_map", uid + " " + uid + " 1"},
		{"/proc/self/gid_map", gid + " " + gid + " 1"},
	} {
		if err := os.WriteFile(w.file, []byte(w.data), 0); err != nil {
			return fmt.Errorf("user namespace: %w", err)
		}
	}
	return nil
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

// privateLoopback brings up only the loopback interface in the private network
// namespace established by privateLoopbackAttr. That namespace has no external
// interfaces or routes, so enabling lo cannot reach or expose host services.
func privateLoopback() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("opening interface control socket: %w", err)
	}
	defer unix.Close(fd)

	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return fmt.Errorf("reading loopback flags: %w", err)
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	if err := unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr); err != nil {
		return fmt.Errorf("bringing loopback up: %w", err)
	}
	return nil
}

// namespace came with, so that none is left for the command: the right to
// mount was wanted for privatePTY, and to configure loopback networking for
// nothing after it.
func dropCapabilities() error {
	if err := unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0); err != nil {
		return err
	}
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var none [2]unix.CapUserData
	return unix.Capset(&header, &none[0])
}
