# Malachi Agent Instructions

Malachi is a Go coding-agent harness modeled on tau (`~/src/tau`), itself a
Python port of Pi. Keep Pi's separation of concerns:

```text
Harness  = reusable agent brain       (agent/)
Session  = coding-agent environment   (coding/)
TUI      = one possible frontend      (tui/)
```

## Packages

```text
agent/     messages, events, tools, loop, harness, session primitives. Imports nothing else in this module.
ai/...     providers (raw net/http + SSE). Import agent for message/stream types.
coding/    coding tools, system prompt, config, CodingSession. Imports agent and ai.
tui/       Bubble Tea frontend. Consumes agent events only.
cmd/       entrypoints.
```

`agent` must never import `ai`, `coding`, `tui`, or any terminal/rendering library.

## Conventions

- Wire JSON is Pi-compatible: camelCase message/event keys, `role`/`type`
  discriminators, optional fields omitted when empty.
- Session JSONL entry envelopes match tau's on-disk shape (`id`, `parent_id`,
  `timestamp`, `type`) so tau sessions load.
- Providers never return errors out of band: a stream always ends with exactly
  one `done` or `error` event carrying the final assistant message.
- Cancellation is `context.Context`.
- Use the fake provider (`ai/fake`) for deterministic loop/harness tests.
- Provider HTTP is hand-written `net/http`; do not add provider SDKs.
- Run `make test lint` before committing. Keep commits atomic.
