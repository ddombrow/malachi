# Handoff: things to try in the macOS sandbox

Written 2026-11-13 on a Linux machine, after reviewing the deny-by-default
macOS profile (v0.4.0) by reading it. Nothing here has run on a Mac. This
file and `sandbox/macprobe_darwin_test.go` are temporary: delete both once
the questions below are answered.

## What changed since v0.4.0

Two commits on `main`, tested on Linux only:

- `2785667` **Commands and MCP servers run without a controlling terminal.**
  They used to run in malachi's session, so a sandboxed command could open
  `/dev/tty`. On Linux it could write over the interface (reproduced). The
  worry on macOS is worse: the profile allows `file-ioctl` everywhere and
  writes to `/dev/tty*`, so `TIOCSTI` may let a command type into malachi
  (`/sandbox off`, `/allow-write ~`) or leave a shell command for when it
  exits. The fix is `Setsid` in `coding/proc_unix.go` (`newSession`).
- `4c43f51` `/home` is canonicalised in `privateHomeContainers`. Linux only
  in effect; nothing to check on a Mac beyond the tests passing.

## Step 1: the ordinary suite

```sh
git pull
MALACHI_REQUIRE_SANDBOX=1 go tool task lint test
```

CI runs this on macOS too, but run the one new test from a real terminal
window, where it means something (without a terminal it passes trivially):

```sh
go test ./coding -run TestBashHasNoControllingTerminal -count=1 -v
```

Expected: pass. A failure saying the command could open `/dev/tty` means
`Setsid` does not detach on macOS the way it does on Linux.

## Step 2: the probes

Run from a real terminal window (Terminal, iTerm, Ghostty), not from an
agent's shell. Open a second window first, run `tty` in it, and pass what
it prints:

```sh
MALACHI_MAC_PROBE=1 MALACHI_PROBE_TTY=/dev/ttys00N \
  go test ./sandbox -run TestMacProbe -v -count=1
```

It never fails; it logs one `PROBE` line per attempt. The probes start
commands the way v0.4.0 did, in the caller's session, so the terminal
probes show what was possible before `2785667`.

| Probe | Expected | If not |
|---|---|---|
| `tty-write` | succeeded, and the bracketed line appears in the terminal | The hole was Linux only; the fix is still right |
| `tty-inject` | unknown: this is the main question | See below |
| `other-tty-write` | probably succeeded; the line appears in the second window | Good: nothing to narrow |
| `listen-0.0.0.0` | unknown | See below |
| `listen-127.0.0.1` | succeeded and `ACCEPTED` | Dev servers are broken with the network off; that would be a bug of its own |
| `chmod-outside`, `touch-outside`, `xattr-outside`, `truncate-outside` | all denied | A write the profile should stop; fix the profile |
| `ps-outside` | no command line shown | Other processes' arguments are readable; add to the README as on Linux |
| `signal-outside` | succeeded (`SIGNALLED`) | Fine either way; the README says commands can signal |
| `open-url`, `osascript`, `pasteboard`, `keychain` | all denied | The deny-by-default profile has a gap; fix the profile |

### `tty-inject`

If it prints `INJECTED`, the text `TIOCSTI_WORKED` will be sitting at the
shell prompt after the test exits: the sandboxed command typed it. That
confirms v0.4.0 let a command type into malachi's terminal on macOS, and
`2785667` is a security fix worth a release note that says so. If it fails
(`Operation not permitted` or similar), the session fix is still worth
having for `tty-write`, with a milder note.

Either way, consider narrowing the profile too, so the terminal is closed
even to something that keeps a controlling terminal: `(allow file-ioctl)`
has no filter, and `(regex #"^/dev/tty")` is writable. Pseudo-terminals a
command creates itself (`TestSandboxedPseudoTerminal`) must keep working.

### `listen-0.0.0.0`

The profile is `(allow network*)` then, with the network off,
`(deny network* (remote ip))`. The guess is that binding and accepting are
matched by local address, so the deny does not cover them. If the probe
logs `LISTENING`, `ACCEPTED from <LAN address>` and the dialler line says
it connected, then with the network "off" a command can serve data to
whoever on the network connects to it.

A fix to try, in `Profile` where the network is off: allow bind and inbound
on localhost only, so dev servers keep working.

```scheme
(deny network-bind network-inbound (local ip))
(allow network-bind network-inbound (local ip "localhost:*"))
```

SBPL filter spellings are easy to get wrong and fail silently, so prove it
with a test in `sandbox/integration_test.go` beside
`TestSandboxedNetwork`: listening on `0.0.0.0` fails with the network off,
listening on `127.0.0.1` works, and both work with it on.

### `other-tty-write`

Output only: a command can draw in your other windows and, through escape
sequences, set titles or the clipboard (OSC 52). If it succeeded, the
write rule could be limited to pseudo-terminals the command made, if
Seatbelt can tell them apart. It may not be able to; if not, add a line to
the README's "What it does not stop".

## Step 3: after the probes

1. Turn each confirmed finding into a real test, and fix it.
2. Update the README's "What it does not stop" for anything that stays.
   The entries for metadata and command lines say "On Linux"; the probes in
   rows 5 to 9 say whether that qualifier is right.
3. Delete this file and `sandbox/macprobe_darwin_test.go`.
4. Cut v0.4.1. `2785667` alone justifies it.

## Not checked from Linux

- Whether `Setsid` breaks anything on macOS that worked: a command that
  wants a password on the terminal (`ssh`, `sudo`, git over HTTPS) now
  fails instead of prompting over the interface. That is intended.
- Signals are not scoped on either platform, by decision: scoping them
  would stop a later command from killing a server an earlier one left
  running.
