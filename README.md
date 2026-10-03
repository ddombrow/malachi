# malachi

A small terminal coding agent in Go, modeled on [tau](https://github.com/huggingface/tau)
(itself a Python port of Pi). It speaks Pi-compatible JSON for messages,
events, and sessions, and reads tau session files.

```text
cmd/malachi   CLI: interactive TUI or print mode (-p)
tui/          inline Bubble Tea frontend (scrollback + live area)
coding/       tools (read, write, edit, bash), system prompt, config, sessions
ai/           providers over raw net/http + SSE (openai-compatible today)
agent/        portable brain: messages, events, loop, harness, session tree
```

## Usage

```sh
make build                       # → bin/malachi
export OPENCODE_API_KEY=...      # default provider: OpenCode Go

bin/malachi                      # interactive
bin/malachi -p "summarize main.go"
bin/malachi -c                   # continue the latest session in this directory
bin/malachi --model glm-5.2 --thinking high
bin/malachi -p "..." -mode json  # Pi event stream, one JSON object per line
```

In the TUI, completed output goes to normal terminal scrollback; only the
streaming message, running tools, input, and status bar are redrawn.
Type while the agent works to steer it, press `esc` to cancel, and use `/help`
for commands (`/model`, `/thinking`, `/new`, `/resume`, `/last`).

## Configuration

`~/.malachi/settings.json` (or `$MALACHI_HOME/settings.json`) overrides or adds
providers. Built-in providers are `opencode-go`, `openai`, `openrouter`, and `ollama`.

```json
{
  "defaultProvider": "opencode-go",
  "defaultModel": "kimi-k2.7-code",
  "thinkingLevel": "medium",
  "providers": {
    "local": { "baseUrl": "http://localhost:8080/v1", "defaultModel": "qwen", "thinkingLevels": ["off"] }
  }
}
```

Project instructions come from `AGENTS.md` (or `CLAUDE.md`) in `~/.malachi`
and in every directory from `/` down to the working directory. Sessions are
append-only JSONL under `~/.malachi/sessions/<project>/`.

## Development

`make test lint`. Loop and harness tests use the scripted provider in
`ai/fake`. Golden fixtures in `agent/testdata` and
`agent/session/testdata` were generated from tau's own models.
