// Package coding turns the portable agent into a coding agent: local file
// and shell tools, system prompt assembly, configuration, and on-disk
// sessions. It is the Go counterpart of tau_coding.
package coding

import "github.com/ddombrow/malachi/agent"

// CodingTools returns the built-in read, bash, edit, and write tools rooted at cwd.
func CodingTools(cwd string) []*agent.Tool {
	return []*agent.Tool{NewReadTool(cwd), NewBashTool(cwd), NewEditTool(cwd), NewWriteTool(cwd)}
}
