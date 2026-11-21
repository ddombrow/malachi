package coding

import (
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/ddombrow/malachi/agent"

	"github.com/ddombrow/malachi/codemode"
)

// CodeModeState is what the session's script tool has done: whether it is
// offered, and how many scripts have run in this session value, with how
// many tool calls between them.
type CodeModeState struct {
	Enabled bool
	// Mode is CodeModeOff, CodeModeOn or CodeModeExplore.
	Mode string
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

func (c *codeModeCounter) setMode(mode string) {
	c.mu.Lock()
	c.st.Mode, c.st.Enabled = mode, mode != CodeModeOff
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

// The code modes, in the order CycleCodeMode goes through them.
const (
	// CodeModeOff offers no script tool.
	CodeModeOff = "off"
	// CodeModeOn offers the script tool beside the other tools.
	CodeModeOn = "on"
	// CodeModeExplore offers read, grep and glob only through scripts.
	CodeModeExplore = "explore"
)

var codeModes = []string{CodeModeOff, CodeModeOn, CodeModeExplore}

// CodeModeChangedEvent reports a change of code mode.
type CodeModeChangedEvent struct{ State CodeModeState }

// toolsFor is the session's built-in tools as offered in mode.
func (s *Session) toolsFor(mode string) []*agent.Tool {
	if mode == CodeModeOff {
		return s.coreTools
	}
	return withScripts(s.coreTools, s.toolOpts, s.diag, s.codeMode.record, s.mcp, mode == CodeModeExplore)
}

// SetCodeMode changes the code mode for the rest of this session: the tools
// offered and the prompt's advice follow from the next request. It is
// refused while the agent works, which would otherwise see its tools change
// between steps. Settings still decide the mode a new session starts in.
func (s *Session) SetCodeMode(mode string) error {
	if !slices.Contains(codeModes, mode) {
		return fmt.Errorf("code mode must be one of %s, not %q", strings.Join(codeModes, ", "), mode)
	}
	if err := s.busy(); err != nil {
		return err
	}
	tools := s.toolsFor(mode)
	if s.mcp != nil {
		// Deferred servers' tools go through scripts only while there are
		// scripts; the prompt built below describes them either way.
		s.mcp.bind(s.sandboxPolicy, s.mcpChanged, mode != CodeModeOff)
	}
	s.mu.Lock()
	s.baseTools = tools
	s.system = s.buildPrompt(tools)
	system := s.system
	s.mu.Unlock()
	s.Harness.Update(func(c *agent.HarnessConfig) { c.System = system })
	s.codeMode.setMode(mode)
	s.syncMCPTools()
	s.emit(CodeModeChangedEvent{State: s.CodeMode()})
	return nil
}

// CycleCodeMode moves to the code mode after the current one, from off to
// on to explore and back to off.
func (s *Session) CycleCodeMode() (string, error) {
	cur := s.CodeMode().Mode
	next := codeModes[(slices.Index(codeModes, cur)+1)%len(codeModes)]
	return next, s.SetCodeMode(next)
}
