package sandbox

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	seccomp "github.com/elastic/go-seccomp-bpf"
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
	if err := restrictFS(s.Policy); err != nil {
		return fmt.Errorf("landlock: %w", err)
	}
	if !s.Policy.Network {
		if err := denyNetwork(); err != nil {
			return fmt.Errorf("seccomp: %w", err)
		}
	}
	return syscall.Exec(path, append([]string{s.Name}, s.Args...), os.Environ())
}

// restrictFS applies p with Landlock: read everything but hidden paths,
// write only under writable roots (minus hidden paths) and a few devices.
// Directories split around a hidden path stay listable, so "ls ~" works;
// on Linux that shows the names inside hidden directories, not their
// contents.
func restrictFS(p Policy) error {
	readable, split := carve("/", p.Hidden)
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
		landlock.PathAccess(ll.AccessFSReadDir, split...).IgnoreIfMissing(),
		landlock.RWDirs(rwDirs...).WithRefer().IgnoreIfMissing(),
		landlock.RWFiles(rwFiles...).IgnoreIfMissing(),
		landlock.RWFiles(devices...).IgnoreIfMissing(),
	}
	// Best effort over the ABIs this kernel lacks, never over Landlock
	// itself: runHelper has already required ABI 1 or later.
	return landlock.V5.BestEffort().RestrictPaths(rules...)
}

// denyNetwork makes creating IP and raw packet sockets fail with EPERM, and
// io_uring (which can create sockets without socket(2)) unavailable. Unix
// sockets keep working.
func denyNetwork() error {
	var conds []seccomp.NameWithConditions
	for _, family := range []uint64{syscall.AF_INET, syscall.AF_INET6, syscall.AF_PACKET} {
		conds = append(conds, seccomp.NameWithConditions{
			Name:       "socket",
			Conditions: seccomp.ArgumentConditions{{Argument: 0, Operation: seccomp.Equal, Value: family}},
		})
	}
	return seccomp.LoadFilter(seccomp.Filter{
		NoNewPrivs: true,
		Flag:       seccomp.FilterFlagTSync,
		Policy: seccomp.Policy{
			DefaultAction: seccomp.ActionAllow,
			Syscalls: []seccomp.SyscallGroup{
				{Action: seccomp.ActionErrno, NamesWithCondtions: conds},
				{Action: seccomp.ActionErrno, Names: []string{"io_uring_setup"}},
			},
		},
	})
}
