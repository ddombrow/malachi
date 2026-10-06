package coding

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/ddombrow/malachi/agent"
)

// bashUpdateInterval is how often running output is pushed as a partial result.
const bashUpdateInterval = 500 * time.Millisecond

const (
	// DefaultBashTimeout bounds a command the model gave no timeout for, so a
	// command that never exits cannot hold the session forever.
	DefaultBashTimeout = 10 * time.Minute
	// MaxSpillBytes caps the full-output file a truncated command leaves
	// behind; output past it is counted, not kept.
	MaxSpillBytes = 64 << 20
	// tailBytesKept is how much of the end of the output stays in memory for
	// display: twice the display limit, so the shown tail never starts at a
	// line the ring cut in half.
	tailBytesKept = 2 * MaxOutputBytes
)

// ToolOptions configure the coding tools. The zero value means defaults.
type ToolOptions struct {
	// BashTimeout applies when the model gives no timeout. Zero means
	// DefaultBashTimeout; negative means none (an explicit choice).
	BashTimeout time.Duration
	// SpillDir receives the full output of truncated commands. It is created
	// on first use; empty means the system temp directory.
	SpillDir string
}

func (o ToolOptions) bashTimeout() time.Duration {
	if o.BashTimeout == 0 {
		return DefaultBashTimeout
	}
	return o.BashTimeout
}

// outputSink collects a command's output with bounded memory. While the
// output fits the display limits it is kept whole; past them, a spill file
// receives everything (up to MaxSpillBytes) and memory keeps only the last
// tailBytesKept bytes plus counts.
type outputSink struct {
	dir       string // where the spill file goes; "" for the temp dir
	mu        sync.Mutex
	pending   []byte // all output, until the spill file exists
	tail      []byte // last tailBytesKept bytes, once spilling
	total     int64  // bytes written
	newlines  int64
	lastByte  byte
	spill     *os.File
	spillPath string
	spilled   int64 // bytes in the spill file
	dropped   int64 // bytes past MaxSpillBytes, counted only
	spillErr  error
}

func (o *outputSink) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.total += int64(len(p))
	o.newlines += int64(bytes.Count(p, []byte{'\n'}))
	if len(p) > 0 {
		o.lastByte = p[len(p)-1]
	}
	if o.spill == nil && o.spillErr == nil {
		o.pending = append(o.pending, p...)
		if len(o.pending) <= MaxOutputBytes && o.lines() <= MaxOutputLines {
			return len(p), nil
		}
		// Past the display limits: the full output now goes to a file.
		f, err := createSpill(o.dir)
		if err != nil {
			o.spillErr = err
		} else {
			o.spill, o.spillPath = f, f.Name()
			o.writeSpill(o.pending)
		}
		o.keepTail(o.pending)
		o.pending = nil
		return len(p), nil
	}
	o.writeSpill(p)
	o.keepTail(p)
	return len(p), nil
}

func createSpill(dir string) (*os.File, error) {
	if dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	return os.CreateTemp(dir, "malachi-bash-*.log")
}

func (o *outputSink) writeSpill(p []byte) {
	if o.spill == nil {
		return
	}
	room := MaxSpillBytes - o.spilled
	if int64(len(p)) > room {
		o.dropped += int64(len(p)) - max(room, 0)
		p = p[:max(room, 0)]
	}
	n, _ := o.spill.Write(p)
	o.spilled += int64(n)
}

func (o *outputSink) keepTail(p []byte) {
	o.tail = append(o.tail, p...)
	if extra := len(o.tail) - tailBytesKept; extra > 0 {
		// Reuse the front of the slice rather than growing forever.
		o.tail = append(o.tail[:0], o.tail[extra:]...)
	}
}

// lines counts lines the way splitLinesForCounting does: a final newline
// does not start another line.
func (o *outputSink) lines() int64 {
	n := o.newlines
	if o.total > 0 && o.lastByte != '\n' {
		n++
	}
	return n
}

// snapshot returns the output to display: all of it while it fits, else the
// kept tail.
func (o *outputSink) snapshot() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.spill == nil && o.spillErr == nil {
		return string(o.pending)
	}
	return string(o.tail)
}

func (o *outputSink) close() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.spill != nil {
		_ = o.spill.Close()
	}
}

func shellPath() string {
	if p, err := exec.LookPath("bash"); err == nil {
		return p
	}
	return "/bin/sh"
}

// NewBashTool returns the bash tool running commands in cwd.
func NewBashTool(cwd string, opts ToolOptions) *agent.Tool {
	return &agent.Tool{
		Name:  "bash",
		Label: "Bash",
		Description: fmt.Sprintf("Execute a bash command in the current working directory. Returns stdout and stderr. "+
			"Output is truncated to last %d lines or %dKB (whichever is hit first). If truncated, "+
			"full output is saved to a temp file. Optionally provide a timeout in seconds.",
			MaxOutputLines, MaxOutputBytes/1024),
		PromptSnippet: "Execute bash commands (ls, grep, find, etc.)",
		PromptGuidelines: []string{
			"When using bash, include a brief present-participle description of the command's purpose (for example, 'Running tests').",
		},
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{"type": "string", "description": "Bash command to execute"},
				"description": map[string]any{
					"type":        "string",
					"description": "Brief present-participle summary of the command's purpose, such as 'Running tests' or 'Validating and committing changes'",
				},
				"timeout": map[string]any{"type": "number", "description": "Timeout in seconds (optional; long-running commands are stopped after a default limit otherwise)"},
			},
			"required": []string{"command", "description"},
		},
		Execute: func(ctx context.Context, _ string, args map[string]any, onUpdate func(agent.ToolResult)) (agent.ToolResult, error) {
			return executeBash(ctx, cwd, opts, args, onUpdate)
		},
	}
}

func executeBash(ctx context.Context, cwd string, opts ToolOptions, args map[string]any, onUpdate func(agent.ToolResult)) (agent.ToolResult, error) {
	command, err := strArg(args, "command")
	if err != nil {
		return agent.ToolResult{}, err
	}
	timeout, hasTimeout, err := optNumber(args, "timeout")
	if err != nil {
		return agent.ToolResult{}, err
	}
	if hasTimeout && timeout <= 0 {
		return agent.ToolResult{}, fmt.Errorf("timeout must be greater than 0")
	}
	if ctx.Err() != nil {
		return agent.ToolResult{}, fmt.Errorf("Command cancelled")
	}

	limit, defaulted := time.Duration(0), false
	switch {
	case hasTimeout:
		limit = time.Duration(timeout * float64(time.Second))
	case opts.bashTimeout() > 0:
		limit, defaulted = opts.bashTimeout(), true
	}
	runCtx := ctx
	if limit > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, limit)
		defer cancel()
	}

	out := &outputSink{dir: opts.SpillDir}
	defer out.close()
	cmd := exec.CommandContext(runCtx, shellPath(), "-c", command)
	cmd.Dir = cwd
	cmd.Stdin = nil // /dev/null: keep interactive programs off our terminal
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.Env = append(os.Environ(), "PAGER=cat", "GIT_PAGER=cat", "TERM=dumb")
	configureProcessGroup(cmd)
	cmd.WaitDelay = 2 * time.Second

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return agent.ToolResult{}, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	// Push live output from this goroutine so updates never race the loop.
	ticker := time.NewTicker(bashUpdateInterval)
	defer ticker.Stop()
	var waitErr error
	var lastTotal int64
wait:
	for {
		select {
		case waitErr = <-done:
			break wait
		case <-ticker.C:
			out.mu.Lock()
			total := out.total
			out.mu.Unlock()
			if total != lastTotal {
				lastTotal = total
				onUpdate(agent.TextResult(TruncateTail(out.snapshot(), MaxOutputLines, MaxOutputBytes).Content))
			}
		}
	}
	out.close()

	exitCode := -1
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	timedOut := limit > 0 && errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
	cancelled := ctx.Err() != nil

	out.mu.Lock()
	spilling := out.spill != nil || out.spillErr != nil
	totalBytes, totalLines := out.total, out.lines()
	spillPath, spilled, dropped := out.spillPath, out.spilled, out.dropped
	out.mu.Unlock()

	t := TruncateTail(out.snapshot(), MaxOutputLines, MaxOutputBytes)
	if spilling {
		// The display came from the kept tail; the totals are the real ones.
		t.Truncated = true
		t.TotalLines, t.TotalBytes = int(totalLines), int(totalBytes)
		if t.TruncatedBy == "" {
			t.TruncatedBy = "bytes"
		}
	}
	text := t.Content
	if text == "" {
		text = "(no output)"
	}
	fullPath := ""
	if t.Truncated {
		fullPath = spillPath
		where := "Full output: " + spillPath
		switch {
		case spillPath == "":
			where = "Full output could not be saved"
		case dropped > 0:
			where = fmt.Sprintf("First %s of %s saved to: %s", FormatSize(int(spilled)), FormatSize(int(totalBytes)), spillPath)
		}
		startLine, endLine := t.TotalLines-t.OutputLines+1, t.TotalLines
		switch {
		case t.LastLinePartial:
			text += fmt.Sprintf("\n\n[Showing last %s of line %d. %s]", FormatSize(t.OutputBytes), endLine, where)
		case t.TruncatedBy == "lines":
			text += fmt.Sprintf("\n\n[Showing lines %d-%d of %d. %s]", startLine, endLine, t.TotalLines, where)
		default:
			text += fmt.Sprintf("\n\n[Showing lines %d-%d of %d (%s limit). %s]", startLine, endLine, t.TotalLines, FormatSize(MaxOutputBytes), where)
		}
	}

	status := ""
	switch {
	case timedOut && defaulted:
		status = fmt.Sprintf("Command timed out after %g seconds (the default limit; pass timeout for longer)", limit.Seconds())
	case timedOut:
		status = fmt.Sprintf("Command timed out after %g seconds", timeout)
	case cancelled:
		status = "Command cancelled"
	case exitCode != 0:
		status = fmt.Sprintf("Command exited with code %d", exitCode)
		if exitCode == -1 && waitErr != nil {
			status = "Command failed: " + waitErr.Error()
		}
	}
	if status != "" {
		text = strings.TrimRight(text, "\n") + "\n\n" + status
	}

	return agent.ToolResult{
		Content: []agent.Content{&agent.TextContent{Text: text}},
		Details: map[string]any{
			"command":          command,
			"exit_code":        exitCode,
			"timed_out":        timedOut,
			"cancelled":        cancelled,
			"duration_seconds": time.Since(start).Round(time.Millisecond).Seconds(),
			"truncation":       t,
			"full_output_path": fullPath,
			"output_bytes":     totalBytes,
			"dropped_bytes":    dropped,
		},
	}, nil
}
