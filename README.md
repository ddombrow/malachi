# malachi

A small terminal coding agent in Go, modeled on [tau](https://github.com/huggingface/tau)
(itself a Python port of Pi). It speaks Pi-compatible JSON for messages,
events, and sessions, and reads tau session files.

> [!WARNING]
> **malachi does not ask permission before it acts.** When the model decides
> to run a shell command, write a file, or edit one, it happens immediately:
> there is no approval prompt and no undo. A [sandbox](#sandbox), on by
> default on macOS and Linux, confines what that can touch: writes only in
> the project, temp and build-cache directories; in your home directory,
> reads only in the project, toolchains and directories you allow; no reading
> credentials (`~/.ssh`, cloud and registry tokens) or malachi's own files; no
> network and no local daemons such as Docker or ssh-agent unless you allow
> them.
> But it is a fence, not a VM:
>
> - Inside the project the model can delete or rewrite anything, including
>   `.git/hooks`, which run outside the sandbox when *you* next use git.
> - Everything a tool returns is sent to your model provider, network on or
>   off, and with the network on whatever a command can read it can send
>   anywhere.
>
> Use it on work you can recover (a version-controlled checkout, committed
> first), and use a container or VM for repositories you do not trust, whose
> files the model will read. The same applies in `-p` and RPC mode, where
> nobody is watching at all.

```text
cmd/malachi   CLI: interactive TUI or print mode (-p)
tui/          full-screen Bubble Tea frontend
rpc/          headless JSONL protocol (-mode rpc)
acp/          Agent Client Protocol for editors such as Zed (-mode acp)
coding/       tools (read, write, edit, bash, fetch, script), MCP servers, system prompt, config, sessions
codemode/     scripts that call tools: QuickJS-ng (WebAssembly) under wazero
mcp/          MCP client over stdio (2026-07-28, falling back to the 2025 handshake)
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
bin/malachi -mode acp            # serve an editor over ACP (see Editors)
bin/malachi -resume 2026-11      # resume a session by file path or name prefix
bin/malachi -p "..." -no-session # don't save this run's session to disk
bin/malachi -cwd ~/src/other     # work in another directory
```

For a single run, these flags override the settings:

| Flag | Values | Default |
| --- | --- | --- |
| `-trust` | `yes` or `no`: load this directory's instruction files or withhold them | the `projectTrust` setting |
| `-network` | `on` or `off`: commands' network access | `off` |
| `-loopback` | `on` or `off`: loopback access for sandboxed commands | `on` on Linux; on macOS only when asked for |
| `-sandbox` | `on` or `off`: `off` lets commands and file tools reach everything you can | `on` |
| `-code-mode` | `off`, `on` or `explore` | `on` |

The TUI is full-screen: a scrollable transcript above a pinned input and status
bar. Each item gets an icon (❯ you, 💬 reply, 💭 thinking, 👀 read, 🔍 grep,
📂 glob, 🔨 edit, 📝 write, >_ bash, ❌ error); set `"icons": "dots"` in
settings for a Claude Code-style ⏺.
The view follows new output unless you scroll up (mouse wheel, PgUp/PgDn,
Shift+↑/↓, Ctrl+Home/End). Drag over the transcript to select text; releasing
copies it (via OSC 52 and the OS clipboard tool, so it works in terminals
without OSC 52). Hold Shift while dragging for the terminal's own selection. Type while the agent works to steer it, press `esc`
to cancel, and use `/help` for the full command list.

| Command | Effect |
| --- | --- |
| `/model` | Switch model; bare `/model` opens a picker of what the provider serves |
| `/thinking` | Set the thinking level; bare `/thinking` opens a picker |
| `/new` (or `/clear`), `/resume`, `/session`, `/last` | Start, resume (bare `/resume` opens a picker), or inspect sessions |
| `/compact [note]` | Fold the conversation into a handover the model writes |
| `/trim [bytes]` | Trim old tool output mechanically before the next request |
| `/ctx` | Context budget: prompt tokens, tool output share, compaction count |
| `/codemode [mode]` | Pick or set the code mode: `off`, `on`, `explore` (`ctrl+x` cycles) |
| `/trust [yes\|no\|parent]` | Decide whether this directory's `AGENTS.md` is obeyed |
| `/sandbox` | Show what commands and file tools may touch |
| `/allow-read <dir>` | Let the model read an existing directory in your home for this session only |
| `/allow-write <dir>` | Add an existing directory as writable for this session only |
| `/network [on\|off]` | Switch commands' network access for this session (`ctrl+o` toggles it) |
| `/mcp [server]`, `/mcp restart <server>` | Show MCP servers; restart one now |
| `/sidebar` | Show or hide the sidebar (`ctrl+s`) |
| `/diff` | Show or hide uncommitted changes beside the transcript (`ctrl+g`): a summary of the files, then each file's diff with an `[ ask ]` that puts it in the prompt |
| `/help` | Show the full command list and key bindings |
| `/copy`, `/quit` | Copy the transcript, exit |

`/compact` and `/trim` are different tools: `/compact` is a lossy,
model-written summary of the conversation, `/trim` is the mechanical
removal of old tool output, replaced by a line saying what each call was
and how it ended. Compaction happens automatically near the context window.
Trimming happens only when you ask: done on its own, it took output the
model was still working from, and it went back for it. Set
`"trimToolOutput": true` to trim old output whenever it passes 64 kB (less
on a small context window).

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
| `get_state` | model, thinking level, `isStreaming`, `isCompacting`, session file and id, queue size, `projectTrust`, `sandbox`, `codeMode` (whether the script tool is offered, and its `mode`: `off`, `on` or `explore`; scripts, calls and failures this session), `mcp` (each configured server: status, error, tools, roots, whether deferred, `stale` and `staleReason`), `jobs` (each background job: id, command, status, exit code, start time, log file) |
| `get_messages`, `get_last_assistant_text` | the transcript; the last reply's text |
| `get_available_models`, `set_model` (`provider?`, `modelId`) | model descriptions |
| `get_available_thinking_levels`, `set_thinking_level` (`level`) | `levels` |
| `add_writable_root` (`path`), `add_readable_root` (`path`) | canonical path and updated sandbox state |
| `set_network` (`enabled`) | updated sandbox state |
| `get_available_code_modes`, `set_code_mode` (`mode`: `off`/`on`/`explore`) | `modes`; the new `codeMode`, as `get_state` reports it |
| `mcp_restart` (`name`) | the servers' state, once it has started or failed |
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
`thinking_level_changed`, `mcp_changed` (a server started, failed, restarted,
changed its tools or went stale; carries `mcp` as `get_state` does),
`jobs_changed` (a background job started or ended; carries `jobs` as
`get_state` does), `code_mode_changed` (carries `codeMode` as `get_state`
does) and `agent_settled` (all work for a prompt is done).

Differences from tau, checked by `rpc/golden_test.go`: `compact` and
`get_available_models` are answered from a goroutine, so their responses can
come after later ones (correlate by `id`; this keeps `abort` working during a
long compaction). `set_trust` and `projectTrust` are malachi's, as are `set_network`, the
roots, `mcp_restart` and the code mode commands: instruction
files are withheld until a client decides, as in `-p`. Malformed or oversized
records (over 16 MiB) are answered with `"command":"parse"` and reading
continues.

## Editors (ACP)

`malachi -mode acp` serves an editor over the
[Agent Client Protocol](https://agentclientprotocol.com), version 1: JSON-RPC
on stdin and stdout, one message per line. Zed speaks it natively; in VS Code,
use an ACP client extension. In Zed, add malachi to `settings.json`:

```json
"agent_servers": {
  "malachi": { "type": "custom", "command": "/path/to/bin/malachi", "args": ["-mode", "acp"] }
}
```

then start a new malachi thread from the agent panel. `-model`, `-thinking`,
`-trust`, `-sandbox`, `-network` and `-code-mode` in `args` apply to every session the
editor opens; each session is opened in the project the editor names, and
saved and resumed like any other.

- **What works.** Streamed replies and thinking, a card per tool call (its
  file and output; an edit or a write shows as the editor's own diff, and a
  command that exits non-zero as failed), Stop, a model and a
  thinking-level picker, a network on/off picker while the sandbox is on
  (like `/sandbox` in the TUI: it changes only that session), the context
  meter, and reopening past threads (`session/list`, `session/load`, which
  replays the conversation).
- **The sandbox is the boundary.** malachi asks no per-tool permission, and
  uses neither the editor's file system nor its terminal: commands and file
  tools run in malachi's own sandbox, as in the TUI. Zed's agent sandbox
  settings (`agent.sandbox_permissions`, its writable paths) are for Zed's
  own agent and change nothing here; widen malachi's with `/allow-read`,
  `/allow-write`, the network picker, or `sandbox` in `~/.malachi/settings.json`.
- **Project trust.** When a project's instruction files are undecided, the
  first prompt brings up the editor's permission dialog: trust for this
  session, always, not now, or never. Dismissing it sends nothing.
- **MCP servers from the editor** are ignored unless `settings.json` says
  `"acp": {"editorMcpServers": true}`; then they run under the same rules as
  `mcp.json`'s (installed commands, sandboxed, a minimal environment), and
  `mcp.json` wins a name clash. Only stdio servers are accepted.
- **Slash commands.** `/sandbox` shows what malachi's sandbox allows in that
  session (the same report as in the TUI). `/allow-read <dir>` and
  `/allow-write <dir>` widen the sandbox for that session, with the same limits as in the TUI (not
  your whole home, nothing hidden). `/codemode [off|on|explore]` shows or
  sets the code mode, which the Code mode option also picks. malachi handles
  them itself; they never reach the model.
- **Other folders in the window.** Folders an editor opens beside the
  project (ACP's `additionalDirectories`) become readable for that session,
  as `/allow-read` would make them; writing there still takes `/allow-write`
  or `writableRoots`.
- **A weakened sandbox is said.** When the sandbox is off or cannot confine
  commands here, the session says so once: as a notice if the editor shows
  them, else at the start of the first reply.
- **Not yet.** Other slash commands, images in prompts, and ACP v2.

Zed's `dev: open acp logs` shows the traffic; malachi's own notes go to
stderr, which Zed keeps in its log.

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
  "commandEnvAllowlist": ["GOCACHE", "GOMODCACHE"],
  "icons": "emoji",
  "projectTrust": "ask",
  "providers": {
    "local": { "baseUrl": "http://localhost:8080/v1", "defaultModel": "qwen", "thinkingLevels": ["off"], "contextWindow": 128000 },
    "my-gateway": { "baseUrl": "https://gw.example/v1", "apiKeyEnv": "GW_KEY", "sessionHeader": "x-session-id", "wireApi": "responses" }
  }
}
```

`bashTimeoutSeconds` stops a command the model gave no timeout for after that
many seconds (default 600; `-1` for no limit). A command's output beyond the
display limit is saved to a file under `$TMPDIR/malachi-spill/`, up to
64 MiB; anything past that is counted but not kept. A session's files are
removed when it closes, and ones left by a crash after seven days.

Commands receive only a small default environment (`PATH`, `HOME`, temporary
directory and locale/terminal settings). Add names to `commandEnvAllowlist`
when a build needs other variables, for example `GOCACHE` or `GOMODCACHE`.
Variables loaded from `~/.malachi/.env` and provider API-key variables are
always withheld from commands, even if listed there. Do not allowlist a secret
unless you intend the model-run commands to be able to read it.

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

Set `wireApi` to `"responses"` to use `/responses` first for every model on a
provider (for example, for AWS Bedrock Mantle). `"chat-completions"` forces
`/chat/completions`; leaving it unset keeps the model-list and automatic-fallback
behavior.

Requests carry `User-Agent: malachi/<version>`. A provider's `sessionHeader`
sends the session id (stable across `-c`/resume) on every request; OpenCode Go
requires `x-opencode-session`, which the built-in preset sets.

Project instructions come from `AGENTS.md` (or `CLAUDE.md`) in `~/.malachi`
and in every directory from `/` down to the working directory — but only
where you have agreed to them. See [Project trust](#project-trust). Sessions
are append-only JSONL under `~/.malachi/sessions/<project>/`.

### Code Mode

The `script` tool runs a short JavaScript program in which every other tool
is a function, so the model can chain or fan out calls and get back only
what the script prints or returns:

```js
const [mod, hits] = await Promise.all([
  tools.read({ path: "go.mod" }),
  tools.grep({ pattern: "TODO", output: "files" }),
]);
return hits.details.results.map(r => r.path).filter(p => p.endsWith(".go"));
```

Each call returns `{text, details, isError}`: `text` is what the model would
see, and `details` is the same result as data. Every built-in tool declares
a schema for its details, so a script reads fields instead of parsing text:
grep's matches with their paths and lines, bash's exit code, the patch an
edit made. A script also gets far more of a command's output than the model
is shown: bash's `details.output` holds it whole up to 1 MiB, and the start
and end of anything longer, so a script can run the tests and return only
the failures. The script tool's description lists each
tool's fields, and `describeTool(name).details` gives the whole schema. An
MCP tool's details carry the server's `structuredContent`, described by the
server's own output schema when it declares one.

In a git repository scripts also get a `git` tool, which the model is not
offered directly: `status`, `log`, `diff`, `show` (a file as of a
revision) and `blame` (which commit last changed each line), all as data,
so one script can take a branch's diff and read around each hunk:

```js
const { details } = await tools.git({ action: "diff", base: "main" });
return details.files.flatMap(f => f.hunks.map(h => `${f.path}:${h.new_start} ${h.header}`));
```

A diff from `base` starts where the branch left it, so the base's own later
work stays out. The tool only looks; commits and the rest are still bash's.
git runs in the sandbox as a command does, without the hooks, external
diffs, textconv and fsmonitor programs the repository's configuration may
name.

A script can do nothing the model could not do one call at a time with the
same tools and bash. It runs
in QuickJS-ng compiled to WebAssembly, with no filesystem, network,
environment or timers of its own, and each call it makes runs the real tool
under the same sandbox, refusals included. Calls made together run
together where the tool allows it (reads and searches; not edits or
commands). A script stops at `bashTimeoutSeconds` or its own `timeout`, at
200 calls, or at a 256 MiB heap. Its output is cut like a command's, with
the rest in a spill file, and the calls it made are recorded in the tool
result's details. In the TUI a running script lists its calls as they go,
a finished one shows a tally and the start of its output, and `/last` shows
its code, every call and the whole output.

The interpreter is compiled on the first script and cached under your user
cache directory (`~/Library/Caches/malachi/codemode` on macOS,
`~/.cache/malachi/codemode` on Linux). Set `"codeMode": false` to leave the
tool out.

Models rarely reach for scripts on their own, even when exploring a
codebase would cost a fraction as much. To make them, set:

```json
{ "exploreInScripts": true }
```

read, grep and glob are then offered only through scripts: the script
tool describes them in full, the model is told not to explore with bash
instead, and a `view` tool shows it image files, which scripts cannot.
Everything else, edits and commands included, stays a direct call. It is
an experiment: compare the tokens a task takes with it on and off.

The setting decides the mode a session starts in, unless `-code-mode
off|on|explore` says otherwise. The mode in effect carries over to the next
session on `/new`, `/resume` or RPC's `switch_session`. To switch while it runs,
press `ctrl+x` in the TUI (off → on → explore) or use `/codemode`; editors
get a Code mode option, and RPC clients `set_code_mode`.

### Fetch

The `fetch` tool GETs a URL and returns it as text: HTML reduced to readable
text with its links, JSON and other text as is, binary refused. It follows
your network setting: with the network off (the default), it is refused
like a command's connection. It never connects to loopback, private,
link-local (including the cloud metadata address `169.254.169.254`) or
shared-address-space hosts, checking the address actually dialed, after DNS
and on every redirect, and it ignores proxy variables. To limit it further,
list the hosts it may reach:

```json
{ "sandbox": { "fetchHosts": ["pkg.go.dev", "*.github.com"] } }
```

It reads at most 10 MiB of a response; text past the display limit goes to a
spill file. Scripts call it as `tools.fetch({url})`, several at once with
`Promise.all`. Whatever it fetches is untrusted input to the model.

### Background jobs

A command that does not exit, such as a dev server or a file watcher, can be
started as a background job: `bash` with `background: true` returns at once
with the job's id and whatever it printed in its first half second. The
`jobs` tool lists the session's jobs, reads what a job has printed since it
was last read (waiting up to 30 seconds for more, if asked), and stops one,
asking it to exit and killing it three seconds later if it has not.

The TUI's sidebar lists them, and RPC's `get_state` reports them as `jobs`.
Jobs run under the same sandbox as any command. At most eight run at once.
They carry over across `/new` and resume, like MCP servers, and end with the
session: a command left running with `&` is no one's and outlives malachi,
so prefer a job. A job's output is kept in memory up to 1 MiB, and all of it
(up to 64 MiB) in a log file the job's output names.

With the network off, a job can serve on localhost only where commands have
a loopback (see the network under [Sandbox](#sandbox)). On Linux they do
by default, and a session's commands share it, so a later command reaches a
job's server. On macOS they do only once you set `"loopback": true` or pass
`-loopback on`; until then a server cannot listen at all, and its job says so.

### MCP servers

malachi runs local [MCP](https://modelcontextprotocol.io) servers you
configure in `~/.malachi/mcp.json`, in the common `mcpServers` shape. That
is the only place servers come from: a repository cannot add one, and the
sandbox hides `~/.malachi`, so no tool can read or change the file.

```json
{
  "mcpServers": {
    "memory": {
      "command": "malachi-memory",
      "description": "Notes about the user and their projects that persist between sessions",
      "env": { "MEMORY_DB_PATH": "${HOME}/.local/share/malachi-memory/memory.grafeo" },
      "writable": ["~/.local/share/malachi-memory"]
    },
    "time": { "command": "mcp-server-time", "deferred": false }
  }
}
```

- **Installed commands only.** `command` must be an installed program: a
  path or a name on `PATH`. Launchers that fetch code each time they start
  are refused (`npx`, `uvx`, `bunx`, `pipx run`, `pnpm dlx`, `npm exec`,
  `uv tool run`, `docker run`, ...), as are wrappers such as `env` or
  `sudo` and interpreters given inline code (`bash -c`, `node -e`). Install
  the server instead (`npm i -g <package>`, `uv tool install <package>`, a
  binary) and point `command` at it.
- **Local servers only.** Each server is a process malachi starts and talks
  to over stdio. Remote and HTTP servers are not supported.
- **Sandboxed.** A server runs under your session's sandbox, network
  setting included, plus its own `readable` and `writable` directories
  (`~` allowed). Those belong to the server's process alone: bash and the
  file tools never get them, and hidden paths stay hidden. A server whose
  sandbox changes (the network toggled, a root added) shows "restart
  pending" and restarts on its next call, since a process's sandbox is
  fixed when it starts.
- **A minimal environment.** Servers get the same small environment as
  commands, plus the variables their `env` names. `${NAME}` takes a value
  from your environment; malachi's own API keys are refused. Put tokens in
  `env`, never `args`: any process can read another's command line.
- **Deferred by default.** A server's tools are not sent to the model with
  every request. The system prompt gets one line per server (its
  `description`), and the model finds and calls the tools from a script:
  `searchTools("mcp__memory")`, then `tools.mcp__memory__recall({...})`.
  `"deferred": false` offers a server's tools directly, as
  `mcp__<server>__<tool>`; with Code Mode off, every server's are.
- **The rest.** `"tools": [...]` offers only the named tools, and
  `"disabled": true` turns a server off. Servers start in the background
  when a session opens and carry over across `/new` and resume. One that
  fails to start is retried after a backoff (5 seconds, doubling to 5
  minutes), and output is cut like a command's, with a spill file.

`/mcp` lists every server with its status, protocol version, command, roots,
how its tools are offered and the tools, or one server with `/mcp <name>`.
`/mcp restart <name>` restarts one now, past any backoff. The sidebar shows
each server's status, and a failed one's last line of stderr. Starts,
failures and restarts go to the diagnostics log.

malachi speaks MCP 2026-07-28 and falls back to the 2025 `initialize`
handshake for older servers.

### Project trust

An `AGENTS.md` is a file the *project* supplies, and supplying one is exactly
what you would do if you could get someone to open a repository: its contents
become the agent's instructions. So they are withheld until you decide.
Instruction files must be regular files; symlinked `AGENTS.md` and `CLAUDE.md`
files are ignored.

```json
{ "projectTrust": "ask" }
```

`ask` (default) withholds and tells you which files were held back. `always`
and `never` skip the question. Withheld files are named at startup and in
`-p` mode; their contents are never printed. A `/trust yes` or `/trust no`
choice is saved in the current session and restored when that session resumes;
it does not change the global trust store. `/trust yes --save` or
`/trust parent` remembers the choice globally for the directory or its parent.

| Command | Effect |
| --- | --- |
| `/trust` | Show what is withheld and pick yes, no (both for this session) or parent |
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

- **Writes** only under the project, the temp directories, build and
  package caches, the Go module cache, and npm, cargo and rustup caches. On
  macOS that is the whole user cache directory. On Linux it is the caches of
  compilers, package managers, linters and test tools inside `~/.cache`, by
  name (`go-build`, `pip`, `uv`, `yarn`, `pnpm`, `ms-playwright`, `ccache`
  and so on), and not the directory: shells load code from it (oh-my-zsh's
  and fish's completions, Powerlevel10k's instant prompt), so a file written
  there would run in your next shell. A tool whose cache is not on the list,
  or has never run and so has no directory yet, fails to write it: create
  the directory and add it to `"writableRoots"`. `/dev/shm` is writable
  too, since semaphores and shared memory live there.
- **Reads in your home directory** only under the project, the writable
  directories and the toolchains commands need to run: cargo, rustup, nvm,
  pyenv, rbenv, asdf, volta, bun, deno, mise, uv, pnpm, sdkman, gradle and
  maven directories, `~/go/bin`, `PATH` entries, git's configuration, and the
  directory holding malachi. Everything outside your home stays readable,
  since commands need the system's files. When malachi starts inside a git
  repository, the repository's top level is readable and its git directories
  (including a linked worktree's shared `.git`) are writable, so git works
  from a subdirectory or a worktree; a repository holding your whole home
  directory is ignored. A command can still tell whether a path in your home
  exists, but not list it or read it.
- **No reading or writing** malachi's home (`~/.malachi`: your API keys,
  sessions, settings) or credential stores: `~/.ssh`, `~/.gnupg`, `~/.aws`,
  `~/.azure`, `~/.kube`, `~/.docker/config.json`, `~/.config/gh`,
  `~/.config/gcloud`, `~/.netrc`, `~/.git-credentials`, `~/.npmrc`,
  `~/.pypirc`, cargo and Terraform credentials, Maven's `settings.xml` and
  Gradle's `gradle.properties` (repository passwords and signing keys; both
  tools build without them), and the macOS keychains. malachi's own cache is
  hidden too (it holds the script interpreter as machine code, which
  malachi runs), and on Linux so are your runtime directory
  (`/run/user/<uid>`: display authority cookies, and podman's registry
  credentials) and the systemd journal.
- **The network** is off by default: commands cannot open IP connections,
  listen for them (a dev server on localhost included), or resolve names
  (which would carry hostnames out as DNS queries), and on Linux cannot
  create sockets of any family but Unix and netlink. Downloads
  (`go mod download`, `npm install`, `git fetch`) fail until you turn it on
  with `/network on`, `-network on`, or `"network": true`. On Linux a
  command still has a loopback of its own, so a test suite that starts a
  server on localhost works: each command gets a network namespace with
  nothing but a loopback interface in it, where it can serve and connect to
  itself but reach neither the network nor services on your own localhost.
  A session's commands share that namespace, so one reaches a server another
  left running as a [background job](#background-jobs).
  `"loopback": false` (or `-loopback off`) takes that away too. It runs the
  command in a user namespace, where files that are not yours appear owned
  by `nobody` (access is unchanged), and it takes a system that allows
  unprivileged user namespaces, which Ubuntu 23.10 and later restrict
  (`kernel.apparmor_restrict_unprivileged_userns`; how to allow them is under
  "How it works" below). Where there are none,
  commands have no network at all, and malachi says so at startup if you set
  `"loopback": true` yourself, and in any command the sandbox then refuses.
  On macOS the loopback is shared with everything you run, so it is off
  unless you set `"loopback": true` (or pass `-loopback on`). Then commands
  may connect to localhost, and to nothing beyond it, but that reaches every
  local service you run (databases, other dev servers), and a server they
  start can listen on any interface, where other machines can reach it:
  Seatbelt cannot confine listening to loopback. That is still less than
  turning the network on, and malachi says so at startup. `/sandbox` shows
  which loopback, if any, is in force.
  This is about commands only: malachi itself still sends the conversation,
  tool output included, to your model provider.
- **Local daemons are out of reach.** Commands cannot connect to Unix sockets
  such as Docker's or ssh-agent's. Through those a command could act outside
  the sandbox: start a container with your home directory mounted, or push
  with your loaded keys. On macOS, sockets under writable directories, name
  resolution and logging still work. On Linux, a command can make Unix
  sockets and use them among its own processes (process pools, language
  servers and browsers talking to their children need that), but not reach
  one made outside the sandbox; that takes a recent kernel (Landlock ABI 9),
  and on an older one commands cannot create Unix sockets at all.
  `"unixSockets": true` lifts this, along with that protection.
- **Devices are out of reach**, the GPU included, so a command that renders
  or computes on it finds no device (on Linux, Mesa falls back to drawing
  on the CPU). `"gpu": true` opens the graphics drivers by name, for
  rendering and compute off screen, and nothing else: a command still
  cannot open a window. It is off by default because those drivers are a
  large part of the kernel to expose. On macOS, Apple silicon is tested;
  the drivers of Intel Macs are listed but untested. On Linux it opens the
  render nodes (`/dev/dri/renderD*`), which carry rendering and compute and
  cannot set a display's mode, and leaves the display's own nodes closed;
  an AMD GPU under Mesa is tested (OpenGL and OpenCL). AMD's compute
  device for ROCm (`/dev/kfd`) opens and answers, though ROCm itself is
  untested, as are NVIDIA's proprietary driver and WSL's GPU, whose devices
  are listed.
- **Terminals are closed.** A command runs without a controlling terminal
  and cannot open a terminal device, so it cannot read what you type in
  your other terminal windows or write to them. On macOS that also means no
  pseudo-terminals: `script`, tmux and `expect` fail in a command. A
  terminal a command makes for itself is the same kind of device as your
  other windows, and macOS cannot tell them apart, so `"pty": true` opens
  both: pseudo-terminals work, and a command can read and write every
  terminal you have open. On Linux commands get no pseudo-terminals either
  by default, but there `"pty": true` costs nothing in privacy: commands
  get a set of pseudo-terminals of their own, in which your other windows
  do not exist. The price is different. To be allowed to make that set, a
  command runs in a user namespace, where every file not owned by you
  appears to belong to `nobody`. Access is unchanged, but programs that
  check who owns a file notice: `ssh`, for one, refuses its system
  configuration. It also needs a system that lets unprivileged processes
  make user namespaces; where it does not, malachi says at startup that
  the setting has no effect.
- **API keys** that malachi loaded from its `.env`, or that a provider's
  `apiKeyEnv` names, are removed from commands' environment. This holds even
  with the sandbox off. On Linux, malachi also marks its own process
  non-dumpable so child commands cannot read the original environment through
  `/proc`; this disables core dumps, and malachi refuses to run as root. This
  process-inspection protection is complemented by a Seatbelt rule on macOS
  that denies sandboxed commands access to information about other processes.

```json
{
  "sandbox": {
    "enabled": true,
    "network": false,
    "loopback": true,
    "unixSockets": false,
    "gpu": false,
    "pty": false,
    "services": [],
    "writableRoots": ["~/scratch"],
    "readableRoots": ["~/src/tau"],
    "readAnywhere": false,
    "hiddenPaths": ["~/.config/my-token"],
    "unhiddenPaths": ["~/.m2/settings.xml"]
  }
}
```

`writableRoots`, `readableRoots` and `hiddenPaths` add to the defaults.
`unhiddenPaths` takes paths off the default hidden list, for a build that
needs one, such as Maven's `settings.xml` for a private repository; it cannot
unhide malachi's home or anything in `hiddenPaths`. `readAnywhere` lets the
model read your whole home directory, hidden paths aside. Settings are read only
from `~/.malachi/settings.json`, which the sandbox hides, so a project cannot
widen its own sandbox. `-sandbox off` turns the sandbox off for one run, and
`-sandbox on` turns it on over a setting. `/network on` and `/network off`
switch commands' network access for the rest of the session, from the next
command; `-network on` does it for one run, and RPC clients send
`set_network` with `{"enabled": true}`. `-loopback on` and `-loopback off`
override the loopback setting for one run.
`/sandbox` shows what is in force.
`/allow-read <dir>` lets the model read one existing directory in your home
for this session only, the same way; hidden paths stay unreadable.
`/allow-write <dir>` adds one existing directory to the writable roots for
this session only; the path is resolved and printed, and the grant takes effect
on the next tool call. Filesystem roots, your entire home directory, and paths
overlapping hidden locations are refused. The grant is not saved in settings
and is lost when starting or resuming another session. Model-run tools can
modify or delete everything in an allowed directory.
RPC clients hear about every such change as a `sandbox_changed` event
carrying the sandbox as `get_state` reports it.
On a terminal at least 100 columns wide, a sidebar beside the transcript
shows the session's surroundings (its `×`, `ctrl+s` or `/sidebar` hides it). Its
sandbox section shows whether the sandbox is on, the network (click it, or
press `ctrl+o`, to switch), the readable and writable directories (built-in
toolchain, cache and temp directories are counted rather than listed), and
how many paths are hidden. Its trust section shows whether the project's
instruction files are loaded and what decided it (`/trust` changes it). Its
code mode section counts the session's scripts, their calls and failures.
Its MCP section, shown when servers are configured, lists each server's
status; click one for its details.
malachi's version sits at the top of the sidebar, beside its `×`.
The status bar says `unsandboxed`, `sandbox unavailable`, `network on` or
`loopback only` when those apply. When a command fails in a way that looks
like the sandbox, its result tells the model to ask you rather than work
around it.

How it works:

- **macOS** runs commands under `sandbox-exec` with a generated profile.
  Apple deprecated the tool but offers no replacement, and other coding
  agents rely on it too. The profile denies by default: a command can run
  and signal processes, read system information, use files and sockets as
  the policy allows, and reach a short list of system services (user
  lookups, logging, preferences, file-change events, and with the network on
  name resolution and certificate trust). It cannot ask another program to
  act for it, so `open`, the clipboard (`pbcopy`, `pbpaste`), AppleScript
  and the keychain do not work in a command, and neither do devices (IOKit),
  apart from the GPU if you allow it. A debugger works on what the command
  started and cannot attach to your other programs. A tool that needs a
  service that is not on the list fails, often with an error that does not
  say why; the system log names what was refused, and `"services"` in the
  sandbox settings allows more by exact name:

  ```sh
  log show --last 1m --predicate 'process == "launchd" AND eventMessage CONTAINS "Sandbox restriction"'
  ```

  Sandboxes do not nest on macOS, so a tool that starts its own fails
  inside malachi's. `swift build` is one: run it as
  `swift build --disable-sandbox`, which skips SwiftPM's sandbox while
  malachi's still applies (for a package built by `xcodebuild`, the option is
  `-IDEPackageSupportDisableManifestSandbox=YES`). malachi tells the model so
  when it sees the error. `xcodebuild test` does not work by default: it
  goes through the test-manager daemon, which UI tests also use to drive
  other apps. `swift test` covers a package; for an app project, list the
  daemon yourself:
  `"services": ["com.apple.testmanagerd", "com.apple.testmanagerd.control"]`.
  Headless browsers: Chromium's headless shell runs, started without its
  own sandbox since sandboxes do not nest. Playwright's headless Chromium
  does both by default. Puppeteer needs to be told:
  `puppeteer.launch({ headless: "shell", args: ["--no-sandbox"] })`, and
  its browsers made readable with `"readableRoots": ["~/.cache/puppeteer"]`. The
  full Chrome binary in headless mode registers as an app first: under
  Playwright (`channel: "chrome"`) it runs with
  `"services": ["com.apple.coreservices.launchservicesd"]`, and under
  Puppeteer, where it is the default, it does not run. A browser with a
  window does not run, nor does headless WebKit, and Firefox does not start
  inside any macOS sandbox.
  A service you list acts with your full access, not the command's, so a
  list is only as tight as its loosest entry. The iOS simulator is the plain
  case and stays closed: a program started in a simulator
  (`simctl spawn`) runs outside the sandbox and can read hidden paths and
  write anywhere you can. Listing `com.apple.CoreSimulator.CoreSimulatorService`
  switches the sandbox off for any command that cares to use it.
- **Linux** uses Landlock (kernel 5.13 or later) for files and a seccomp
  filter for sockets, on x86-64, arm64, riscv64 and loong64. malachi applies
  it through itself, with nothing else to install.
- **Failing closed.** Where the sandbox cannot run (an older kernel, Windows,
  `sandbox-exec` gone), malachi says so at startup and commands fail rather
  than run unconfined. That includes malachi started by a command of its
  own: a sandbox cannot be built inside one, on either system. Its test
  suite, run that way, skips the tests that need a sandbox.
- **Saying what it cannot do.** On Linux before 6.2, Landlock cannot stop a
  command truncating a file, so a command can empty any file you can write,
  though not read a hidden one or write contents outside the writable
  directories. The sandbox still runs there, and malachi says so at startup.
- **User namespaces, for the loopback and pty settings on Linux.** Both start
  each command's helper in a user namespace of its own, which takes a system
  that lets an unprivileged process make one and keep its capabilities
  there. Ubuntu 23.10 and later (and Lima's Ubuntu images) let one be made
  but take its capabilities away. `/sandbox` shows whether the loopback is in
  force, and `sysctl kernel.apparmor_restrict_unprivileged_userns` prints 1
  where Ubuntu restricts them. To allow them for every program:

  ```sh
  echo 'kernel.apparmor_restrict_unprivileged_userns=0' | sudo tee /etc/sysctl.d/60-userns.conf
  sudo sysctl --system
  ```

  or for malachi alone, with an AppArmor profile naming its binary (the
  helper is the same binary):

  ```sh
  sudo tee /etc/apparmor.d/malachi <<'EOF'
  abi <abi/4.0>,
  include <tunables/global>
  profile malachi /home/you/.local/bin/malachi flags=(unconfined) {
    userns,
  }
  EOF
  sudo apparmor_parser -r /etc/apparmor.d/malachi
  ```

  The profile does not cover `go test`, whose test binaries are built at
  temporary paths. In a Lima VM, the sysctl can go in the instance's
  `lima.yaml`, which runs it at every boot (restart malachi after it has
  run, since malachi asks once at startup):

  ```yaml
  provision:
  - mode: system
    script: |
      #!/bin/sh
      sysctl -w kernel.apparmor_restrict_unprivileged_userns=0
  ```

What it does not stop:

- **Anything in the project.** The model can delete or rewrite files there.
  That includes `.git/hooks`, which run with no sandbox when *you* next use
  git.
- **Anything readable and not hidden** can be read: everything outside your
  home directory, and the readable directories inside it. With the network
  on it can be sent somewhere. Whatever a tool returns is sent to the model
  provider, network on or off.
- **On Linux**, the names inside a hidden directory can be listed, though not
  their contents. While the home directory is private, the directories that
  hold it (`/`, `/home`) cannot be listed either: Landlock would extend the
  listing to everything under them.
- **Signalling your other programs.** A command can stop or kill any process
  of yours, malachi included. Keeping signals inside the sandbox would also
  keep a command from stopping a server an earlier command left running.
- **On Linux, changing a file's metadata.** Landlock covers a file's contents,
  not its mode, timestamps or extended attributes, so a command can `chmod`
  or `touch` any file you own, hidden ones and those outside the writable
  directories included. It could make a private key world-readable or
  unusable, though it cannot read or rewrite it.
- **Other programs' command lines.** A command can read the arguments of
  your other processes (on Linux through `/proc/<pid>/cmdline`, on macOS by
  asking the kernel, though `ps` itself is refused there), so a secret passed
  as an argument to something else you are running can be seen. Their
  environments cannot.
- **On Linux**, 32-bit programs get no sockets, whatever the
  network setting: on x86-64 they run otherwise, and on other architectures
  they are killed. The socket filter can only read the native system-call
  table, so it refuses what it cannot check.
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
and `test` are the usual loop. `go tool task wasm` rebuilds
`codemode/quickjs.wasm` from the pinned QuickJS-ng source (it needs zig).
`go tool task install` puts the binary in `~/.local/bin` (per-user XDG;
override with `PREFIX`, e.g. `PREFIX=/opt/homebrew` for a Homebrew prefix), and
`uninstall` removes it, leaving the directory alone if anything else is in it.
Builds are stamped with `git describe` (`bin/malachi -version`). Loop and
harness tests use the scripted provider in `ai/fake`. Golden fixtures in
`agent/testdata` and `agent/session/testdata` were generated from tau's own models.

## License

MIT; see [LICENSE](LICENSE). malachi ports parts of
[tau](https://github.com/huggingface/tau), also MIT; [NOTICE](NOTICE)
carries tau's copyright and license.
