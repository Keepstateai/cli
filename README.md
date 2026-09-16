# ks — the KeepState CLI

Durable agent sessions: run an AI agent in a microVM, checkpoint it,
kill it, wake it — and it resumes mid-sentence, files, memory, and
running processes intact.

This is the thin client. Every verb terminates at the KeepState
control plane; the client is a key, the building is
[keepstate.ai](https://keepstate.ai).

## Install (any one of three)

```sh
curl -fsSL https://keepstate.ai/install | sh
```

```sh
brew install keepstateai/tap/ks
```

```sh
npm install -g @keepstateai/cli
```

Every path downloads the same public-CI-built binary and verifies its
SHA256 against the release's `SHA256SUMS` before installing. Releases
carry GitHub build-provenance attestations.

## Three commands to a session

```sh
ks login          # browser device flow
ks run            # a durable session on the fleet
ks checkpoint <id> --stop && ks wake <id>   # the plug-pull, survived
```

`ks doctor` checks connectivity, token, and version. `ks update`
self-updates (checksum-verified; a mismatch is refused).

## Cruise

A Cruise job is a goal, a check and a ladder of models. The fleet runs one
attempt per rung from an exact save point, verifies the candidate in a
verifier the agent cannot reach, and accepts only when the check passes.
A failed rung escalates to the next one; after the last rung the job waits
in your review queue. Nothing is accepted on a model's word.

```sh
ks cruise init --goal "make the inventory tests pass"   # drafts .keepstate/cruise.json
ks cruise approve                                       # locks it: .keepstate/cruise.lock
ks cruise run                                           # uploads the workspace, starts the job
ks cruise status                                        # rung, attempts, save points, spend, verdict
ks cruise artifact <job>                                # the accepted tarball, sha256-verified
```

`init` reads the repository: the check is the test command it finds
(`pytest`, `npm test`, `go test`) or the one you name with `--tests`. A
goal with no check is not a job; `init` stops and says so. It makes no
request and never calls a model. `approve` records the manifest's digest;
`run` refuses a draft that is not approved, a test or workspace changed
since approve, and a workspace over 50 MiB (`.git` and `.keepstate`
excluded). Defaults: 1800 s per attempt and a $2 spend ceiling
(`--spend`); the ladder comes from the model table (`ks cruise models`,
`--ladder family:model,...`). `ks cruise logs`, `cancel` and `resume`
complete the set. Every `--help` makes no request.

## Uninstall (one line)

```sh
rm -f $(command -v ks) && rm -rf ~/.config/keepstate
```

MIT licensed.
