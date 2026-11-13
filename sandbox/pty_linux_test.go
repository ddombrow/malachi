package sandbox

import (
	"fmt"
	"os"
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
