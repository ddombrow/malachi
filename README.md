# malachi

A small terminal coding agent in Go, modeled on [tau](https://github.com/huggingface/tau)
(itself a Python port of Pi). It speaks Pi-compatible JSON for messages,
events, and sessions, and reads tau session files.

> [!WARNING]
> **malachi does not ask permission before it acts.** When the model decides
> to run a shell command, write a file, or edit one, it happens immediately:
> there is no approval prompt and no undo. A [sandbox](#sandbox), on by
> default on macOS and Linux, confines what that can touch: writes only in
> the project, temp and build-cache directories; no reading credentials
> (`~/.ssh`, cloud and registry tokens) or malachi's own files; no network
> and no local daemons such as Docker or ssh-agent unless you allow them.
> But it is a fence, not a VM:
>
> - Inside the project the model can delete or rewrite anything, including
>   `.git/hooks`, which run outside the sandbox when *you* next use git.
> - Everything a tool returns is sent to your model provider, network on or
>   off, and with the network on whatever a command can read it can send
>   anywhere.
> - On macOS a command can still ask system services to act for it, such as
>   opening a URL in your browser.
>
> Use it on work you can recover (a version-controlled checkout, committed
> first), and use a container or VM for repositories you do not trust, whose
> files the model will read. The same applies in `-p` and RPC mode, where
> nobody is watching at all.

```text
cmd/malachi   CLI: interactive TUI or print mode (-p)
tui/          full-screen Bubble Tea frontend
rpc/          headless JSONL protocol (-mode rpc)
coding/       tools (read, write, edit, bash), system prompt, config, sessions
sandbox/      what commands and file tools may touch; Seatbelt and Landlock backends
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
needs `bash` or `sh` on the PATH, cancelling a command there stops only the
shell, not the processes it started, and there is no [sandbox](#sandbox):
commands fail until you run with `-sandbox off`.

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
display limit is saved to a file under `$TMPDIR/malachi-spill/`, up to
64 MiB; anything past that is counted but not kept. A session's files are
removed when it closes, and ones left by a crash after seven days.

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
or ignored; what tools may touch is the [sandbox](#sandbox)'s job.

### Sandbox

Commands the model runs are confined by the operating system, and the file
tools (read, write, edit, grep, glob) check the same rules themselves:

- **Writes** only under the project, the temp directories, the user cache
  directory (Go's build cache, among others), the Go module cache, and npm,
  cargo and rustup caches.
- **No reading or writing** malachi's home (`~/.malachi`: your API keys,
  sessions, settings) or credential stores: `~/.ssh`, `~/.gnupg`, `~/.aws`,
  `~/.azure`, `~/.kube`, `~/.docker/config.json`, `~/.config/gh`,
  `~/.config/gcloud`, `~/.netrc`, `~/.git-credentials`, `~/.npmrc`,
  `~/.pypirc`, cargo and Terraform credentials, and the macOS keychains.
- **The network** is off by default: commands cannot open IP connections or
  resolve names (which would carry hostnames out as DNS queries), and on
  Linux cannot create sockets of any family but Unix and netlink. Downloads
  (`go mod download`, `npm install`, `git fetch`) fail until you turn it on
  with `/network on`, `-network on`, or `"network": true`. This is about
  commands only: malachi itself still sends the conversation, tool output
  included, to your model provider.
- **Local daemons are out of reach.** Commands cannot connect to Unix sockets
  such as Docker's or ssh-agent's. Through those a command could act outside
  the sandbox: start a container with your home directory mounted, or push
  with your loaded keys. On macOS, sockets under writable directories, name
  resolution and logging still work. On Linux, commands cannot create Unix
  sockets at all. `"unixSockets": true` lifts this, along with that
  protection.
- **API keys** that malachi loaded from its `.env`, or that a provider's
  `apiKeyEnv` names, are removed from commands' environment. This holds even
  with the sandbox off.

```json
{
  "sandbox": {
    "enabled": true,
    "network": false,
    "unixSockets": false,
    "writableRoots": ["~/scratch"],
    "hiddenPaths": ["~/.config/my-token"]
  }
}
```

`writableRoots` and `hiddenPaths` add to the defaults. Settings are read only
from `~/.malachi/settings.json`, which the sandbox hides, so a project cannot
widen its own sandbox. `-sandbox off` turns the sandbox off for one run, and
`-sandbox on` turns it on over a setting. `/network on` and `/network off`
switch commands' network access for the rest of the session, from the next
command; `-network on` does it for one run, and RPC clients send
`set_network` with `{"enabled": true}`.
`/sandbox` shows what is in force.
The status bar says `unsandboxed`, `sandbox unavailable` or `network on` when
those apply. When a command fails in a way that looks like the sandbox, its
result tells the model to ask you rather than work around it.

How it works:

- **macOS** runs commands under `sandbox-exec` with a generated profile.
  Apple deprecated the tool but offers no replacement, and other coding
  agents rely on it too.
- **Linux** uses Landlock (kernel 5.13 or later), plus seccomp when the
  network is off. malachi applies it through itself, with nothing else to
  install.
- **Failing closed.** Where the sandbox cannot run (an older kernel, Windows,
  `sandbox-exec` gone), malachi says so at startup and commands fail rather
  than run unconfined.

What it does not stop:

- **Anything in the project.** The model can delete or rewrite files there.
  That includes `.git/hooks`, which run with no sandbox when *you* next use
  git.
- **Anything readable and not hidden** can be read, and with the network on
  it can be sent somewhere. Whatever a tool returns is sent to the model
  provider, network on or off.
- **On macOS, other programs acting for a command.** The profile allows by
  default and confines files, IP and Unix sockets, but a command can still
  ask system services for things, such as opening a URL in your browser
  (`open`), which then runs outside the sandbox.
- **On Linux**, the names inside a hidden directory can be listed, though not
  their contents.
- **On Linux**, a writable directory that itself contains a hidden path (say,
  malachi run in your home directory) cannot gain new entries at its top
  level.

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
