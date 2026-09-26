# Release candidate v0.1.13

This file records the client release candidate: the commit, the binaries
built from it, the tests run at it, and the public-export gate's result. It
binds the reviewed client changes to one integrated artifact, as the
third-party review asked (finding R04).

The candidate is not merged, tagged or released. Publishing is the owner's
step. That tag must be made on the candidate commit below, not on the
commit that adds this file.

## Candidate

| | |
|---|---|
| repository | `github.com/keepstateai/cli` (module `github.com/esrygrtc/cli`) |
| branch | `ks-client-round-7` |
| candidate commit | `6137f2fc4e792016b90118b94f814c939c2c2c90` |
| tree | `e873bf6f7f4b4a0d9174654fbbf53de71a7da4b1` |
| base (main, v0.1.12) | `d4b61e5116ec` |
| toolchain | go1.22.12 (the release workflow's pin) |
| clean clone | a fresh clone of the branch checked out at the candidate; `git status --porcelain` was empty before and after the build and every test run |

## Command statuses, from the live service

`commands.json` statuses were re-derived from production on 2026-09-26
(22:35 UTC). Production runs ctl `4f9b4ad`.

- The public capability registry (`GET https://ctl.keepstate.ai/api/v2/capabilities`)
  reported registry `2026-09-20` and build `8a22c753f8e4d8ab`.
  - `agent.workspace`, `advisers.advice`, `session.migration` and
    `cruise.proposals` read `unavailable`.
  - So every agent, adviser, advice, session-migration and cruise-proposal
    verb stays `planned`, including those whose own row reads available.
    An agent session cannot be started while `agent.workspace` is
    unavailable.
- `GET /api/jobs/{id}/status` and `GET /api/jobs/{id}/changeset` are in
  ctl `4f9b4ad`'s route table (`ctl/web.go`). One unauthenticated GET to
  each answered `401`, not `404`, so both are served.
  - `cruise review`, `cruise result` and `cruise apply` are therefore
    `available`.
  - `cruise status --watch`, and the review-first `cruise resume` and
    `cruise cancel`, work against production.

## Binaries

Built in the clean clone at the candidate:

```
GOTOOLCHAIN=go1.22.12 GOOS=<os> GOARCH=<arch> CGO_ENABLED=0 go build -trimpath \
  -ldflags "-s -w -X main.version=v0.1.13" -o dist/ks-<os>-<arch> .
cd dist && sha256sum ks-* > SHA256SUMS
```

```
3b5870544d3158582eddd1dcb9c5800768f94ec826da408d900065a03ad74907  ks-darwin-amd64
853944931a72e1929c8ec921d866f7321064652a9ea5cdef410707bf46c51569  ks-darwin-arm64
ec970b0d3e8a8ea70bf1da4f47f2f1659b7392142df351eccc7b39d24c5dabb7  ks-linux-amd64
c2f628fd34fcae07d054bd643faddcec7eae3f1f26fb357cbb4f9d2e8e9098af  ks-linux-arm64
```

The sha256 of `SHA256SUMS` is
`2259bbea37055c53ce39fe3c8f82b1ab1b2b4b31e5ce2968f9b402d9347f3863`.

Every binary embeds `go1.22.12`,
`vcs.revision=6137f2fc4e792016b90118b94f814c939c2c2c90` and
`vcs.modified=false`, and `ks version` prints `ks v0.1.13`. The release
workflow builds the same four targets with the same flags, so CI must
reproduce this `SHA256SUMS`.

## Tests at the candidate (clean clone, go1.22.12)

| run | command | result |
|---|---|---|
| vet | `go vet ./...` | clean |
| adviser, focused | `go test -race -count=5 -run '^(TestAConnectionIsGrantedOnlyAfterBothEndsAreShownAndTheAdviserIsConfirmed\|TestAdviserListShowsLiveAndRevokedAndDisconnectSaysWhatStops\|TestAdviserRefusalsWrongRoleAndCapabilityUnavailable\|TestAdviserDisconnectCarriesAnIdempotencyKey\|TestAdviserDiscoverListsAndNeverChooses)$' -v .` | 25 PASS, 0 FAIL, 0 DATA RACE; `ok 16.128s` |
| cancellation, focused | `go test -race -count=1 -run '^(TestCruiseCancelReportsCleanupApartFromAccounting\|TestBulkCancellationIsPreviewedAndConfirmedByRevision\|TestCancelledSuccessorsStayCancelledWhenTheQueueContinues\|TestTaskCancel[A-Za-z]*\|TestTaskShowCarriesTheCancelRecoveryAndReconcileWaitsOutTheInterrupt\|TestAgentWindowCtrlCInterruptsAndQLeaves)$' -v .` | 13 PASS, 0 FAIL, 0 DATA RACE; `ok 8.298s` |
| full suite | `go test -count=1 ./...` | `ok github.com/esrygrtc/cli 181.387s` |
| full suite, race detector | `go test -race -count=1 ./...` | `ok github.com/esrygrtc/cli 192.551s`, 0 DATA RACE |

## Public-export gate

`scripts/check-public-export.py --client <clean clone> --release-dir <clean clone>/dist`
was run from the product repository. The checker's sha256 is
`c8b8c1b6662c736d2edfc0d38a87b09d9b0bc675025ba2c0db80b8f0895d6b07`, with
rules `2026-09-20.2`. Its output:

```
check-public-export: PASS: every required check passed (client.repository, client.working_tree, client.allowlist, client.names, client.text_scan, client.npm_pack, client.workflows, client.release_assets)
```

The attestation line for the tag:

```
ks-publication-gate: status=PASS tree=e873bf6f7f4b4a0d9174654fbbf53de71a7da4b1 checker=c8b8c1b6662c rules=2026-09-20.2 sums=2259bbea37055c53ce39fe3c8f82b1ab1b2b4b31e5ce2968f9b402d9347f3863
```

The first release-candidate commit (`ef9e206`) did not pass the gate.
- `client.text_scan`: private backlog ids in Go comments, and one in the
  vendored vectors.
- `client.allowlist`: paths outside the allowlist (the vendored fixture
  builders, the skip-dir record, the drift workflow's name, the changelog,
  and the earlier `docs/review/` evidence).

`6137f2f` fixes both in this repository; the gate was not changed.

## Binding to the reviewed candidate (R04)

The earlier integrated candidate is `0e0123f`. Its changes were reviewed,
and the R01 race fixes were tested at it. It was recorded with binaries
`33b253c9…` (linux/arm64), `4e168c88…` (darwin/arm64) and `f7238c5c…`
(darwin/amd64), built with go1.27.1 and `main.version=ks-client-round-7-0e0123f`.

Between `0e0123f` and this candidate (`git diff --stat 0e0123f 6137f2f`):

- **Version and manifest metadata.** The version comes from `-X main.version`,
  and `commands.json` changes only the `status` and `note` fields of the
  cruise rows.
- **Release notes.** The new `docs/changelog.md`.
- **Gate compliance, not behaviour:**
  - comment-only edits in `agent.go`, `agent_view.go`, `attach_client.go`,
    `cruise.go`, `legacy_refusals.go`, `parked.go`, `session.go` and
    `tasks.go`, where private ids became plain words;
  - file moves (`internal/c12` to `testdata/c12/builders`, the drift
    workflow's name);
  - the vendored vectors' comment text, under a declared scrub;
  - test-side path and guard updates.
- **One non-comment production change.** `skipdirs.go` reads the verifier's
  SKIP_DIRS from a Go literal instead of an embedded JSON file. The list is
  the same, and a test keeps it equal to the vendored record:

```
-import (
-	_ "embed"
-	"encoding/json"
-)
+var verifierSkipDirList = []string{".claude", ".git", ".hg", ".keepstate", ".mypy_cache", ".nox", ".pytest_cache", ".ruff_cache", ".svn", ".tox", ".venv", "__pycache__", "build", "dist", "node_modules", "venv"}
-var verifierSkipDirsJSON []byte
-	var doc struct {
-		Dirs []string `json:"dirs"`
-	}
-	if err := json.Unmarshal(verifierSkipDirsJSON, &doc); err != nil || len(doc.Dirs) == 0 {
-		panic("verifier_skip_dirs.json is unreadable: the upload policy cannot be built")
-	}
-	for _, d := range doc.Dirs {
+	for _, d := range verifierSkipDirList {
```

Every other line of production Go source is identical to `0e0123f`
(`git diff -M 0e0123f 6137f2f -- '*.go' ':!*_test.go'` shows only comment
lines apart from the above). Every test that passed at `0e0123f` passes at
`6137f2f`, including the full suite under the race detector.
