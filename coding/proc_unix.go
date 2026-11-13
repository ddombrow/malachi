//go:build unix

package coding

import (
	"os/exec"
	"syscall"
)

// newSession starts cmd in a session of its own, which has no controlling
// terminal. Left in malachi's session, a command could open /dev/tty and
// write over the interface, and where the terminal allows it (TIOCSTI) type
// into it: slash commands that change the sandbox, or a shell command for
// when malachi exits. The file rules cannot close this, since /dev/tty is a
// device commands are otherwise right to use. It also stops a program that
// wants a password from asking for it on top of the interface.
//
// A session leader leads a process group too, so the group can still be
// signalled by the command's pid.
func newSession(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// configureProcessGroup starts the shell in its own session and process
// group and makes cancellation kill the whole group, so pipeline children
// do not linger.
func configureProcessGroup(cmd *exec.Cmd) {
	newSession(cmd)
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
