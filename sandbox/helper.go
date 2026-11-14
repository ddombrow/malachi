package sandbox

import (
	"fmt"
	"os"
)

// helperArg is the hidden first argument that turns the malachi binary (or a
// test binary) into the sandbox helper on platforms that confine a command by
// restricting the process that will exec it.
const helperArg = "__sandbox-exec"

// spec is what the helper is told: the policy and the command to exec.
type spec struct {
	Policy Policy   `json:"policy"`
	Name   string   `json:"name"`
	Args   []string `json:"args"`
	// PrivatePTY says the helper was started in namespaces of its own and is
	// to give the command a private set of pseudo-terminals (Linux).
	PrivatePTY bool `json:"privatePty,omitempty"`
}

// probeArg makes the binary try, and report by its exit status, what a
// helper with PrivatePTY would do, so the question is asked once at startup
// and not answered by every command failing.
const probeArg = "__sandbox-probe-pty"

// MaybeRunHelper runs the sandbox helper and never returns if this process
// was started as one; otherwise it returns at once. Call it first thing in
// main, and in TestMain of packages that run sandboxed commands, before
// anything else touches the process.
func MaybeRunHelper() {
	if len(os.Args) == 2 && os.Args[1] == probeArg {
		if err := privatePTY(); err != nil {
			fmt.Fprintf(os.Stderr, "malachi sandbox: %v\n", err)
			os.Exit(HelperFailed)
		}
		os.Exit(0)
	}
	if len(os.Args) < 3 || os.Args[1] != helperArg {
		return
	}
	err := runHelper(os.Args[2])
	// runHelper only returns on failure: the command never started.
	fmt.Fprintf(os.Stderr, "malachi sandbox: %v\n", err)
	os.Exit(HelperFailed)
}

// HelperFailed is the exit status of a helper that could not confine and
// start the command.
const HelperFailed = 126
