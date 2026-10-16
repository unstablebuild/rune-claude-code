# Rune for Claude Code

Connects [Claude Code](https://www.anthropic.com/claude-code) to the Rune
IDE. Claude Code treats Rune as its IDE: it sees what you select, reviews
its edits with you in Rune, checks Rune's diagnostics after each edit, and
uses Rune's language servers through MCP tools. Its prompt also completes
workspace symbols and resolves `@pkg.Symbol` mentions to the defining file.

Requires Claude Code 2.1.300 or later.

## Install

In Rune's console:

```
pkg install claude-code
```

In Claude Code, started from a Rune terminal:

```
/plugin marketplace add unstablebuild/rune-claude-code
/plugin install rune@unstablebuild
```

See the [Claude Code guide](https://docs.rune.build/agent/claude-code) for
everything the integration does.

## Develop

| Path | What it is |
| --- | --- |
| `cmd/extension_claude_code` | The Rune extension. One runs per workspace; it serves Claude Code's IDE protocol and Rune's tools on a loopback port it advertises in `~/.claude/ide`. |
| `cmd/rune-claude` | `rune-claude mcp`, the stdio MCP server the plugin starts. It finds the workspace that contains Claude Code's working directory and relays to its extension. |
| `plugin/` | The Claude Code plugin: the MCP server entry, a skill, and a hooks module for prompt completion, mentions, Grep references and the diagnostics pane. |
| `.claude-plugin/marketplace.json` | Makes this repository a plugin marketplace. |

The extension, `rune-claude` and the plugin are released together under
one tag. `version.Protocol` and the plugin's `PROTOCOL` change together
when a tool the plugin calls changes incompatibly.

```
make test             # Go tests, with the race detector
make lint             # golangci-lint
make plugin-validate  # claude plugin validate plugin
make plugin-test      # claude plugin test plugin
make verify           # build claude-code.tar.gz and check its contents
```

`make verify TARGET_OS=linux TARGET_ARCH=arm64` packages another target;
release builds have cgo off, so any host can build any target.

To release, set `version.Version` and the plugin's `version` to the new
version, tag the commit `v<version>`, and run `make dist-staging-all`, then
`make dist-prod-all`. Each uploads the four darwin and linux, arm64 and
amd64 packages, skipping any already published. They need the bluectl
configs in `deploy/bluectl/<env>/<os>-<arch>/config`, which are not
committed, and `BLUE_PGP_KEY` and `BLUE_PGP_KEYRING` to sign the upload.

Run Claude Code against the plugin in this checkout with
`claude --plugin-dir plugin`.
