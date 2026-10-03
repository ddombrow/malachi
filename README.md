# malachi

A small terminal coding agent in Go, modeled on [tau](https://github.com/huggingface/tau)
(itself a Python port of Pi). It speaks Pi-compatible JSON for messages,
events, and sessions, and reads tau session files.

```text
cmd/malachi   CLI: interactive TUI or print mode (-p)
tui/          full-screen Bubble Tea frontend
coding/       tools (read, write, edit, bash), system prompt, config, sessions
ai/           providers over raw net/http + SSE (openai-compatible today)
agent/        portable brain: messages, events, loop, harness, session tree
```

## Usage

```sh
go tool task install                      # build, install to ~/.local/bin as `malachi`
go tool task uninstall                    # remove it again
go tool task install PREFIX=/usr/local    # elsewhere (may need sudo)
echo 'OPENCODE_API_KEY=...' >> ~/.malachi/.env   # default provider: OpenCode Go

bin/malachi                      # interactive
bin/malachi -p "summarize main.go"
bin/malachi -c                   # continue the latest session in this directory
bin/malachi --model glm-5.2 --thinking high
bin/malachi -p "..." -mode json  # Pi event stream, one JSON object per line
bin/malachi -list-models         # models the provider serves right now
```

The TUI is full-screen: a scrollable transcript above a pinned input and status
bar. Each item gets an icon (💬 reply, 💭 thinking, 📖 read, 📝 edit, 📄 write,
💻 bash, ❌ error); set `"icons": "dots"` in settings for a Claude Code-style ⏺.
The view follows new output unless you scroll up (mouse wheel, PgUp/PgDn,
Shift+↑/↓, Ctrl+Home/End). Hold Shift (Option in iTerm2/Terminal) while
dragging to select text. Type while the agent works to steer it, press `esc`
to cancel, and use `/help` for commands (`/model`, `/thinking`, `/new`,
`/resume`, `/last`). `/model` with no argument fetches the provider's live
model list.

## Configuration

API keys can be exported in your shell or kept in `~/.malachi/.env`
(`KEY=value` lines; a non-empty shell variable takes precedence). Only that
file is read, never a `.env` in the project directory, so a repo's own secrets
don't leak into the agent's shell commands.

`~/.malachi/settings.json` (or `$MALACHI_HOME/settings.json`) overrides or adds
providers. Built-in providers are `opencode-go`, `openai`, `openrouter`, and `ollama`.

```json
{
  "defaultProvider": "opencode-go",
  "defaultModel": "kimi-k2.7-code",
  "thinkingLevel": "medium",
  "icons": "emoji",
  "providers": {
    "local": { "baseUrl": "http://localhost:8080/v1", "defaultModel": "qwen", "thinkingLevels": ["off"] },
    "my-gateway": { "baseUrl": "https://gw.example/v1", "apiKeyEnv": "GW_KEY", "sessionHeader": "x-session-id" }
  }
}
```

Requests carry `User-Agent: malachi/<version>`. A provider's `sessionHeader`
sends the session id (stable across `-c`/resume) on every request; OpenCode Go
requires `x-opencode-session`, which the built-in preset sets.

Project instructions come from `AGENTS.md` (or `CLAUDE.md`) in `~/.malachi`
and in every directory from `/` down to the working directory. Sessions are
append-only JSONL under `~/.malachi/sessions/<project>/`.

## Development

Tasks live in `Taskfile.yml` and run through the pinned tool, so no global
install is needed: `go tool task` lists them; `go tool task build`, `lint`,
and `test` are the usual loop. `go tool task install` puts the binary in
`~/.local/bin` (per-user XDG; override with `PREFIX`, e.g. `PREFIX=/opt/homebrew`
for a Homebrew prefix), and `uninstall` removes it, leaving the directory alone
if anything else is in it. Builds are stamped with `git describe`
(`bin/malachi -version`). Loop and harness tests use the scripted provider in
`ai/fake`. Golden fixtures in `agent/testdata` and
`agent/session/testdata` were generated from tau's own models.
