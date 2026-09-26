# Integrated client candidate, 2026-09-27

Third-party review findings R01 and R04 (ks main 002b039,
`docs/upgrade/reviews/2026-09-26-3rd-party-reviewer/`). The reviewed client
changes are bound to ONE integrated artifact: the commit below on branch
`ks-client-round-7`, and the binaries built from it. The artifact is not merged
to main and not tagged or released; publishing a release stays the owner's step.

## The candidate

| | |
|---|---|
| repository | `github.com/keepstateai/cli` (module `github.com/esrygrtc/cli`) |
| branch | `ks-client-round-7` |
| candidate commit | `0e0123ff45e56089f7818b2067d8cb7615beb923` |
| base (main, v0.1.12) | `d4b61e5116ec` |
| integrates | the KS-044 patch (`1512b0b`), the KS-062 patch (`3052e88`), and every change since (66 commits over main) |
| clean clone | fresh `git clone` of the branch, `git checkout 0e0123f`, `git status --porcelain` empty ([clone.txt](2026-09-27-integrated-candidate/clone.txt)) |
| toolchain | go1.27.1 darwin/arm64 |

## Binaries

Built in the clean clone with the release workflow's own flags, and a
candidate version string in place of a tag:

```
GOOS=<os> GOARCH=<arch> CGO_ENABLED=0 go build -trimpath \
  -ldflags "-s -w -X main.version=ks-client-round-7-0e0123f" -o ks-<os>-<arch> .
```

| binary | sha256 |
|---|---|
| ks-linux-arm64 | `33b253c908fb5f38595e0d065745c71ada8132245dc15c19818c8f846a716397` |
| ks-darwin-arm64 | `4e168c88fc329af7893065ab5a88b04d44132ee179eaff33eb8a0b7b54091bfc` |
| ks-darwin-amd64 | `f7238c5c2664b05d48c8a297b9d555281d775a4a0f4e2d3419defd3fcf852b69` |

Every binary's embedded build information reads
`vcs.revision=0e0123ff45e56089f7818b2067d8cb7615beb923` and
`vcs.modified=false`, with `-trimpath=true`
([buildinfo.txt](2026-09-27-integrated-candidate/buildinfo.txt),
[sha256.txt](2026-09-27-integrated-candidate/sha256.txt)). The build is
reproducible: rebuilding the linux/arm64 binary at the earlier candidate
`ef6d4ab` gave the identical sha256 twice. Anyone can rebuild at
`0e0123f` with the command above and compare.

## Tests at the candidate (in the clean clone)

| run | command | result |
|---|---|---|
| vet | `go vet ./...` | clean ([vet.txt](2026-09-27-integrated-candidate/vet.txt)) |
| adviser, focused (R01) | `go test -race -count=5 -run '^(TestAConnectionIsGrantedOnlyAfterBothEndsAreShownAndTheAdviserIsConfirmed\|TestAdviserListShowsLiveAndRevokedAndDisconnectSaysWhatStops\|TestAdviserRefusalsWrongRoleAndCapabilityUnavailable\|TestAdviserDisconnectCarriesAnIdempotencyKey\|TestAdviserDiscoverListsAndNeverChooses)$' -v .` | 25 PASS (5 tests x 5), 0 FAIL, 0 DATA RACE, `ok 15.643s` ([adviser_race_x5.txt](2026-09-27-integrated-candidate/adviser_race_x5.txt)) |
| cancellation, focused | `go test -race -count=1 -run '^(TestCruiseCancelReportsCleanupApartFromAccounting\|TestBulkCancellationIsPreviewedAndConfirmedByRevision\|TestCancelledSuccessorsStayCancelledWhenTheQueueContinues\|TestTaskCancel[A-Za-z]*\|TestTaskShowCarriesTheCancelRecoveryAndReconcileWaitsOutTheInterrupt\|TestAgentWindowCtrlCInterruptsAndQLeaves)$' -v .` | 13 PASS, 0 FAIL, 0 DATA RACE, `ok 8.331s` ([cancel_race.txt](2026-09-27-integrated-candidate/cancel_race.txt)) |
| full suite | `go test -count=1 ./...` | `ok github.com/esrygrtc/cli 213.147s` ([full.txt](2026-09-27-integrated-candidate/full.txt)) |
| full suite, race detector | `go test -race -count=1 ./...` | `ok github.com/esrygrtc/cli 199.221s`, 0 DATA RACE ([full_race.txt](2026-09-27-integrated-candidate/full_race.txt)) |

## R01: what changed

- **The finding.** The KS-062 adviser confirmation test read `c.posts[0]`
  without a lock, while the fake's handler appended to it under its lock on
  the HTTP server's goroutine.
  - Red before: at the commit before the fix, `-race` reported
    `WARNING: DATA RACE`, and
    `TestAConnectionIsGrantedOnlyAfterBothEndsAreShownAndTheAdviserIsConfirmed`
    failed.
  - The fix (`ef6d4ab`) is in the fixture, not the assertion.
    `adviserCtl.postAt` returns a copy of a received grant body taken under
    the fake's lock. Every grant-confirmation check is unchanged, and
    `-race` is kept.
- **The same defect elsewhere.** Running `-race` over the whole suite found
  the same class of defect in more fakes.
  - At first: 26 reports in 15 tests. At the first candidate (`ef6d4ab`) one
    more, intermittent, in `session_delete_test`.
  - The fix (`ef6d4ab`, `0e0123f`) is also in the fixtures. A fake registers
    its mutex with `syncFixture`, and every client run (`auditExec`, `ksIn`)
    passes through each registered mutex before the client starts and after
    it exits (`fixture_sync_test.go`).
  - That ordering is what the process boundary could not give the race
    detector: a handler's record happens before the test's read, and a fault
    the test switches on happens before the next request.
  - No assertion was weakened.

## Not done here, by design

- Merging to main, tagging, releasing and Homebrew. The candidate commit and
  these hashes are what a release would publish; publishing is the owner's
  step.
- Signing the binaries. The release workflow signs at publication.
