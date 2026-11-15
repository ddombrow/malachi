package acp

import (
	"errors"
	"io/fs"
	"os"
	"testing"

	"github.com/ddombrow/malachi/sandbox"
)

func TestMain(m *testing.M) {
	sandbox.MaybeRunHelper() // this binary is the helper for sandboxed commands on Linux
	// Sessions opened without an explicit Home land in a throwaway directory,
	// never the developer's ~/.malachi.
	home, err := os.MkdirTemp("", "malachi-test-home-")
	if err != nil {
		panic(err)
	}
	os.Setenv("MALACHI_HOME", home)
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

// requireSandbox skips a test of what a sandboxed session shows or does
// where there is no sandbox to be had: with it unavailable a session opens
// by saying so and its commands fail. That includes a test run by malachi
// itself, whose sandbox cannot hold another.
func requireSandbox(t *testing.T) {
	t.Helper()
	if err := sandbox.Available(); err != nil {
		if os.Getenv("MALACHI_REQUIRE_SANDBOX") != "" {
			t.Fatalf("sandbox required but unavailable: %v", err)
		}
		t.Skipf("no sandbox here: %v", err)
	}
}

// homeDir makes a directory in the user's home, which is where a directory
// has to be for the sandbox to treat it as private, and removes it after
// the test. It skips where the home directory cannot be written, as in a
// sandbox.
func homeDir(t *testing.T, pattern string) string {
	t.Helper()
	dir, err := os.MkdirTemp(os.Getenv("HOME"), pattern)
	if errors.Is(err, fs.ErrPermission) {
		t.Skipf("cannot write the home directory: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}
