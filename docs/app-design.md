# The ks app: design spec

`ks` with no arguments, at a terminal, is the app. This is how everything in it
looks and reads, so a new screen or message fits without re-deciding it.
Code: `appui.go` (components), `apptheme.go` (colour), `app*.go` (screens).

## Principles

1. **Every screen has an identity.** A state the person is looking at (their
   agents, a paused session, a permission request, a confirmation) is a
   titled card. Its border colour says what kind of screen it is.
2. **State screens live in the live region.** While a card is current it is
   drawn at the bottom of the terminal (the live region) and redrawn in place;
   when it is not, it goes away. The scrollback keeps only what is worth
   keeping: the conversation, outcomes ("✓ Resumed api · 5335fa (resuming)."),
   errors, help, details someone asked for.
3. **One voice per fact.** The client states a state it knows in its own
   words. The service's note is shown when the client does not know the state,
   or as details (d). The service's billing disclosure is always shown before
   the key that resumes, once, under "Billing".
4. **Prose wraps; only cells are cut.** Text is wrapped at word boundaries to
   the terminal (ANSI-aware, `wrapText`/`wrapHang`, by `displayWidth`). Only a
   table cell is cut, with an ellipsis (`cell`). Key hints and status parts are
   dropped whole, least important first (`keyHintsFit`, `joinFit`).
5. **Colour never carries meaning alone.** Every icon has words beside it;
   with colour off (NO_COLOR, TERM=dumb, pipes) the words are identical.
6. **Plain words, no internals.** No record ids, step names, lease or fence
   vocabulary in what a person reads by default; ids live in details, /tasks
   and the command line.

## Components

| Component | Shape | Use |
|---|---|---|
| card | `╭─ Title ─╮`, headline (icon + bold sentence), short sentences, aligned label/value facts, rows, inline error (✗, red), keys at the bottom | every state screen; also kept cards (help, permission details, recovery, while you were away) |
| notice | `  ✓ Sentence.` wrapped under itself | one outcome or event in the conversation |
| error | `  ✗ Couldn't <do X>.` / why / `Next: <a key or /command>` | every failure the app reports |
| key hints | `key what · key what` (key plain, words grey) | bottom of a card, or under the input |
| status line | `● State · what waits for you · who controls · queue · saved · spend` | under the window's conversation; the spinner replaces ● while something runs |
| flash | a hint that goes away after 3 s | things that did nothing (Esc with nothing to interrupt) |

Border colours (tones): accent (claret) = an ordinary screen; warn (amber) =
paused, or waiting on you; bad (red) = failed, or destructive; info (blue) =
a permission request; dim = details. Light terminals get darker swatches of the
same hues (`swatch.light`).

Icons, one meaning each: ✓ done/allowed · ✗ failed/couldn't · ⊘ denied ·
⏸ paused · ⚑ needs you · ● a state or in progress · ○ no agents · ↳ a result ·
! warning or confirm · ❯ selected · ✻ KeepState.

## Screen catalogue

| Screen | Where | Tone | Keys |
|---|---|---|---|
| Welcome | scrollback, once | accent | none |
| Your agents (home): groups Needs you / Agents / Sessions without agents; columns icon, name, session, state, age; scrolls within the terminal height | live | accent | ↑↓ Enter n r q |
| Plain session | live | accent | n, Esc |
| New agent: name (rule up front, inline error) | live + input | accent | Enter, Esc |
| New agent: which key | live | accent | ↑↓ Enter Esc |
| New agent: starting (steps: session, key, machine, agent) | live + spinner | accent | none |
| Session paused / Out of credit / Session stopping / Session starting / Session not running | live | warn (bad for failed) | r (with Billing shown), d, Esc |
| Waiting for a resumed session | status line + spinner | | Esc |
| Needs attention (on open) | live | bad | Esc |
| Controlled elsewhere | live | warn | w, t, Esc |
| Agent window header | scrollback | accent rule | |
| Permission needed | live | info | y n d |
| Permission details | scrollback | info | |
| Confirm: Stop agent | live | warn | y n |
| Confirm: Delete session / Kill session | live | bad | y n d (whole plan) |
| Confirm: any other command that asks for one | live | warn | y n |
| Help, Status of X, While you were away, Needs attention (in the conversation), Whole deletion plan | scrollback | accent / bad / dim | |
| Status line, flash | live | | |

## Copy rules

- Sentence case; a full stop at the end of a sentence; short sentences, one
  idea each. No semicolon chains.
- No capitals for emphasis ("NOT"): bold or colour plus an icon, and the words
  carry it alone.
- Errors: "Couldn't <do X>." then why, then what to do, as a key or a
  /command (inside the app `ks x` reads `/x`).
- A state is named for a person: "Needs your permission", "Out of credit",
  "Paused", "Needs attention", never `waiting_approval` or `parked`. A state
  the client does not know reads "Unknown (<word>)", never the nearest one.
- Nothing destructive happens without y on a card that said what it does;
  nobody retypes a command with `--confirm`.
- Commands run as / commands keep the command line's own output; their
  refusals read in the app's words (`KS_IN_APP`), and the contract lines a
  script reads (`Remote work started:`, `Next: ks ...`) stay on the command
  line.

## Before and after

| Where | Before | After |
|---|---|---|
| Paused session | `agent dev1: the session is parked and was NOT woken. the session is saved and stopped. Opening did not wake it; nothing runs and no runtime is charged until you resume it` / `resuming: resuming starts …` / `r resume the session (billing resumes) · Esc back` | card "Session paused": `⏸ dev1's session is paused. Opening it did not wake it.` / `Its work is saved and nothing is running.` / Agent, Session, Last saved, `Billing  Resuming starts the session's runtime again, …` / `r resume (billing starts) · d details · Esc back` |
| Out of credit | the same parked text, nothing about credit | card "Out of credit": `ops's session is paused: your account ran out of credit.` / `Add credit in the KeepState console, then press r to resume. Adding credit alone does not resume it.` |
| Home, funds-paused agent | `ops … working` | `⏸ ops … Out of credit`, under Needs you |
| Plain session | `697ca7 is a plain session: it has no agents. Its terminal: ks attach 697ca7 (outside the app). For an agent: press n.` | card "Plain session": `○ Session 697ca7 has no agents.` / Terminal `ks attach 697ca7` / `n start an agent in a new session · Esc back` |
| Recovery on open | `agent fixer reads recovery_required: the runner stopped …` / `  Open recovery` | card "Needs attention": `⚑ fixer stopped in the middle of an instruction.` / `It is not ready. …` / Details, To decide |
| Recovery in the conversation | `!! RECOVERY: … This agent is NOT ready and the instruction is NOT finished.` | card "Needs attention": `dev1 stopped in the middle of an instruction. It is not ready, and the instruction did not finish.` |
| New agent, starting | `session session_19de… created with its primary agent; …`, `anthropic calls go through your key vlt_… (…ygAA)`, `record_intent done: …`, `create_guest done: …`, `agent test3 is supervised and waiting …` | card steps `✓ Session created` · `✓ Model calls use your Anthropic key ending ygAA` · `● Starting a machine…` · `✓ Agent started`, then `✓ test3 is ready in session 6e766c.` |
| Invalid name | `"my agent!" is not a name: 1 to 48 ASCII …; nothing was created` (scrollback, back home) | inline in the card: `✗ "my agent!" can't be used as a name: it has a space in it.` (the rule is on the card, the text stays to edit) |
| Message sent | `       queued · task tsk_new1` | `       Queued` |
| Instruction finished | `  ● finished (task_02c9…)` | `  ✓ Finished.` |
| Permission | `the agent wants to: shell - git push … {"cmd":…` cut at the edge | card "Permission needed": Action, Arguments (wrapped), Expires `11:49 (in 8m)`; `y allow once · n deny · d details` |
| Decision | `approved permission request apr_push1: shell - git push origin main; the agent was told, and nothing else was decided` | `✓ Allowed once: git push origin main (shell). The agent was told.` |
| Esc, nothing running | scrollback `Esc: nothing is running, so nothing was interrupted; leave with Ctrl-C twice` | a 3-second note under the input: `● Nothing is running, so there was nothing to interrupt.` |
| /agent stop | `error: stopping an agent is confirmed by naming it: add --confirm test3; …` `(exit 2)` | card "Stop agent": `Stop main?` …; y → `✓ Stop requested for main.` + what is held and what is still charged |
| /kill, /session delete | legacy route, refusal, ~40-line plan, `error: … was NOT killed` `Remote work started: no.` `Next: ks session delete …` `(exit 5)` | card "Delete session": what stops, what is removed, content, `This cannot be undone.`, Cost; `y delete · n keep it · d whole plan`; y → `✓ Deleted session …` + machine and content standing |
| Unknown / command | `/Stop is not a ks command` | `/Stop` runs `/stop`; `/stpo` → `✗ Couldn't run /stpo.` `/stpo is not a ks command. Did you mean /stop?` |
| A command's refusal | `error: …` `Remote work started: no.` `Next: ks …` `(exit 2)` | `✗ <Sentence>.` `Next: /…` (contract lines stay on the command line) |
| ks typed as a message | `"ks agent list" is a ks command, not an instruction: run it here as /agent list. Nothing was sent to main; Enter again sends it to main as written` | `! "ks agent list" is a ks command, so it was not sent to main. Run it here as /agent list, or press Enter again to send it to main as written.` (and the text stays in the input, so Enter again does) |
| /switch | `no agent of yours is named "nosuch"; /home lists them, n there starts one` | `✗ Couldn't switch to nosuch.` / `You have no agent named "nosuch".` / `Next: /home lists your agents, …` |
| Leaving | `left main; it keeps working` | `● Left main. It keeps working.` |
| Status line | `● Ready · queue 0 · not saved yet · 1234 tokens · you control` (cut with … at the edge) | `● Ready · you control · not saved yet · 1,234 tokens`; parts dropped whole when narrow; `(stale: the last known status)` only when the service says so |

## Command-line output changed with this redesign

The website mirrors CLI output; these are the strings that changed outside the
app (all listed in the pull request):

- `ks session delete <session>` with no flags now plans (it used to refuse
  with "a deletion is planned first (--plan) …").
- `ks session delete --plan` and `ks kill` on an agent session print a short
  summary (`Deletion plan for session X. Nothing has been deleted.` + what
  stops, what is removed, content, cost, expiry) instead of the section dump;
  the whole plan is in `--json`. `ks kill` on a session the client knows is an
  agent session goes straight to the plan (no legacy request first). Its
  refusal reads `session X was not deleted: …` (was "was NOT killed").
- `ks session delete --execute … --json` also carries `agents_removed`,
  `tasks_cancelled`, `results_deleted`.
- The whole plan's first line reads `Deletion plan <id> for session X. Nothing
  has been deleted; the plan expires at T.`
