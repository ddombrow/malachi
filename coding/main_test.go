package coding

import (
	"os"
	"testing"

	"github.com/ddombrow/malachi/sandbox"
)

// TestMain points MALACHI_HOME at a throwaway directory, so a test that opens
// a session without an explicit Home writes there instead of into the
// developer's real ~/.malachi.
func TestMain(m *testing.M) {
	sandbox.MaybeRunHelper() // this binary is the helper for sandboxed commands on Linux
	if os.Getenv(fakeMCPEnv) != "" {
		fakeMCPServer() // this binary is also a fake MCP server
		os.Exit(0)
	}
	// Run as malachi does (see cmd/malachi): not dumpable, which changes
	// what starting a sandboxed command in a namespace takes. Root is refused.
	if os.Geteuid() != 0 {
		if err := sandbox.ProtectProcess(); err != nil {
			panic(err)
		}
	}
	home, err := os.MkdirTemp("", "malachi-test-home-")
	if err != nil {
		panic(err)
	}
	os.Setenv("MALACHI_HOME", home)
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}
