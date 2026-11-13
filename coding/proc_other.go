//go:build !unix

package coding

import "os/exec"

func newSession(*exec.Cmd) {}

func configureProcessGroup(*exec.Cmd) {}
