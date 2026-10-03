//go:build !unix

package coding

import "os/exec"

func configureProcessGroup(*exec.Cmd) {}
