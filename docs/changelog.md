# Changelog

## Unreleased

- `ks import list | claude-code | codex | cursor` brings a conversation from
  your machine into a new agent session. A Claude Code or Codex CLI session
  continues the same conversation (`ks import claude-code --latest --plan`,
  then `--execute DIGEST`); a Cursor chat, or a conversation going into the
  other tool, goes across as a written summary built on your machine by fixed
  rules, with no model. The session file is read locally; what must not travel
  is taken out, old project paths are said as the new workspace's and anything
  shaped like a secret is replaced before anything is sent, with the counts by
  kind in the plan. `--with-files` brings the project's git repository under
  the upload policy. A conversation over 50 MB is refused. `ks import list`
  never prints a message. The same import is the MCP tool `ks_import_session`,
  which plans first and acts only on a confirm value.
- The Claude Code plugin now runs `@keepstateai/cli@0.1.44`.

## v0.1.44 (2026-11-14)

- `ks agent folder list | create | rename | order | delete` and `ks agent move
  NAME --folder FOLDER | --top [--before AGENT]` sort your agents into the
  folders the console's agents menu shows. A folder is a label on your own
  view, kept by the service per person and per context; it changes no agent,
  session or permission. A folder is named by its name (quoted when it has
  spaces, capitals aside) or its id. Deleting a folder puts its agents back at
  the top level and deletes none.
- `ks organization credits` now says how much of an organization's balance is
  credit KeepState gave it, that it was given and not bought, and labels those
  ledger rows. An organization that held only given credit used to read
  "bought: $0.00" and "It has bought no credit yet". An older service sends
  nothing to show, and the output is unchanged.
- The Claude Code plugin now runs `@keepstateai/cli@0.1.43`.
- An agent's name and a session's name may have spaces, for example `ks agent
  create "Senior Analyst"`. Spaces at the ends are dropped and a run of spaces
  becomes one; an agent's name is up to 48 characters and a session's up to 64.
  A name is not an identifier: quote it in a shell, and the commands `ks`
  suggests quote it for you. A project's name and a key alias keep the older
  rule (no spaces).
- `ks run --agent` without `--name` no longer makes up `run-<time>`: the service
  names the session after its agent, "<agent name> session", then
  "<agent name> session 2" when that name is taken, as the console does. A name
  you give is used as it is.
- An adviser connection never expires. It stays until you remove it
  (`ks adviser disconnect`), or until the connected agent is no longer available
  (removed, its session deleted or ended, or its account lost access), when the
  service ends it and `ks adviser list` says why. `--expires` is still accepted
  and ignored, so a script that passes it keeps working.

## v0.1.43 (2026-11-13)

- `ks cruise auto` runs Cruise Autopilot from a terminal, in seven steps.
  `ks cruise auto brief <draft> [--text TEXT] --consent` starts a plan from what
  you want and a ready draft of your files; at most three questions come back,
  each already answered with its default. `ks cruise auto answer <plan> [--set
  QUESTION=ANSWER]` answers them (giving no answer accepts every default).
  `ks cruise auto plan <plan> [--wait]` has the checks written by a model that
  is not on the job's ladder and run twice on your files, each of which must
  fail; planning stops at its free limit. `ks cruise auto status [plan]` shows
  your plans, or one plan in words: the checks as plain sentences, today's
  count, what cannot be checked, the cost limit, what planning cost and the
  next step. `ks cruise auto approve <plan>` runs a plan only when you confirm
  the digest of the exact manifest shown. The digest is computed here, the
  sentences shown are the ones inside that manifest, and `--yes` never approves
  (without a terminal, `--confirm DIGEST` is required). `ks cruise auto receipt
  <plan> [--sources]` prints the receipt's five lines for the plan's job:
  checked independently, locked before work, re-checkable, measured cost, not
  checked. `ks cruise auto discard <plan>` discards a plan that is not approved;
  its copy of the files is deleted and it never runs. The client refuses a plan
  card whose spend limit or models are not the manifest's. The same steps are
  MCP tools; the approve tool asks a person in the host app and takes no
  confirm value, and the brief tool never takes consent from the model.
- The default ladder `ks cruise init` drafts with, and a run with no `--ladder`
  uses, is now Claude Haiku 5.5, then Claude Sonnet 5.5 (it was Claude Haiku
  4.5, then Claude Sonnet 5). The model table the client carries lists the 5.5
  models, so `--ladder claude-haiku-5-5,claude-opus-5-5` reads. A job already
  approved keeps the ladder it locked.
- `ks cruise status` no longer prints a `record:` line that always read
  "unavailable". An accepted job's status now says `receipt: ks cruise receipt
  <job>`; any other job says plainly that it has no receipt (yet, or because it
  was not accepted).
- The Claude Code plugin now runs `@keepstateai/cli@0.1.42`.

## v0.1.42 (2026-11-12)

- `ks cruise receipt <job> [--out FILE]` downloads an accepted job's signed
  receipt exactly as the service sealed it (written as received, never parsed
  and written out again, never over an existing file). The bytes are checked
  against the digest the job's result names before the file exists, and the
  command says whether the receipt is publicly verifiable (an Ed25519
  signature anyone can check with the published public key) or
  service-verifiable only. A job accepted before the service kept receipt
  files says so in plain words and writes nothing. This needs the service's
  receipt release.
- `ks cruise verify RECEIPT` checks a Cruise receipt's Ed25519 signature
  offline, against the public key list KeepState publishes at
  https://ctl.keepstate.ai/.well-known/keepstate-receipt-keys.json (or a saved
  copy with `--keys-file`, or one key with `--public-key`). It needs no
  sign-in and no service key, and the receipt is never sent anywhere. It
  exits 0 when the signature is valid under a key that was valid at the
  receipt's time, 1 when it is not (altered, another key, a key that is
  unknown, outside its window or revoked), and 3 for a receipt with no
  Ed25519 signature, which only the service can check ("service-verifiable
  only"). The signed bytes are reproduced exactly as the service writes them,
  checked against vectors shared with the service.
- Agents can reach the files of the agent a person connected them to, from the
  terminal and from inside a session. `ks agent files list` shows a folder of
  the adviser's workspace, `ks agent files get` brings one file (checked
  against its sha256, up to 250 MiB, never replacing anything) into
  `.shared-in/`, `ks agent files log` lists every file asked for and every ask
  made of an agent, and `ks agent files allow <adviser> read|none` sets what a
  connection allows (read is the default). Nothing is woken and nobody is
  asked; links, secret names and files that hold a credential are refused, and
  a connection may fetch 1 GiB an hour. This needs the service's file-sharing
  release (agents.files) and refuses with its reason where it is not there.
- The Claude Code plugin now runs `@keepstateai/cli@0.1.41`.

## v0.1.41 (2026-11-08)

- Repositories from GitHub and GitLab, from the terminal. `ks connect list`,
  `show`, `github`, `gitlab`, `remove`, `repos` and `branches` manage the
  connections; `ks organization connections` lists the organization's; `ks
  session repo` and `ks cruise draft create --repo` name the repository to
  work on. A token is read from a prompt that does not echo it or from
  standard input, never from a flag or an argument, and is never printed; a
  stray argument on `ks connect github` or `gitlab` is refused without being
  repeated back. When several connections can reach a repository, every
  choice is printed with the way to pick one, and none is picked for you.
- The Claude Code plugin now runs `@keepstateai/cli@0.1.40`.

## v0.1.40 (2026-11-08)

- Approving a permission request through `ks mcp` is recorded by how it was
  decided. A decision a person answered in the host app is sent as such
  (`via` `ks-mcp:elicitation`), and a denial the calling model confirmed with
  the confirm value is sent as that (`ks-mcp:model-confirm`); a decision
  made in a terminal or the console names no channel. A service that cannot
  record the channel refuses the decision from `ks mcp` with the words and the
  command to decide in a terminal, and nothing is decided. The server's
  instructions no longer tell the model it may confirm an approval: there is
  no confirm value for `ks_agent_approve`, and a test holds that nothing the
  model writes in a tool call approves, on every kind of host. The plugin
  README and `ks skill` say what is recorded and that an app that answers
  MCP questions by itself would answer in the person's name.
- The Claude Code plugin now runs `@keepstateai/cli@0.1.39`.

## v0.1.39 (2026-11-08)
- The Claude Code plugin now runs `@keepstateai/cli@0.1.38`.
- The organization's Agent library, from the terminal. `ks agent share AGENT
  --handle HANDLE --title TITLE --save-now` (or `--checkpoint ID`; one is
  always named, none is chosen for you) adds an agent of the organization and
  publishes its first version. `ks library list` and `show` find and read
  entries (a count that cannot be read says `unavailable`, never 0), `ks
  library connect @handle` lets one of your agents ask it (confirmed by
  naming the handle; the library agent sees the question only, and the
  organization's credit and your budget pay), and `ks agent ask` takes
  `@handle` like any adviser. `ks agent fork @handle` plans a fork and shows
  what it copies and what it does not, then forks exactly that plan by its
  digest. The owner's and admins' verbs are `ks library publish`, `edit`,
  `withdraw`, `review`, `deprecate`, `retire`, `transfer`, `feature`,
  `verify`, `forks on|off`, `usage` and `settings`; `ks library report` and
  `ks advice rate` are everyone's. All of it needs the library switched on
  for your organization.
- Open models via OpenRouter. `ks cruise models` lists each open model
  under every harness that runs it, beside the words "open model via
  OpenRouter", with its price per million tokens on your OpenRouter key
  (`$2.00 in / $10.00 out per million tokens`) and, where KeepState's key
  offers it, its price there; a model not offered yet shows the catalog's
  reason. `ks run --agent --model`, `ks agent run --model`, `ks session
  model` and `ks session runner --model` take any model the catalog offers
  for the harness and refuse a withheld one with the catalog's reason,
  sending nothing. An open model calls an OpenRouter key: one of yours by
  id, or `keepstate` for KeepState's key; `ks session model` takes `--key`
  for it. `ks key default openrouter own|keepstate` and `ks session key set
  --key keepstate --provider openrouter` work, and `--key keepstate` on a
  session running an open model defaults to OpenRouter.

## v0.1.38 (2026-11-06)

- `ks mcp` can be listed and installed as a package. Every tool has a
  title a person reads in the host app (`List agents`, `Approve a permission
  request (asks a person)`), beside the annotations it already had. The npm
  package names its MCP Registry entry (`mcpName`), `server.json` describes
  the server for the MCP Registry (the npm package, stdio, the argument
  `mcp`), and `plugins/keepstate` is a Claude Code plugin that runs it, with
  a marketplace at the repository root: `/plugin marketplace add
  Keepstateai/cli`, then `/plugin install keepstate@keepstate`. Approving a
  permission request still needs a person's answer in the host app.
- `ks agent open`, the `ks` app and `ks agent wait` wait out a restart of
  the service (a deploy) instead of ending: they reconnect by themselves from
  the last event shown, so nothing is shown twice or missed, and say
  "reconnecting" and "reconnected" in one quiet line each. Only a restart
  that lasts longer than 60 s ends the window, with the command that
  reconnects.
- An instruction a money limit paused (the token budget, the spend limit,
  credit) reads as paused, not failed: the app says "Paused at the token
  budget. It carries on when the session resumes." and, when it does, one
  line ("Budget raised, carrying on."), and shows no needs-attention card for
  it. `ks agent queue show` calls it a paused instruction; `ks agent queue
  retry` carries one on that was stopped while a step was still running.

## v0.1.37 (2026-11-04)

- Cruise works in an organization's context (`ks context use ORGANIZATION`).
  A job you start there is the organization's: it runs on the
  organization's own keys, never yours, and is paid from the organization's
  credit within its budgets. `ks cruise approve` binds the organization's
  keys, which it reads through the preflight, so a member who is not an
  owner or admin can approve; `ks cruise run` checks them again and sends
  the job in the context; both say so on their first line. `ks cruise
  status` lists the organization's jobs you may see (yours; an owner or
  admin sees every one), and every read and action on a job by id carries
  the context. The organization's refusals come back in the service's
  words with the command that fixes them: no credit (`ks organization
  buy`), a budget (`ks budget ask`), no organization key (`ks key add`, by
  an owner or admin), a role that reads spend rather than jobs (`ks
  organization spend`). In your personal context nothing changes.

- Cruise drafts work in an organization's context too. `ks cruise draft
  create` makes the organization's draft: its files are kept for the
  organization, it binds the organization's own keys, and approving it
  (`ks cruise draft approve`) starts the organization's job, paid from its
  credit within its budgets. `ks cruise draft list` says its context and
  lists the organization's drafts you may see; every draft and `ks cruise
  tests` command carries the context. In your personal context nothing
  changes.
- A plain `ks run` (a machine with no agent) is refused in an organization's
  context before anything starts, and names the way on: `ks run --agent`,
  which runs as the organization, on its keys and credit. When the service
  refuses something in a context and names a better step than leaving the
  context, that step is the one shown.
- `ks key default` works in an organization's context: it shows which key
  the organization's new sessions use for each provider (its own key or
  KeepState's), and its owners and admins change them; another member is
  told who sets them. It says which context it read, and your personal
  defaults are unchanged.
- `ks agent open NAME` finds an organization's agent in its context (yours,
  and every one for an owner or admin), never a personal one there, and
  `--session SESSION` narrows the name to one session, by its id or name.
- `ks member remove` says what happened to the member's organization Cruise
  jobs: with `--content transfer` how many moved to you, tagged transferred;
  with `--content remove` how many live jobs were cancelled, their records
  kept. The plan it shows first lists them too.

## v0.1.36 (2026-11-04)

- `ks organization spend <organization> --by day` shows the organization's
  spend for each UTC day of the month (`--period YYYY-MM` for an earlier
  one) and the month's total; a day that could not be read says so instead
  of showing $0.

- `ks mcp` can now decide permission requests, only with a person's answer.
  `ks_agent_approve` asks the person in the host app (the agent, the exact
  command or tool, and what approving lets it do; Approve or Deny), and
  only their Approve approves, that request at that revision, once. It
  cannot approve on its own: a host app that cannot ask its person
  approves nothing and says to decide in the KeepState console or with
  `ks agent approve`. `ks_agent_deny` uses the confirm value step, since
  denying only blocks. A request already decided or expired is refused.
  `ks_approval_list` and `ks approval list` take `--agent` for one agent's
  requests. `ks skill` says how.
- `ks session runner <session> --token-budget N` gives a session a new token
  budget, more than it has already used, in any state; with `--resume` a
  session paused at its budget continues where it was saved. The service
  checks the figure against what the session has used and writes it where
  its model calls are measured. A figure at or below what is used, a meter
  that cannot be read, and a free-tier session above the free tier's cap are
  each refused with nothing changed and the next command.
- An agent's error counts in "N need you" only until you have read it.
  `ks agent status <name>` now says the agent's error and its next step,
  and marks the error read on the service, as opening the agent's page in
  the console does; it then leaves the count in `ks agent list --all`, the
  `ks` app and the console on every device. The error is still listed, in
  the same words. A permission request, a question, a sign-in, credit or a
  budget keeps counting until it is resolved, and a new error counts again.
  Nothing is marked read with `--json`.
- You choose how hard the model thinks: `ks session runner <session> --effort
  <level>` changes it from the next instruction, with no pause, and `--effort
  default` goes back to the harness's own level for the model;
  `ks run --agent --effort <level>` and `ks agent run --effort <level>` choose
  it for a new session. The levels are low, medium, high, xhigh (extra high)
  and max; the service accepts only a level the session's model takes and
  says which ones it does. `ks session runner <session>` shows the effort
  chosen, or the default and which level that is, or why the session cannot
  take one, and `ks cruise models` lists each model's levels and its default.
  Higher effort thinks longer: more output tokens per instruction at the same
  per-token price, so it spends more and reaches the session's token budget
  and spend limit sooner; the words say so next to the choice.
- In an organization's context (`ks context use <organization>`), `ks agent
  list --all` lists that organization's agents that are yours and says the
  context on its first line; it no longer lists your personal agents there.
  The lists and creations that have no organization view yet (`ks cruise`
  and `ks cruise status`, `ks cruise draft create` and `list`, `ks adviser
  discover`, `ks agent open` by name alone, `ks key default`, a plain `ks
  run`) are refused in the service's words with `ks context use personal` as
  the next step, instead of showing or making your personal account's.

## v0.1.35 (2026-11-03)

- When `ks agent run` or `ks run --agent` cannot start a session because no
  machine could be made for it, the run says so in the service's words
  ("nothing ran and nothing is billed" where that is so) and gives one way
  on: `ks session delete <id>`, then the same start again. It no longer says
  to run it again, which the same name would have refused.
- `ks file upload <local path>` puts one file or a folder, up to 1 GB, into
  a session's workspace: `--to` names where (default: its own name at the
  workspace root), `--agent` or `--session` names whose. A folder is packed
  under the same upload policy as Cruise (the repository's history,
  dependency and tool folders such as node_modules, credential and secret
  file names, what .gitignore lists, and links are left out, and each is
  said). The bytes go in chunks; an interrupted upload is kept for 24 hours
  and the same command continues it from what arrived. The session's machine
  places the upload only when its whole sha256 matches, so nothing
  half-written is left at the path. An existing path is refused unless
  `--replace` is given, and then it says so. More than 1 GB, a path outside
  the workspace, a secret file name, a session that is not running and a
  full disk are each refused in plain words with the next command. `--tell`
  queues one instruction telling the agent where the files are (or
  `--tell-text` with your words); it is off by default. `--discard` throws an
  unfinished upload away. Uploaded files count toward your storage: saved
  points keep them, as they keep everything in the workspace.
- `ks file upload <folder> --with-git` carries the folder's `.git` too, so a
  project arrives with its history. Without it `.git` is left out as before,
  and the list of what was left out says how to include it. The other skips
  stay (dependency folders, `.env` and other secret file names). `.git`
  counts toward the 1 GB limit like any file, and the size refusal says how
  much of the size it is.
- The `ks herdr` commands' notes say they have run against a real herdr
  0.9.5 and hc 0.1.16, not only a stand-in.
- The herdr plugin is published from this repository: `herdr plugin install
  Keepstateai/cli/herdr` installs the static manifest in `herdr/`, whose
  panes, action and one install step run only `ks` (the install step stops
  the install when `ks` is not on PATH). `herdr/README.md` says what it needs
  and the two herdr settings for toasts and the sidebar's words. A release
  fails when that manifest states another version than its tag. `ks herdr
  install` leaves a plugin herdr installed from GitHub as it is, writing and
  linking nothing; `ks herdr status` says where the plugin came from and its
  version; `ks herdr uninstall` removes the GitHub one with `herdr plugin
  uninstall`, and says which side to update when the two versions differ.
  The test copy of the manifest is renamed so herdr's marketplace lists only
  the published one. The notes of the `ks herdr` commands say they have run
  against herdr 0.9.5 both ways and hc 0.1.18.

## v0.1.34 (2026-11-03)

- Leaving `ks agent open` (q, or the terminal closing) gives the window's
  control back, as the console's Leave window does. The next window opens
  the agent at once: herdr's resume command after its server restarts, an
  `hc run` after `hc kill`, a new tab. Before, it was refused with "another
  window holds control" until the control ran out. Nothing is cancelled and
  the agent keeps working.
- herdr: the sidebar now gets KeepState's words for the state ("Needs you:
  permission", "Needs you: a question", "Done", "Error") as a display label,
  because herdr does not show the message of a status report. herdr shows
  the label in rows that have `state_text` under `[ui.sidebar.agents]`;
  `ks herdr install` says so. The label is cleared when the window is left.
- herdr: an agent opened from the KeepState pane starts its tab in your home
  folder, not in the plugin's folder, so herdr can restore the tab after a
  restart even when the plugin was reinstalled.
- `ks herdr uninstall` says the plugin was not linked when herdr removed
  nothing, instead of "Unlinked".
- The agent's window marks a permission request as waiting for you only
  while it is open; its decision and what came of it are plain lines.

## v0.1.33 (2026-11-03)

- `ks cruise result <job> --wait` follows a Cruise job every 5 seconds until
  it ends, saying each change once, then prints the receipt: the badge
  exactly as served, the checks and provenance, the limitations, the
  changeset, the spend (a figure only where the service gives one, never
  $0 for a missing one) and the pull-request line. Until KeepState opens
  pull requests the receipt says there is none yet and offers `ks cruise
  apply <job>`; a served pull request is shown only as a plain https link.
  A job that fails, waits for review or is cancelled ends the wait with its
  own card and one next command. Ctrl-C ends only the wait.
- Terminal status: `ks agent open`, `ks agent status --watch`, an agent's
  window in the `ks` app and `ks cruise status --watch` write the agent's
  (or the job's) status as OSC 7501, the program status protocol, so a
  terminal or multiplexer that reads it shows whether the agent works,
  needs you (with permission, question or sign-in), is done or failed. The
  status is the service's own record: a session paused for credit reads
  as needing you even when its harness would say otherwise. It is written
  once per change and cleared when the stream ends. `KS_STATUS_OSC=off`
  turns it off and `on` writes it even when stdout is not a terminal; by
  default it is written only to a terminal and never with `--json` or
  `--plain`. `ks doctor` says which applies. Inside a herdr pane the same
  status also goes to herdr through `herdr pane report-agent`, in the
  background and never slowing the stream, with the command that reopens
  the agent; the pane is released when the stream ends.
- `ks agent list --all` lists every agent across your sessions in the
  console's words (Needs you: permission, a question, or sign-in or key;
  Working; Done; Error; Idle), what needs you first, under a "2 need you"
  line, with each one's next step. `--watch` redraws it every 2 seconds and
  writes terminal status for each agent and for all of them together;
  `--notify` adds a notification (OSC 9 and a bell, and a herdr
  notification inside herdr) each time an agent comes to need you or
  fails. The `ks` app's list of agents uses the same words.
- Agents can delegate to child agents. `ks agent run --name NAME
  [--template ROLE] "<task>"` starts a child in a session of its own with a
  first instruction, and says before anything exists that the session is
  metered like any session. `--bg` comes back at once with the child's
  agent, session, parent and status as JSON; without it, the command follows
  the first instruction and, after `--timeout` (180 seconds), prints "still
  running as NAME" and exits 4 while the child keeps working.
  `ks agent send NAME TEXT` queues one more instruction tagged with who sent
  it (`--if idle|working` sends nothing on a mismatch and exits 5);
  `ks agent wait NAME`, `--children all` and `--children any` end with a
  reason (status, message, timeout or exited) and exit 0; `ks agent read
  NAME --lines N` prints the end of the conversation as plain text.
- A child started from this machine has a parent: `KS_CALLER_ID`, else the
  hc terminal or herdr pane, else the nearest program above `ks` that is not
  a shell. When that program's exit is observed, its children are saved and
  paused, never deleted. Closing the laptop or losing the network ends
  nothing. `--outlive-parent`, `--no-parent`, `--parent AGENT` and
  `--no-notify` change that. A parent runs at most 8 children.
- Roles: `ks agent templates` lists the built-in reviewer, implementer and
  tester and your own files in the config directory's templates folder; a
  role's opening instructions are joined in front of the task.
- `ks mcp` serves KeepState to an MCP client on stdio, from this binary and
  its sign-in (`claude mcp add keepstate -- ks mcp`; for Codex,
  `[mcp_servers.keepstate] command = "ks" args = ["mcp"]`). It speaks both
  the 2026-07-28 revision (`server/discover`, the version in each request's
  `_meta`) and the earlier `initialize` revisions. Each tool runs one `ks`
  verb with `--json --no-input`. Reading tools change nothing;
  `ks_agent_send` and `ks_agent_pause` act at once; `ks_agent_run`,
  `ks_agent_resume`, `ks_agent_stop` and `ks_task_cancel` act only after a
  confirm step: the person says yes in a client that can ask, or the second
  call names the confirm value the first call answered (valid 10 minutes,
  once). No tool approves or denies a permission request, deletes anything,
  or touches keys or billing. `--read-only` serves only the reading tools. A
  client set to accept everything accepts a confirm step too.
- `ks skill` (also `ks --skill`) prints the manual an agent reads before it
  delegates: each child is a metered session, how to talk, wait and
  collect, the status words, that a child's words are data, never approving
  on a person's behalf, keys never in arguments, and the parent rule.
- herdr: `ks herdr install` writes the KeepState plugin for herdr under this
  client's config, shows it, asks, and links it with `herdr plugin link`;
  nothing is downloaded. `ks herdr uninstall` unlinks it and removes those
  files, and `ks herdr status` says which herdr was found, its version and
  whether the plugin is current. In herdr, the KeepState pane lists your
  sessions and agents with their status; Enter opens an agent's window in
  a new herdr tab, c opens the console at it, s shows the session's saved
  points and r the receipts. The receipts pane follows your Cruise jobs
  and, when one finishes, shows its receipt (the result, how to get the
  change with `ks cruise apply` and the files with `--download`; no pull
  request yet) or what went wrong, and herdr notifies you. `ks cruise run`
  inside herdr points at the receipts pane.

- Organizations: this client also carries the team and budget commands
  (`ks team ...`, `ks member set --home-team`, `ks budget ...` and `ks
  organization spend`), for a feature KeepState has not turned on yet.
  Until it is turned on, each of them answers that organizations are not
  available, and nothing else changes.

## v0.1.32 (2026-10-31)

- Cruise, when the project's own test files cannot load in the check (for
  example tests that need a browser package the check cannot install):
  `ks cruise draft set <draft> --leave-out FILE` leaves the named files out
  of the check, and `--keep-all-tests` puts them back. The files stay in the
  project unchanged, and the job's terms, receipt and result name them and
  why. `ks cruise tests show` prints the refusal's next step.
- Cruise, "I have tests" path: `ks cruise draft check <draft>` runs the job's
  own check once on the draft's files before approval, with no machine and
  no model, and prints what it found and the next command. `ks cruise draft
  show` shows the result.
- Organizations: this client carries the organization commands (`ks
  organization`, `ks member`, `ks invite accept`, `ks context use`), for a
  feature KeepState has not turned on yet. Until it is turned on, each of
  them answers that organizations are not available, and nothing else
  changes: `ks run`, keys and sessions act on your own account as before.

## v0.1.31 (2026-10-29)

- `ks file download <path>` downloads one file of any size from a session's
  workspace to a new local file (`--out FILE`, or the file's own name in
  this directory). The bytes are checked against the size and sha256 the
  service states before sending them; an interrupted or changed download
  writes nothing, and an existing file is never replaced. A secret file, a
  folder and a session that is not running are refused, and nothing wakes a
  session.

## v0.1.30 (2026-10-29)

- `ks session runner <session>` shows what a session runs (harness, model,
  key) and changes any part of it in one step, in any state: `--harness`,
  `--model`, `--key <key-id|keepstate>` and `--spend-limit DOLLARS`. The
  service checks the change first, then saves the session and starts it
  again only as the change needs (`--resume` starts it once applied). A
  harness switch while an instruction runs waits for it to finish;
  `--stop-running` asks that instruction to stop instead, and `--cancel`
  cancels a change still waiting.
- `ks agent queue retry <name>` runs a failed instruction again first, when
  it failed before doing anything (for example for a missing key); what
  waited behind it runs after. `--finding` is optional.
- Cruise: the client's runner list now also knows Java and Kotlin (Maven),
  C# (dotnet), PHP (PHPUnit) and Ruby under RSpec, and it skips a project's
  own dependency folders (such as vendor or node_modules) when it looks for
  tests. A runner is used only when the service offers it.

## v0.1.29 (2026-10-28)

- Cruise checks in Rust and Ruby: the client reads the service's runner
  list, so a Rust crate with its Cargo.lock selects `cargo test` and a Ruby
  project selects minitest, beside Python, Go and Node as before. `--check`
  and its refusals name the runners the service offers, and `ks cruise init
  --tests` maps `cargo test` and `ruby ...` to them.

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
