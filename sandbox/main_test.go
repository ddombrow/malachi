package sandbox

import (
	"os"
	"testing"
)

// The test binary doubles as the sandbox helper, as malachi does.
func TestMain(m *testing.M) {
	MaybeRunHelper()
	os.Exit(m.Run())
}
