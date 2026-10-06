package sandbox

import (
	"os"
	"strconv"
)

// fdPath returns the path of the file f refers to, as the kernel knows it.
func fdPath(f *os.File) (string, error) {
	return os.Readlink("/proc/self/fd/" + strconv.Itoa(int(f.Fd())))
}
