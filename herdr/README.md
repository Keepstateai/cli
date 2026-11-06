# KeepState for herdr

Your KeepState sessions, agents, saved points and Cruise receipts in herdr
panes.

```sh
herdr plugin install Keepstateai/cli/herdr
```

## What it runs

The plugin runs only `ks`, the KeepState client, on your machine. Every
command in `herdr-plugin.toml` is `ks` with its arguments, with no shell:

- the **agents** pane, `ks herdr pane agents`: your sessions and agents with
  each one's status. Enter opens an agent in a new herdr tab, `c` the
  console, `s` saved points, `r` receipts;
- the **receipts** pane, `ks herdr pane receipts`: your recent Cruise jobs,
  the running ones followed, and a herdr notification when one finishes;
- the **console** action, `ks herdr action console`: the KeepState console
  in your browser;
- one install step, `ks herdr pane agents --help`, which changes nothing.
  It stops the install when `ks` is not on your PATH.

## What it needs

- `ks` on your PATH. Install it with any one of:

  ```sh
  curl -fsSL https://keepstate.ai/install.sh | sh
  brew install keepstateai/tap/ks
  npm install -g @keepstateai/cli
  ```

- A KeepState account, signed in with `ks login`.
- herdr 0.9.2 or later, on Linux or macOS.

`ks herdr status` shows the herdr it found, where the plugin came from and
its version.

## Two herdr settings

Both go in herdr's `config.toml`.

herdr's toasts are off by default. To see the receipts pane's notices:

```toml
[ui.toast]
delivery = "herdr"
```

herdr's default sidebar rows show an agent's state only as a colour. To see
KeepState's words (Needs you: permission, Done), give a row `state_text`:

```toml
[ui.sidebar.agents]
rows = [["state_icon", "agent"], ["state_text"], ["workspace", "tab"]]
```

## Update and remove

```sh
herdr plugin update keepstate
herdr plugin uninstall keepstate
```

`ks herdr uninstall` removes it too. `ks herdr install` links a copy this
client writes instead, with nothing downloaded; it leaves a plugin that
herdr installed from GitHub as it is.

More: [keepstate.ai/docs/herdr-and-mcp](https://keepstate.ai/docs/herdr-and-mcp).
