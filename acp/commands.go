package acp

import (
	"fmt"
	"strings"
)

// commands are the slash commands malachi handles itself, never sending
// them to the model. They show the sandbox, or are the user widening it
// for one session, as the same commands do in the TUI: the model cannot type into
// the editor's prompt box, so nothing it does reaches them.
var commands = []command{
	{Name: "sandbox", Description: "Show what malachi's sandbox lets commands and file tools touch in this session"},
	{Name: "allow-read", Description: "Allow reading a directory in your home for this session", Input: &commandInput{Hint: "directory"}},
	{Name: "allow-write", Description: "Allow writes to a directory for this session", Input: &commandInput{Hint: "directory"}},
	{Name: "codemode", Description: "Show or set the code mode: off, on, or explore (read, grep and glob only through scripts)", Input: &commandInput{Hint: "off, on or explore"}},
}

// advertise tells the client which commands the session has.
func (as *acpSession) advertise() {
	as.update(availableCommandsUpdate{SessionUpdate: "available_commands_update", AvailableCommands: commands})
}

// slashCommand reports whether a prompt is one of commands: its first block
// is text naming one after a slash. Anything else, including a prompt that
// starts with an absolute path, goes to the model.
func slashCommand(prompt []contentBlock) (name, arg string, ok bool) {
	if len(prompt) == 0 || prompt[0].Type != "text" {
		return "", "", false
	}
	rest, found := strings.CutPrefix(strings.TrimSpace(prompt[0].Text), "/")
	if !found {
		return "", "", false
	}
	name, arg, _ = strings.Cut(rest, " ")
	for _, c := range commands {
		if c.Name == name {
			return name, strings.TrimSpace(arg), true
		}
	}
	return "", "", false
}

// command runs a slash command and reports its outcome as the turn's reply.
func (as *acpSession) command(name, arg string) {
	var msg string
	switch name {
	case "sandbox":
		// The TUI's own description, fenced so its indentation survives
		// the client's markdown.
		msg = "```text\n" + strings.TrimRight(as.s.Sandbox().Describe(), "\n") + "\n```"
	case "allow-read":
		root, err := as.s.AddReadableRoot(arg)
		if err != nil {
			msg = "Not added: " + err.Error()
			if arg == "" {
				msg = "Usage: /allow-read <directory>"
			}
			break
		}
		msg = fmt.Sprintf("Added readable root %s for this session. Whatever model-run tools read there is sent to your model provider.", root)
	case "allow-write":
		root, err := as.s.AddWritableRoot(arg)
		if err != nil {
			msg = "Not added: " + err.Error()
			if arg == "" {
				msg = "Usage: /allow-write <directory>"
			}
			break
		}
		msg = fmt.Sprintf("Added writable root %s for this session. Model-run tools can modify or delete its contents.", root)
	case "codemode":
		if arg == "" {
			msg = "Code mode is " + as.s.CodeMode().Mode + ". Set it with /codemode off, /codemode on or /codemode explore, or with the Code mode option."
			break
		}
		if err := as.s.SetCodeMode(strings.ToLower(arg)); err != nil {
			msg = "Not changed: " + err.Error()
			break
		}
		msg = "Code mode is now " + as.s.CodeMode().Mode + ", from the next prompt."
	}
	as.s.Flush() // the change's config_option_update comes first
	as.update(chunkUpdate{SessionUpdate: "agent_message_chunk", Content: textBlock(msg)})
}
