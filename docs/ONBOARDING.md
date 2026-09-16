# Onboarding — getting an agent onto a metiche team

Three pieces, in the order they matter:

1. **The skill** — [`skill/metiche-teamwork/SKILL.md`](../skill/metiche-teamwork/SKILL.md).
   Prose that changes how an agent behaves: declare before acting, heartbeat while working, claim
   narrowly (the specific files, relative to the git root; a repo-wide `**` is recorded at low
   severity and warns nobody), act on what comes back. Any assistant that reads a rules or instructions file can use
   it as-is. An agent that spawns subagents briefs each one to run its own session; see the skill's
   "When you delegate to subagents".
2. **The MCP server** — its tools, recording decisions and judging plans against them among them.
   Nothing to install; it is an HTTP endpoint.
3. **The token** — minted by the server when you join. It is what every client sends as its
   bearer. It never goes in a repo.

### A join code is not a token

They are different things and the difference is the whole of this page.

| | what it is | where it goes |
|---|---|---|
| **join code** | an *invite*. A shared team secret you hand to a teammate. | an **argument** to the `join_team` tool, once |
| **token** | a *credential*, minted by the server when you redeem an invite. Identifies the **person** — not one agent, not one team. | the `Authorization: Bearer` header on **every** call |

A client configured with the join code as its bearer is rejected on its very first request:

```
initialize with a join code as the bearer -> 401
{"error":"unauthorized","detail":"that metiche token is not valid; ..."}
```

and it never gets far enough to call `join_team` and fix itself. So `install.sh` performs the join
itself, once, and configures your clients with the token that comes back.

One join per machine is right. Each agent later distinguishes itself with its own `client_key` on
`start_session`, and the same token works for every team the account joins.

> **`mcp.metiche.xyz` is live.** The installer joins over the network, so it needs the endpoint
> to answer — which it now does. If you are working against a local server instead, point it
> there with `--url`. Either way, a join that fails writes nothing at all.

## The one-liner

```sh
curl -fsSL https://metiche.xyz/install.sh | sh
```

It asks whether you are joining a team or creating one, and never guesses. Non-interactively,
one environment variable picks:

```sh
METICHE_JOIN_CODE=your-code sh install.sh          # join a team that exists
METICHE_TEAM_NAME="Payments squad" sh install.sh   # create one, and print its
                                                   # join code for your teammates
METICHE_TOKEN=your-token sh install.sh             # already joined elsewhere:
                                                   # skip the join, just configure
```

**Never as an argument.** Arguments are visible in `ps` to every user on the machine and land in
your shell history. The script refuses a bare argument for exactly that reason, and for the same
reason it hands the credential to `curl` through a config document on stdin rather than on
`curl`'s own command line.

### How a team is created

There is no signup and no web form. `create_team` makes the team and joins you to it in one call,
and returns three things: your **token**, the team **slug**, and the team's first **join code** —
which the installer prints in a box, because a join code nobody can see is a team nobody can be
invited to. Send it to your teammates the way you would send a password; they run the one-liner
with `METICHE_JOIN_CODE=` set to it. That first code has no use limit and no expiry, so for each
new teammate a fresh invite is the better thing to send (see "Invite a teammate" below).

### What it does before it writes anything

1. `join_team` (or `create_team`) against the endpoint. The server mints the token.
2. **A second, fresh connection carrying that token from the first byte**, and one `list_teams`
   call on it — the state a configured assistant actually starts in, which is not the anonymous
   state the token was minted in. `list_teams` rather than `health` because `health` answers
   without a token at all and would prove nothing.
3. Only if that succeeds: `~/.metiche/env`, then every client config.

If any of it fails, **nothing** is written — no env file, no client config, no partial state — and
it says so. A half-configured machine is worse than an unconfigured one, because you believe it is
done.

Re-running is a no-op. `client_key` defaults to something derived from the hostname so it is
stable, and a token already in `~/.metiche/env` is *sent* with the join — without it the server
would mint a second account, and with it a second member and a second agent on the board.

`jq` is required for the join step, and only for it. The response is a JSON document nested inside
a JSON string inside a JSON-RPC envelope, holding a token next to a join code and an account key;
a regex that picks the wrong one of those does not fail, it writes the wrong secret everywhere. If
`jq` is missing the installer refuses that step and tells you to install it or to pass
`METICHE_TOKEN` from a machine that has it.

See everything it would do, and do none of it:

```sh
sh install.sh --dry-run
```

### What it touches

| path | what |
|---|---|
| `~/.metiche/env` | your metiche **token**, mode 0600 |
| `~/.metiche/src` | shallow clone, only if it needs one to install the plugin |
| `~/.cursor/mcp.json` | Cursor's global MCP config |
| `~/.codeium/windsurf/mcp_config.json` | Windsurf's MCP config |
| Claude Code user scope | via `claude plugin install` |
| your shell profile | **only** with `--write-profile`; otherwise it prints the line |

It is POSIX `sh`, idempotent, never uses `sudo`, backs a file up before changing it, skips a file
it cannot merge safely, and writes nothing inside the current directory.

Useful flags: `--dry-run`, `--only claude,cursor,windsurf`, `--url <endpoint>`,
`--marketplace <source>`, `--write-profile`.

`--dry-run` makes **no** network call. Creating a team and joining a team both mutate, so under
`--dry-run` the installer prints the call it would make and skips it.

### The last step: your board, signed in

Boards of private teams are for their members, after signing in (`docs/BOARD_LOGIN.md`). There is
no password: the installer's last step, "Your board", asks

```
    Open your board in a browser now, signed in? [Y/n]
```

and on yes (the default) calls `open_board` with the first client's own token. The server returns
a sign-in link, `https://metiche.xyz/signin#mbl_<secret>`. It signs **one** browser in as you,
works **once**, and expires after **10 minutes**.

- **On this machine's desktop** (macOS `open`, or Linux `xdg-open` with a display, and not over
  SSH): the link goes into a 0600 file in the run's private temporary directory. The installer
  opens **that file**, never the link itself, because a link on a command line is visible in `ps`.
  The file is removed 10 seconds later. The link is also printed once, with its expiry, in case
  nothing opened.
- **Over SSH, or with no desktop:** the link and its expiry are printed, to open in a browser on
  your own machine.
- **An older server** without `open_board`: one line saying so, and the install still succeeds.
  Any other failure is a warning. Joining is the product, so the board step never fails an install.

It asks only in a terminal and never when `CI` is set. Without a terminal, or under CI, no link is
created at all, since a CI log is the worst place for a secret. `--open` skips the question
(still only in a terminal), `--no-open` never creates a link, and
`METICHE_OPEN_BOARD=ask|yes|no` says the same (default `ask`).

Later, ask your assistant to "open the metiche board": it calls `open_board` the same way, and
`sign_out_browsers` lists (or, when asked, signs out) your signed-in browsers. The board's
`/account` page does the same.

**A stale token in this shell.** An install that starts a new identity rewrites `~/.metiche/env`
and keeps the old file as a backup, but the shell that ran it still exports the old
`METICHE_TOKEN`. A re-run in that shell used to treat it as a token you passed on purpose, and
refused. Now a `METICHE_TOKEN` that matches a backup of `~/.metiche/env` is recognised as stale:
the current saved token is used, with a warning to open a new terminal (or run `. ~/.metiche/env`).
The end of every run warns when this terminal still holds a different token than the one saved.

## Invite a teammate

Anyone already on the team can bring someone in. Three steps:

1. **Ask your assistant to create an invite**, e.g. "make a metiche invite for Ana". It calls
   `create_invite`, optionally with a label, a use limit and an expiry, and shows you the join
   code. By default a code admits **one** person and expires in **7 days**. Team owners can raise
   that to 100 uses and 30 days; members to 25 uses and 7 days.

   Or open your board → **Invites**. Signed-in team members see that tab; it has the same form,
   the same limits and a copy button for the code, and it lists and revokes invites too.
2. **Share the code privately**: a direct message, a password manager, in person. You see it
   once. `list_invites` shows the outstanding invites but never their codes.
3. **Your teammate runs the installer and pastes it.**

   ```sh
   curl -fsSL https://metiche.xyz/install.sh | sh
   ```

   They choose **join** and paste the code at the prompt, which does not echo it. Without a
   prompt:

   ```sh
   curl -fsSL https://metiche.xyz/install.sh | METICHE_JOIN_CODE=<code> sh
   ```

Changed your mind, or the code ended up somewhere public? Ask your assistant to revoke it: it finds
the invite with `list_invites` and calls `revoke_invite` with that `invite_id`, and the code stops
working.

Never paste a join code into a repository, an issue, a PR or a public chat.

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

Then put your **token** in your environment — the plugin's `.mcp.json` expands the bearer from
the environment, so it is never written into any file in the repository:

```sh
printf 'METICHE_TOKEN=%s\nexport METICHE_TOKEN\n' 'your-token' > ~/.metiche/env
chmod 600 ~/.metiche/env
echo '[ -f "$HOME/.metiche/env" ] && . "$HOME/.metiche/env"' >> ~/.zshrc
```

> **Known mismatch.** `plugin/.mcp.json` and the repository's own `.mcp.json` still name
> `${METICHE_JOIN_CODE}` as the bearer. That is the same 401 described at the top of this page and
> both need to become `${METICHE_TOKEN}`; `install.sh` already writes only `METICHE_TOKEN`.

Restart Claude Code. `/mcp` should list `metiche`.

### Just the MCP server, no plugin

```sh
claude mcp add --transport http --scope user metiche https://mcp.metiche.xyz/v1/mcp \
  --header "Authorization: Bearer $METICHE_TOKEN"
```

Scopes are `local`, `user` and `project`. Note that this expands your token into a command
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
      "headers": { "Authorization": "Bearer your-metiche-token" }
    }
  }
}
```

Cursor also reads a project-scoped `.cursor/mcp.json`, and `install.sh` deliberately never writes
it: that file lives inside your repository, and a token in a repository is a leaked credential one
`git add -A` later. Use the global file.

There is no plugin format for the skill. Point Cursor at
`skill/metiche-teamwork/SKILL.md`, or paste it into your project rules.

## Windsurf

`~/.codeium/windsurf/mcp_config.json`, same `mcpServers` shape as above. Same story for the
skill — it is one markdown file; put it wherever Windsurf reads workspace rules.

## Zed, everything else

Codex *is* automated, through its own `codex mcp add` CLI — see `install.sh --only codex`. It is
the one client where no secret reaches disk: its config stores the **name** of an environment
variable (`METICHE_TOKEN`) and it reads the value at connect time, which means `~/.metiche/env`
must be loaded in the shell you launch `codex` from.

Zed is not automated, on purpose: its MCP config format was not verified when this was written,
and a confidently wrong config file is worse than no config file. Any MCP client needs exactly
three things:

```
transport   streamable http
url         https://mcp.metiche.xyz/v1/mcp
header      Authorization: Bearer <your metiche token>
```

and the skill, which is one markdown file with no dependencies.

## Binding a repo to a team

A team's board is visible to every member of that team, so metiche never puts a repository's work
there on a guess. Once, per repository, per team, somebody decides.

**The `.metiche` file.** Two lines at the git root, safe to commit (it names a team and grants no
access; never put a token, join code or URL in it):

```
team = taqueria-tracker
project = taqueria
```

`metiche init` writes it (docs/CLI.md §1.10). Agents look for it walking up from their working
directory to the git root, pass `team` as `team_slug` and `project` as `project_key`, and confirm
with `confirm_new_project: "metiche_file"`. A `project` line above the git root is ignored.

**When the agent asks.** Only when `start_session` would create a new project: no active project on
the team has this repository's remote or this key, and there is no `.metiche` for it. Then
`start_session` creates nothing and answers `code: "confirm_repo_binding"` (not an error), with the
team, the repository and the team's existing projects. The agent asks you:

> Should work in github.com/acme/shop go on team Taqueria Tracker's board, where its members can see it?

On yes it starts again with `confirm_new_project: "person"` and writes the `.metiche` above. A
repository that is already a project on the team never asks, and neither does any clone that has the
committed `.metiche`.

**Picking a different team.** Say no. The agent either leaves metiche out of this repository, or
calls `create_team` for this work (or uses another team you are on) and starts the session with that
`team_slug`. On several teams, a call without `team_slug` is refused until a `.metiche` or you name
one. To rebind a repository later, `metiche init --team <slug> --force`.

## Dogfooding — metiche pointed at itself

[`.mcp.json`](../.mcp.json) at the repository root registers metiche for agents working **on**
metiche. It carries no credential: the header is expanded from your environment at load time, and
the endpoint is `${METICHE_MCP_URL:-https://mcp.metiche.xyz/v1/mcp}` so you can point at a local
server without editing a tracked file.

It currently expands `${METICHE_JOIN_CODE}`, and must expand `${METICHE_TOKEN}` — see the known
mismatch above. If the variable is unset, Claude Code warns and leaves the placeholder unexpanded.
Visible failure, not a silent one.

## The rule that matters

**Neither a token nor a join code ever enters this repository.** Not in `.mcp.json`, not in the
plugin, not in an example, not in a screenshot, not in a test fixture. Every config in this repo
references an environment variable; every config that holds the real value lives in your own home
directory.
