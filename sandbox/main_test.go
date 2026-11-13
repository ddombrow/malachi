package sandbox

import (
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

// sleepEnv set makes the test binary a process that only waits, for tests
// that need one outside the sandbox.
const sleepEnv = "MALACHI_SANDBOX_TEST_SLEEP"

// listenEnv set to "tcp 127.0.0.1:0" or "udp [::]:0" makes the test binary
// try to listen there and say whether it could, for tests of what a
// sandboxed command may serve.
const listenEnv = "MALACHI_SANDBOX_TEST_LISTEN"

func listenAndExit(spec string) {
	network, addr, _ := strings.Cut(spec, " ")
	var err error
	if strings.HasPrefix(network, "udp") {
		var c net.PacketConn
		if c, err = net.ListenPacket(network, addr); err == nil {
			c.Close()
		}
	} else {
		var l net.Listener
		if l, err = net.Listen(network, addr); err == nil {
			l.Close()
		}
	}
	if err != nil {
		fmt.Println("refused:", err)
		os.Exit(1)
	}
	fmt.Println("listening")
	os.Exit(0)
}

// The test binary doubles as the sandbox helper, as malachi does.
func TestMain(m *testing.M) {
	MaybeRunHelper()
	if spec := os.Getenv(listenEnv); spec != "" {
		listenAndExit(spec)
	}
	if os.Getenv(sleepEnv) != "" {
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}
