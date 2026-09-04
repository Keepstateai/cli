# Releasing ks

One tag push publishes everything. `git tag vX.Y.Z && git push origin vX.Y.Z`
runs `.github/workflows/release.yml`, which:

1. **tests** (the manifest drift test gates the release),
2. **builds** four platform binaries with `SHA256SUMS` + build-provenance
   attestations and creates the GitHub release,
3. **publishes to npm** via OIDC trusted publishing (no token), with
   `--provenance`, after syncing the shim version to the tag,
4. **bumps the Homebrew tap** formula (if `HOMEBREW_TAP_TOKEN` is set).

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
