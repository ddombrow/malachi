# malachi

A small terminal coding agent in Go, modeled on [tau](https://github.com/huggingface/tau)
(itself a Python port of Pi). It speaks Pi-compatible JSON for messages,
events, and sessions, and reads tau session files.

> [!WARNING]
> **malachi does not ask permission before it acts.** When the model decides
> to run a shell command, write a file, or edit one, it happens immediately,
> with your user account's privileges: there is no approval prompt and no
> undo. It can delete files, change anything your account can change, and
> reach the network. Use it on work you can recover (a version-controlled
> checkout, committed first), and run it in a container, VM, or disposable
> environment for anything you do not trust — including repositories you
> did not write, whose files the model will read. [Project
> trust](#project-trust) only decides whether a repository's `AGENTS.md` is
> obeyed; it is not a sandbox. The same applies in `-p` and RPC mode, where
> nobody is watching at all.

```text
cmd/malachi   CLI: interactive TUI or print mode (-p)
tui/          full-screen Bubble Tea frontend
coding/       tools (read, write, edit, bash), system prompt, config, sessions
ai/           providers over raw net/http + SSE (openai-compatible today)
agent/        portable brain: messages, events, loop, harness, session tree
```

## Install

With Go 1.27 or newer:

```sh
go install github.com/ddombrow/malachi/cmd/malachi@latest
```

`malachi -version` reports the release it was installed from. To build from a
checkout instead, see [Development](#development).

macOS and Linux are supported. malachi builds on Windows, but its bash tool
needs `bash` or `sh` on the PATH, and cancelling a command there stops only
the shell, not the processes it started.

## Usage

```sh
go tool task install                      # from a checkout: build, install to ~/.local/bin as `malachi`
go tool task uninstall                    # remove it again
go tool task install PREFIX=/usr/local    # elsewhere (may need sudo)
echo 'OPENCODE_API_KEY=...' >> ~/.malachi/.env   # default provider: OpenCode Go

bin/malachi                      # interactive
bin/malachi -p "summarize main.go"
bin/malachi -c                   # continue the latest session in this directory
bin/malachi --model glm-5.2 --thinking high
bin/malachi -p "..." -mode json  # Pi event stream, one JSON object per line
bin/malachi -list-models         # models the provider serves right now
bin/malachi -mode rpc            # headless: JSON commands in, events out (see RPC mode)
```

The TUI is full-screen: a scrollable transcript above a pinned input and status
bar. Each item gets an icon (❯ you, 💬 reply, 💭 thinking, 👀 read, 🔍 grep,
🗂️ glob, ✏️ edit, 📝 write, >_ bash, ❌ error); set `"icons": "dots"` in
settings for a Claude Code-style ⏺.
The view follows new output unless you scroll up (mouse wheel, PgUp/PgDn,
Shift+↑/↓, Ctrl+Home/End). Drag over the transcript to select text; releasing
copies it (via OSC 52 and the OS clipboard tool, so it works in terminals
without OSC 52). Hold Shift while dragging for the terminal's own selection. Type while the agent works to steer it, press `esc`
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

## RPC mode

`malachi -mode rpc` runs headless for another program to drive: a GUI, an
editor plugin, a script. It reads one JSON command per line on stdin and
writes one JSON object per line on stdout, either a response correlated by
`id` or an event, and exits when stdin closes. The protocol is tau's RPC mode,
which follows Pi's, so clients written for those should work here.

```text
→ {"id":1,"type":"prompt","message":"fix the failing test"}
← {"type":"response","command":"prompt","success":true,"id":1}
← {"type":"agent_start"}
← {"type":"message_update","message":{…},"assistantMessageEvent":{"type":"text_delta","delta":"Looking…",…}}
← {"type":"tool_execution_start","toolCallId":"…","toolName":"bash","args":{"command":"go test ./..."}}
→ {"id":2,"type":"steer","message":"only the parser package"}
← {"type":"response","command":"steer","success":true,"id":2}
← {"type":"queue_update","steering":["only the parser package"],"followUp":[]}
…
← {"type":"agent_end","messages":[…],"willRetry":false}
← {"type":"agent_settled"}
```

| Command | Data in the response |
| --- | --- |
| `prompt` (`message`, `streamingBehavior?`), `steer`, `follow_up` | — |
| `abort` | — |
| `get_state` | model, thinking level, `isStreaming`, `isCompacting`, session file and id, queue size, `projectTrust` |
| `get_messages`, `get_last_assistant_text` | the transcript; the last reply's text |
| `get_available_models`, `set_model` (`provider?`, `modelId`) | model descriptions |
| `get_available_thinking_levels`, `set_thinking_level` (`level`) | `levels` |
| `compact` (`customInstructions?`), `set_auto_compaction` (`enabled`) | the summary, `firstKeptEntryId`, token estimates before and after |
| `new_session`, `switch_session` (`sessionPath` or `sessionId`) | `cancelled` |
| `get_session_stats` | message and token counts, cost, context usage |
| `set_trust` (`decision`: `trusted`/`untrusted`, `remember?`, `scope?: "parent"`) | the new `projectTrust` |

Ordering: a `prompt`'s response comes before the events of the run it
starts; any other command's response comes after the events its own work
produced (a `compact`'s `compaction_start` … `compaction_end`, then its
response).

A `prompt` while the agent is working must say what to do with it:
`"streamingBehavior": "steer"` (inject before the next model call) or
`"followUp"` (send when the run would otherwise stop). During a compaction a
prompt is held and sent when it finishes. Besides the agent's events, the
session reports `compaction_start`/`_progress`/`_end`, `queue_update`,
`thinking_level_changed` and `agent_settled` (all work for a prompt is done).

Differences from tau, checked by `rpc/golden_test.go`: `compact` and
`get_available_models` are answered from a goroutine, so their responses can
come after later ones (correlate by `id`; this keeps `abort` working during a
long compaction). `set_trust` and `projectTrust` are malachi's: instruction
files are withheld until a client decides, as in `-p`. Malformed or oversized
records (over 16 MiB) are answered with `"command":"parse"` and reading
continues.

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

`bashTimeoutSeconds` stops a command the model gave no timeout for after that
many seconds (default 600; `-1` for no limit). A command's output beyond the
display limit is saved to a temporary file, up to 64 MiB; anything past that
is counted but not kept.

`contextWindow` is a provider's context window in tokens, defaulting to
128000. Set it to a real published figure when you have one: `/ctx` and the
status gauge measure against it, and a window that is too large will not warn
you before the provider rejects a request.

Some models are served only over the OpenAI Responses API (`/responses`)
rather than `/chat/completions`; on OpenCode Go that is the GPT, Grok and Muse
models, which the built-in preset routes there. A provider's
`responsesModels` lists them. A model the gateway refuses on chat with "does
not support this protocol" is moved to `/responses` automatically for the rest
of the session, so newly added models work before they are listed.

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
JSON object per line, tagged with the session and run. At 8 MB it rotates to
`agent.jsonl.1`, so it never takes more than about 16 MB. `/session` prints the
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

## License

MIT; see [LICENSE](LICENSE). malachi ports parts of
[tau](https://github.com/huggingface/tau), also MIT; [NOTICE](NOTICE)
carries tau's copyright and license.
