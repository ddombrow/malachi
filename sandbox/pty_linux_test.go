package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"testing"

	"golang.org/x/sys/unix"
)

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
