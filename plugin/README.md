# The metiche Claude Code plugin

One install gets both halves of the client side:

- **the skill** — `skills/metiche-teamwork/SKILL.md`, which teaches an agent the cadence:
  declare before acting, heartbeat while working, act on what comes back.
- **the MCP server** — `.mcp.json` at the plugin root, which is where the tools come from.

A skill without the server is advice about tools that do not exist. A server without the skill is
fifteen tools an agent calls once and forgets. They ship together on purpose.

## Layout

```
plugin/                                  ← the marketplace root AND the plugin root
  .claude-plugin/
    plugin.json                          the plugin manifest
    marketplace.json                     the marketplace manifest ("source": "./")
  .mcp.json                              the metiche MCP server, auto-loaded
  skills/
    metiche-teamwork/SKILL.md            auto-discovered by folder name
  README.md
```

Both the plugin-root `.mcp.json` and `skills/*/SKILL.md` are discovered automatically, so neither
is declared in `plugin.json`.

## Installing it

From a clone of the metiche repository:

```sh
claude plugin marketplace add ./plugin
claude plugin install metiche@metiche --scope user -y
```

Without a clone, `install.sh` at the repository root does the same thing after making a shallow
clone at `~/.metiche/src`. Scopes are `local`, `user` or `project`; `user` is the sensible one for
a coordination tool you want in every repository.

Verify:

```sh
claude plugin details metiche@metiche
```

It should report one skill and one MCP server.

## The credential

`.mcp.json` carries **no** credential and never will:

```json
"headers": { "Authorization": "Bearer ${METICHE_TOKEN}" }
```

Claude Code expands `${VAR}` and `${VAR:-default}` in `.mcp.json` values, headers included. So the
token lives in your environment and nowhere in this repository. `install.sh` writes it to
`~/.metiche/env` (mode 0600) and prints the one line that loads it from your shell profile.

**A token is not a join code**, and the bearer is always the token. A join code is an *invite*: you
hand it to `join_team` as an argument, once, and the server mints you a token in exchange. Sending
the join code as a bearer gets you a 401 — it is not a credential and the server does not accept it
as one. The installer performs that exchange for you and writes the token it gets back.

`${METICHE_MCP_URL:-https://mcp.metiche.xyz/v1/mcp}` lets you point at a local server without editing
anything: `METICHE_MCP_URL=http://127.0.0.1:8788/mcp`.

If `METICHE_JOIN_CODE` is unset, Claude Code loads the server with the placeholder unexpanded and
warns you. That is the intended failure — visible, not silent.

## Why the skill is copied, not referenced

`skills/metiche-teamwork/SKILL.md` is a **copy** of `skill/metiche-teamwork/SKILL.md` at the
repository root. The canonical file is the one at the repository root; this one is the shipped
artifact.

A symlink would be tidier and is the wrong call: a plugin can be fetched as a subdirectory of a
repository, with sparse checkout, into a directory tree where a link pointing at `../../skill`
resolves to nothing. A plugin has to be self-contained, so the file is duplicated deliberately.

Keep them in sync when you edit the skill:

```sh
cp skill/metiche-teamwork/SKILL.md plugin/skills/metiche-teamwork/SKILL.md
diff -q skill/metiche-teamwork/SKILL.md plugin/skills/metiche-teamwork/SKILL.md
```

## Status

The metiche backend is not deployed. `mcp.metiche.xyz` does not answer yet, so the plugin
installs, the skill loads, and the server sits there unreachable. The client side is deliberately
ready first.
