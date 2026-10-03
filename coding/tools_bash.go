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

// lockedBuffer is a bytes.Buffer safe for concurrent Write and snapshotting.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func shellPath() string {
	if p, err := exec.LookPath("bash"); err == nil {
		return p
	}
	return "/bin/sh"
}

// NewBashTool returns the bash tool running commands in cwd.
func NewBashTool(cwd string) *agent.Tool {
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
				"timeout": map[string]any{"type": "number", "description": "Timeout in seconds (optional, no default timeout)"},
			},
			"required": []string{"command", "description"},
		},
		Execute: func(ctx context.Context, _ string, args map[string]any, onUpdate func(agent.ToolResult)) (agent.ToolResult, error) {
			return executeBash(ctx, cwd, args, onUpdate)
		},
	}
}

func executeBash(ctx context.Context, cwd string, args map[string]any, onUpdate func(agent.ToolResult)) (agent.ToolResult, error) {
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

	runCtx := ctx
	if hasTimeout {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, time.Duration(timeout*float64(time.Second)))
		defer cancel()
	}

	var out lockedBuffer
	cmd := exec.CommandContext(runCtx, shellPath(), "-c", command)
	cmd.Dir = cwd
	cmd.Stdin = nil // /dev/null: keep interactive programs off our terminal
	cmd.Stdout = &out
	cmd.Stderr = &out
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
	lastLen := 0
wait:
	for {
		select {
		case waitErr = <-done:
			break wait
		case <-ticker.C:
			if s := out.String(); len(s) != lastLen {
				lastLen = len(s)
				onUpdate(agent.TextResult(TruncateTail(s, MaxOutputLines, MaxOutputBytes).Content))
			}
		}
	}

	output := out.String()
	exitCode := -1
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	timedOut := hasTimeout && errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
	cancelled := ctx.Err() != nil

	t := TruncateTail(output, MaxOutputLines, MaxOutputBytes)
	text := t.Content
	if text == "" {
		text = "(no output)"
	}
	fullPath := ""
	if t.Truncated {
		fullPath = writeTempOutput(output)
		startLine, endLine := t.TotalLines-t.OutputLines+1, t.TotalLines
		switch {
		case t.LastLinePartial:
			text += fmt.Sprintf("\n\n[Showing last %s of line %d. Full output: %s]", FormatSize(t.OutputBytes), endLine, fullPath)
		case t.TruncatedBy == "lines":
			text += fmt.Sprintf("\n\n[Showing lines %d-%d of %d. Full output: %s]", startLine, endLine, t.TotalLines, fullPath)
		default:
			text += fmt.Sprintf("\n\n[Showing lines %d-%d of %d (%s limit). Full output: %s]", startLine, endLine, t.TotalLines, FormatSize(MaxOutputBytes), fullPath)
		}
	}

	status := ""
	switch {
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
		},
	}, nil
}

func writeTempOutput(output string) string {
	f, err := os.CreateTemp("", "malachi-bash-*.log")
	if err != nil {
		return "(unavailable)"
	}
	defer f.Close()
	_, _ = f.WriteString(output)
	return f.Name()
}
