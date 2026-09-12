# Onboarding — getting an agent onto a metiche team

Three pieces, in the order they matter:

1. **The skill** — [`skill/metiche-teamwork/SKILL.md`](../skill/metiche-teamwork/SKILL.md).
   Prose that changes how an agent behaves: declare before acting, heartbeat while working, claim
   narrowly, act on what comes back. Any assistant that reads a rules or instructions file can use
   it as-is.
2. **The MCP server** — the fifteen tools. Nothing to install; it is an HTTP endpoint.
3. **The join code** — your team's, from the board. It is a credential. It never goes in a repo.

> **The backend is not deployed yet.** `mcp.metiche.xyz` does not answer. Everything below
> configures clients correctly for the moment it does; nothing here is a claim that the endpoint
> works today.

## The one-liner

```sh
curl -fsSL https://metiche.xyz/install.sh | sh
```

It prompts for the join code (echo off), or takes it from the environment:

```sh
METICHE_JOIN_CODE=your-code sh install.sh
```

**Never as an argument.** Arguments are visible in `ps` to every user on the machine and land in
your shell history. The script refuses a bare argument for exactly that reason.

See everything it would do, and do none of it:

```sh
sh install.sh --dry-run
```

### What it touches

| path | what |
|---|---|
| `~/.metiche/env` | your join code, mode 0600 |
| `~/.metiche/src` | shallow clone, only if it needs one to install the plugin |
| `~/.cursor/mcp.json` | Cursor's global MCP config |
| `~/.codeium/windsurf/mcp_config.json` | Windsurf's MCP config |
| Claude Code user scope | via `claude plugin install` |
| your shell profile | **only** with `--write-profile`; otherwise it prints the line |

It is POSIX `sh`, idempotent, never uses `sudo`, backs a file up before changing it, skips a file
it cannot merge safely, and writes nothing inside the current directory.

Useful flags: `--dry-run`, `--only claude,cursor,windsurf`, `--url <endpoint>`,
`--marketplace <source>`, `--write-profile`.

## Claude Code, by hand

The plugin bundles the skill and the MCP server, so one install gets both.

```sh
git clone https://github.com/mklfarha/metiche.git
cd metiche
claude plugin marketplace add ./plugin
claude plugin install metiche@metiche --scope user -y
claude plugin details metiche@metiche      # expect: 1 skill, 1 MCP server
```

**Where the marketplace manifest lives:** `plugin/.claude-plugin/marketplace.json`. That makes
`plugin/` both the marketplace root and the plugin (`"source": "./"`), which is why you point
`marketplace add` at `./plugin` and not at the repository root.

`claude plugin marketplace add` also accepts a GitHub shorthand such as `mklfarha/metiche`, but
that form looks for `.claude-plugin/marketplace.json` at the **repository root**, which this repo
does not have. If one is added there later, the shorthand starts working and the directory form
keeps working; `install.sh --marketplace mklfarha/metiche` will use it.

Then put the join code in your environment — the plugin's `.mcp.json` reads
`${METICHE_JOIN_CODE}`, so it is never written into any file in the repository:

```sh
printf 'METICHE_JOIN_CODE=%s\nexport METICHE_JOIN_CODE\n' 'your-code' > ~/.metiche/env
chmod 600 ~/.metiche/env
echo '[ -f "$HOME/.metiche/env" ] && . "$HOME/.metiche/env"' >> ~/.zshrc
```

Restart Claude Code. `/mcp` should list `metiche`.

### Just the MCP server, no plugin

```sh
claude mcp add --transport http --scope user metiche https://mcp.metiche.xyz/v1/mcp \
  --header "Authorization: Bearer $METICHE_JOIN_CODE"
```

Scopes are `local`, `user` and `project`. Note that this expands your join code into a command
line, where it is visible in the process list — which is why `install.sh` prints this command
rather than running it. You also do not get the skill this way, and the skill is most of the
value.

## Cursor

Global config, `~/.cursor/mcp.json`:

```json
{
  "mcpServers": {
    "metiche": {
      "type": "http",
      "url": "https://mcp.metiche.xyz/v1/mcp",
      "headers": { "Authorization": "Bearer your-join-code" }
    }
  }
}
```

Cursor also reads a project-scoped `.cursor/mcp.json`, and `install.sh` deliberately never writes
it: that file lives inside your repository, and a join code in a repository is a leaked
credential one `git add -A` later. Use the global file.

There is no plugin format for the skill. Point Cursor at
`skill/metiche-teamwork/SKILL.md`, or paste it into your project rules.

## Windsurf

`~/.codeium/windsurf/mcp_config.json`, same `mcpServers` shape as above. Same story for the
skill — it is one markdown file; put it wherever Windsurf reads workspace rules.

## Zed, Codex, everything else

Not automated, on purpose: their MCP config formats were not verified when this was written, and
a confidently wrong config file is worse than no config file. Any MCP client needs exactly three
things:

```
transport   streamable http
url         https://mcp.metiche.xyz/v1/mcp
header      Authorization: Bearer <your join code>
```

and the skill, which is one markdown file with no dependencies.

## Dogfooding — metiche pointed at itself

[`.mcp.json`](../.mcp.json) at the repository root registers metiche for agents working **on**
metiche. It carries no credential: the header is `Bearer ${METICHE_JOIN_CODE}`, expanded from your
environment at load time, and the endpoint is `${METICHE_MCP_URL:-https://mcp.metiche.xyz/v1/mcp}`
so you can point at a local server without editing a tracked file.

If `METICHE_JOIN_CODE` is unset, Claude Code warns and leaves the placeholder unexpanded. Visible
failure, not a silent one.

## The rule that matters

**A join code never enters this repository.** Not in `.mcp.json`, not in the plugin, not in an
example, not in a screenshot, not in a test fixture. Every config in this repo references an
environment variable; every config that holds the real value lives in your own home directory.
