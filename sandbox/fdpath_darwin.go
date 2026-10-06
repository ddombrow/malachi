package sandbox

import (
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// fdPath returns the path of the file f refers to, as the kernel knows it.
func fdPath(f *os.File) (string, error) {
	buf := make([]byte, unix.PathMax)
	_, err := unix.FcntlInt(f.Fd(), unix.F_GETPATH, int(uintptr(unsafe.Pointer(&buf[0]))))
	runtime.KeepAlive(buf)
	if err != nil {
		return "", err
	}
	return unix.ByteSliceToString(buf), nil
}
