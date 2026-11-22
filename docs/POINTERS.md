# Pointer map for the client

How to use: read this file first, before any code. Find your flow, grep the
named symbol in the named file, and read only that region of the file. Do not
read whole files (several are thousands of lines). Anchors are a file path and
a symbol name, never a line number, so they survive edits. If you change a flow,
move a symbol or add a flow, update this map in the same pull request.
`go test -run TestPointerAnchorsExist .` checks that every anchor still exists;
CI runs it with the rest of the tests.

Format: a file in backticks, then the symbols in that file in backticks. Paths
are from the repository root. Every anchor is a whole word that must occur in
its file.

## The command tree and help

- The registry of verbs and the dispatcher: `main.go` `registry`, `main`, `groupUsage`.
- The parser, flags, usage errors and "did you mean": `command.go` `Command`, `Invocation`, `lookup`, `parse`, `UsageError`, `suggestFlag`, `commandHelp`.
- Top-level and per-verb help text: `help.go` `registryUsage`.
- The machine-readable manifest the docs render from: `commands.json`; the test that holds it to the source: `manifest_test.go`. The global flags (`--json`, `--plain`, `--no-color`, `--quiet`, `--no-input`, `--yes`) are `output.go` `globalFlags`.
- Shell completion: `completion.go`.

## Output and errors (the one shape)

- Global flags read once, JSON envelope, progress on stderr: `output.go` `applyGlobals`, `emit`, `emitLine`, `progress`, `outputSettings`.
- The one error shape, the exit table and `fail`: `output.go` `cliError`, `classify`, `fail`, `hostedErr`, `sanitize`. In plain mode `fail` prints the contract lines the app and scripts read (`error:`, `operation:`, `Remote work started:`, `Next:`); on a terminal with colour on it calls `renderError`.
- The component kit for one-shot verbs (title, facts, table, next, steps, progress bar, confirm, error block): `ui.go` `cmdUI`, `ui`, `Title`, `Facts`, `Table`, `Next`, `Success`, `Warn`, `ErrorBlock`, `renderError`, `Steps`, `Progress`, `Confirm`, `ConfirmName`, `phaseWords`, `kitBytes`, `kitDuration`.
- Colour and theme decision per stream (NO_COLOR, `--no-color`, CI, FORCE_COLOR, TERM=dumb, `--json`): `apptheme.go` `detectTheme`, `enableAppTheme`, `palAccent`.
- The app's components the kit reuses (wrapping, cut cells, cards, notices): `appui.go` `wrapHang`, `cell`, `card`, `notice`, `errorLines`, `joinFit`, `spinFrames`. Width helpers: `termwidth.go` `displayWidth`, `fitWidth`; the terminal's columns: `session.go` `termColumns`.
- The design rules in prose: `docs/app-design.md`.

## The app (`ks` with no verb)

- Entry and loop: `app.go` `hostedApp`. Screen and input: `appterm.go` `newScreen`. Slash commands run as child processes with `KS_IN_APP=1`: `appslash.go`; the confirm hand-off: `appconfirm.go`; new session flow: `appnew.go`.
- Child-process failures in the app's words: `output.go` `appChild`, `failInApp`, `appConfirmMarker`.

## Login and tokens

- Device-flow sign-in, token file: `login.go` `runLogin`, `storeToken`, `whoamiAccount`. Credentials for a verb: `hosted.go` `hostedCreds`, `hostedToken`, `hostedTokenQuiet`.
- Sign-out, doctor, uninstall: `doctor.go` `runDoctor`.

## Run and session

- `ks run`, checkpoint, wake, meter: `hosted.go` `hostedRun`, `hostedCheckpoint`, `hostedWake`, `hostedMeter`. The agent start path and its phases: `run_agent.go` `hostedRunAgent`, `phaseDone`.
- Session list and show, the inventory table: `session.go` `renderInventory`; fork and restore: `fork_restore.go`.
- Upload of a folder or file with progress: `file_upload.go` `progressBytes`, `stderrIsTerminal`.

## Agent

- Resolving a session and an agent: `agent.go` `agentSession`, `resolveAgent`, `fetchAgents`. Window: `agent_view.go`; send and tell: `agent_send.go`; stop: `agent_stop.go`; queue and tasks: `tasks.go`; recovery: `recovery.go`; approvals: `approvals.go`.

## Cruise and cruise auto

- Verbs: `cruise.go` `cruiseInit`, `cruiseApprove`, `cruiseRun`, `cruiseStatus`, `cruiseUsage`. Drafts: `cruise_drafts.go`; automatic cruise: `cruise_auto.go` `hostedCruiseAutoBrief`; watching a job: `cruise_watch.go`; receipts: `cruise_receipt.go`, `cruise_verify.go`; the runner list vendored from the service: `runners_list.go`.

## Import

- The verb rows and flags (`--latest`, `--session`, `--with-files`, `--no-files`, `--brief`, `--plan`, `--execute`, `--out`): `import_commands.go` `importFlags`, `importRow`, `importListRow`. The tool's own entry, one function for the three modes: `import_run.go` `importTool`.
- The three modes: no `--plan` and no `--execute` at a terminal is the guided flow, `import_guided.go` `importGuided`; with `--yes`, or with no terminal or `--no-input`, it is `import_run.go` `importBare` (plans, shows, and without `--yes` stops with exit 2 and a Next line); `--plan` is `importShowPlan`; `--execute DIGEST` re-reads the file and refuses a changed digest (`plan_changed`); `--json` without `--yes` is the plan's document. The bare and guided flows read the file once and execute from that copy.
- The guided steps: the list and the picker `import_guided.go` `guidedRows`, `igChoose` (arrows, a typed number, Esc; a plain numbered question when raw keys are unavailable); the files question `guidedFilesProbe` (not asked with `--yes`, which takes the conversation only; end of input in the numbered fallback is a cancel); the review menu and the big-conversation choices `igBigChoices`, `igBigChoice`, `compactInstruction` (the paste text for Claude Code and the Codex variant; Cursor has no compact; the place for a later "compact on KeepState" choice is a comment in `igBigChoices`).
- The screens, on the shared kit (`ui.go`): `import_screens.go` `printImportPlan`, `importPlanFacts`, `estimateWords`; the done card is the end of `import_run.go` `importExecute`.
- Progress: `import_progress.go` `importBar`, `newImportBar`, `importSteps`. Cleaning counts bytes read out of the file (`import.go` `prepareImport`, with `import_sanitize.go` `readImportLines`); uploading counts bytes the service acknowledged (`file_upload.go` `uploader`, `send`, field `ackBar`); placing is a step with a spinner. `--json` and `--quiet` draw nothing. A test keeps what a bar was told in the variables importCleanSeen (`import.go`) and importBarSeen (`import_run.go`).
- Reading and cleaning transcripts: `import_sanitize.go` `sanitizeTranscript`, `importLimit` (120 MB, what a session can resume; KS_IMPORT_MAX_BYTES can only lower it), `sanitizeTranscriptTrim` and `trimOpts` (over the limit, the cleaning is run again from the last compaction on: for Claude Code the boundary record and its summary open the chain, for Codex the session_meta and the compacted record with its replacement history; `codexHistoryItemOK` holds that history to the rules of any record; a trim is made only when the last compaction's summary can travel: for Claude Code its summary record follows the boundary, for Codex the compacted record has a readable message and a replacement history, which real Codex sessions do not (their summary is an encrypted item, so they are not trimmed and the usual choices are shown with the reason); `trimTranscript` returns the error of the second reading instead of falling back); also `import_redact.go`, `import_paths.go`, `import_summary.go`, `import_cursor.go`. The over-limit refusal is in `import.go` `prepareImport`; the Next line it names is built by `import_run.go` `importSessionCommand`.
- The first-read estimate: `import_estimate.go` `tokenEstimator` (the method is in the file's header: the text after the last compaction boundary, three bytes a token, six a message, 15,000 for the harness's own prompt); it is declared as est_tokens in `import.go` `importDeclaration`, priced by the service, and shown by `import_screens.go` `estimateWords`. The client holds no price.
- A service that does not know est_tokens yet (deploy order: the service first, then this client): `import_run.go` `serverImportPlan` asks again once without the new fields (`retryWithoutNewFields`: est_tokens, est_compacted, trimmed, untrimmed_bytes; `unknownFieldRefusal` recognises the refusal); the declaration is changed in place, so the apply carries the planned one and no estimate is shown.
- The apply and the long waits: `import_run.go` `importExecute` (Ctrl-C during the uploads stops at a chunk and says what was kept; Ctrl-C while placing, in the final check or the apply, stops at once with exit 130 and says the placement may still finish on KeepState, the same command run again shows it); `hosted.go` `longClient`, `hostedCallWith` (it takes the signal context) (the apply call waits up to 55 minutes; a cut line is run again, and placing is idempotent).
- The MCP tool `ks_import_session` (`import_mcp.go` `importTargetArgv`, `importInspect`): at v0.1.46 it plans with `ks import TOOL --plan --json --no-input`, shows the plan's lines (the estimate sentence included), and only on a confirm value bound to the plan's digest runs `--execute DIGEST`. It never uses the guided flow and draws no progress.
- Tests: `import_run_test.go` (the fake service `importCtl`), `import_flow_test.go`, `import_progress_test.go` (the sparse-file matrix up to 1 GB, resume, Ctrl-C), `import_guided_test.go` (the pty flows under script(1)), `import_estimate_test.go`.

## MCP

- The stdio server: `mcp.go` `runMCP`. Tool definitions and handlers: `mcp_tools.go`.

## Update and release

- Self-update with checksum and provenance checks: `update.go` `runUpdate`. How a release is cut: `RELEASING.md`; workflows: `.github/workflows/release.yml`, `.github/workflows/regressions.yml`, `.github/workflows/vendored-drift.yml`.

## Lints and goldens

- The text rules (no em dash, no tracker ids, no capitals for emphasis, one `Next:`) over source strings and human goldens, with a baseline that only shrinks: `textlint_test.go` `textViolations`, `TestSourceStringsObeyTheTextRules`; the baseline is `testdata/text-lint-baseline.txt` (`KS_UPDATE_GOLDEN=1` rewrites it).
- Kit goldens at 80, 60 and 40 columns, colour and plain, and the checks that run over them (width, text rules, every `Next:` parses): `ui_test.go` `TestKitGoldens`, `TestHumanGoldensObeyTheTextRules`, `nextParses`; files in `testdata/golden/ui`.
- Stream contract (plain error bytes, `--json` bytes unchanged under any colour setting): `ui_test.go` `TestPlainErrorBytesAndJSONBytesIgnoreColour`; a real terminal: `ui_pty_test.go`.
- Output goldens of the verbs (JSON and human): `output_test.go` `TestGoldenOutputs`, `goldenServer`; files in `testdata/golden`.
- Anchor check for this map: `pointers_test.go` `TestPointerAnchorsExist`.
- The publication scan (a workstation home path or a private tracker id anywhere in the repository fails CI, the same two rules the release gate runs; build such a string in pieces, or use /home/you in a comment; only the fictional homes alice and bob are allowed): `publication_scan_test.go` `homeHits`, `TestRepositoryHasNoWorkstationPathOrTrackerID`, `TestPublicationScanRules`.
