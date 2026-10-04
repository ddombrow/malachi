# Context management and compaction

Where the request view, the transcript, and the session file stand, and what
still has to be built. Written after microcompaction shipped (`809320e`),
observability landed (`2d36eaf`), and context measurement landed (`7401383`).

## What exists today

Two independent mechanisms, both deliberate:

1. **Request-view tool trim** (`coding/context.go`). Before every provider
   request, tool results older than the newest are replaced with
   `[compacted <tool> tool output: N bytes]` markers until the context-visible
   tool output fits a byte ceiling, and a deterministic ledger of tool facts
   (`<untrusted_tool_ledger>`) is injected as a user message. Copy-on-write:
   the transcript and the session file are untouched, so the history on disk
   stays complete and tau-compatible.
2. **Context measurement** (`coding/context_stats.go`). Each finished assistant
   message pairs the provider's reported input tokens with the tool bytes that
   request carried; the median of the deltas between consecutive requests is
   the measured tokens-per-byte for tool output on that model. `/ctx` reports
   it, `ctx_sample` records go to the diagnostics log.

## What is still true of the gaps

- **The ceiling is a fixed byte count** (64 kB), which assumes a conversion
  rate. Measurement now supplies the real one; the ceiling should use it and
  scale with the model's window.
- **The transcript and session file grow without bound.** Disk is not the
  problem — RAM is, worst on resume, where `Replay` rebuilds everything
  including base64 images. Only a *persisted* summary bounds this.
- **Compaction is user-triggered.** Auto-trigger needs both a window to
  measure against and a trigger policy, so it comes after those.

## Stage 1 — free up `/compact`

`/compact [bytes]` currently means the mechanical tool trim, which is not what
the word means everywhere else in the agent world.

- `/trim [bytes]` — the mechanical tool-output trim (today's `/compact`)
- `/compact [instructions…]` — the agent summary (stage 4)
- `/ctx` — measured context, unchanged

Pure rename plus help text. The byte-argument form moves to `/trim`.

## Stage 2 — derive the ceiling from the measurement

Add `ContextWindow int` (tokens) to `ProviderConfig`, defaulting to 128 000
when unset, matching tau's conservative default. Then:

```
otherTokens = last reported input tokens − tool tokens in that request
budget      = clamp(contextWindow − reserve − otherTokens, floor, ceiling)
budgetBytes = budget / measuredTokensPerByte
```

`reserve` covers the next response; tau uses 16 000 and so will we, as a named
constant. Before anything is measured, keep the 64 kB default rather than
guessing from characters.

Two properties to preserve:

- **Observation first, config as a bound.** A wrong `ContextWindow` should make
  the ceiling smaller or larger, never catastrophic.
- **One writer.** The session recomputes the ceiling after each request, when
  the measurement updates, and pushes it into the preparer. The forced-override
  for `/trim` still wins, and still applies to a single request.

## Stage 3 — context gauge

In the status bar, beside `ctx`: `ctx 12.3k ▓▓▓░░░░ 18%`, built from
`bubbles/v2/progress` with `WithoutPercentage` (the numeric part is already
shown) and a fixed width. It drops as one part when the bar is narrow, which
the existing part-dropping logic handles for free.

Worth noting: an alternative needs no config and is available today — show
*tool share* ("60% of your context is tool output") instead of window fill.
Less satisfying, honest immediately.

The gauge is the first line of defence against needing stage 4: being able to
see the window filling is the reason to reach for `/compact` deliberately.

## Stage 4 — agent-based `/compact`

**What the summary replaces.** Not tool output: the ledger already owns tool
facts, exactly and for free. The summary replaces the *conversation prefix* —
user turns, assistant text, thinking. One representation per span. After a
summary compaction the ledger's records are truncated to the retained tail, or
you carry two accounts of the same history.

**Persistence.** Write a real `compaction` entry in tau's shape: `summary`,
`first_kept_entry_id`, `tokens_before`, `usage`, `provider`, `model`. The read
path already exists (`TypeCompaction`, `session.Replay`,
`CompactionSummaryMessage`), and this is the only thing that bounds memory on
resume. Two consequences:

- `first_kept_entry_id` requires `Session` to map a message index to an entry
  id, which it can do since it writes one entry per message. If the kept tail
  has not been persisted yet, omit the field: `Replay` keeps everything after
  the entry, which is the tail for an append-only file.
- A **full-replay escape hatch** is required, not optional. `Replay` takes the
  last compaction entry; add a mode that ignores compaction entries entirely,
  so a transcript the summary lost stays recoverable from disk. Ten lines, and
  the difference between compaction and data loss.

**The call.** `coding/compact.go`, `Provider.Stream` directly, `tools: []`,
not through the harness — the summarizer's own conversation must not land in
the transcript or the session. Capture its usage onto the entry, and produce
an `agent.CompactionSummaryMessage` for the model-facing side as tau does.

**Prompt.** Tau's structured template (Goal, Constraints & Preferences,
Progress split into Done / In Progress / Blocked, Key Decisions, Next Steps,
Critical Context), plus the user's instructions when given, plus any previous
summary folded in for a repeat compaction. State explicitly that tool facts
come from the ledger, and that exact paths, function names and error messages
must be preserved — those are what weaker models flatten.

**Validation gate**, before anything is mutated:

- summary non-empty and above a floor length
- section headers present
- identifiers and paths from the dropped span survive a spot check

Fail → retry once with a stricter instruction → fall back to the deterministic
ledger. Never leave the session worse off than before.

**Atomicity.** Write the entry only after the summary is accepted, so an
interrupt or crash leaves the session untouched. `esc` cancels. Then **print
the summary into the transcript**: seeing what the model now believes is the
whole debuggability win, and it is the difference between "it forgot
something" and "I can see it forgot the auth constraint".

## Stage 5 — summarization progress

In the live area, where `working…` already lives. Named phases, because only
one is determinate:

```
compact · summarizing ▓▓▓▓▓░░░░░  54%
compact · validating
compact · writing
```

`bubbles/v2/progress` is available; its `ViewAs(percent)` can be driven from
outside, which matters because the work happens on the harness goroutine and
the UI only reads state. Streaming is determinate (output tokens against a
target: the previous summary's length, else ~2k). The other phases use the
existing spinner with the phase named. A percentage that is not real is worse
than a spinner.

## Stage 6 — ledger consistency after a summary

Truncate `c.records` to the retained tail when a summary compaction lands, so
the ledger and the summary do not both describe the same span.

## Sequencing

| stage | what | blocked on | size |
|---|---|---|---|
| 1 | `/trim` rename | — | tiny |
| 2 | `ContextWindow` + derived ceiling | — | ~40 lines |
| 3 | context gauge | 2 | ~30 lines |
| 4 | summarizer + validation + entry + ids | — | ~200 lines |
| 5 | phase progress | 4 | ~60 lines |
| 6 | ledger truncation | 4 | small |
| 7 | overflow recovery (`isContextLimit` already exists) | 2 | ~40 lines |

Stages 1–3 are small and unblock the gauge. Stage 7 is last because it is a
safety net for a wrong `ContextWindow`, and stages 2–3 make that less likely;
`isContextLimit` in `coding/context_stats.go` is already the classifier.

## Explicitly not doing

- **Rewriting the session file** to drop old entries. Breaks append-only,
  breaks tau compatibility, destroys what makes resume debuggable. Disk is
  cheap; keep the history.
- **Auto-trigger** until there is a window and a policy. Guessing a threshold
  before stage 2 produced the numbers to justify it is how sessions end up
  mysteriously forgetting things.
- **A second representation of tool facts.** The ledger is exact and free; a
  model rewriting it would only add error.