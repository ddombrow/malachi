package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// ownUnixSocketsHere reports whether this kernel lets commands have Unix
// sockets of their own.
func ownUnixSocketsHere() bool { return landlockABI() >= unixSocketABI }

// openTestPTY makes a pseudo-terminal in the test process, outside any
// sandbox, to stand in for one of the user's other terminal windows. It
// returns the controlling end and the device a command would open.
func openTestPTY(t *testing.T) (*os.File, string) {
	t.Helper()
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pseudo-terminal here: %v", err)
	}
	master := os.NewFile(uintptr(fd), "ptmx")
	t.Cleanup(func() { master.Close() })
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		t.Skipf("no pseudo-terminal here: %v", err)
	}
	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		t.Skipf("no pseudo-terminal here: %v", err)
	}
	return master, holdOpen(t, fmt.Sprintf("/dev/pts/%d", n))
}

// holdOpen keeps the terminal's own end open, as the shell in a real
// window does.
func holdOpen(t *testing.T, name string) string {
	t.Helper()
	fd, err := unix.Open(name, unix.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pseudo-terminal here: %v", err)
	}
	t.Cleanup(func() { unix.Close(fd) })
	return name
}

// Process pools need a semaphore, which on Linux is a file in /dev/shm.
func TestSandboxedSharedMemory(t *testing.T) {
	requireBackend(t)
	p, project, _, _, _ := fixture(t)
	if out, ok := sh(t, p, project, "f=/dev/shm/malachi-test-$$; echo x > $f && cat $f && rm $f"); !ok || out != "x" {
		t.Fatalf("could not use /dev/shm: %s", out)
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3")
	}
	if out, ok := sh(t, p, project, `python3 -c "import multiprocessing; multiprocessing.Lock(); print('locked')"`); !ok || out != "locked" {
		t.Fatalf("multiprocessing could not make a lock: %s", out)
	}
}

// What the runtime directory and the journal hold is not a command's to
// read, though the directories around them still list.
func TestSandboxedRuntimeDirAndJournal(t *testing.T) {
	requireBackend(t)
	p, project, _, _, _ := fixture(t)
	runtimeDir := "/run/user/" + strconv.Itoa(os.Getuid())
	if _, err := os.ReadDir(runtimeDir); err != nil {
		t.Skipf("no runtime directory to protect here: %v", err)
	}
	// Opened, not tested with access(2), which Landlock does not answer for.
	read := "for f in " + runtimeDir + "/* " + runtimeDir + "/*/*; do [ -f \"$f\" ] && head -c1 \"$f\" >/dev/null 2>&1 && echo \"$f\"; done | head -1"
	if out, _ := sh(t, p, project, read); out != "" {
		t.Errorf("a file in the runtime directory is readable: %s", out)
	}
	if out, ok := sh(t, p, project, "ls /run >/dev/null && ls /var/log >/dev/null"); !ok {
		t.Errorf("/run and /var/log should still list: %s", out)
	}
}

// A command may make Unix sockets and use them among its own processes,
// where the kernel can keep them from reaching anyone else's.
func TestSandboxedOwnUnixSockets(t *testing.T) {
	requireBackend(t)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3")
	}
	p, project, _, _, _ := fixture(t)
	own := `python3 -c "
import os, socket
server = socket.socket(socket.AF_UNIX)
server.bind('s.sock')
server.listen()
if os.fork() == 0:
    client = socket.socket(socket.AF_UNIX)
    client.connect('s.sock')
    client.send(b'hello')
    os._exit(0)
conn, _ = server.accept()
print(conn.recv(5).decode())
"`
	out, ok := sh(t, p, project, own)
	if want := landlockABI() >= unixSocketABI; ok != want {
		t.Fatalf("own Unix socket: worked=%v, want %v on Landlock ABI %d: %s", ok, want, landlockABI(), out)
	}
	if ok && out != "hello" {
		t.Fatalf("own Unix socket carried %q", out)
	}
}

func TestOwnUnixSocketsNeedsANewKernel(t *testing.T) {
	for _, c := range []struct {
		allowAll bool
		abi      int
		want     bool
	}{
		{false, 5, false}, {false, 8, false}, {false, 9, true}, {false, 11, true},
		{true, 11, false}, // every Unix socket is allowed: nothing to keep in
	} {
		if got := ownUnixSockets(Policy{Enabled: true, UnixSockets: c.allowAll}, c.abi); got != c.want {
			t.Errorf("ownUnixSockets(unixSockets=%v, abi %d) = %v, want %v", c.allowAll, c.abi, got, c.want)
		}
	}
}

// requirePTYBackend skips where commands cannot be given pseudo-terminals:
// a system that does not let an unprivileged process make a user namespace
// (some CI runners).
func requirePTYBackend(t *testing.T) {
	t.Helper()
	if err := privatePTYAvailable(); err != nil {
		skipOrFail(t, "no private pseudo-terminals here: %v", err)
	}
}

// requireLoopbackBackend skips where commands cannot be given a network
// namespace of their own, for the same reason.
func requireLoopbackBackend(t *testing.T) {
	t.Helper()
	if err := privateLoopbackAvailable(); err != nil {
		skipOrFail(t, "no private network namespace here: %v", err)
	}
}

// skipOrFail skips, or with MALACHI_REQUIRE_SANDBOX set fails: CI arranges
// for what these tests need, and one that skipped there would be a test
// that runs nowhere.
func skipOrFail(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("MALACHI_REQUIRE_SANDBOX") != "" {
		t.Fatalf("required by MALACHI_REQUIRE_SANDBOX: "+format, args...)
	}
	t.Skipf(format, args...)
}

// With pseudo-terminals on, a command's are a set of its own: the user's
// other terminals are not in it, and the right to mount that made the set
// is gone before the command runs.
func TestSandboxedPseudoTerminalsArePrivate(t *testing.T) {
	requireBackend(t)
	requirePTYBackend(t)
	master, name := openTestPTY(t)
	if _, err := master.Write([]byte("typed-elsewhere\n")); err != nil {
		t.Fatal(err)
	}
	p, project, _, _, _ := fixture(t)
	p.PTY = true
	if out, ok := sh(t, p, project, "head -n 1 < "+name); ok || strings.Contains(out, "typed-elsewhere") {
		t.Errorf("read another terminal's input: %v %s", ok, out)
	}
	if out, ok := sh(t, p, project, "echo drawn > "+name+" && echo WROTE"); ok || strings.Contains(out, "WROTE") {
		t.Errorf("wrote to another terminal: %v %s", ok, out)
	}
	if out, _ := sh(t, p, project, "ls /dev/pts"); out != "ptmx" {
		t.Errorf("/dev/pts should start empty but for ptmx, got: %s", out)
	}
	out, _ := sh(t, p, project, "grep -E '^Cap(Inh|Prm|Eff|Amb)' /proc/self/status")
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasSuffix(line, "0000000000000000") {
			t.Errorf("the command kept a capability: %s", line)
		}
	}
	if out, ok := sh(t, p, project, "mkdir m && mount -t tmpfs none m"); ok {
		t.Errorf("the command could mount: %s", out)
	}
	// The rest of the sandbox is as it was.
	if out, ok := sh(t, p, project, "echo x > inside && cat inside"); !ok || out != "x" {
		t.Errorf("could not write in the project: %s", out)
	}
	if out, ok := sh(t, projectOnly(p, project), project, "echo x > ../outside/y"); ok {
		t.Errorf("wrote outside the project: %s", out)
	}
}

func TestWeaknessOnlyMentionsPTYWhenAsked(t *testing.T) {
	if got := Weakness(Policy{Enabled: true}); strings.Contains(got, "pty") {
		t.Errorf("Weakness without the setting = %q", got)
	}
}
