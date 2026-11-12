package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// stderrKept is how much of a server's stderr is kept, from the end, to
// explain why it failed.
const stderrKept = 16 << 10

// shutdownGrace is how long Close waits for a server to exit after its input
// closes, and again after asking it to terminate, before killing it.
const shutdownGrace = 2 * time.Second

// Process is a server malachi started, and the client connected to it.
type Process struct {
	*Client
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  io.Closer
	stderr  *tailBuffer
	exited  chan struct{}
	waitErr error
	once    sync.Once
}

// Start runs cmd as a server over its standard streams and connects to it.
// The caller builds cmd, sandbox and environment included; Start sets its
// stdin, stdout and stderr. If the server fails to start or to connect, the
// error carries the end of what it wrote to stderr.
func Start(ctx context.Context, cmd *exec.Cmd, opts Options) (*Process, error) {
	// Plain pipes, not cmd's: Wait closes those as soon as the process
	// exits, which could drop the last message it wrote.
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return nil, err
	}
	p := &Process{cmd: cmd, stdin: inW, stdout: outR, stderr: &tailBuffer{max: stderrKept}, exited: make(chan struct{})}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, p.stderr
	err = cmd.Start()
	inR.Close()
	outW.Close()
	if err != nil {
		inW.Close()
		outR.Close()
		return nil, fmt.Errorf("mcp: start %s: %w", cmd.Path, err)
	}
	go func() {
		p.waitErr = cmd.Wait()
		close(p.exited)
	}()
	// When the output ends, say why: the exit status and the end of stderr.
	opts.eof = func() error {
		select {
		case <-p.exited:
			return p.explain(fmt.Errorf("mcp: server exited: %v", exitText(p.waitErr)))
		case <-time.After(time.Second):
			return p.explain(errors.New("mcp: server closed its output"))
		}
	}
	cl, err := Connect(ctx, outR, inW, opts)
	if err != nil {
		// A server that died early can fail the handshake on a write
		// (broken pipe) before its output is seen to end; either way, why
		// it exited is the useful part.
		if !errors.As(err, new(closedError)) {
			select {
			case <-p.exited:
				err = p.explain(fmt.Errorf("mcp: server exited: %v (%w)", exitText(p.waitErr), err))
			case <-time.After(time.Second):
			}
		}
		p.stop()
		return nil, err
	}
	p.Client = cl
	return p, nil
}

// Stderr is the end of what the server wrote to stderr.
func (p *Process) Stderr() string { return p.stderr.String() }

// Exited is closed when the server process has exited.
func (p *Process) Exited() <-chan struct{} { return p.exited }

// Close ends the connection and stops the server: its input closes, as the
// spec's graceful shutdown asks, and it is killed if it does not exit.
func (p *Process) Close() {
	if p.Client != nil {
		p.Client.Close()
	}
	p.stop()
}

func (p *Process) stop() {
	p.once.Do(func() {
		// Its output closes last, once it has exited: a child it started
		// could otherwise hold the pipe, and the reader, open.
		defer p.stdout.Close()
		_ = p.stdin.Close()
		select {
		case <-p.exited:
			return
		case <-time.After(shutdownGrace):
		}
		_ = p.cmd.Process.Signal(sigterm)
		select {
		case <-p.exited:
			return
		case <-time.After(shutdownGrace):
		}
		_ = p.cmd.Process.Kill()
		<-p.exited
	})
}

// explain adds the end of the server's stderr to err.
func (p *Process) explain(err error) error {
	tail := strings.TrimSpace(p.stderr.String())
	if tail == "" {
		return err
	}
	lines := strings.Split(tail, "\n")
	if len(lines) > 10 {
		lines = lines[len(lines)-10:]
	}
	return fmt.Errorf("%w\nserver stderr:\n%s", err, strings.Join(lines, "\n"))
}

func exitText(err error) string {
	var ee *exec.ExitError
	switch {
	case err == nil:
		return "status 0"
	case errors.As(err, &ee):
		return ee.ProcessState.String()
	}
	return err.Error()
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if extra := len(t.b) - t.max; extra > 0 {
		t.b = append(t.b[:0], t.b[extra:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}
