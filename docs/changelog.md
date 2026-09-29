# Changelog

## v0.1.16

### New: the ks app

Run `ks` on its own, at a terminal, and it opens an app you stay in:

- a home screen of your agents (↑/↓, Enter to open, `n` to start a new agent
  session, `q` to quit);
- an agent's window: the conversation scrolls above, you type below and
  Enter sends; Esc interrupts the instruction in flight; a permission
  request appears as a prompt that `y` allows once, `n` denies and `d`
  explains; a status line shows what the agent is doing, its queue, what
  waits for you, when it was last saved, its model use and whether you have
  control; Ctrl-C twice leaves (the agent keeps working).

`/help`, `/status`, `/take`, `/home` and `/quit` work in the window. For a
script, a pipe, `--json` or `--no-input`, bare `ks` prints the usage as
before.

### Fixed

- An idle, healthy agent no longer reads STALE in `ks agent status` and the
  agent window: with a live supervisor, an old resting report reads
  "no change since". A working or waiting agent still goes stale after 15 s.

## v0.1.15

Readability fixes reported by the owner.

- `ks --help` is organised for reading: a "Start here" block, then every
  command and command group on one line under what it is for (Agents,
  Sessions, Cruise, Keys and setup, Account). Options moved to each
  command's own `--help`, where they are explained.
- `ks <group> --help` (for example `ks agent --help`) lists the group's
  commands with their arguments and a one-line description.
- An agent command pointed at a plain session (started by `ks run` without
  `--agent`) now says the session has no agents and how to start an agent
  session, instead of "no such resource". This holds for the short id and the
  full id alike.
- `ks run` (plain) and `ks session list` say that the short id in the list is
  the start of the full id `ks run` prints, and that either works everywhere.

## v0.1.14

Release candidate. The production service (ctl build `66863064`, 2026-09-28)
reports every capability these commands need as available, so all 103
commands in `commands.json` read available; none is planned.

### Available in this release

**Agents, advisers, results, migration and Cruise proposals**

The 57 commands that were listed as planned in v0.1.13 are available: the
`ks agent ...` family (open, list, status, tell, view, stop, pause, resume,
queue, approvals and the rest), `ks adviser ...`, `ks advice ...`,
`ks result ...`, `ks session migrate` and `ks cruise proposals ...`. The
v0.1.13 binary already dispatched them and ran each one wherever the service
reported its capability available; this release changes what the manifest
says, and the two fixes below.

### Fixed

- `ks run --agent --task TEXT` never submitted its task. A new agent reports
  Ready only after its first instruction, and the command waited for Ready.
  It now treats an agent whose supervisor has reported as started, submits
  the task, and says "supervised and waiting for its first instruction";
  Ready is still printed only when the agent reports it.
- `ks run --agent` bound no provider key to the session, so the agent's first
  instruction was refused by the gateway ("not bound to a key"; nothing was
  billed). It now chooses the key before anything is created -- `--key KEY`,
  or your one enabled key for the agent's provider -- and binds it before the
  machine starts. No usable key, several, another provider's or a disabled
  key is refused with "Nothing was created"; a key is never guessed.

## v0.1.13

Release candidate. Every command's status in `commands.json` was re-derived
from the production service (capability registry build `8a22c753`) before
this release.

### Available in this release

**Cruise**

- `ks cruise status --watch` follows a job live. It shows:
  - the plain state, the attempt in flight, a pending check result, and the
    last save point;
  - spend with model, validation, runtime and storage apart ("unavailable"
    is never shown as $0), and why savings are not shown;
  - the next actions.

  It follows new events at the pace the service names, reconnects with
  backoff, and Ctrl-C ends only the watch.
- `ks cruise review` shows a job waiting for your decision:
  - why it stopped, and every attempt with its exact check result and cost;
  - what remains of the spend ceiling, or that it is unknown;
  - each option with its effect.
- `ks cruise resume` and `ks cruise cancel` show the chosen option's effect
  (and, for a resume, what it may still spend) before acting, and read back
  who decided and on which revision. A cancel reports the cleanup apart from
  the costs, which stand.
- `ks cruise result` shows an accepted result's badge exactly as the service
  gives it, its provenance chain and every limitation. A result accepted
  without provenance is never shown as verified.
- Downloads are checked before anything is written. This covers
  `ks cruise result --download` and `ks cruise artifact`.
  - The download must match the recorded sha256.
  - For a verified result, the tree it contains must match the candidate
    the signed receipt verified.
  - An existing file is never written over.
- `ks cruise apply` applies an accepted result to your project file by file.
  - It works only onto the base each file was changed from.
  - A changed local file refuses the whole apply and keeps your work.
  - It is confirmed by the changeset's digest, and is transactional
    (`ks result apply --recover` restores an interrupted one).
- `ks cruise run --separate-upload` and `ks cruise upload` send the
  workspace as its own upload. The archive is kept until the upload is
  stored, so a failed upload can be sent again with the same bytes.
- `ks cruise models` lists the model catalog. A ladder is checked against
  it before upload and at approve.
- The approval binds what will run: the selection, the inputs and the key
  routes. `ks cruise run` rechecks all of it and sends nothing on a change.
- Test discovery finds nested tests (for example `pkg/sub/tests/…` or
  `__tests__/…`) exactly as the verifier does, with a drift guard against
  the verifier's reference.

**Uploads**

- Upload policy `ks-upload-policy/2`: dependency and tool directories that
  the verifier never reads are no longer uploaded (`node_modules`, `.venv`,
  `venv`, `build`, `dist`, tool caches). `--allow <path>` still includes one
  you want. The list is the verifier's own. An approval made under policy v1
  still verifies.

**Sessions and keys**

- `ks session rename`, `ks session usage` (model, runtime and storage apart;
  "unavailable" is never $0) and `ks session delete` (through a plan you
  confirm).
- Legacy verbs addressed to an agent session:
  - `exec`/`attach` show the service's refusal by name and name the command
    to use instead.
  - `kill`, `wake` and `fork` follow the service's pointer to the workspace
    verbs. A kill never becomes a deletion: it produces a deletion plan you
    confirm.
- The service's rollout pause, read-only recovery and legacy-retired
  refusals are shown by name, with what still works.

**Terminal and safety**

- Lines are fitted by the columns they occupy, so wide text (CJK, emoji) no
  longer overflows. A terminal smaller than 80x24 is told so.
- A multi-line paste into the agent window is one instruction, never several
  commands.
- Result archives are extracted under stricter name rules: no `./` names,
  no control characters, no names over 1024 bytes, and nothing under `.git`
  or `.keepstate`. Case and Unicode-normalization collisions are refused
  with both names shown.
- Unknown states are always shown as unknown, never as a successful default.

### Fixed

- The approvals list and show read the service's field names, so a request's
  kind, scope, arguments and decision are no longer shown blank.
- An interrupted result download's partial file is removed by default
  (`--keep-partial` keeps it).
- Test fixtures are now synchronized with the tests that read them. The full
  test suite passes under the race detector.

### Built, not yet available

These commands are in this build and refuse with the service's reason until
the service enables them. The agent workspace is not yet available.

- Agent sessions and windows: `ks agent …`, `ks task …`, `ks approval …`,
  `ks check …`, `ks file …`, `ks result …`, `ks session fork`,
  `ks session restore`, `ks session idle`, `ks session checkpoints`,
  `ks session checkpoint-policy` and `ks project configure`.
- Advisers and advice: `ks adviser …` and `ks advice …`.
- `ks session migrate` (a legacy session to an agent session, through a
  reviewed preview).
- `ks cruise proposal …`.

See `commands.json` for each command's status.
