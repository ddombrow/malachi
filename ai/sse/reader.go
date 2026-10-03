// Package sse parses text/event-stream bodies.
package sse

import (
	"bufio"
	"io"
	"strings"
)

// Event is one dispatched server-sent event.
type Event struct {
	Event string // "event:" field; empty means "message"
	Data  string // "data:" lines joined with "\n"
	ID    string
}

// Reader yields events from an SSE stream.
type Reader struct {
	r *bufio.Reader
}

// NewReader wraps r. Lines may be arbitrarily long.
func NewReader(r io.Reader) *Reader {
	return &Reader{r: bufio.NewReaderSize(r, 64*1024)}
}

// Next returns the next event. It returns io.EOF when the stream ends; a
// final event not terminated by a blank line is still dispatched first.
func (r *Reader) Next() (Event, error) {
	var (
		ev      Event
		data    strings.Builder
		hasData bool
		hasAny  bool
	)
	dispatch := func() Event {
		ev.Data = data.String()
		return ev
	}
	for {
		line, err := r.r.ReadString('\n')
		if err != nil && (err != io.EOF || line == "") {
			if err == io.EOF && hasAny {
				return dispatch(), nil
			}
			return Event{}, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if hasAny {
				return dispatch(), nil
			}
			if err == io.EOF {
				return Event{}, io.EOF
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // comment / keep-alive
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "data":
			if hasData {
				data.WriteByte('\n')
			}
			data.WriteString(value)
			hasData, hasAny = true, true
		case "event":
			ev.Event, hasAny = value, true
		case "id":
			ev.ID, hasAny = value, true
		}
		if err == io.EOF {
			if hasAny {
				return dispatch(), nil
			}
			return Event{}, io.EOF
		}
	}
}
