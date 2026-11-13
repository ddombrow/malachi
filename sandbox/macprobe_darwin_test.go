package sandbox

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestMacProbe is not a test: it tries things a sandboxed command should or
// should not be able to do on macOS and logs what happened, for the
// questions in docs/handoff-mac-sandbox.md. It was written on Linux and has
// never run. It skips unless MALACHI_MAC_PROBE is set, and never fails.
//
// Run it from a real terminal window, not from an agent's shell: the
// terminal probes need a controlling terminal.
//
//	MALACHI_MAC_PROBE=1 go test ./sandbox -run TestMacProbe -v -count=1
//
// Commands here are started the way v0.4.0 started them (in the caller's
// session), not the way the bash tool starts them now (in their own).
func TestMacProbe(t *testing.T) {
	if os.Getenv("MALACHI_MAC_PROBE") == "" {
		t.Skip("set MALACHI_MAC_PROBE=1 to run the probes")
	}
	requireBackend(t)
	p, project, outside, _, _ := fixture(t)
	p = projectOnly(p, project)
	report := func(name, script string, pol Policy) {
		out, ok := sh(t, pol, project, script)
		t.Logf("PROBE %-22s succeeded=%-5v output: %s", name, ok, out)
	}
	const py = "/usr/bin/python3"

	// 1. The controlling terminal.
	report("tty-write", "echo '[malachi probe wrote this to your terminal]' > /dev/tty && echo WROTE", p)
	report("tty-inject", py+` -c "
import fcntl, termios, os
fd = os.open('/dev/tty', os.O_RDWR)
for c in 'TIOCSTI_WORKED':
    fcntl.ioctl(fd, termios.TIOCSTI, c.encode())
print('INJECTED')
"`, p)

	// 2. Another terminal window: MALACHI_PROBE_TTY=/dev/ttys00N, from `tty`
	// run in that window.
	if other := os.Getenv("MALACHI_PROBE_TTY"); other != "" {
		report("other-tty-write", "echo '[malachi probe wrote this from another window]' > "+other+" && echo WROTE", p)
	} else {
		t.Log("PROBE other-tty-write       skipped: set MALACHI_PROBE_TTY to another window's tty")
	}

	// 3. Listening with the network off, and being reached from the LAN
	// address (dialled from this unsandboxed process, standing in for
	// another machine).
	off := p
	off.Network = false
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	for _, host := range []string{"0.0.0.0", "127.0.0.1"} {
		dial := "127.0.0.1"
		if host == "0.0.0.0" {
			if dial = lanAddress(); dial == "" {
				t.Log("PROBE listen-0.0.0.0        skipped: no LAN address on this machine")
				continue
			}
		}
		dialled := make(chan string, 1)
		go func() {
			time.Sleep(2 * time.Second)
			c, err := net.DialTimeout("tcp", net.JoinHostPort(dial, strconv.Itoa(port)), 3*time.Second)
			if err != nil {
				dialled <- "dial failed: " + err.Error()
				return
			}
			c.Close()
			dialled <- "dial to " + dial + " connected"
		}()
		report("listen-"+host, fmt.Sprintf(py+` -c "
import socket
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(('%s', %d))
s.listen()
print('LISTENING')
s.settimeout(6)
conn, peer = s.accept()
print('ACCEPTED from', peer[0])
"`, host, port), off)
		t.Logf("PROBE %-22s %s", "listen-"+host+" (dialler)", <-dialled)
	}

	// 4. A file outside the writable roots: metadata. Expected: all denied
	// (on Linux these succeed, which the README says).
	f := filepath.Join(outside, "f")
	if err := os.WriteFile(f, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	report("chmod-outside", "chmod 777 "+f, p)
	report("touch-outside", "touch -t 200101010000 "+f, p)
	report("xattr-outside", "xattr -w user.probe x "+f, p)
	report("truncate-outside", ": > "+f, p)

	// 5. A process outside the sandbox. Expected: its command line hidden;
	// signalling it allowed (the README says a command can).
	victim := exec.Command("sleep", "61")
	if err := victim.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = victim.Process.Kill(); _ = victim.Wait() }()
	pid := strconv.Itoa(victim.Process.Pid)
	report("ps-outside", "ps -p "+pid+" -o command=", p)
	report("signal-outside", "kill -0 "+pid+" && echo SIGNALLED", p)

	// 6. Other programs acting for the command. Expected: all denied. If
	// "open" is not, a browser tab opens on example.com.
	report("open-url", "open https://example.com/malachi-probe", p)
	report("osascript", `osascript -e 'tell application "Finder" to get name of startup disk'`, p)
	report("pasteboard", "pbpaste | head -c 40", p)
	report("keychain", "security dump-keychain 2>&1 | head -3", p)
}

// lanAddress is this machine's first IPv4 address that is not loopback.
func lanAddress() string {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && n.IP.To4() != nil {
			return n.IP.String()
		}
	}
	return ""
}
