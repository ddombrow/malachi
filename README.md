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
bar. Each item gets an icon (❯ you, 💬 reply, 💭 thinking, 👀 read, 🔍 grep,
🗂️ glob, ✏️ edit, 📝 write, >_ bash, ❌ error); set `"icons": "dots"` in
settings for a Claude Code-style ⏺.
The view follows new output unless you scroll up (mouse wheel, PgUp/PgDn,
Shift+↑/↓, Ctrl+Home/End). Hold Shift (Option in iTerm2/Terminal) while
dragging to select text. Type while the agent works to steer it, press `esc`
to cancel, and use `/help` for the full command list.

| Command | Effect |
| --- | --- |
| `/model` | Show or switch model; bare `/model` lists what the provider serves |
| `/thinking` | Show or set the thinking level |
| `/new`, `/resume`, `/session`, `/last` | Start, resume, or inspect sessions |
| `/compact [note]` | Fold the conversation into a handover the model writes |
| `/trim [bytes]` | Trim old tool output mechanically before the next request |
| `/ctx` | Context budget: prompt tokens, tool output share, compaction count |
| `/trust [yes\|no\|parent]` | Decide whether this directory's `AGENTS.md` is obeyed |
| `/copy`, `/quit` | Copy the transcript, exit |

`/compact` and `/trim` are different tools: `/compact` is a lossy,
model-written summary of the conversation, `/trim` is the mechanical and
lossless removal of tool output that has already served its purpose. Both
happen automatically near the context window, which is also when they matter.

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
  "projectTrust": "ask",
  "providers": {
    "local": { "baseUrl": "http://localhost:8080/v1", "defaultModel": "qwen", "thinkingLevels": ["off"], "contextWindow": 128000 },
    "my-gateway": { "baseUrl": "https://gw.example/v1", "apiKeyEnv": "GW_KEY", "sessionHeader": "x-session-id" }
  }
}
```

`contextWindow` is a provider's context window in tokens, defaulting to
128000. Set it to a real published figure when you have one: `/ctx` and the
status gauge measure against it, and a window that is too large will not warn
you before the provider rejects a request.

Requests carry `User-Agent: malachi/<version>`. A provider's `sessionHeader`
sends the session id (stable across `-c`/resume) on every request; OpenCode Go
requires `x-opencode-session`, which the built-in preset sets.

Project instructions come from `AGENTS.md` (or `CLAUDE.md`) in `~/.malachi`
and in every directory from `/` down to the working directory — but only
where you have agreed to them. See [Project trust](#project-trust). Sessions
are append-only JSONL under `~/.malachi/sessions/<project>/`.

### Project trust

An `AGENTS.md` is a file the *project* supplies, and supplying one is exactly
what you would do if you could get someone to open a repository: its contents
become the agent's instructions. So they are withheld until you decide.

```json
{ "projectTrust": "ask" }
```

`ask` (default) withholds and tells you which files were held back. `always`
and `never` skip the question. Withheld files are named at startup and in
`-p` mode; their contents are never printed.

| Command | Effect |
| --- | --- |
| `/trust` | Explain the current state and the options |
| `/trust yes` | Load them (this session, if you have not answered yet) |
| `/trust yes --save` | Load them, remembered for this directory |
| `/trust parent` | Remember trust for the parent, covering every package beneath it |
| `/trust no` | Decline |

Decisions live in `~/.malachi/trust.json` and are inherited: a decision about
a directory applies to everything beneath it, and the nearest one wins, so one
package can differ without disturbing its siblings. Refusing a parent is not
offered, because that would withhold instructions from unrelated repositories
that merely share an ancestor. `-trust yes`/`-trust no` applies to one run.

Your own `~/.malachi/AGENTS.md` is never gated — those are your instructions,
not project input. Trust governs whether instruction *files* are instructions
or ignored; it is not a filesystem, network, or tool sandbox.

Failures with nowhere else to go — panics, provider errors, and failures to
write the session file — are appended to `~/.malachi/logs/agent.jsonl`, one
JSON object per line, tagged with the session and run. `/session` prints the
path. Provider errors and tool errors also live in the session file itself, as
messages.

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
