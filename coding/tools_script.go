package coding

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
// one; onScript, if set, hears how each script ended.
func NewScriptTool(tools []*agent.Tool, opts ToolOptions, diag *Diagnostics, onScript func(codemode.Details)) *agent.Tool {
	return newScriptTool(tools, opts, diag, onScript, nil, nil)
}

// exploreTools are the tools that, when exploring in scripts, scripts alone
// may call.
var exploreTools = map[string]bool{"read": true, "grep": true, "glob": true}

// exploreGuidelines replace the script tool's own when exploring in scripts.
var exploreGuidelines = []string{
	"read, grep and glob can be called only from scripts. To look at a file or search, write a script; for one file, `return (await tools.read({path: \"go.mod\"})).text` is enough.",
	"When you look at several files or run several searches, do them in one script and return only the parts you need, not whole files or every match.",
	"Do not read or search files with bash (cat, head, sed, grep, rg, find, ls); do it in a script.",
	"A script can branch on what it finds, so a step that depends on a result does not need a turn of its own.",
}

// withScripts is tools as offered to the model with code mode on, the
// script tool last. Each other tool says how a script calls it, and read's
// advice on several files allows for scripts. With explore, read, grep and
// glob are left out and the model reaches them only through scripts, whose
// tool describes them in full; view shows it images, which scripts cannot.
// scriptOnly are reached only through scripts in either case.
func withScripts(tools, scriptOnly []*agent.Tool, opts ToolOptions, diag *Diagnostics, onScript func(codemode.Details), servers *MCPManager, explore bool) []*agent.Tool {
	direct := map[string]bool{}
	var offered, callable []*agent.Tool
	for _, t := range tools {
		c := *t
		c.PromptGuidelines = slices.Clone(t.PromptGuidelines)
		if explore && exploreTools[t.Name] {
			// Parallel reads are advice for direct calls; in a script the
			// calls run together anyway.
			c.PromptGuidelines = slices.DeleteFunc(c.PromptGuidelines, func(g string) bool { return g == readSeveral })
			if t.Name == "read" {
				c.Description += " A script sees text only: to look at an image, call view."
			}
			callable = append(callable, &c)
			continue
		}
		for i, g := range c.PromptGuidelines {
			if g == readSeveral {
				c.PromptGuidelines[i] = readSeveralWithScripts
			}
		}
		direct[t.Name] = true
		callable = append(callable, t)
		offered = append(offered, &c)
	}
	callable = append(callable, scriptOnly...)
	script := newScriptTool(callable, opts, diag, onScript, servers, direct)
	offered = codemode.Advertise(offered)
	if explore {
		script.PromptGuidelines = slices.Clone(exploreGuidelines)
	}
	if slices.ContainsFunc(scriptOnly, func(t *agent.Tool) bool { return t.Name == "git" }) {
		script.PromptGuidelines = append(script.PromptGuidelines, gitGuideline)
	}
	if explore {
		for _, t := range tools {
			if t.Name == "read" {
				offered = append(offered, newViewTool(t))
			}
		}
	}
	return append(offered, script)
}

// newViewTool shows the model an image file through read, for a model whose
// reads go through scripts, which see text only.
func newViewTool(read *agent.Tool) *agent.Tool {
	return &agent.Tool{
		Name:          "view",
		Label:         "View",
		Description:   "Show you an image file (png, jpg, gif or webp) so you can see it. Text files are read in a script with tools.read.",
		PromptSnippet: "Look at an image file",
		Parallel:      true,
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"path": map[string]any{"type": "string", "description": "The image file"}},
			"required":   []string{"path"},
		},
		OutputSchema: readOutputSchema,
		Execute: func(ctx context.Context, id string, args map[string]any, onUpdate func(agent.ToolResult)) (agent.ToolResult, error) {
			res, err := read.Execute(ctx, id, args, onUpdate)
			if err != nil {
				return res, err
			}
			for _, c := range res.Content {
				if _, ok := c.(*agent.ImageContent); ok {
					return res, nil
				}
			}
			if d, ok := res.Details.(map[string]any); ok && imageTypes[fmt.Sprint(d["mime_type"])] {
				return res, nil // an image too large to show says so itself
			}
			return agent.ToolResult{}, errors.New("not an image: read text files in a script, `return (await tools.read({path})).text`")
		},
	}
}

// newScriptTool is NewScriptTool whose scripts also call the deferred MCP
// servers' tools, when servers is set: each script starts with the tools of
// the servers running then, waiting for any still starting.
func newScriptTool(tools []*agent.Tool, opts ToolOptions, diag *Diagnostics, onScript func(codemode.Details), servers *MCPManager, direct map[string]bool) *agent.Tool {
	var extra func(context.Context) []*agent.Tool
	var notFound func(string) string
	if servers != nil {
		extra = func(ctx context.Context) []*agent.Tool {
			servers.await(ctx, true)
			return servers.AgentTools(true)
		}
		notFound = servers.missing
	}
	t := codemode.NewTool(tools, codemode.Options{
		Extra:    extra,
		Direct:   direct,
		NotFound: notFound,
		Engine:   scriptEngine,
		Timeout:  opts.bashTimeout(),
		OnCall: func(call *agent.ToolCall, result agent.ToolResult, isError bool) {
			if source, reason := sandboxDenialOf(result, isError); source != "" {
				diag.LogSandboxDenied(codemode.ToolName+":"+call.Name, source, reason, toolTarget(call.Arguments))
			}
		},
		OnScript: func(d codemode.Details) {
			diag.LogScript(d)
			if onScript != nil {
				onScript(d)
			}
		},
	})
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
	shown, cut := fitText(r.Text(), spillDir, "malachi-script-*.log")
	if !cut {
		return r
	}
	// A failure's message comes last, where the cut would hide it.
	if d, ok := r.Details.(codemode.Details); ok && d.Error != "" {
		shown += "\n\nScript failed: " + TruncateHead(d.Error, 50, 4096).Content
	}
	r.Content = []agent.Content{&agent.TextContent{Text: shown}}
	return r
}

// fitText keeps the start of text within the display limits, saving the
// whole of it to a spill file named by pattern and saying where. It reports
// whether it cut anything.
func fitText(text, spillDir, pattern string) (string, bool) {
	t := TruncateHead(text, MaxOutputLines, MaxOutputBytes)
	if !t.Truncated {
		return text, false
	}
	where := "Full output could not be saved"
	if path, err := writeSpillText(spillDir, pattern, text); err == nil {
		where = "Full output: " + path
	}
	if t.FirstLineTooBig {
		cut := MaxOutputBytes
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		return text[:cut] + fmt.Sprintf("\n\n[Showing the first %s of %s. %s]", FormatSize(cut), FormatSize(len(text)), where), true
	}
	return t.Content + fmt.Sprintf("\n\n[Showing lines 1-%d of %d. %s]", t.OutputLines, t.TotalLines, where), true
}

func writeSpillText(dir, pattern, text string) (string, error) {
	f, err := createSpillNamed(dir, pattern)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		return "", err
	}
	return f.Name(), nil
}
