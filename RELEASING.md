# Releasing ks

One tag push publishes everything, under a publication attestation. The tag
is ANNOTATED and its message carries the gate's result, bound to the exact
source tree and the checksums the build must reproduce:

```
git tag -a vX.Y.Z -m "ks-publication-gate: status=PASS tree=$(git rev-parse 'HEAD^{tree}') checker=<12 hex> rules=<version> sums=<sha256 of SHA256SUMS or none>"
git push origin vX.Y.Z
```

The gate runs privately on the builder before the tag exists (its rules are
not in this repository); a lightweight tag, a missing line, a status other
than PASS or another tree refuses the release before anything is built, and
a build whose `SHA256SUMS` hashes differently from the attested value refuses
to publish. `.github/workflows/release.yml` then:

0. **gates** (reads the attestation from the tag; refuses otherwise),
1. **tests** (the manifest drift test gates the release),
2. **builds** four platform binaries with `SHA256SUMS` + build-provenance
   attestations and creates the GitHub release,
3. **publishes to npm** via OIDC trusted publishing (no token), with
   `--provenance`, after syncing the shim version to the tag,
4. **bumps the Homebrew tap** formula (if `HOMEBREW_TAP_TOKEN` is set).

A rehearsal of the same workflow (manual dispatch with the attestation line
as an input and an optional injected fault) runs the gate and the build and
publishes nothing; it proves the refusal paths on the real pipeline.
Reproduce the CI bytes locally with `GOTOOLCHAIN=go1.22.12` and the same
`go build -trimpath -ldflags "-s -w -X main.version=vX.Y.Z"` line.

## One-time setup: npm trusted publishing (founder, on npmjs.com)

Trusted publishing lets CI publish with no npm token. It must be linked
once, in the npm package settings UI:

1. Sign in to <https://www.npmjs.com> as the package owner.
2. Go to the package: **<https://www.npmjs.com/package/@keepstateai/cli>** →
   **Settings** → **Trusted Publisher** (or **Publishing access**).
3. Choose **GitHub Actions** and enter exactly:
   - **Organization / user:** `keepstateai`
   - **Repository:** `cli`
   - **Workflow filename:** `release.yml`
   - **Environment:** *(leave blank)*
4. Save. From then on, a `v*` tag push publishes automatically; a run from
   any other repo, branch, or workflow presents a different OIDC claim and
   npm refuses it — nothing publishes.

Until this link exists, the `npm` job in `release.yml` fails (the rest of
the release still succeeds). This is the only human step in the release.

## Once trusted publishing is green

The local `npm login` on the founder's Mac is **no longer needed for
releases** — CI publishes via OIDC. Running `npm logout` on the Mac is
safe and recommended (removes a long-lived credential from a laptop);
it does not affect CI, which authenticates per-run with a short-lived
OIDC token.

## Gotcha: no registry-url on setup-node

Do NOT pass `registry-url` to `actions/setup-node` for the publish job.
It writes an `.npmrc` with `//registry.npmjs.org/:_authToken=${NODE_AUTH_TOKEN}`,
and an unset token then shadows trusted publishing (npm uses the empty
token and gets a 404 instead of exchanging the OIDC id-token). Trusted
publishing needs no authToken.

## Homebrew tap automation (optional)

Set repo secret `HOMEBREW_TAP_TOKEN` (a fine-grained PAT with
`contents: write` on `keepstateai/homebrew-tap`) to have the release bump
the tap formula automatically. Absent the secret, the tap step is skipped
with a warning and the formula is bumped by hand.
