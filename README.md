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
brew install esrygrtc/tap/ks
```

```sh
npm install -g @keepstate/cli
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

## Uninstall (one line)

```sh
rm -f $(command -v ks) && rm -rf ~/.config/keepstate
```

MIT licensed.
