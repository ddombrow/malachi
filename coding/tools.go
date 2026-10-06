// Package coding turns the portable agent into a coding agent: local file
// and shell tools, system prompt assembly, configuration, and on-disk
// sessions. It is the Go counterpart of tau_coding.
package coding

import "github.com/ddombrow/malachi/agent"

// CodingTools returns the built-in read, grep, glob, bash, edit, and write
// tools rooted at cwd. grep and glob come before bash so the system prompt
// steers the model toward structured search over shelling out.
func CodingTools(cwd string, opts ToolOptions) []*agent.Tool {
	return []*agent.Tool{
		NewReadTool(cwd, opts),
		NewGrepTool(cwd, opts),
		NewGlobTool(cwd, opts),
		NewBashTool(cwd, opts),
		NewEditTool(cwd, opts),
		NewWriteTool(cwd, opts),
	}
}
