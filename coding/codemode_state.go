package coding

import (
	"sync"

	"github.com/ddombrow/malachi/codemode"
)

// CodeModeState is what the session's script tool has done: whether it is
// offered, and how many scripts have run in this session value, with how
// many tool calls between them.
type CodeModeState struct {
	Enabled bool
	// Scripts counts scripts run; Failed, those that ended in an error.
	Scripts, Failed int
	// Calls counts the tool calls scripts made; CallErrors, those that
	// failed (a script can carry on past them).
	Calls, CallErrors int
}

type codeModeCounter struct {
	mu sync.Mutex
	st CodeModeState
}

func (c *codeModeCounter) enable() {
	c.mu.Lock()
	c.st.Enabled = true
	c.mu.Unlock()
}

func (c *codeModeCounter) record(d codemode.Details) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.st.Scripts++
	if d.Error != "" {
		c.st.Failed++
	}
	c.st.Calls += len(d.Calls)
	for _, call := range d.Calls {
		if call.Status == codemode.StatusError {
			c.st.CallErrors++
		}
	}
}

// CodeMode reports the script tool's state.
func (s *Session) CodeMode() CodeModeState {
	s.codeMode.mu.Lock()
	defer s.codeMode.mu.Unlock()
	return s.codeMode.st
}
