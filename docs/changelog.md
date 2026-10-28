# Changelog

## v0.1.28 (2026-10-28)

- `ks session harness <session> <harness> --key <key-id|keepstate>` names the
  key the new harness calls. A switch to a harness of another provider now
  binds a key for that provider in the same change, or is refused before
  anything changes; the refusals (no key, more than one key, a key that is
  not that provider's, a switch that would cut off a provider the session
  calls) print with what to do next.
- A session being saved reads "being saved" for harness and model changes,
  not "pause it first".

## v0.1.27 (2026-10-22)

- Model calls on KeepState's key, where the service offers it: `ks run --agent
  --key keepstate` (or the account's default) creates the session with its
  provider on KeepState's key and its spend limit (`--spend-limit DOLLARS`),
  `ks session key set --key keepstate [--provider P] [--spend-limit D]` moves a
  provider to KeepState's key and `--key <id>` moves it back, `ks key list`
  shows KeepState's key per offered provider, `ks key default` reads and sets
  the account's default per provider, `ks preflight` gains `--key
  keepstate|own`, and `ks session usage` and `ks meter` show what calls on
  KeepState's key were charged.
- Generated tests for a hosted Cruise draft: `ks cruise draft create
  --generate-tests --aim TEXT --acceptance TEXT [--constraints TEXT] [--model
  family:model]` asks the service to write the tests a job must pass; `ks
  cruise tests estimate`, `generate`, `show`, `review` (every file in full and
  the set digest computed here from the files' own bytes, confirmed with
  `--confirm` or at the terminal) and `discard`; `ks cruise draft show
  --verify` also checks a generated set against the manifest.
- The client is built with Go 1.26.0 (it was 1.22.12), the same toolchain the
  service builds with, pinned in `go.mod` and in every workflow. A Cruise
  draft's workspace digest is SHA-256 over the gzip bytes of its archive, so a
  draft the console builds and `ks cruise init` on the same files agree only
  while both are built by the same Go. No command changes.

## v0.1.26 (2026-10-16)

A second harness, Codex CLI, beside Claude Code; and four usable saved points per session.

- `ks run --agent --harness <harness>` starts the agent inside a harness the
  catalog offers; Claude Code stays the default and is sent as nothing. The
  catalog is read before anything is created: a harness it withholds (until
  KeepState's certification run on it has passed) or does not list is
  refused with its reason. Codex calls your OpenAI key, so the preflight, the
  key choice and the session's binding are for OpenAI; the model is the
  harness's default unless `--model` names one of its models, and the run's
  line names the harness.
- `ks session harness <session> [<harness>] [--model <id>]` reads a
  session's harness, its model, the harnesses its image carries and whether
  it can switch now, or switches it while the session is paused, in effect
  from its next resume. Its files stay; the next instruction starts a new
  conversation that is handed KeepState's handoff brief of the work so far
  instead of the old conversation, which stays readable with
  `ks agent history`. A session created before its image carried a harness
  cannot switch to it; the refusal says to start a new session.
- `ks session brief <session>` reads the handoff brief of the session's
  latest harness switch: KeepState's record of the work so far, which the
  new agent is handed with its first instruction after the switch. It says
  who wrote the brief's narrative (the outgoing harness's smallest model, on
  the session's own key, billed by that provider), its size in tokens, what
  the code checks kept and dropped, and prints the brief itself.
- `ks session checkpoints` shows which saved points can be used: the newest
  three and one from up to an hour back can each be restored to or forked
  from. A USABLE and a POSITION column say which, and a saved point that
  cannot be named says why.
- `ks cruise models` lists the harnesses: the provider each calls, its
  models, and whether it is offered, with the note of a withheld one.

## v0.1.25 (2026-10-15)

Cruise jobs the service drafts from your files.

- `ks cruise draft create` asks the service to draft a Cruise job from a
  folder (packed here with the same selection `ks cruise init` uses), a
  `.zip`, `.tar.gz` or `.tar` archive, one of your sessions (`--session`,
  running or paused), or an agent's proposal (`--proposal`). It prints what
  was found and the job's digest, which this client computes too.
- `ks cruise draft list`, `show` (`--manifest`, and `--verify` to download the
  archive and recompute every digest here), `set` (goal, paths, check, ladder,
  spend limit, keys), `approve <draft> --confirm DIGEST` and `discard`. A job
  starts only for the digest you confirm; a job made from another digest is
  refused before anything is printed as a success.
- When the service answers that a proposal needs its files,
  `ks cruise proposal approve` says to draft the proposal and approve the
  draft's digest.

## v0.1.24 (2026-10-15)

A question asked of a connected agent can be taken back.

- Interrupting `ks agent ask` while it waits for the reply (Ctrl-C, or
  SIGTERM) now cancels the question, with the same request the browser
  console's Cancel sends: the request is closed, a reply to it is refused,
  and the adviser's own work is not touched. The line says what came of it:
  cancelled, the reply had already arrived (nothing is cancelled, and
  `ks advice show` reads it), the cancel was refused (the question stands), or
  the cancel could not be confirmed. A second Ctrl-C stops at once, saying the
  cancel's outcome is not known. Before, Ctrl-C only stopped the waiting and
  the question stood.

## v0.1.23 (2026-10-14)

Parity with the browser console, found by checking the client against it.

- `ks adviser connect` sends only the limits you name; a connection with none
  named is sized by the service for one turn of the adviser's model, and the
  answer prints the resolved limits and where they came from (`limits_from`).
- Model choice: `ks run --agent --model <id>` starts the agent on a model
  `ks cruise models` lists as offered and names it on the run's own line;
  `ks session model <session> [<model>]` reads a session's model and whether
  it can change now, or changes it while the session is paused, in effect
  from its next resume (the service's refusals are printed with the next
  action); `ks session show` prints a `runner model` line; `ks cruise models`
  prints each model's plain name and whether it is offered to sessions, with
  the note of a withheld model.
- `ks agent ask <name> "@<adviser> <question>"` asks one of the agent's
  connected advisers as a person and waits for the reply, saying when the
  question wakes a paused adviser; the reply is headed as the adviser's words,
  advice only, unverified. In `ks agent open` and in the app a line that
  starts with `@` takes the same path and is never sent to the agent as an
  instruction; one that names no connected adviser is refused with the known
  handles.
- `ks agent history <name> [--last N]` prints the conversation in any session
  state, with the whole text of every entry. The backlog of `ks agent open`
  and of the app is now the journal's tail: the events route pages forward
  only, and one page with a limit was the oldest rows.
- Every session and agent state is printed as the console's word beside the
  raw state: `Paused (parked)`, `Running (running)`, `Saving (stopping)`,
  `Starting (provisioning)`, `Ended (dead)`, `Failed (failed)`, `Needs
  recovery (recovery_required)`. The app's home says Paused for an agent whose
  session is parked. `ks session list` lists ended sessions by default, in a
  final Ended group.
- `ks session usage` prints the receipt the console prints, from the
  service's summary: charges so far, running time, saved data, model use
  (with No charge and the reason), the budget spent and left; a figure the
  meter did not answer reads unavailable, never 0. The entries stay under it.

## v0.1.19

Found by checking every documentation page against the client.

- In an agent's window, a `/` command that is missing an agent gets this
  agent: `/agent tell "run the tests"` tells this agent. A command whose first
  argument is a new name (`/agent create`, `/check define`) never gets one.
- Commands that name an agent with `--agent` (`/tasks`, `/results`,
  `/adviser connect`, `/advisers`, `/check define`, `/check list`) get this
  agent, for this session.
- `/help` lists all sixteen short forms, including `/checkpoints`,
  `/restore`, `/diff` and `/apply`.
- The status line reads durations as `2m` or `1h 5m`, not `2m0s`.
- Help and manifest wording: `ks run --agent` starts an agent that is
  running and supervised (it reads Ready after its first instruction);
  `--yes` names the one deletion plan it carries out (`ks key delete`);
  `ks cruise approve` reads the model catalog and your keys; the paid budget
  default cites price book v1.3.

## v0.1.18

Fixes found by the production check of the ks app.

- The app names a session by the id `ks session list` shows, everywhere (a
  new agent session's window showed a different, record-derived id).
- Leaving an agent no longer prints "the control could not be renewed": the
  app stops renewing before it releases control.

## v0.1.17

The ks app, continued.

- **Every ks command inside the app.** In an agent's window, `/<command>`
  runs that command: `/session checkpoints`, `/result diff <id>`,
  `/cruise status <job>`, all of them, with their own options (`--help`
  shows them). The window fills in its session and agent when the command
  takes them. Short forms: `/stop` `/pause` `/resume` `/queue` `/tasks`
  `/results` `/approvals` `/advisers` `/usage` `/keys` `/logs` `/agents`
  `/checkpoints` `/restore` `/diff` `/apply`. Tab completes a `/` command;
  Ctrl-C stops one that is running. A command that needs a confirmation
  names the flag instead of asking.
- **While you were away.** Opening an agent again starts with one line: what
  finished, what did not, the results, and what waits for you, since this
  terminal last showed that agent.
- **Several agents.** `/switch <agent> [session]` opens another agent
  without going home (it never guesses between two of the same name). The
  home screen puts agents that need you first, marked ⚑, and the status line
  says when another agent needs you.
- Quieter: no "operation accepted" lines inside the app; the agent's full
  name in the conversation; the per-turn changeset result is listed by
  `/results` instead of every turn.

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
