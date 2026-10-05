// Package rpc runs a coding session headless, driven by JSON commands on one
// stream and reporting responses and events on another, one JSON object per
// line. It is a frontend like tui: the session's rules apply, and nothing
// here decides what may run.
//
// The protocol is tau's RPC mode (tau_coding/rpc.py), which follows Pi's, so
// clients written for either should work against malachi.
package rpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/ddombrow/malachi/coding"
)

// maxRecordBytes bounds one command line, as tau does.
const maxRecordBytes = 16 << 20

// Server serves one session over a pair of streams.
type Server struct {
	in  io.Reader
	out io.Writer

	// wmu serializes every write, so records never interleave. Commands that
	// start work hold it across the session call and their response, which
	// is what puts a prompt's response ahead of the prompt's events: the
	// session delivers events from its own goroutine, and they wait here.
	wmu sync.Mutex
	enc *json.Encoder

	smu   sync.Mutex
	s     *coding.Session
	unsub func()

	ctx context.Context
	bg  sync.WaitGroup // commands answered from a goroutine
}

// New returns a server for s reading commands from in and writing to out.
func New(s *coding.Session, in io.Reader, out io.Writer) *Server {
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	return &Server{in: in, out: out, enc: enc, s: s}
}

// Session returns the session the server is currently driving; new_session
// and switch_session replace it.
func (sv *Server) Session() *coding.Session {
	sv.smu.Lock()
	defer sv.smu.Unlock()
	return sv.s
}

// Run serves commands until in reaches EOF, reading in fails, or ctx ends.
// However it ends, it stops whatever the session is doing and delivers every
// event that work produced before returning, so a client reading until the
// stream closes misses nothing. It returns nil at EOF, the read error, or
// ctx.Err().
//
// Reading happens on its own goroutine so that ctx can end Run while the
// input is idle. Run does not own in and does not close it, so if ctx ends
// while a Read is blocked, that goroutine exits when the Read returns; it
// never touches the session again. Close in to release it sooner.
func (sv *Server) Run(ctx context.Context) error {
	sv.ctx = ctx
	sv.attach(sv.Session())

	type read struct {
		line    []byte
		tooLong bool
		err     error
	}
	reads := make(chan read)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		r := bufio.NewReaderSize(sv.in, 64*1024)
		for {
			line, tooLong, err := readRecord(r)
			select {
			case reads <- read{line, tooLong, err}:
			case <-stop:
				return
			}
			if err != nil {
				return
			}
		}
	}()

	var runErr error
loop:
	for {
		select {
		case <-ctx.Done():
			runErr = ctx.Err()
			break loop
		case rd := <-reads:
			// Records are dispatched here, on Run's goroutine, in order.
			if rd.tooLong {
				sv.fail(nil, "parse", "RPC record exceeds 16 MiB")
			} else if len(bytes.TrimSpace(rd.line)) > 0 {
				sv.handle(rd.line)
			}
			if rd.err != nil {
				if rd.err != io.EOF {
					runErr = rd.err
				}
				break loop
			}
		}
	}

	sv.shutdown()
	return runErr
}

// shutdown stops the session's work, waits for it and for commands still
// being answered, delivers the remaining events, and detaches.
func (sv *Server) shutdown() {
	s := sv.Session()
	s.Abort()
	waitIdle(s)
	sv.bg.Wait()
	s.Flush()
	sv.detach()
}

// readRecord reads one LF-terminated record, without the LF or a trailing CR.
// Only LF ends a record: a U+2028 inside a JSON string must survive. A record
// over maxRecordBytes is consumed and reported as too long.
func readRecord(r *bufio.Reader) (line []byte, tooLong bool, err error) {
	for {
		chunk, err := r.ReadSlice('\n')
		if len(line)+len(chunk) > maxRecordBytes {
			tooLong = true
		} else {
			line = append(line, chunk...)
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		line = bytes.TrimSuffix(line, []byte("\n"))
		line = bytes.TrimSuffix(line, []byte("\r"))
		if tooLong {
			line = nil
		}
		return line, tooLong, err
	}
}

// waitIdle waits for an aborted session to stop, so its last events exist
// before they are flushed.
func waitIdle(s *coding.Session) {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if st := s.State(); !st.Running && !st.Compacting {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// attach starts forwarding s's events.
func (sv *Server) attach(s *coding.Session) {
	unsub := s.Subscribe(func(e any) {
		raw, err := eventJSON(e)
		if err != nil {
			return
		}
		sv.wmu.Lock()
		defer sv.wmu.Unlock()
		sv.writeRawLocked(raw)
	})
	sv.smu.Lock()
	sv.s, sv.unsub = s, unsub
	sv.smu.Unlock()
}

func (sv *Server) detach() {
	sv.smu.Lock()
	unsub := sv.unsub
	sv.unsub = nil
	sv.smu.Unlock()
	if unsub != nil {
		unsub()
	}
}

func (sv *Server) writeRawLocked(raw []byte) {
	_, _ = sv.out.Write(append(raw, '\n'))
}

func (sv *Server) writeLocked(v any) {
	_ = sv.enc.Encode(v) // Encode writes the trailing newline
}

func (sv *Server) ok(id any, command string, data any) {
	sv.wmu.Lock()
	defer sv.wmu.Unlock()
	sv.okLocked(id, command, data)
}

func (sv *Server) okLocked(id any, command string, data any) {
	sv.writeLocked(response{Type: "response", Command: command, Success: true, ID: id, Data: data})
}

func (sv *Server) fail(id any, command, msg string) {
	sv.wmu.Lock()
	defer sv.wmu.Unlock()
	sv.failLocked(id, command, msg)
}

func (sv *Server) failLocked(id any, command, msg string) {
	sv.writeLocked(response{Type: "response", Command: command, Success: false, ID: id, Error: msg})
}

// handle parses and dispatches one record. Malformed records are answered
// and reading continues, as tau does.
func (sv *Server) handle(line []byte) {
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber() // echo numeric ids exactly
	var v any
	if err := dec.Decode(&v); err != nil {
		sv.fail(nil, "parse", "Failed to parse command: "+parseError(err))
		return
	}
	// A record is exactly one JSON value: trailing data, including a second
	// object, makes the whole record malformed, as a full parse would.
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		sv.fail(nil, "parse", "Failed to parse command: unexpected data after the JSON value")
		return
	}
	cmd, ok := v.(map[string]any)
	if !ok {
		sv.fail(nil, "parse", "Command must be a JSON object")
		return
	}
	id := cmd["id"]
	typ, ok := cmd["type"].(string)
	if !ok {
		sv.fail(id, "parse", "Command requires a string 'type'")
		return
	}
	sv.dispatch(id, typ, cmd)
}

// parseError shortens encoding/json's messages to the part a client can use.
func parseError(err error) string {
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		return fmt.Sprintf("%s (at byte %d)", syntax.Error(), syntax.Offset)
	}
	return err.Error()
}
