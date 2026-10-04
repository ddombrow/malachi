package coding

import (
	"fmt"
	"html"
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
func LoadContextFiles(home, cwd string) []ContextFile {
	var out []ContextFile
	seen := map[string]bool{}
	add := func(dir string) {
		for _, name := range contextFileNames {
			p := filepath.Join(dir, name)
			if seen[p] {
				return
			}
			data, err := os.ReadFile(p)
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

// PromptOptions are the inputs to BuildSystemPrompt.
type PromptOptions struct {
	Cwd          string
	Tools        []*agent.Tool
	ContextFiles []ContextFile
	Append       string // appended verbatim after the default prompt
	Override     string // replaces the default prompt entirely
	Date         time.Time
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
			add("Use bash for file operations like ls, rg, find")
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
