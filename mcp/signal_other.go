//go:build !unix

package mcp

import "os"

// Without SIGTERM, the polite step is an interrupt.
var sigterm = os.Interrupt
