package coding

import (
	"fmt"
	"html"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ddombrow/malachi/agent"
)

// ContextFile is a project instruction file folded into the system prompt.
type ContextFile struct {
	Path    string
	Content string
}

// contextFileNames are tried in order in each directory; the first match wins.
var contextFileNames = []string{"AGENTS.md", "CLAUDE.md"}

// LoadContextFiles returns home/AGENTS.md followed by one instruction file
// per directory from the filesystem root down to cwd.
//
// When trusted is false, the files below the user's home directory are left
// out: a project supplying an instruction file is a project asking for its
// instructions to be obeyed, and that needs a decision first. See trust.go.
func LoadContextFiles(home, cwd string, trusted bool) []ContextFile {
	var out []ContextFile
	seen := map[string]bool{}
	add := func(dir string) {
		for _, name := range contextFileNames {
			p := filepath.Join(dir, name)
			if seen[p] {
				return
			}
			data, err := readInstructionFile(p)
			if err == nil {
				seen[p] = true
				out = append(out, ContextFile{Path: p, Content: strings.TrimSpace(string(data))})
				return
			}
		}
	}
	if home != "" {
		add(home)
	}
	if !trusted {
		return out
	}
	var dirs []string
	for d := filepath.Clean(cwd); ; d = filepath.Dir(d) {
		dirs = append(dirs, d)
		if filepath.Dir(d) == d {
			break
		}
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		add(dirs[i])
	}
	return out
}

// readInstructionFile accepts only regular, non-symlink files. Comparing the
// opened file to the lstat result also rejects a symlink swap between checking
// the path and opening it.
func readInstructionFile(path string) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("instruction file is not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(before, after) {
		return nil, fmt.Errorf("instruction file changed while opening")
	}
	return io.ReadAll(f)
}

// PromptOptions are the inputs to BuildSystemPrompt.
type PromptOptions struct {
	Cwd          string
	Tools        []*agent.Tool
	ContextFiles []ContextFile
	Append       string // appended verbatim after the default prompt
	Override     string // replaces the default prompt entirely
	Date         time.Time
	// MCPServers get a line each, without their tools' schemas.
	MCPServers []PromptServer
}

// BuildSystemPrompt assembles a Pi-style coding agent system prompt.
// Port of tau_coding/system_prompt.py.
func BuildSystemPrompt(o PromptOptions) string {
	if o.Date.IsZero() {
		o.Date = time.Now()
	}
	var b strings.Builder
	if o.Override != "" {
		b.WriteString(o.Override)
	} else {
		b.WriteString("You are an expert coding assistant operating inside malachi, a coding agent harness. " +
			"You help users by reading files, executing commands, editing code, and writing new files.")
		b.WriteString("\n\nAvailable tools:\n")
		b.WriteString(formatTools(o.Tools))
		b.WriteString("\n\nIn addition to the tools above, you may have access to other custom tools depending on the project.")
		if len(o.MCPServers) > 0 {
			b.WriteString("\n\n")
			b.WriteString(formatMCPServers(o.MCPServers))
		}
		b.WriteString("\n\nGuidelines:\n")
		for _, g := range guidelines(o.Tools) {
			b.WriteString("- ")
			b.WriteString(g)
			b.WriteByte('\n')
		}
		s := strings.TrimSuffix(b.String(), "\n")
		b.Reset()
		b.WriteString(s)
	}
	if o.Append != "" {
		b.WriteString("\n\n")
		b.WriteString(o.Append)
	}
	if len(o.ContextFiles) > 0 {
		b.WriteString("\n\n<project_context>\n\nProject-specific instructions and guidelines:\n\n")
		for i, f := range o.ContextFiles {
			fmt.Fprintf(&b, "<project_instructions path=\"%s\">\n%s\n</project_instructions>", html.EscapeString(f.Path), f.Content)
			if i < len(o.ContextFiles)-1 {
				b.WriteString("\n")
			}
		}
		b.WriteString("\n</project_context>")
	}
	fmt.Fprintf(&b, "\nCurrent date: %s", o.Date.Format("2006-01-02"))
	fmt.Fprintf(&b, "\nCurrent working directory: %s", o.Cwd)
	return b.String()
}

func formatTools(tools []*agent.Tool) string {
	var lines []string
	for _, t := range tools {
		if t.PromptSnippet != "" {
			lines = append(lines, fmt.Sprintf("- %s: %s", t.Name, t.PromptSnippet))
		}
	}
	if len(lines) == 0 {
		return "(none)"
	}
	return strings.Join(lines, "\n")
}

// formatMCPServers describes the user's MCP servers in a line each. A
// deferred server's tools are not offered to the model; it finds and calls
// them from scripts, so their schemas cost nothing until used.
func formatMCPServers(servers []PromptServer) string {
	var b strings.Builder
	b.WriteString("MCP servers the user configured:")
	deferred := false
	for _, srv := range servers {
		fmt.Fprintf(&b, "\n- %s", srv.Name)
		if srv.Deferred {
			deferred = true
			b.WriteString(" (script)")
		}
		if d := strings.Join(strings.Fields(srv.Description), " "); d != "" {
			b.WriteString(": " + d)
		}
	}
	if deferred {
		b.WriteString("\nThe tools of servers marked (script) are not offered as tools: call them from script as " +
			"tools.mcp__<server>__<tool>(args). searchTools(\"mcp__<server>\") lists a server's tools and " +
			"describeTool(name) gives one's parameters.")
	}
	return b.String()
}

func guidelines(tools []*agent.Tool) []string {
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, t := range tools {
		if t.Name == "bash" {
			// Not for finding things. grep and glob exist for that, and telling
			// the model to use ls and rg while three lines later it is told not
			// to is a contradiction it resolves arbitrarily.
			add("Use bash for builds, tests, version control, and other shell work.")
		}
	}
	for _, t := range tools {
		for _, g := range t.PromptGuidelines {
			add(g)
		}
	}
	for _, g := range []string{
		"Inspect relevant files and project instructions before editing",
		"Make focused changes that preserve the project's architecture and style",
		"Do not overwrite or discard unrelated user changes",
		"Use the project's documented commands and package manager",
		"Run relevant tests, formatting, linting, and type checks after changes",
		"Report checks honestly; never claim a command passed unless you ran it",
		"Ask before destructive operations or materially ambiguous design choices",
		"Be concise in your responses",
		"Show file paths clearly when working with files",
	} {
		add(g)
	}
	return out
}
