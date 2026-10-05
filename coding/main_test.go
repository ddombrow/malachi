package coding

import (
	"os"
	"testing"
)

// TestMain points MALACHI_HOME at a throwaway directory, so a test that opens
// a session without an explicit Home writes there instead of into the
// developer's real ~/.malachi.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "malachi-test-home-")
	if err != nil {
		panic(err)
	}
	os.Setenv("MALACHI_HOME", home)
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}
