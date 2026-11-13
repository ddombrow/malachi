package sandbox

import (
	"os"
	"testing"
	"time"
)

// sleepEnv set makes the test binary a process that only waits, for tests
// that need one outside the sandbox.
const sleepEnv = "MALACHI_SANDBOX_TEST_SLEEP"

// The test binary doubles as the sandbox helper, as malachi does.
func TestMain(m *testing.M) {
	MaybeRunHelper()
	if os.Getenv(sleepEnv) != "" {
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}
