package sandbox

import (
	"os"
	"os/exec"
	"testing"
)

func TestProtectProcessHidesEnvironmentFromChild(t *testing.T) {
	if mode := os.Getenv("MALACHI_PROTECT_TEST_MODE"); mode != "" {
		if mode == "protected" {
			if err := ProtectProcess(); err != nil {
				os.Exit(2)
			}
		}
		cmd := exec.Command("/bin/sh", "-c", `grep -aq '^MALACHI_PROTECT_TEST_MARKER=red-team-probe$' /proc/$PPID/environ`)
		if err := cmd.Run(); err == nil {
			os.Exit(42) // the child could read this process's environment
		}
		os.Exit(0)
	}

	run := func(mode string) int {
		cmd := exec.Command(os.Args[0], "-test.run=^TestProtectProcessHidesEnvironmentFromChild$")
		cmd.Env = append(os.Environ(), "MALACHI_PROTECT_TEST_MODE="+mode, "MALACHI_PROTECT_TEST_MARKER=red-team-probe")
		err := cmd.Run()
		if err == nil {
			return 0
		}
		if exit, ok := err.(*exec.ExitError); ok {
			return exit.ExitCode()
		}
		t.Fatalf("run %s child: %v", mode, err)
		return -1
	}
	if code := run("baseline"); code != 42 {
		t.Skipf("kernel already prevents child process environment inspection (exit %d)", code)
	}
	if code := run("protected"); code != 0 {
		t.Fatalf("protected process environment remained visible to a child (exit %d)", code)
	}
}

// Landlock before ABI 3 runs but cannot stop truncation; that is said, and
// nothing is said where there is nothing to say.
func TestWeaknessNamesOldLandlock(t *testing.T) {
	for abi, weak := range map[int]bool{0: false, 1: true, 2: true, 3: false, 6: false} {
		if got := abiWeakness(abi); (got != "") != weak {
			t.Errorf("abiWeakness(%d) = %q, want a weakness: %v", abi, got, weak)
		}
	}
}
