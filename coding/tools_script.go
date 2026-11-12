package coding

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/codemode"
)

// scriptEngine compiles the script interpreter once per process, on the
// first script. The compiled form is cached under the user's cache
// directory, so later processes skip compiling.
var scriptEngine = sync.OnceValues(func() (*codemode.Engine, error) {
	ctx := context.Background()
	if dir, err := os.UserCacheDir(); err == nil {
		if e, err := codemode.NewEngine(ctx, filepath.Join(dir, "malachi", "codemode")); err == nil {
			return e, nil
		}
	}
	// An unusable cache costs a compile per process, nothing worse.
	return codemode.NewEngine(ctx, "")
})

// NewScriptTool returns the script tool over tools: each call a script makes
// runs one of them as the model's own call would, under the same sandbox.
// Its output is cut to the limits read and bash keep, with the rest in a
// spill file. diag, if set, logs each script and any sandbox refusal inside
// one.
func NewScriptTool(tools []*agent.Tool, opts ToolOptions, diag *Diagnostics) *agent.Tool {
	t := codemode.NewTool(tools, codemode.Options{
		Engine:  scriptEngine,
		Timeout: opts.bashTimeout(),
		OnCall: func(call *agent.ToolCall, result agent.ToolResult, isError bool) {
			if source, reason := sandboxDenialOf(result, isError); source != "" {
				diag.LogSandboxDenied(codemode.ToolName+":"+call.Name, source, reason, toolTarget(call.Arguments))
			}
		},
		OnScript: diag.LogScript,
	})
	t.Description += " " + scriptResults
	inner := t.Execute
	t.Execute = func(ctx context.Context, callID string, args map[string]any, onUpdate func(agent.ToolResult)) (agent.ToolResult, error) {
		res, err := inner(ctx, callID, args, onUpdate)
		var failed *agent.ResultError
		switch {
		case err == nil:
			return fitScriptOutput(res, opts.SpillDir), nil
		case errors.As(err, &failed):
			return agent.ToolResult{}, agent.FailWith(fitScriptOutput(failed.Result, opts.SpillDir))
		}
		return res, err
	}
	return t
}

// scriptResults tells the model what data search tools give scripts, so a
// script filters values instead of parsing the text the model would see.
const scriptResults = "In scripts, search tools also return data in details, with paths relative to the working " +
	"directory so they can be passed straight to read or edit: grep's details.results is a list of " +
	"{path, line, text} per match (output \"lines\"), {path} per file (\"files\") or {path, count} (\"count\"); " +
	"glob's details.paths is a list of paths. Prefer these to parsing text."

// scriptPath is a search result's path as a script should get it: relative
// to the working directory when it is inside it, so the script can hand it
// to read or edit as is, else absolute.
func scriptPath(cwd, root, rel string) string {
	abs := filepath.Join(root, rel)
	if r, err := filepath.Rel(cwd, abs); err == nil && r != ".." && !strings.HasPrefix(r, "../") {
		return r
	}
	return abs
}

func nonNil(v []map[string]any) []map[string]any {
	if v == nil {
		return []map[string]any{}
	}
	return v
}

// fitScriptOutput keeps the start of a script's output within the display
// limits. A script prints in order, so the start is what it meant first;
// the whole text goes to a spill file the model can read.
func fitScriptOutput(r agent.ToolResult, spillDir string) agent.ToolResult {
	text := r.Text()
	t := TruncateHead(text, MaxOutputLines, MaxOutputBytes)
	if !t.Truncated {
		return r
	}
	where := "Full output could not be saved"
	if path, err := writeScriptSpill(spillDir, text); err == nil {
		where = "Full output: " + path
	}
	var shown string
	if t.FirstLineTooBig {
		cut := MaxOutputBytes
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		shown = text[:cut] + fmt.Sprintf("\n\n[Showing the first %s of %s. %s]", FormatSize(cut), FormatSize(len(text)), where)
	} else {
		shown = t.Content + fmt.Sprintf("\n\n[Showing lines 1-%d of %d. %s]", t.OutputLines, t.TotalLines, where)
	}
	// A failure's message comes last, where the cut would hide it.
	if d, ok := r.Details.(codemode.Details); ok && d.Error != "" {
		shown += "\n\nScript failed: " + TruncateHead(d.Error, 50, 4096).Content
	}
	r.Content = []agent.Content{&agent.TextContent{Text: shown}}
	return r
}

func writeScriptSpill(dir, text string) (string, error) {
	f, err := createSpillNamed(dir, "malachi-script-*.log")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		return "", err
	}
	return f.Name(), nil
}
