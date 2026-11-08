# KeepState for Claude Code

This plugin connects Claude Code to KeepState through `ks mcp`, the MCP
server built into the KeepState client. From a Claude Code conversation you
can list your KeepState sessions and agents, read what an agent wrote, wait
for it, send it one more instruction, pause it, and start, resume or stop a
child agent.

## Approving needs a person

The approve tool, `ks_agent_approve`, cannot approve anything by itself, and
neither can Claude. It asks you through MCP elicitation: Claude Code shows
you the agent, the exact command or tool it asked for and what approving
lets it do, with Approve and Deny. Only your Approve approves, that one
request, once. A host app without elicitation approves nothing; you can then
decide in the KeepState console or with `ks agent approve` in a terminal.
There is no value Claude can pass to approve, and nothing it writes in a
tool call approves.

The decision is recorded as made by your account, through `ks mcp`, by your
answer to the host app's question. That relies on the host app showing the
question to you: an app or setting that answers MCP questions by itself (a
hook or a script, for example) would answer it in your name, so do not set
one up for KeepState. A denial Claude confirmed with a confirm value, not you,
is recorded as that.

Starting, resuming or stopping an agent, cancelling an instruction and
denying a request each answer a plan first and act only on a second call
with the plan's confirm value; ask Claude to show you the plan before it
makes that call. The server never deletes anything and never touches keys,
billing or organizations.

## What it runs and where it connects

- Claude Code starts the server with `npx -y @keepstateai/cli@0.1.40 mcp`.
  npm installs that exact version of the package from the public npm
  registry. Its install step downloads the latest `ks` release binary for
  your platform from the GitHub releases of Keepstateai/cli and checks its
  SHA-256 against the release's SHA256SUMS; on a mismatch nothing is
  installed.
- The server talks only to the KeepState service at `ctl.keepstate.ai`,
  signed in with the device token that `ks login` stored. It reads no other
  credential from your machine.
- Sessions and model use are metered by KeepState, as they are from the
  console or the terminal.

## Before you start

Sign in once:

```
npx -y @keepstateai/cli@0.1.40 login
```

If `ks` is already installed (Homebrew, the install script or npm), `ks
login` does the same. Then add the plugin:

```
/plugin marketplace add Keepstateai/cli
/plugin install keepstate@keepstate
```

Documentation: <https://keepstate.ai/docs/herdr-and-mcp>. Support:
<https://keepstate.ai/support>.

## License

MIT, as in [LICENSE](LICENSE).
