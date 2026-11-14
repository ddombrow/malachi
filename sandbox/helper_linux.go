package sandbox

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/landlock-lsm/go-landlock/landlock"
	ll "github.com/landlock-lsm/go-landlock/landlock/syscall"
)

// devices commands write to as a matter of course.
var devices = []string{"/dev/null", "/dev/zero", "/dev/full", "/dev/random", "/dev/urandom", "/dev/tty"}

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
	// Whether Landlock can tell a command's own Unix sockets from the
	// user's daemons is decided once, since the file rules and the socket
	// filter must agree: the filter may let Unix sockets be made only if
	// Landlock is keeping them in.
	own := ownUnixSockets(s.Policy, landlockABI())
	if err := restrictFS(s.Policy, own); err != nil {
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
func restrictFS(p Policy, own bool) error {
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
