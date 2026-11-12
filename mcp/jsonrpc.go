package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
)

// MaxMessageBytes caps one message from a server. A line past it ends the
// connection: a server that floods its output is not one to keep reading.
const MaxMessageBytes = 32 << 20

// JSON-RPC and MCP error codes.
const (
	CodeParseError         = -32700
	CodeInvalidRequest     = -32600
	CodeMethodNotFound     = -32601
	CodeInvalidParams      = -32602
	CodeInternalError      = -32603
	CodeUnsupportedVersion = -32022
)

// Error is a JSON-RPC error a server returned.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("%s (code %d)", e.Message, e.Code) }

// ErrClosed is returned for requests on a connection that has ended.
var ErrClosed = errors.New("mcp: connection closed")

// closedError is ErrClosed with why the connection ended (the server
// exited, and the end of its stderr), which it reads as alone.
type closedError struct{ why error }

func (e closedError) Error() string   { return e.why.Error() }
func (e closedError) Unwrap() []error { return []error{ErrClosed, e.why} }

// message is any JSON-RPC message, decoded loosely: which kind it is follows
// from which fields are present.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

type response struct {
	result json.RawMessage
	err    error
}

// conn is one JSON-RPC connection over newline-delimited messages. It reads
// in its own goroutine, matching responses to requests by id; notifications
// go to onNotify, and requests from the server to onRequest.
type conn struct {
	w   io.Writer
	wmu sync.Mutex

	mu      sync.Mutex
	nextID  int64
	pending map[string]chan response
	closed  bool
	err     error // why the connection ended
	done    chan struct{}

	onNotify  func(method string, params json.RawMessage)
	onRequest func(method string, params json.RawMessage) (any, *Error)
	// eof, if set, explains the end of the input.
	eof func() error
}

func newConn(r io.Reader, w io.Writer, eof func() error) *conn {
	c := &conn{w: w, pending: map[string]chan response{}, done: make(chan struct{}), eof: eof}
	go c.read(r)
	return c
}

func (c *conn) read(r io.Reader) {
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := readLine(br)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) {
				err = ErrClosed
				if c.eof != nil {
					err = closedError{c.eof()}
				}
			}
			c.shut(err)
			return
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var m message
		if err := json.Unmarshal(line, &m); err != nil {
			// The spec forbids anything but messages on stdout; a stray line
			// is the server's bug, not a reason to drop the connection.
			continue
		}
		c.dispatch(m)
	}
}

// readLine reads one newline-terminated line of at most MaxMessageBytes.
func readLine(br *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > MaxMessageBytes {
			return nil, fmt.Errorf("mcp: message over %d bytes", MaxMessageBytes)
		}
		switch {
		case err == nil:
			return buf, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && len(bytes.TrimSpace(buf)) > 0:
			return buf, nil
		default:
			return nil, err
		}
	}
}

func (c *conn) dispatch(m message) {
	switch {
	case m.Method != "" && len(m.ID) > 0:
		go c.answer(m)
	case m.Method != "":
		if c.onNotify != nil {
			c.onNotify(m.Method, m.Params)
		}
	case len(m.ID) > 0:
		c.mu.Lock()
		ch := c.pending[string(m.ID)]
		delete(c.pending, string(m.ID))
		c.mu.Unlock()
		if ch == nil {
			return // cancelled, timed out, or never ours
		}
		if m.Error != nil {
			ch <- response{err: m.Error}
		} else {
			ch <- response{result: m.Result}
		}
	}
}

// answer replies to a request from the server.
func (c *conn) answer(m message) {
	var result any
	rpcErr := &Error{Code: CodeMethodNotFound, Message: "method not found: " + m.Method}
	if c.onRequest != nil {
		result, rpcErr = c.onRequest(m.Method, m.Params)
	}
	reply := map[string]any{"jsonrpc": "2.0", "id": m.ID}
	if rpcErr != nil {
		reply["error"] = rpcErr
	} else {
		reply["result"] = result
	}
	_ = c.send(reply)
}

func (c *conn) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if _, err := c.w.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("mcp: write: %w", err)
	}
	return nil
}

// request sends method and waits for its response. When ctx ends first, the
// server is told the request is cancelled.
func (c *conn) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	if c.closed {
		err := c.err
		c.mu.Unlock()
		return nil, err
	}
	c.nextID++
	id := c.nextID
	key := strconv.FormatInt(id, 10)
	ch := make(chan response, 1)
	c.pending[key] = ch
	c.mu.Unlock()

	if err := c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		c.forget(key)
		return nil, err
	}
	select {
	case r := <-ch:
		return r.result, r.err
	case <-ctx.Done():
		c.forget(key)
		_ = c.notify("notifications/cancelled", map[string]any{"requestId": id, "reason": ctx.Err().Error()})
		return nil, ctx.Err()
	}
}

func (c *conn) forget(key string) {
	c.mu.Lock()
	delete(c.pending, key)
	c.mu.Unlock()
}

func (c *conn) notify(method string, params any) error {
	m := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		m["params"] = params
	}
	return c.send(m)
}

// shut ends the connection, failing every pending request with err.
func (c *conn) shut(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed, c.err = true, err
	pending := c.pending
	c.pending = map[string]chan response{}
	c.mu.Unlock()
	for _, ch := range pending {
		ch <- response{err: err}
	}
	close(c.done)
}
