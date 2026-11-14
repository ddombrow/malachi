package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
)

// maxMessageBytes bounds one message from the client. A longer line is
// consumed and answered with a parse error; reading continues.
const maxMessageBytes = 16 << 20

// JSON-RPC error codes.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// Error is a JSON-RPC error, sent to the client or received from it.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("%s (code %d)", e.Message, e.Code) }

func invalidParams(err error) *Error {
	return &Error{Code: codeInvalidParams, Message: err.Error()}
}

// errClosed is returned for requests on a connection that has ended.
var errClosed = errors.New("acp: connection closed")

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

// handler is the agent side of a connection.
type handler interface {
	// request answers a request from the client. It runs on its own
	// goroutine; a returned *Error is sent as is, any other error as an
	// internal error.
	request(ctx context.Context, method string, params json.RawMessage) (any, error)
	// notify handles a notification, on the reading goroutine and in order.
	notify(method string, params json.RawMessage)
}

type reply struct {
	result json.RawMessage
	err    error
}

// conn is one JSON-RPC 2.0 connection over newline-delimited messages, the
// stdio transport ACP uses. It serves the client's requests and sends the
// agent's own (session/request_permission), matching replies by id.
type conn struct {
	r   io.Reader
	w   io.Writer
	wmu sync.Mutex // every write; messages never interleave

	mu      sync.Mutex
	nextID  int64
	pending map[string]chan reply
	closed  bool

	inflight sync.WaitGroup // requests being answered
}

func newConn(r io.Reader, w io.Writer) *conn {
	return &conn{r: r, w: w, pending: map[string]chan reply{}}
}

// serve reads messages until the input ends, ctx ends, or reading fails,
// then waits for the requests it started to be answered. It returns nil at
// EOF.
//
// Reading happens on its own goroutine so ctx can end serve while the input
// is idle; that goroutine exits when its Read returns and touches nothing
// after.
func (c *conn) serve(ctx context.Context, h handler) error {
	type read struct {
		line    []byte
		tooLong bool
		err     error
	}
	reads := make(chan read)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		br := bufio.NewReaderSize(c.r, 64<<10)
		for {
			line, tooLong, err := readRecord(br)
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

	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var err error
loop:
	for {
		select {
		case <-ctx.Done():
			err = ctx.Err()
			break loop
		case rd := <-reads:
			switch {
			case rd.tooLong:
				c.fail(nil, &Error{Code: codeParseError, Message: fmt.Sprintf("message over %d bytes", maxMessageBytes)})
			case len(bytes.TrimSpace(rd.line)) > 0:
				c.dispatch(reqCtx, h, rd.line)
			}
			if rd.err != nil {
				if rd.err != io.EOF {
					err = rd.err
				}
				break loop
			}
		}
	}
	c.close()
	return err
}

// wait waits for the requests being answered.
func (c *conn) wait() { c.inflight.Wait() }

// readRecord reads one LF-terminated record, without the LF or a trailing CR.
// A record over maxMessageBytes is consumed and reported as too long.
func readRecord(r *bufio.Reader) (line []byte, tooLong bool, err error) {
	for {
		chunk, err := r.ReadSlice('\n')
		if len(line)+len(chunk) > maxMessageBytes {
			tooLong = true
		} else if !tooLong {
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

func (c *conn) dispatch(ctx context.Context, h handler, line []byte) {
	var m message
	if err := json.Unmarshal(line, &m); err != nil {
		c.fail(nil, &Error{Code: codeParseError, Message: "parse error: " + err.Error()})
		return
	}
	switch {
	case m.Method != "" && len(m.ID) > 0:
		c.inflight.Go(func() {
			result, err := h.request(ctx, m.Method, m.Params)
			if err != nil {
				var rpcErr *Error
				if !errors.As(err, &rpcErr) {
					rpcErr = &Error{Code: codeInternalError, Message: err.Error()}
				}
				c.fail(m.ID, rpcErr)
				return
			}
			c.ok(m.ID, result)
		})
	case m.Method != "":
		h.notify(m.Method, m.Params)
	case len(m.ID) > 0:
		c.mu.Lock()
		ch := c.pending[string(m.ID)]
		delete(c.pending, string(m.ID))
		c.mu.Unlock()
		if ch == nil {
			return // cancelled, or never ours
		}
		if m.Error != nil {
			ch <- reply{err: m.Error}
		} else {
			ch <- reply{result: m.Result}
		}
	default:
		c.fail(nil, &Error{Code: codeInvalidRequest, Message: "not a JSON-RPC request, notification or response"})
	}
}

func (c *conn) ok(id json.RawMessage, result any) {
	if result == nil {
		result = struct{}{}
	}
	_ = c.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

// fail sends an error response; a nil id is sent as null, for a message
// whose id could not be read.
func (c *conn) fail(id json.RawMessage, e *Error) {
	var rid any = id
	if id == nil {
		rid = nil
	}
	_ = c.send(map[string]any{"jsonrpc": "2.0", "id": rid, "error": e})
}

func (c *conn) send(v any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil { // Encode writes the trailing newline
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err := c.w.Write(buf.Bytes())
	return err
}

// notifyClient sends a notification to the client.
func (c *conn) notifyClient(method string, params any) error {
	return c.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// call sends a request to the client and waits for its reply, ctx, or the
// connection's end.
func (c *conn) call(ctx context.Context, method string, params, result any) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errClosed
	}
	c.nextID++
	id := c.nextID
	key := strconv.FormatInt(id, 10)
	ch := make(chan reply, 1)
	c.pending[key] = ch
	c.mu.Unlock()

	if err := c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		c.forget(key)
		return err
	}
	select {
	case r := <-ch:
		if r.err != nil {
			return r.err
		}
		if result == nil {
			return nil
		}
		return json.Unmarshal(r.result, result)
	case <-ctx.Done():
		c.forget(key)
		return ctx.Err()
	}
}

func (c *conn) forget(key string) {
	c.mu.Lock()
	delete(c.pending, key)
	c.mu.Unlock()
}

// close fails every request waiting on the client.
func (c *conn) close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	pending := c.pending
	c.pending = map[string]chan reply{}
	c.mu.Unlock()
	for _, ch := range pending {
		ch <- reply{err: errClosed}
	}
}
