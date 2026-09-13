# The metiche Claude Code plugin

**Skill only.** The plugin ships `skills/metiche-teamwork/SKILL.md`, which teaches an agent the
cadence: declare before acting, heartbeat while working, act on what comes back.

**The MCP server is not in the plugin.** It is registered by `install.sh` with a per-agent token:

```sh
claude mcp add --transport http --scope user metiche https://mcp.metiche.xyz/v1/mcp \
  --header "Authorization: Bearer <Claude Code's own token>"
```

The installer runs that for you, after joining your team *as Claude Code* and verifying the token it
got back. You never type it.

## Why the server moved out

Until 0.1.x the plugin carried a `.mcp.json` that expanded `${METICHE_TOKEN}` from your environment.
That broke identity in two ways:

- **One token for every client.** A token names one agent. Every Claude Code window, and any other
  client reading the same variable, presented the same token, so they were one agent on the board.
- **Custom headers are dropped.** A second header, `X-Metiche-Client-Key`, was added to say which
  agent was calling. The server log showed that Claude Code does not forward custom headers from a
  plugin's `.mcp.json`; only `Authorization` arrives.

Now the token *is* the agent. `install.sh` makes one join per client (`<machine>-claude`,
`<machine>-codex`, ...), and writes each client's own token into that client's own config. The
`Authorization` header is the only one needed, and it is the one header every MCP client forwards.

`0.2.0` removes `.mcp.json`. The installer runs `claude plugin update`, because `plugin install`
reports an older installed version as already installed and ships nothing.

## Layout

```
plugin/                                  ← the marketplace root AND the plugin root
  .claude-plugin/
    plugin.json                          the plugin manifest (version 0.2.0)
    marketplace.json                     the marketplace manifest ("source": "./", version 0.2.0)
  skills/
    metiche-teamwork/SKILL.md            auto-discovered by folder name
  README.md
```

Keep the two `version` fields equal. Claude Code caches a plugin by version, so a change that does
not bump both is a change nobody receives.

## Installing it

Use the installer; it installs this plugin and registers the server:

```sh
curl -fsSL https://metiche.xyz/install.sh | sh
```

By hand, from a clone of the metiche repository, the skill alone:

```sh
claude plugin marketplace add ./plugin
claude plugin install metiche@metiche --scope user -y
claude plugin details metiche@metiche     # one skill, no MCP server
```

## The credential

Nothing in this plugin carries a credential, and nothing ever will. Each client's token lives in
that client's own home-directory config: `~/.claude.json` for Claude Code, written by
`claude mcp add`.

**A token is not a join code**, and the bearer is always the token. A join code is an *invite*: it
goes to `join_team` as an argument, once, and the server mints a token in exchange. A join code sent
as a bearer gets a 401.

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
