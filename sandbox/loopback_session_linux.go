//go:build linux

package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const loopbackStopGrace = 3 * time.Second

// LoopbackSession owns one private network namespace. Every command it starts
// still gets its own Landlock and seccomp restrictions, but shares that
// namespace so session jobs can talk to later commands over loopback.
//
// The namespace is held by a session helper, which runs outside any sandbox
// and starts each command under the policy it is sent. So nothing but
// malachi may reach it: there is no socket with a path, which a command
// allowed Unix sockets could connect to and send a policy of its choosing.
// Malachi holds the only end of a control socketpair; for each command it
// makes a new socketpair, sends one end to the helper over the control
// channel and gives the other to the client process it starts (see
// runSessionClient) as an inherited descriptor.
type LoopbackSession struct {
	mu      sync.Mutex
	cmd     *exec.Cmd
	control *os.File
	closed  bool
}

type loopbackRequest struct {
	Policy Policy   `json:"policy"`
	Name   string   `json:"name"`
	Args   []string `json:"args"`
	Dir    string   `json:"dir"`
	Env    []string `json:"env"`
}

// NewLoopbackSession starts a private network namespace for the session.
func NewLoopbackSession(p Policy) (*LoopbackSession, error) {
	if !LoopbackInForce(p) {
		return nil, errors.New("private loopback is unavailable")
	}
	// SOCK_SEQPACKET keeps each passed descriptor with the byte it rides on.
	control, theirs, err := socketPair(unix.SOCK_SEQPACKET)
	if err != nil {
		return nil, fmt.Errorf("start private loopback session: %w", err)
	}
	defer theirs.Close() // the helper has its copy once started
	readyR, readyW, err := os.Pipe()
	if err != nil {
		control.Close()
		return nil, err
	}
	defer readyR.Close()
	cmd := exec.Command(selfExe, sessionArg, selfIDs())
	cmd.ExtraFiles = []*os.File{theirs, readyW}
	cmd.SysProcAttr = privateLoopbackSessionAttr()
	cmd.SysProcAttr.Setsid = true
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		_ = readyW.Close()
		control.Close()
		return nil, fmt.Errorf("start private loopback session: %w", err)
	}
	_ = readyW.Close()
	s := &LoopbackSession{cmd: cmd, control: control}
	ready := make(chan error, 1)
	go func() {
		var b [1]byte
		_, err := io.ReadFull(readyR, b[:])
		if err == nil && b[0] != 'R' {
			err = errors.New("invalid loopback session readiness response")
		}
		ready <- err
	}()
	select {
	case err := <-ready:
		if err == nil {
			return s, nil
		}
	case <-time.After(3 * time.Second):
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	control.Close()
	return nil, errors.New("private loopback session did not become ready")
}

// socketPair returns a connected pair of Unix sockets of type typ, neither
// inherited by commands unless handed to one.
func socketPair(typ int) (*os.File, *os.File, error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, typ|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	return os.NewFile(uintptr(fds[0]), "loopback-session"), os.NewFile(uintptr(fds[1]), "loopback-session"), nil
}

// Command returns a client process connected to the session helper. The
// command runs under the policy passed here, in the shared private namespace.
// The caller must call Started once it has started the client, or given up
// on it, so the helper sees the client go when it does.
func (s *LoopbackSession) Command(ctx context.Context, p Policy, name string, args ...string) (*exec.Cmd, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("private loopback session is closed")
	}
	p.Loopback = true
	p.Network = false
	raw, err := json.Marshal(loopbackRequest{Policy: p, Name: name, Args: args})
	if err != nil {
		return nil, err
	}
	helperEnd, clientEnd, err := socketPair(unix.SOCK_STREAM)
	if err != nil {
		return nil, fmt.Errorf("private loopback session: %w", err)
	}
	defer helperEnd.Close() // the helper has its copy once sent
	rights := unix.UnixRights(int(helperEnd.Fd()))
	if err := unix.Sendmsg(int(s.control.Fd()), []byte{'C'}, rights, nil, unix.MSG_NOSIGNAL); err != nil {
		clientEnd.Close()
		return nil, fmt.Errorf("private loopback session: %w", err)
	}
	cmd := exec.CommandContext(ctx, selfExe, sessionClientArg, base64.RawURLEncoding.EncodeToString(raw))
	cmd.ExtraFiles = []*os.File{clientEnd}
	return cmd, nil
}

// Close terminates the namespace helper and all command process groups it owns.
func (s *LoopbackSession) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	cmd := s.cmd
	s.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(2*loopbackStopGrace + time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}
	return s.control.Close()
}

// receiveConn waits on the control socket for the helper's end of the next
// command's socketpair. It fails when malachi has closed its end, or gone.
func receiveConn(control int) (net.Conn, error) {
	var b [1]byte
	oob := make([]byte, unix.CmsgSpace(4))
	for {
		n, oobn, _, _, err := unix.Recvmsg(control, b[:], oob, unix.MSG_CMSG_CLOEXEC)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if n == 0 && oobn == 0 {
			return nil, io.EOF
		}
		msgs, err := unix.ParseSocketControlMessage(oob[:oobn])
		if err != nil || len(msgs) != 1 {
			return nil, errors.New("bad loopback session control message")
		}
		fds, err := unix.ParseUnixRights(&msgs[0])
		if err != nil || len(fds) != 1 {
			for _, fd := range fds {
				_ = unix.Close(fd)
			}
			return nil, errors.New("bad loopback session control message")
		}
		f := os.NewFile(uintptr(fds[0]), "loopback-command")
		conn, err := net.FileConn(f)
		f.Close()
		return conn, err
	}
}

func runSessionHelper(ids string) error {
	runtime.LockOSThread()
	if err := mapSelf(ids); err != nil {
		return err
	}
	if err := privateLoopback(); err != nil {
		return fmt.Errorf("loopback networking: %w", err)
	}
	if err := dropCapabilities(); err != nil {
		return fmt.Errorf("dropping capabilities: %w", err)
	}
	if err := ProtectProcess(); err != nil {
		return fmt.Errorf("protecting session helper: %w", err)
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("enabling child reaping: %w", err)
	}
	// The control socket was inherited without close-on-exec; no command
	// may inherit it.
	control := 3
	unix.CloseOnExec(control)
	conns := make(chan net.Conn)
	go func() {
		defer close(conns)
		for {
			conn, err := receiveConn(control)
			if err != nil {
				return // malachi closed its end, or is gone
			}
			conns <- conn
		}
	}()

	var mu sync.Mutex
	children := map[int]*exec.Cmd{}
	groups := map[int]struct{}{}
	// reapOrphans takes every child that is not a command's, so the only
	// other child the helper starts, the pty probe, is run now, before it
	// does, and is not taken from under its own Wait.
	_ = privatePTYAvailable()
	go reapOrphans(&mu, children)
	var closing bool
	var wg, groupWG sync.WaitGroup
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)
	ready := os.NewFile(4, "loopback-session-ready")
	if ready != nil {
		_, _ = ready.Write([]byte{'R'})
		_ = ready.Close()
	}
	defer func() {
		mu.Lock()
		closing = true
		pids := make([]int, 0, len(groups))
		for pid := range groups {
			pids = append(pids, pid)
			_ = syscall.Kill(-pid, syscall.SIGTERM)
		}
		mu.Unlock()
		done := make(chan struct{})
		go func() {
			wg.Wait()
			groupWG.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(loopbackStopGrace):
			for _, pid := range pids {
				_ = syscall.Kill(-pid, syscall.SIGKILL)
			}
			<-done
		}
	}()
	for {
		var conn net.Conn
		select {
		case <-signals:
			return nil
		case c, ok := <-conns:
			if !ok {
				return nil
			}
			conn = c
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			serveLoopbackCommand(conn, &mu, children, groups, &groupWG, &closing)
		}()
	}
}

func serveLoopbackCommand(conn net.Conn, mu *sync.Mutex, children map[int]*exec.Cmd, groups map[int]struct{}, groupWG *sync.WaitGroup, closing *bool) {
	defer conn.Close()
	var req loopbackRequest
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		return
	}
	cwd := req.Dir
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	cmd, err := commandInLoopbackSession(context.Background(), req.Policy, req.Name, req.Args...)
	if err != nil {
		writeSessionOutput(conn, nil, 126)
		return
	}
	cmd.Dir, cmd.Env = cwd, req.Env
	cmd.Stdin = nil
	writer := &sessionWriter{conn: conn}
	cmd.Stdout, cmd.Stderr = writer, writer
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
	cmd.WaitDelay = 2 * time.Second
	mu.Lock()
	if *closing {
		mu.Unlock()
		return
	}
	if err := cmd.Start(); err != nil {
		mu.Unlock()
		writeSessionOutput(conn, []byte(err.Error()+"\n"), 126)
		return
	}
	children[cmd.Process.Pid] = cmd
	groups[cmd.Process.Pid] = struct{}{}
	groupWG.Add(1)
	mu.Unlock()
	defer func() {
		mu.Lock()
		delete(children, cmd.Process.Pid)
		mu.Unlock()
	}()

	processDone := make(chan struct{})
	controlDone := make(chan struct{})
	go func() {
		defer close(controlDone)
		// A normal completion is marked explicitly. EOF means the client was
		// cancelled or stopped, and takes the command group down with it.
		var marker [1]byte
		n, _ := io.ReadFull(conn, marker[:])
		if n == 1 && marker[0] == 'N' {
			return
		}
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-time.After(loopbackStopGrace):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		case <-processDone:
		}
	}()
	waitErr := cmd.Wait()
	close(processDone)
	go reapLoopbackGroup(cmd.Process.Pid, mu, groups, groupWG)
	_ = conn.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
	writeSessionOutput(conn, nil, processExitCode(cmd, waitErr))
	// Closing conn here would end the read above with EOF, and so kill the
	// group (servers the command left running included) before the client
	// could say the command finished normally. The client answers or exits,
	// either of which ends the read.
	<-controlDone
}

// reapOrphans reaps the processes reparented to the session helper, its
// commands' orphans: being their subreaper (PR_SET_CHILD_SUBREAPER), it is
// their parent once theirs has gone, and one it does not reap stays a zombie
// until the session ends. A process left in a session of its own (setsid, a
// daemon, a background job's processes once its shell is gone) is not in any
// command's group, so reapLoopbackGroup never reaches it. A command's own
// process is left to its cmd.Wait, which reports how it ended.
func reapOrphans(mu *sync.Mutex, children map[int]*exec.Cmd) {
	sigchld := make(chan os.Signal, 1)
	signal.Notify(sigchld, syscall.SIGCHLD)
	tick := time.NewTicker(time.Second) // a SIGCHLD may be coalesced
	defer tick.Stop()
	self := os.Getpid()
	for {
		for _, pid := range zombieChildren(self) {
			mu.Lock()
			_, command := children[pid]
			mu.Unlock()
			if !command {
				var status unix.WaitStatus
				_, _ = unix.Wait4(pid, &status, unix.WNOHANG, nil)
			}
		}
		select {
		case <-sigchld:
		case <-tick.C:
		}
	}
}

// zombieChildren lists the children of parent that have exited and not been
// reaped, from /proc.
func zombieChildren(parent int) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		// "pid (comm) state ppid ...": comm may hold spaces and parentheses,
		// so the fields are read after its last ")".
		i := bytes.LastIndexByte(stat, ')')
		if i < 0 {
			continue
		}
		fields := strings.Fields(string(stat[i+1:]))
		if len(fields) < 2 || fields[0] != "Z" {
			continue
		}
		if ppid, err := strconv.Atoi(fields[1]); err == nil && ppid == parent {
			out = append(out, pid)
		}
	}
	return out
}

func reapLoopbackGroup(pgid int, mu *sync.Mutex, groups map[int]struct{}, wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		var status unix.WaitStatus
		_, _ = unix.Wait4(-pgid, &status, unix.WNOHANG, nil)
		err := unix.Kill(-pgid, 0)
		if errors.Is(err, unix.ESRCH) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	delete(groups, pgid)
	mu.Unlock()
}

// processExitCode is the status sent to the client: the exit code, or minus
// the signal that ended the command, which the client then ends by too (see
// exitAs).
func processExitCode(cmd *exec.Cmd, err error) int {
	if cmd.ProcessState != nil {
		if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return -int(ws.Signal())
		}
		return cmd.ProcessState.ExitCode()
	}
	if err != nil {
		return 1
	}
	return 0
}

type sessionWriter struct {
	conn net.Conn
	mu   sync.Mutex
}

func (w *sessionWriter) Write(p []byte) (int, error) {
	length := len(p)
	w.mu.Lock()
	defer w.mu.Unlock()
	const frame = 32 << 10
	for len(p) > 0 {
		n := min(len(p), frame)
		if err := writeFrame(w.conn, p[:n]); err != nil {
			return 0, err
		}
		p = p[n:]
	}
	return length, nil
}

func writeFrame(w io.Writer, p []byte) error {
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(p)))
	if err := writeAll(w, header[:]); err != nil {
		return err
	}
	return writeAll(w, p)
}

func writeAll(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}

func writeSessionOutput(w io.Writer, p []byte, exit int) {
	if len(p) != 0 {
		_ = writeFrame(w, p)
	}
	var trailer [8]byte
	binary.BigEndian.PutUint32(trailer[:4], 0)
	binary.BigEndian.PutUint32(trailer[4:], uint32(int32(exit)))
	_ = writeAll(w, trailer[:])
}

// exitAs ends the client the way its command ended, so whoever waits on the
// client sees what it would have of the command: the exit code, or, for
// minus a signal, death by that signal. A signal the Go runtime will not let
// end the process by default leaves the shell's convention, 128 plus it.
func exitAs(code int) {
	if code >= 0 {
		if code > 255 {
			code = 1
		}
		os.Exit(code)
	}
	sig := syscall.Signal(-code)
	signal.Reset(sig)
	_ = syscall.Kill(os.Getpid(), sig)
	time.Sleep(time.Second)
	os.Exit(128 + int(sig))
}

// runSessionClient runs one command through the session helper: it sends the
// request over the socket malachi handed it as descriptor 3, which is the
// only way to reach the helper, and relays the command's output and exit
// status. The request carries its own working directory and environment,
// which the caller set on this process.
func runSessionClient(encoded string) error {
	if err := ProtectProcess(); err != nil {
		return fmt.Errorf("protecting session client: %w", err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return err
	}
	var req loopbackRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	req.Dir, req.Env = cwd, os.Environ()
	inherited := os.NewFile(3, "loopback-session")
	conn, err := net.FileConn(inherited)
	inherited.Close()
	if err != nil {
		return fmt.Errorf("no connection to the loopback session: %w", err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return err
	}
	reader := bufio.NewReader(conn)
	for {
		var header [4]byte
		if _, err := io.ReadFull(reader, header[:]); err != nil {
			return err
		}
		n := binary.BigEndian.Uint32(header[:])
		if n == 0 {
			var trailer [4]byte
			if _, err := io.ReadFull(reader, trailer[:]); err != nil {
				return err
			}
			_ = writeAll(conn, []byte{'N'})
			exitAs(int(int32(binary.BigEndian.Uint32(trailer[:]))))
		}
		if n > 1<<20 {
			return errors.New("invalid loopback output frame")
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(reader, buf); err != nil {
			return err
		}
		if _, err := os.Stdout.Write(buf); err != nil {
			return err
		}
	}
}
