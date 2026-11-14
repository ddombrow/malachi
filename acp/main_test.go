package acp

import (
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
