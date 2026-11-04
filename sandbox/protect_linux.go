package sandbox

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// ProtectProcess prevents same-UID child processes from inspecting this
// process through /proc or ptrace, including reading environment variables
// that were present when Malachi started. This also disables core dumps.
func ProtectProcess() error {
	if os.Geteuid() == 0 {
		return errors.New("refusing to run as root: child commands could bypass process-environment protection")
	}
	var header unix.CapUserHeader
	header.Version = unix.LINUX_CAPABILITY_VERSION_3
	var caps [2]unix.CapUserData
	if err := unix.Capget(&header, &caps[0]); err != nil {
		return err
	}
	bit := uint32(1) << (unix.CAP_SYS_PTRACE % 32)
	for _, cap := range caps {
		if cap.Effective&bit != 0 || cap.Permitted&bit != 0 || cap.Inheritable&bit != 0 {
			return errors.New("refusing to run with CAP_SYS_PTRACE: child commands could inspect this process")
		}
	}
	return unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0)
}
