//go:build unix

package mcp

import "syscall"

var sigterm = syscall.SIGTERM
