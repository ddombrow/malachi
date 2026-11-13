package sandbox

import (
	"bytes"
	"os"
	"testing"
	"unsafe"

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
	var name [128]byte
	for _, c := range []struct {
		req uint
		arg uintptr
	}{{unix.TIOCPTYGRANT, 0}, {unix.TIOCPTYUNLK, 0}, {unix.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0]))}} {
		if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(c.req), c.arg); errno != 0 {
			t.Skipf("no pseudo-terminal here: %v", errno)
		}
	}
	return master, holdOpen(t, string(name[:bytes.IndexByte(name[:], 0)]))
}

// holdOpen keeps the terminal's own end open, as the shell in a real
// window does; without it a write to the controlling end blocks.
func holdOpen(t *testing.T, name string) string {
	t.Helper()
	fd, err := unix.Open(name, unix.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pseudo-terminal here: %v", err)
	}
	t.Cleanup(func() { unix.Close(fd) })
	return name
}
