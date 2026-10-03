//go:build unix

package coding

import (
	"os/exec"
	"syscall"
)

// configureProcessGroup starts the shell in its own process group and makes
// cancellation kill the whole group, so pipeline children do not linger.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
