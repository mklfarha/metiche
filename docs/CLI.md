# metiche CLI — a person's view, and a doctor that has caught something

## Context

Per-agent tokens fixed the identity model (see `docs/IDENTITY.md`). They did not fix what the
night after the demo exposed: **a human has no way to see metiche, and onboarding failures are
invisible until somebody spends hours reading logs by hand.**

- **A person cannot see their own teams.** Only agents can call `list_teams`.
  - The board has no login and shows **public teams only**. The board service runs without a board
    token, and `app/authz` answers a private team with the same 404 as a missing one, so
    `/t/<private-slug>` is a 404 (`code/frontend/internal/web/discovery_test.go`,
    `privacy_test.go`; `app/authz/authz_test.go`). Teams are private by default.
  - So for most real teams there is no human view at all.
  - Nothing lists projects anywhere: `list_teams` returns five fields and none of them is a project.
- **Every onboarding failure that night was found by hand, and slowly:**
  1. a correct token that never reached the client — no shell-profile line, and a GUI-launched
     Codex that never reads the shell environment, refuses to start the server, and puts the
     reason only in `~/.codex/logs_2.sqlite` (`MCP server startup failed … Environment variable
     METICHE_TOKEN for MCP server 'metiche' is not set`);
  2. Claude Code silently dropping custom headers from a plugin's `.mcp.json`, proven only by a
     server log of which header **names** arrived;
  3. a wrong endpoint path (`/mcp` instead of `/v1/mcp`);
  4. two agents on one machine collapsing into one identity;
  5. stale duplicate registrations (a hand-added `metiche-direct` beside the plugin's `metiche`);
  6. a Claude Code plugin whose config was cached by version and never updated;
  7. tokens dead after a server reset;
  8. an agent that could not learn its own identity, and guessed.
- **Invites were revoked with a raw SQL statement**, because no tool lists, creates or revokes one.
- **Nothing writes a `.metiche`, and nothing ties a project to its repository.** On 2026-09-12 two
  agents of one person, on one clone of `github.com/mklfarha/taqueria`, called `start_session` with
  different free-text `project_key`s: `taqueria_tracker` from the parent folder and `taqueria` from
  the git root. The server finds a project by `(team, key)` alone (`app/mcp/sessions.go:111`,
  `handler.go:226-240`, unique index `uq_project_team_key`) and stores `repo_url` only when it
  creates one (`sessions.go:43,130`). So it made two projects, and a real collision on `app/rest.go`
  was never detected. A server fix is committed (`1b44075`): `start_session` matches by normalized `repo_url`.
  `metiche init` (§1.10) is the CLI half, and doctor detects the split (§2.2.1).
- **A second team with the same name is one call away.** `create_team` dedupes only on its
  idempotency key, from which it derives the team's uuid (`createteam.go:129-132`). The same name
  with another key is a second team whose slug gets a `-xxxxxx` suffix (`createteam.go:215-221`).
  `install.sh` checks `list_teams` for a same-named team before creating (`install.sh:1350-1358`);
  an agent calling `create_team` directly does not.

The REST API is default-deny now (`code/backend/metiche/app/rest.go`, `AllowedRoutes`): only
`/healthz`, `/v1/mcp`, `/v1/metrics/mcp` and the board's `/v1/teams/{slug}…` routes answer, and the
public ingress exposes only `/v1/mcp` and `/v1/metrics/mcp`. **So the CLI speaks MCP over streamable
HTTP, and every new capability is an MCP tool.** Nothing here adds a REST route.

The principle, carried over from `IDENTITY.md` because the CLI is where it matters most: **verify
through the artifact, not around it.** A CLI that reads a client's config file and calls the server
with the token it finds proves the token works. It does not prove the *client* can use it — and
every failure above was in what a client did with a file that looked correct. So `doctor` labels
every check with how it knows:

| mode | means | example |
|---|---|---|
| **through** | the client itself did it — its own CLI, its own log | `claude mcp get metiche` connects and reports ✘ HTTP 401 |
| **server** | the metiche server saw the client's own request | `whoami` shows a request carrying Claude Code's token arrived after the probe |
| **around** | the CLI checked the file or the token on its own | the token in `~/.cursor/mcp.json` is accepted by `whoami` |

An around check that passes is never reported as "the client works".

### What was verified on this machine (read-only, 2026-09-12)

These findings shape the design, and several of them contradict what one would assume.

| client | capability | what was found |
|---|---|---|
| Claude Code 2.1.270 | `claude mcp get <name>` / `claude mcp list` | **Really connects**, with the configured headers. Against the real `metiche-direct` entry: `✘ Failed to connect — Server rejected the configured Authorization header (HTTP 401) … Error detail: {"error":"unauthorized","detail":"that metiche token is not valid; …"}`. In a scratch `CLAUDE_CONFIG_DIR` (probes since deleted): path `/mcp` → `MCP endpoint not found at https://mcp.metiche.xyz. Check the URL in your MCP config.`; unknown host → `ENOTFOUND: getaddrinfo ENOTFOUND …`; **no Authorization header at all → `✔ Connected`**, because `initialize` and `health` need no token. The exit status is 0 in every case, so the text has to be parsed. `get` **prints header values**. `list` health-checks *every* configured server, third-party ones included. |
| Claude Code plugin cache | `~/.claude/plugins/` | `cache/metiche/metiche/0.1.0/.mcp.json` still defines `metiche` with header names `Authorization`, `X-Metiche-Client-Key`, and carries `.orphaned_at`. `installed_plugins.json` does not list metiche; `known_marketplaces.json` does. The repo's `plugin.json` is `0.2.0` (uncommitted). |
| Claude Code config | `~/.claude.json` (0600) | User scope has `metiche-direct` (`type`,`url`,`headers` = `Authorization`, `X-Metiche-Client-Key`) and **no `metiche`**: the pre-v4 shape (the rewritten installer has not been run here). |
| Codex 0.154.0 | `codex mcp list` / `codex mcp get metiche [--json]` | **Config only**: returns in 0.08 s and does not connect. `--json` gives `transport.{type: streamable_http, url, bearer_token_env_var, http_headers, env_http_headers, http_headers_helper}`, `enabled`, `disabled_reason`. The text form masks `Authorization` but prints other header values. |
| Codex | `codex doctor --json` | Has a `checks["mcp.config"]` row, with per-server details like `env var X is not set`. It is evaluated in the **terminal's** environment, which is exactly the environment a Dock-launched Codex does not have. |
| Codex | `~/.codex/logs_2.sqlite` (~100 MB + WAL) | Table `logs(id, ts, ts_nanos, level, target, feedback_log_body, module_path, file, line, thread_id, process_uuid, estimated_bytes)`, index on `ts`. It opens read-only with `file:…?mode=ro`. There are 10 rows with `target='codex_mcp::rmcp_client'`, `level='WARN'` and a body starting `MCP server startup failed server_name="metiche" error=`. Two error classes are present: `Environment variable METICHE_TOKEN for MCP server 'metiche' is not set` and `handshaking with MCP server failed: Send message error Transport […]`. |
| Codex config | `~/.codex/config.toml` (0600) | `[mcp_servers.metiche]` with `http_headers` = `Authorization`, `X-Metiche-Client-Key`: pre-v4. Backups `config.toml.bak2` and `config.toml.metiche-backup` are both 0600. |
| Cursor | `cursor-agent` 2026.07.01 | `cursor-agent mcp list` prints `<name>: ready` / `<name>: requires_authentication`. It takes 5.6 s and **starts every configured server**. `cursor-agent mcp list-tools <name>` connects to one server and prints `Tools for <name> (N):`, or `MCP '<name>' requires authentication.` No metiche entry is configured here, so its 401 behaviour is **unobserved**. |
| Cursor IDE | logs | `~/Library/Application Support/Cursor/logs/<session>/window*/exthost/anysphere.cursor-mcp/MCP user-<server>.<ts>.log`, lines `YYYY-MM-DD HH:MM:SS.mmm [level] …` (seen for `user-nuzur`). |
| Windsurf | — | **Not installed** on this machine; nothing verified locally. The docs (docs.windsurf.com → docs.devin.ai/desktop/cascade/mcp) give `~/.codeium/windsurf/mcp_config.json`, remote servers as `serverUrl` (or `url`) plus `headers`, and `${env:VAR}` interpolation. They document no CLI, logs or status command. |
| metiche | `~/.metiche/env` (0600, dir 0700) | Exports `METICHE_TOKEN` **and** `METICHE_CLIENT_KEY`: pre-v4. |
| metiche | root `.mcp.json` | Its comment gives `METICHE_MCP_URL=http://127.0.0.1:8788/mcp` as an example: the wrong path (failure 3), in the documentation itself. |
| metiche | `install.sh` as committed in `11782d0` (identical to `code/frontend/static/install.sh`) | One join per client, a generated `client_key`, literal per-client tokens. Convergence: removes the legacy `metiche-direct` on sight (via the `claude` CLI), rewrites stale entries, drops `METICHE_CLIENT_KEY`. `~/.metiche/env` is now a managed block `# >>> metiche >>>` … `# <<< metiche <<<` (older files start with `# metiche — created by install.sh…`). Backups are `<file>.metiche-backup-<timestamp>`. Codex tables are removed with awk, **not** `codex mcp remove`, which re-serialises the user's other tables (verified there against 0.154.0). A saved anchor rejected with 401 **starts a new identity**; a rejected `$METICHE_TOKEN` refuses. `--uninstall` contacts no server: it removes the client entries, the Codex `AGENTS.md` block, the profile line, and all of `~/.metiche` (after copying it to `~/.metiche.metiche-backup-<timestamp>`), and keeps the plugin. |
| metiche server | 401 semantics (`install.sh` `token_is_dead`) | **The server answers 401 both for an unknown token and when its database is down**, by design. A 401 means "dead token" only after an anonymous `health` reports `ok` with `database: reachable`. The CLI and doctor apply the same rule. |

The consequence for design: **`✔ Connected` from Claude Code is not proof that the token reaches the
server.** A client that drops the `Authorization` header connects just as happily. Proving delivery
takes a server-side record of which requests arrived with that client's token (`whoami`, below).

---

## Decisions

| | |
|---|---|
| Transport | MCP streamable HTTP via `github.com/modelcontextprotocol/go-sdk` v1.7.0, the SDK the server uses. No REST. |
| Module | New Go module `code/cli`, `module github.com/mklfarha/metiche/cli`. It does **not** import the backend module. |
| Credential | The anchor (`$METICHE_TOKEN`, else `~/.metiche/env`). The CLI is **not** its own agent. Fallback rules in §3. |
| Scope | Read-mostly. The server writes are invite create and revoke (§1.6) and team create, rename and leave (§1.4). The CLI never touches sessions. The **one file it writes is `.metiche`**, and only through `metiche init` (§1.10). |
| Repo binding | `metiche init` writes `.metiche` at the git root by default: `team` (required) and `project` (the key the server has, or will derive, for this repository). On disagreement **the server's repository match wins over `project`**: it is reported, never silently obeyed (§1.10.4). |
| Doctor | Through-the-client checks where a client offers one; server evidence via `whoami`; around checks labelled as such. Never modifies anything. |
| Server additions | `whoami` (includes recent-request evidence and received header names), `get_team_state scope=projects` and `scope=members`, `list_invites`, `create_invite`, `revoke_invite`, `rename_team`, `leave_team`, and a duplicate-name guard on `create_team`. Six new tools, two new scopes, one changed tool (§4). |
| Uninstall | `install.sh --uninstall` is the single owner. `metiche uninstall` prints that command and does nothing else. |
| Release | GoReleaser v2, modeled on `/Users/mklfarha/Dropbox/nuzur-24/code/nuzur-cli/.goreleaser.yaml` and its `.github/workflows/release.yml`. darwin/linux × amd64/arm64, `CGO_ENABLED=0`, sha256 checksums, ldflags version. |
| Install location | `~/.metiche/bin/metiche`, plus a symlink `~/.local/bin/metiche` when that directory is on `PATH`. Downloaded and checksum-verified by `install.sh`. |
| Self-update | Deferred (§6.5). `metiche version --check` says when a newer release exists and tells you to re-run the installer. |

---

## 1. Command surface

### 1.1 Global behaviour

```
metiche <command> [flags]

Global flags (accepted by every command):
  --json            machine-readable output on stdout; human text goes nowhere
  --url <url>       MCP endpoint (default: $METICHE_MCP_URL, else https://mcp.metiche.xyz/v1/mcp)
  --timeout <dur>   per network call, default 15s
  --no-color        also honoured: NO_COLOR, non-TTY stdout
  -h, --help
  --version         same as `metiche version`
```

- **Endpoint.** `--url` beats `METICHE_MCP_URL`, which beats `https://mcp.metiche.xyz/v1/mcp`. It must
  be `https://`; `http://localhost*` and `http://127.0.0.1*` are allowed with a warning (the
  installer's rule). **Board base:** `METICHE_BOARD_URL`, default `https://metiche.xyz`. A board is
  `<base>/t/<slug>` (`code/frontend/internal/web/server.go`), and the CLI prints or opens a board
  URL **only for a public team**. A private team's board 404s in a browser until the board has a
  viewer gate. The CLI learns visibility from `get_team_state` (§4.2).
- **Secrets are never accepted as arguments.** Tokens come from the environment or files; join codes
  are only ever *output*.
- **Human output goes to stdout, diagnostics to stderr.** With `--json`, stdout carries exactly one
  JSON document. Every document has `"schema": "metiche.cli.<command>/1"` and `"ok"`. An error prints
  `{"schema":"metiche.cli.error/1","ok":false,"exit":N,"error":"<code>","detail":"…"}`.
- The CLI sends `User-Agent: metiche-cli/<version> (<goos>/<goarch>)` on every request. `whoami`
  evidence depends on this (§4.1).

### 1.2 Exit codes

| code | meaning |
|---|---|
| 0 | success; for `doctor`, no errors (warnings allowed unless `--strict`) |
| 1 | `doctor` found at least one error, or a tool returned an error not covered below |
| 2 | usage error: unknown command or flag, ambiguous team with no `--team` (or an ambiguous project for `init`) and no TTY to ask on, a confirmation needed with no TTY and no `--yes` |
| 3 | no usable credential: none found, or every candidate rejected. A 401 counts as a rejection only when an anonymous `health` then reports the database reachable; a 401 with the database down is exit 4. |
| 4 | metiche unreachable: DNS, TLS, connect, timeout, HTTP 404 at the endpoint (wrong URL), 5xx, or `health` reporting the database unreachable |
| 5 | refused by the server: `not_permitted`, `not_found` or `rate_limited` from a tool (§4 error codes) |
| 6 | conflict, nothing changed: `already_exists` from the server (you already have a team with that name; a project key belongs to another repository), or `init` finding a different binding in the `.metiche` it would write, without `--force` |

### 1.3 `metiche status`

Who am I, what am I on, what is live, and where are the boards.

```
metiche status [--team <slug>] [--no-clients] [--json]
```

Calls:
- `health` (no token);
- `whoami` with the credential;
- `list_teams`;
- per team (or just `--team`): `get_team_state scope=projects` and `get_team_state scope=sessions`;
- unless `--no-clients`, `whoami` with each client's configured token, which fills in the "this
  machine" block.

```
$ metiche status
metiche   https://mcp.metiche.xyz/v1/mcp · server 1.0 · database reachable
you       account AC-3f9c · credential: ~/.metiche/env (agent laptop-claude)

this machine (laptop)
  Claude Code  laptop-claude  agent AG-12  last request 2m ago
  Codex        laptop-codex   agent AG-13  no request since the server started
  Cursor       not configured

teams (2)
  taqueria-tracker  Taqueria Tracker   private · owner · 3 members · you are live here
    board     not viewable in a browser: private team, and the board has no login yet
    projects  taqueria   2 live   last activity 12s ago
              api        0 live   last activity 3d ago
    live      S-41  Mark · claude on laptop  taqueria  feat/lanes  "wiring the lane builder"   12s ago  mine
              S-39  Ana · cursor on ana-mbp  api       main        "rate limiter tests"         1m ago
  hack-night        Hack Night         public · member · 6 members
    board     https://metiche.xyz/t/hack-night
    projects  (none yet)
    live      (nobody)
```

Keys and names above are illustrative. `--json`:

```json
{"schema":"metiche.cli.status/1","ok":true,
 "endpoint":"https://mcp.metiche.xyz/v1/mcp","server":{"protocol_version":"1.0","database":"reachable"},
 "credential":{"source":"anchor_file","path":"~/.metiche/env","token_scope":"agent","agent_key":"AG-12"},
 "account_key":"AC-3f9c",
 "machine":{"machine_id":"laptop","clients":[{"client":"claude","client_key":"laptop-claude","agent_key":"AG-12","last_request_at":"…"}]},
 "teams":[{"slug":"taqueria-tracker","name":"Taqueria Tracker","role":"owner","members":3,"active_session":true,
           "visibility":"private","board_url":null,"board_note":"private team: not viewable in a browser until the board has a viewer gate",
           "projects":[{"key":"taqueria","name":"taqueria","live_sessions":2,"last_activity_at":"…"}],
           "sessions":[{"key":"S-41","member":"Mark","agent":"claude on laptop","project":"taqueria","branch":"feat/lanes","status_line":"…","status":"live","last_seen":"…","mine":true}]}]}
```

### 1.4 `metiche teams [create | show | rename | leave]`

```
metiche teams                                            # the list (below)
metiche teams create <name> [--allow-duplicate-name] [--quiet] [--dry-run]
metiche teams show   [<slug>]
metiche teams rename <new-name> [--team <slug>] [--allow-duplicate-name]
metiche teams leave  [<slug>] [--yes]
```

Every subcommand takes `--json`. A team argument or `--team` resolves exactly like `open` (§1.5):
the argument, then the nearest `.metiche`, then your only team. It is never guessed.

`metiche teams` with no subcommand stays the cheap, scriptable list: one `list_teams` call.

```
$ metiche teams
SLUG              NAME              ROLE    MEMBERS  YOU LIVE
taqueria-tracker  Taqueria Tracker  owner   3        yes
hack-night        Hack Night        member  6        no
hack-night-3f9a1c Hack Night        owner   1        no     ! same name as hack-night
```

- No BOARD column: `list_teams` does not carry visibility, and printing a URL that 404s for every
  private team would be wrong more often than right. `metiche status` and `metiche open <slug>` show
  the board where one is viewable.
- **Duplicate names are marked**, computed locally from the same call. Two of your teams whose names
  normalize to the same slug base (`slugKey(name, 40)`, `handler.go:285-305`: lowercase, runs of
  non-alphanumerics collapsed to one `-`) get `! same name as <slug>`. `--json` adds
  `"duplicate_of": ["<slug>", …]` per team. `doctor` reports the same thing as a warning (§2.2.1).
- Zero teams prints the server's `note` (create or join) and exits 0.

#### 1.4.1 `teams create <name>`

1. **Duplicate check first.** `list_teams`, then compare normalized names as above. On a match it
   exits 6 and creates nothing:

   ```
   $ metiche teams create "hack night"
   You are already on a team named "Hack Night": hack-night (member, 6 members).
   Nothing was created. Use it in a repository:  metiche init --team hack-night
   To create a second team with the same name anyway:  metiche teams create "hack night" --allow-duplicate-name
   ```

   The server makes the same check (§4.8), so an agent that skips `list_teams` is refused too. The
   CLI checks first only because it can give a better message before any write.
2. **Who creates it.** `create_team` runs with the anchor, and only when `whoami` reports
   `token_scope: agent`. The call sends **no** `client_key`, `member_name` or `agent_label`: with an
   agent token the server defaults all three to the token's own agent and the account (§4.8). This
   matters. Under `joinAs`'s token table (`join.go:162-171`), any other `client_key` would create a
   new agent and mint it a token, which is exactly the `<machine_id>-cli` agent that §3 rejects. So:
   - with no agent token available it exits 3 ("re-run the installer first"). A legacy account
     anchor, or a dead one, falls back to a client's own agent token under §3's one-account rule, as
     `open_board` does (`docs/BOARD_LOGIN.md` §5.3). That is safe here because the defaults keep
     that client's token and mint nothing;
   - a server that still requires those parameters (detected from `create_team`'s input schema in
     `tools/list`) exits 1 with "this metiche server is too old for `teams create`; create the team
     with the installer (`METICHE_TEAM_NAME`)", before any call;
   - a response carrying `token` or `token_kept: false` is a bug. The token is dropped unread into a
     `Secret` and never printed, and the command exits 1 naming `metiche doctor`.
3. **Retry safety.** One random UUID per invocation is the `idempotency_key`, reused for the
   command's own retries. A retry returns the same team and the same code (`created: false`,
   `ensureFirstInvite` is idempotent, `createteam.go:305-313`).
4. **The join code** follows §1.6's rules for `invite create`: shown once in human output, never
   listable later. `--quiet` prints only the **slug**, with no code, because what a script wants
   from `create` is the slug. `--json` carries `join_code`, as `create_invite`'s does. Unlike an
   invite made with `invite create`, the first invite is uncapped and never expires
   (`ensureFirstInvite`), so the output says so and suggests a narrower one:

   ```
   $ metiche teams create "Taqueria Tracker"
   created team taqueria-tracker ("Taqueria Tracker") · private · you are its owner

     join code   <shown here once — it never expires and has no use limit>

     For a narrower code:  metiche invite create --team taqueria-tracker --max-uses 5 --expires 48h
     then revoke this one: metiche invite list --team taqueria-tracker; metiche invite revoke <invite-id>
     Bind a repository:    cd <repo> && metiche init --team taqueria-tracker
   ```

   Every client on this machine can use the team at once: membership belongs to the account
   (`uq_member_account_team`), and `RequireTeam` checks the account (`auth.go:571-615`).
5. `--dry-run` runs the duplicate check and the version check, prints what it would call, and
   creates nothing.

Exit codes: 0 created or replayed; 6 duplicate name (from the CLI's check or the server's
`already_exists`); 3; 4; 5 (`rate_limited`: `createLimit`, `createteam.go:102-106`); 1 server too
old.

#### 1.4.2 `teams show [<slug>]`

One team in full, for a person. It calls `get_team_state` with `scope=members` (new, §4.9),
`scope=projects` (§4.2) and `scope=sessions`, and reads the `team` block (§4.2) for visibility.

```
$ metiche teams show
taqueria-tracker  "Taqueria Tracker"  private · you are owner · bound here by /Users/me/work/taqueria/.metiche
board     not viewable in a browser: private team, and the board has no login yet
members   Mark (owner, you)   joined 2026-09-01   agents: claude on laptop (live 12s ago), codex on laptop (3d ago)
          Ana  (member)       joined 2026-09-03   agents: cursor on ana-mbp (1m ago)
projects  taqueria   github.com/mklfarha/taqueria   2 live   last activity 12s ago
          api        (no repository recorded)       0 live   last activity 3d ago
live      S-41  Mark · claude on laptop  taqueria  feat/lanes  "wiring the lane builder"  12s ago  mine
          S-39  Ana · cursor on ana-mbp  api       main        "rate limiter tests"        1m ago
```

- Two projects that share a repository are flagged `! same repository as <key>`: the split from the
  incident, seen from the team side (`doctor` sees it from the repo side, §2.2.1).
- `--json`: `metiche.cli.teams.show/1` with `team`, `role`, `binding` (the `.metiche` path that names
  this team, if any), `members[]`, `projects[]`, `sessions[]`, `board_url` (null unless public).
- Exit codes: 0; 2 ambiguous team; 3; 4; 5 not a member or no such team.

#### 1.4.3 `teams rename <new-name>`

Calls `rename_team` (new, §4.10), which is **owner only**. It changes the display name and **never
the slug**. The slug is what `.metiche` files, board URLs and `METICHE_TEAM_SLUG` installs point at,
so changing it would silently unbind every clone. The same duplicate-name guard applies:
`--allow-duplicate-name` passes through.

```
$ metiche teams rename "Taqueria Tracker v2" --team taqueria-tracker
renamed taqueria-tracker: "Taqueria Tracker" → "Taqueria Tracker v2" (the slug does not change)
```

Renaming to the current name prints `unchanged` and exits 0. Exit codes: 0; 2; 3; 4; 5
`not_permitted` (a member); 6 duplicate name.

#### 1.4.4 `teams leave [<slug>]`

Calls `leave_team` (new, §4.11) for **yourself only**. The server refuses, and the CLI exits 5 with
its message, when:
- **any of your agents has a live or stale session on the team.** It lists them, and you end them
  in those agents or wait for the sweeper; the CLI never ends a session (§7);
- **you are the only owner and other members remain.** Transferring ownership needs a role change,
  which is not in the CLI yet (below);
- **you are the only member.** Leaving would orphan the team and its uncapped first invite, and
  archiving is not in the CLI yet (§10).

Leaving is a destructive act for you. On a TTY the CLI asks you to type the slug; without a TTY it
needs `--yes`, or it exits 2.

```
$ metiche teams leave hack-night
Leave hack-night ("Hack Night", 6 members)? You will lose access to its board and team state.
Getting back needs a new join code from a member. Type the slug to confirm: hack-night
left hack-night.
  /Users/me/work/orbital/.metiche still names hack-night; your agents there will stop until you rebind:
    cd /Users/me/work/orbital && metiche init --force
```

The `.metiche` hint appears only for the nearest binding from the working directory; the CLI does
not scan your disk. Leaving a team you already left prints `already left` and exits 0. Exit codes:
0; 2; 3; 4; 5.

#### 1.4.5 Not in the CLI until board login ships

These stay out of the CLI for now:
- **Changing visibility.** `create_team` keeps every team private so that going public is "a
  deliberate act" (`createteam.go:231-235`). Today the move is unsafe to offer in either direction:
  - private → public exposes the board to anyone holding the slug;
  - public → private does not take effect: the board keeps serving a team it discovered while public
    (`docs/BOARD_LOGIN.md` §0, finding F2).
- **Removing a member, or changing a member's role** (which ownership transfer needs).

Both are owner acts against *another person*, on anonymous accounts whose display names nobody
verified (`createteam.go:73-80`).

**Dependency on `docs/BOARD_LOGIN.md`** (a design the owner approved on 2026-09-13; nothing is implemented):
- It gives people an authenticated browser view of private boards, through a sign-in link minted
  by `open_board`.
- It fixes F2 and re-checks membership on every request, and every 60 s on an open stream (its
  §2.8, §4.4). So a removal or a visibility change would finally take effect everywhere a person
  can look.
- It keeps the board **read-only against the backend** (its Non-goals), so these writes will not
  live on the board either.

**Decided (§10 Q11):** add them after board login's deploy, as owner-only MCP tools
(`set_team_visibility`, `remove_member`, `set_member_role`). Each gets a `metiche teams` subcommand,
destructive annotations and a structural event (§10). §1.4.4's "only owner" refusal then gets its
remediation.

### 1.5 `metiche open`

```
metiche open [<slug>] [--print]
```

> **This command changes when board login lands** (`docs/BOARD_LOGIN.md` §5.3, a design the owner
> approved on 2026-09-13). What changes:
> - a private team: `open` calls `open_board`, writes the single-use sign-in link into a 0600
>   redirect file in a private temp directory, opens that file, and removes it;
> - `--print` prints the link and its expiry;
> - a public team: `--signin` also signs the browser in;
> - a new `metiche signout` command;
> - §7 gains that temp file as a second write exception, and `open_board` and `sign_out_browsers`
>   join the tool allowlist.
>
> Team resolution below does not change. Until board login ships, the behaviour below stands.

The team is resolved in this order, never guessed (the same order `invite`, `teams show|rename|leave`
and `status --team` use):
1. the argument;
2. the nearest `.metiche` walking up from the working directory (`team = <slug>`, the rule in
   `PLAN.md`, as written by `metiche init`, §1.10);
3. `list_teams` with exactly one team.

With several teams and no binding it exits 2 and lists the slugs, adding: "bind this repository once
with `metiche init`". When the slug came from a `.metiche` and the server answers `not_found`, the
exit-5 message names the file and the two ways out: join that team, or rebind with
`metiche init --force`.

**Decision: `open` opens only boards a browser can show.** Once the slug is resolved, it always calls
`get_team_state` for that team (`scope=projects`, `limit=1`). That confirms you are a member, and the
response's `team.visibility` (§4.2) decides:

- **Public team:** opens `<board>/t/<slug>` with `open` (darwin) or `xdg-open` (linux). `--print`
  only prints the URL. Exit 0.
- **Private team:** opens nothing and prints the URL nowhere, because that page is a 404 in a
  browser today. Exit 1 (nothing to open). `--json` gives `"visibility":"private","board_url":null`.

  ```
  $ metiche open
  taqueria-tracker is a private team (from /Users/me/work/taqueria/.metiche).
  Its board is not viewable in a browser yet: the board has no login, so it only shows public teams.
  See it from here instead:  metiche status --team taqueria-tracker
  ```
- **Not a member, or no such team:** exit 5 with the server's error. No URL is guessed.

The CLI never makes a private board viewable itself: it holds no board token and adds no link that
carries one. Showing a private board needs a viewer gate in the board service, which does not exist
yet.

```
$ metiche open hack-night
https://metiche.xyz/t/hack-night
```

### 1.6 `metiche invite list | create | revoke`

These replace the raw SQL. All three take `--team <slug>`, resolved exactly like `open`.

```
metiche invite list   [--team <slug>] [--all]          # --all includes exhausted/expired/revoked
metiche invite create [--team <slug>] [--label <text>] [--max-uses <n>] [--expires <dur>] [--quiet]
metiche invite revoke <invite-id> [--team <slug>]
```

`list` **never shows a code**. The server does not return one; it returns `code_hint`, the last 4
characters.

```
$ metiche invite list --team taqueria-tracker --all
ID        LABEL          CODE      USES  EXPIRES           STATUS     CREATED BY
7c1e09a2  first invite   ••••PLUM  4/∞   never             active     Mark
9f2a61c0  hack night     ••••7QX2  2/5   2026-09-14 21:00  revoked    Mark
b3d41e77  for Ana        ••••M3KD  1/1   never             exhausted  Ana
```

`create` is the one command that prints a join code, because showing it is the point. It is
printed once, and `--quiet` prints only the code (for piping).

```
$ metiche invite create --team taqueria-tracker --label "hack night" --max-uses 5 --expires 48h
created invite 9f2a61c0 on taqueria-tracker · max 5 uses · expires 2026-09-14 21:00 UTC

  join code   <shown here once — share it with your teammates; it cannot be listed again>

  They run:   curl -fsSL https://metiche.xyz/install.sh | sh
              (the installer asks for the code; never put it on a command line)
```

`revoke` takes the full uuid or a unique prefix of at least 8 characters. The prefix is resolved
through `list_invites --all`; an ambiguous prefix exits 2.

```
$ metiche invite revoke 9f2a61c0 --team taqueria-tracker
revoked invite 9f2a61c0 ("hack night", 2 of 5 uses spent).
People who already joined keep their access; nobody new can join with this code.
```

Revoking an already-revoked invite prints `already revoked` and exits 0. A plain member revoking
someone else's invite exits 5 (`not_permitted`).

### 1.7 `metiche doctor`

```
metiche doctor [--client claude,cursor,windsurf,codex] [--offline] [--no-exec]
               [--prove <client>] [--strict] [--verbose] [--json]
```

- `--offline`: file checks only, no network. Server and delivery checks become `skip`.
- `--no-exec`: run no client CLI (`claude`, `codex`, `cursor-agent`). Through checks become `skip`.
- `--prove <client>`: the interactive delivery proof for GUI clients (§2.6).
- `--strict`: warnings also exit 1.

The full design is §2. Example, modeled on this machine's actual state (keys illustrative, no values):

```
$ metiche doctor
metiche doctor · endpoint https://mcp.metiche.xyz/v1/mcp · machine laptop

machine
  ✔ endpoint          /v1/mcp answers · server 1.0 · database reachable                    server
  ✘ anchor token      the server rejects the token in ~/.metiche/env (HTTP 401)             around
                      → Tokens do not survive a server reset. Re-run the installer with a join
                        code from your team:  curl -fsSL https://metiche.xyz/install.sh | sh
  ! anchor file       ~/.metiche/env still exports METICHE_CLIENT_KEY (pre-per-agent install) around
                      → Re-run the installer; it rewrites this file without it.

Claude Code 2.1.270
  ✘ registrations     no "metiche" entry; stale "metiche-direct" (user scope) points at metiche  around
                      → Re-run the installer; it removes metiche-direct and registers "metiche"
                        with Claude Code's own token.
  ✘ connection        claude mcp get metiche-direct → ✘ HTTP 401                            through
                      → Claude Code reached metiche and was refused: its token is dead. Re-run the installer.
  ! headers           metiche-direct also sends X-Metiche-Client-Key                        around
  · plugin            orphaned plugin cache 0.1.0 is not loaded by Claude Code (informational)

Codex 0.154.0
  ! headers           [mcp_servers.metiche] also sends X-Metiche-Client-Key                 around
  ✔ read-back         codex mcp get metiche: streamable_http at the expected URL           through (config reader)
  ✘ startup           Codex logged "MCP server startup failed" for metiche 10 times; the latest
                      after config.toml last changed: handshake failed                      through (Codex's log)
                      → Usually a dead token or a wrong URL; see "token" below.
  ✘ token             the server rejects the token in config.toml (HTTP 401)                around

Cursor           not configured for metiche (skipped)
Windsurf         not installed (skipped)

summary: 5 errors · 2 warnings · 3 ok · 2 skipped   → exit 1
```

### 1.8 `metiche version`

```
$ metiche version
metiche 0.1.0 (commit 1a2b3c4, built 2026-09-20T18:02:11Z, darwin/arm64)

$ metiche version --short
0.1.0

$ metiche version --check          # phase 3; the only call to GitHub
metiche 0.1.0 · 0.2.0 is available. Update by re-running the installer:
  curl -fsSL https://metiche.xyz/install.sh | sh
```

`--check` exits 0 whether or not an update exists, and 4 if GitHub is unreachable.

### 1.9 `metiche uninstall`

One owner, not two. The installer wrote every file (client configs, `~/.metiche/env`, the profile
line, the plugin, the binary), so it is the only thing that removes them. This command runs
nothing and deletes nothing:

```
$ metiche uninstall
metiche is removed by the installer that set it up, so nothing is left half-undone:

  curl -fsSL https://metiche.xyz/install.sh | sh -s -- --uninstall --dry-run   # see every change
  curl -fsSL https://metiche.xyz/install.sh | sh -s -- --uninstall

It backs up and removes ~/.metiche, including this binary (~/.metiche/bin/metiche), and contacts
no server. The Claude Code plugin stays installed; the uninstaller prints how to remove it.
```

Neither the installer nor this command removes `.metiche` files. They live in your repositories,
and may be committed; they are yours.

### 1.10 `metiche init`

Bind a directory (by default the repository) to a team and a project, once, so no agent in it has to
ask or guess again. This is the command `PLAN.md` "Binding a repo to a team" assumes exists, and the
one the installer was meant to be (`IDENTITY.md`: "The installer writing `.metiche`", still open).

```
metiche init [--team <slug>] [--project <key>] [--here | --parent] [--dry-run] [--force] [--json]
```

#### 1.10.1 Where the file goes

The CLI learns the repository from git, through `execx` with a fixed argv (§5):
`git rev-parse --show-toplevel` and `git config --get remote.origin.url`. With no `origin` but
exactly one remote (`git remote`), that remote is used; with several and no `origin`, there is no
repository URL, and it says so. No `git` on `PATH` → walk up for a `.git` directory or file for the
root, and treat the URL as unknown.

| flag | target | writes `project`? |
|---|---|---|
| (default), inside a git repo | `<git root>/.metiche` | yes |
| (default), not in a git repo | `<cwd>/.metiche`, with a note that there is no repository to match | only with `--project` |
| `--here` | `<cwd>/.metiche`, even below the git root | yes when inside a repo (it is the same repository); otherwise only with `--project` |
| `--parent` | the directory **containing** the git root: the "folder of five hackathon repos" placement from `PLAN.md` | **never**. `--parent --project` exits 2. Not in a git repo → exit 2 ("use --here"). |

`--here` and `--parent` together exit 2.

**Why `project` never goes above the git root.** A `project` line in a parent directory would bind
every repository beneath it to one project. That is the incident, made permanent in a file. So
readers (the CLI, doctor, agents) **ignore `project` from a `.metiche` above the git root**, and
doctor warns about it (§2.2.1). `team` inherits downward as `PLAN.md` says; `project` does not.

#### 1.10.2 Resolution: team, then project, never guessed

**Team:**
1. `--team <slug>`.
2. Otherwise, a `team` already in effect here: the file being rewritten, or an inherited parent
   `.metiche`. It is used and announced ("inherited from `<path>`"), because a person wrote it.
3. Otherwise `list_teams`:
   - **0 teams** → exit 1: "you are not on any team: `metiche teams create <name>`, or join one
     with the installer and a join code".
   - **1 team** → used, and **said once**: "you are on one team, taqueria-tracker; binding to it".
   - **several** → on a TTY, a numbered prompt in `list_teams` order (most recently used first,
     `listteams.go:146-164`). There is **no default**: Enter alone asks again, because a
     preselected answer is a guess the person did not make. Without a TTY, or with `--json`, it
     exits 2 and lists the slugs.

**Membership is verified.** The slug must appear in `list_teams`; otherwise exit 5 with "`<slug>` is
not one of your teams". The server does not say whether the team exists (`app/authz` answers private
and missing alike), and neither does the CLI.

**Project** (skipped for `--parent`, and outside a repo without `--project`). `get_team_state
scope=projects` is read for the chosen team (§4.2; after the `start_session` fix it returns the
normalized `repo_url`):
1. `--project <key>`. It must match `^[a-z0-9][a-z0-9._-]{0,63}$`. If the server already has that key
   bound to a **different** repository, exit 6: "project `<key>` on `<team>` belongs to
   `<other repo>`; choose another key". That is the same refusal `start_session` will give.
2. Otherwise, the project whose normalized `repo_url` equals this repository's is used, with the
   note "this repository is already project `taqueria` on taqueria-tracker".
   - **Several projects with this repository** (a split made before the fix backfilled `repo_url`):
     a TTY prompt listing their keys, live sessions and last activity, with no default; without a
     TTY, exit 2. Either way the output points at `metiche doctor`, which reports the split
     (§2.2.1).
3. Otherwise the key the server **will** derive from this repository on the first `start_session`
   with no `project_key`: the repository name from the normalized remote (`taqueria` for
   `github.com/mklfarha/taqueria`). With no remote, it is the git root's directory name, normalized
   the same way.
   - The derivation and the URL normalization are the server fix's. The CLI copies them, and a
     shared vector file guards drift (§8.1), exactly like `machine_id`.
   - A project with no `repo_url` whose key equals the derived key is used: the note says the next
     `start_session` will record the repository on it (the fix's backfill).

**The repository URL is sanitized before anything else.** `userinfo` is stripped
(`https://<user>:<token>@host/…` happens with credential helpers and CI), the raw value is never
printed, and the redactor is seeded with it (§7). `.metiche` never contains a URL.

#### 1.10.3 Writing: format, overwrite, idempotence, parents

The format is `PLAN.md`'s, with nothing added:

```
# metiche: which team (and project) agent sessions in this repository belong to.
# Safe to commit: it names a team and grants no access. Written by `metiche init`.
team = taqueria-tracker
project = taqueria
```

- **Never a credential, a join code or a URL.** A test asserts the written bytes against canaries
  (§8.1).
- **Existing file at the target, same `team` and `project`:** prints `already bound`, writes
  nothing (the file stays byte-identical, mtime included), exits 0. This is the idempotent re-run.
- **Existing file with a different binding:** prints the diff and exits 6, unless `--force`.

  ```
  $ metiche init --team hack-night
  /Users/me/work/taqueria/.metiche already binds this repository differently:
    - team = taqueria-tracker
    + team = hack-night
    - project = taqueria
    + project = taqueria        (resolved again on hack-night: project keys are per team)
  Nothing was written. Re-run with --force to replace it.
  ```

- **`--force`** rewrites only the `team` and `project` lines in place. Comments and unknown keys are
  kept, and unknown keys are warned about. There is no backup file, because a backup would litter
  the repository; the diff above is printed either way.
- **The write** is atomic: a temp file in the same directory, then rename. The file is mode 0644,
  since it is meant to be committed and holds nothing secret. A `.metiche` that is a symlink or not
  a regular file is refused (exit 1), as is a target directory not owned by you.
- **Parents.** After writing, the CLI resolves nearest-wins again from the working directory and
  prints what is now in effect:
  - A parent `.metiche` naming **another** team → "this file overrides `<parent>` (hack-night) for
    `<dir>` and below". That is `PLAN.md`'s opt-out, stated so it is not a surprise.
  - A parent naming the **same** team → "redundant for you, but it binds every clone of this
    repository once committed".
  - With `--parent`, a repository's own `.metiche` naming a different team → a warning that the
    repository's file still wins inside it.
  - A parent file carrying `project` → a warning that it is ignored (§1.10.1).
- **Commit suggestion.** When the file is at or below the git root, is untracked, and
  `git check-ignore -q .metiche` says it is not ignored, the CLI prints
  `git add .metiche && git commit -m "Bind to metiche team <slug>"`. It never runs `git add`. It also
  prints one caveat line: "If this repository is public, committing publishes the team slug. A slug
  grants no access, but if you would rather not publish it, add `.metiche` to `.git/info/exclude`."
  An ignored file gets "`.metiche` is git-ignored here; teammates' agents will not see it". With
  `--parent` there is no suggestion: the file is not inside a repository.
- **`--dry-run`** does every read and resolution step, prints the file it would write and the diff,
  writes nothing, and exits with the code the real run would have.

```
$ cd ~/work/taqueria/app && metiche init
repository  /Users/me/work/taqueria  (origin github.com/mklfarha/taqueria)
You are on 2 teams. Which one does this repository belong to?
  1) taqueria-tracker  Taqueria Tracker  owner   3 members  you are live here
  2) hack-night        Hack Night        member  6 members
team [1-2]: 1
project     taqueria: already a project on taqueria-tracker for this repository
wrote       /Users/me/work/taqueria/.metiche
  team = taqueria-tracker
  project = taqueria
Commit it so every clone binds the same way:
  git add .metiche && git commit -m "Bind to metiche team taqueria-tracker"
```

`--json` (`metiche.cli.init/1`):

```json
{"schema":"metiche.cli.init/1","ok":true,
 "path":"/Users/me/work/taqueria/.metiche","placement":"git_root",
 "action":"created","dry_run":false,
 "team":{"slug":"taqueria-tracker","name":"Taqueria Tracker","source":"prompt"},
 "project":{"key":"taqueria","source":"server_repo_match","server_project_exists":true},
 "repository":{"root":"/Users/me/work/taqueria","remote":"origin","repo_url":"github.com/mklfarha/taqueria"},
 "previous":null,
 "effective":{"path":"/Users/me/work/taqueria/.metiche","team":"taqueria-tracker","project":"taqueria"},
 "overrides":[],"warnings":[],"commit_hint":true}
```

- `action` is `created` | `updated` | `unchanged` | `would_create` | `would_update`.
- `team.source` is `flag` | `inherited` | `only_team` | `prompt`.
- `project.source` is `flag` | `server_repo_match` | `derived_remote` | `derived_dirname` | `none`.
- `repo_url` is the normalized, credential-free form, or null.
- An ambiguous team prints `metiche.cli.error/1` with `"error":"ambiguous_team","candidates":[…]`; an
  ambiguous project uses `"error":"ambiguous_project"`.

Exit codes:
- 0: written, unchanged or dry run;
- 1: no teams, or a write refused (symlink, ownership, I/O);
- 2: usage, or ambiguous with no TTY;
- 3, 4: as §1.2;
- 5: not a member of `--team`, or no such team;
- 6: a different binding without `--force`, or a key bound to another repository.

#### 1.10.4 `project` from `.metiche` versus the repository URL: which wins

With the `start_session` fix (commit `1b44075`), an agent sends both `project_key` (from `.metiche`) and
`repo_url`, and they can disagree: the file is stale, it was copied from another repository, or it
was written by hand.

**Decided (§10 Q5): the repository wins.** The server matches the project by normalized `repo_url`
first. When that project's key differs from the `project_key` sent, the session goes on the
repository's project, and the response note says so ("this repository is project `taqueria`;
`.metiche` says `taqueria_tracker`"). The agent tells the person once and suggests
`metiche init --force`. `doctor` reports the mismatch (§2.2.1). Why:
- The failure that matters is two projects for one repository, because collisions stop being
  detected. A stale file must not be able to recreate that split.
- The disagreement is loud, never silent. The person fixes the file; the server never guesses a
  *team* from the URL. Team binding stays "ask, never infer" (`listteams.go:33-34`); inferring a
  project **within the team the person chose** is a different act.
- **No remote** (a local-only repository): `project` from `.metiche` is the only key and is used as
  is.
- **The key named in `.metiche` is bound to another repository:** the fix refuses. The agent stops
  and tells the person, and never invents a third key.
- **A consequence to accept:** one repository is one project. A monorepo that wants several
  projects is not supported until the server supports it (§10).

**Gap in commit `1b44075`** (verified against `app/mcp/projectrepo.go` and `app/mcp/sessions.go`):
- The precedence matches: an active project with the same normalized `repo_url` is used whatever key
  was sent, and a key bound to another repository is refused, naming that repository.
- **The disagreement is not reported.** `resolveProject`'s repository match returns no note; the only
  project note is `newProjectNote`, on creation; and the response does not carry the project's key.
  So the note quoted above, and §4.12 item 3, are still owed by the server's owner. Until then
  `doctor`'s `binding.project` is the only place the mismatch is reported.
- **The disagreeing key is not entirely unused.** The `session_started` event summary
  ("… started work on <key>", `sessions.go:100`) is built from the key as sent, before resolution,
  so the board timeline can show the `.metiche` key while the session is on the repository's project.
- **"The key decides only without a remote" is narrower in the code.** With a remote but no active
  project for that repository yet, the key still picks the project: an existing project with that
  key and no `repo_url` is backfilled, otherwise a new project is created under that key. A remote
  with no host (a local path) counts as no remote.

The alternative, `.metiche` wins, was considered and rejected: an explicit human declaration is
attractive, but tonight's split came from a *plausible* key, and a committed file would make a wrong
key permanent for every clone.

#### 1.10.5 What agents and the skill do with it

The contract that the skill, the server instructions, the `list_teams` note and the installer's
Codex `AGENTS.md` block must all say (their owners write it; §4.12 lists the text changes):
1. **Team.** Walk up from the working directory; the nearest `.metiche` with `team` wins. Pass it
   as `team_slug` on every call, and do not call `list_teams` to second-guess it. When a call with
   that slug fails with not-a-member, **stop and tell the person** which file names which team,
   suggesting `metiche doctor`. Never fall back to another team.
2. **Project.** Take `project` only from a `.metiche` at or below the git root. Pass it as
   `project_key`, and **always** pass `repo_url` (from `git remote get-url origin`, with
   credentials stripped). When `start_session`'s note reports that the repository is a different
   project, work on that one (the server already put the session there), tell the person once, and
   suggest `metiche init --force`. Never edit `.metiche` silently.
3. **No `.metiche`.** Follow `list_teams`'s note: one team → bind and announce; several → ask. Then
   record the answer, preferring `metiche init --team <slug>` when `metiche` is on `PATH` so format
   and placement stay consistent. Otherwise write the two lines of §1.10.3 at the **git root**, not
   in the working directory. The latter is how a parent-folder session made `taqueria_tracker`.
4. **Before `create_team`**, call `list_teams`. If you are already on a team with that name, use its
   slug. Create a second only when the person explicitly asks, with `allow_duplicate_name: true`.

#### 1.10.6 Is `.metiche` honoured today? (verified 2026-09-12, before the `start_session` fix `1b44075`)

| where | what it does | gap |
|---|---|---|
| server | never reads the disk, by design (`PLAN.md:303-304`) | correct; honouring it is entirely the agent's job, so the instructions below are the whole mechanism |
| `list_teams` description (`app/mcp/server.go:239-242`) | "Read a .metiche file first … the team it names WINS" | the **only** place a connected agent is told `.metiche` exists; says nothing about `project`, or where to write the file |
| `list_teams` note (`listteams.go:173-197`) | tells the agent to write `team = <slug>` "into a .metiche file in the repo" | an agent reads this only if it calls `list_teams`; no `project` line; "in the repo" is read as the working directory |
| server instructions (`server.go:115-160`) | the loop starts at `join_team`; "pass team_slug when you are on more than one" | no mention of `.metiche` or `list_teams` |
| skill (`skill/metiche-teamwork/SKILL.md`, byte-identical to `plugin/skills/metiche-teamwork/SKILL.md`) | the loop (lines 29-37) and tool table (lines 47-63) | **zero mentions** of `.metiche`, `list_teams` or `team_slug`. The worked example (line 267) calls `start_session(project: "metiche", …)`, with a parameter name the tool does not have (`project_key`, `sessions.go:35`) and no `repo_url` |
| installer's Codex block (`install.sh:2120-2141`) | "Call `start_session` before you touch any file" | no team binding, no `.metiche`, no project guidance; `install.sh` never writes `.metiche` (`IDENTITY.md:305-306`) |
| `start_session` (`sessions.go:35,61-64,111-143`) | `project_key` is required free text; lookup is by `(team, key)`; `repo_url` is stored only on creation | the incident. The fix, since committed as `1b44075`, covers matching, derivation, backfill and refusal; nothing server-side can read `project` from the file |
| `RequireTeam` with no slug (`auth.go:578-592`) | exactly one team → silently used | "say so once" (`PLAN.md:286-291`) is agent-side only; nothing enforces or records the announcement |
| CLI (this plan, before this revision) | `open` and `doctor` read `.metiche`; nothing writes one; `binding/` is a reader only (§5) | closed by §1.10 |

---

## 2. `doctor` in depth

### 2.1 The check model

```go
type Check struct {
    ID          string            // stable: "claude.connection"
    Client      string            // "machine" | "claude" | "cursor" | "windsurf" | "codex" | "identity" | "binding" | "account"
    Mode        string            // "through" | "server" | "around"
    Status      string            // "ok" | "warn" | "error" | "skip" | "info"
    Summary     string            // one line, redacted
    Evidence    map[string]string // command run, parsed status, http status, file path, mode bits — never values
    Remediation string            // the exact next action, empty when ok
}
```

**Ordering rules** (the runner enforces them):
1. Machine checks first; a failing `machine.endpoint` turns every server-mode check into `skip`
   with "the endpoint is unreachable".
2. Per client: presence → config file → registrations → entry shape → **through probe** →
   **server delivery evidence** → around token check.
3. Identity checks next, across every token that was read; then binding and account checks
   (§2.2.1), last.
4. One root cause, one error. When `claude.connection` already reported HTTP 401, `claude.token`
   is `info` ("same finding as connection"), not a second error.

**Detecting "points at metiche"**, for registrations: an entry whose URL host equals the endpoint
host, or whose URL or name contains `metiche`. The expected name is `metiche` (the installer's
`SERVER_NAME`).

**The expected identity** is `client_key = <machine_id>-<client>`, where `machine_id` is computed
exactly as `install.sh`'s `machine_id()`: `$METICHE_MACHINE_ID`, else `hostname`, cut at the first
dot, lowercased, with non-`[a-z0-9-]` replaced by `-`. A drift test runs the shell function and
compares (§8).

### 2.2 Machine checks

| ID | mode | source | healthy | remediation (exact) |
|---|---|---|---|---|
| `machine.endpoint.url` | around | `--url` / `METICHE_MCP_URL` / default | https (or local http); path `/v1/mcp` | path ≠ `/v1/mcp` → warn: "The endpoint path is `<p>`; metiche serves MCP at `/v1/mcp` (e.g. https://mcp.metiche.xyz/v1/mcp). Fix or unset METICHE_MCP_URL." |
| `machine.endpoint` | server | `health` with no token | answers; `database: reachable` | 404 → error: "Nothing answers MCP at `<url>`. The path is `/v1/mcp`." DNS/TLS/timeout → error: "Cannot reach `<host>`: `<err>`. Nothing else can be checked against the server until this passes." DB down → error: "metiche is up but its database is not. Retry in a few minutes; your configuration may be fine." |
| `machine.anchor.file` | around | `~/.metiche/env` (parsed, never sourced). Read from the managed `# >>> metiche >>>` block, or from a legacy file starting `# metiche — created by install.sh`; other lines in the file are the user's and are ignored. | exists; file 0600, dir 0700, owned by you; one `METICHE_TOKEN=` line of token shape (`[A-Za-z0-9._-]{16,}`, the installer's `token_shape_ok`) | missing → warn: "No ~/.metiche/env. Run the installer: curl -fsSL https://metiche.xyz/install.sh \| sh". Mode → error: "~/.metiche/env is mode `<m>` and holds a token. Run: chmod 600 ~/.metiche/env". `METICHE_CLIENT_KEY` present → warn: "~/.metiche/env still exports METICHE_CLIENT_KEY from an install before per-agent tokens. Re-run the installer; it rewrites the file." |
| `machine.anchor.token` | around | `whoami` with the anchor | accepted; `token_scope: agent` | 401 with the database reachable → error: "The server rejects the anchor token in ~/.metiche/env (HTTP 401). Tokens do not survive a server reset. Re-run the installer with a join code from your team (METICHE_JOIN_CODE, or it asks); a rejected saved token makes it start a new identity, and it says so." 401 with the database down → reported only under `machine.endpoint`. `account` scope → warn: "The anchor is a pre-per-agent account token. It still works; re-run the installer to convert it." |
| `machine.env.shell` | around | current process env, cwd's `.mcp.json` | only relevant when a project `.mcp.json` in cwd uses `${METICHE_TOKEN}` | var unset → warn: "This repository's .mcp.json expands ${METICHE_TOKEN}, which is not set in this shell. Claude Code started from here gets an empty bearer. Load it: `. ~/.metiche/env`" |
| `machine.binding` | around + server | nearest `.metiche` up from cwd; `list_teams` | the named slug is one of your teams | not on it → error: "`<path>` binds this directory to team `<slug>`, which is not one of your teams (metiche does not say whether it exists). Your agents here will stop at every call. Join it with a code from one of its members, or rebind: `metiche init --force`." A team you left (§4.11) → the same error, worded "you left `<slug>` on `<date>`". No file and several teams → info: "No .metiche here and you are on N teams; your agent will ask which one. Bind it once: `metiche init`." The other binding checks are in §2.2.1. |
| `machine.files.perms` | around | every file doctor read that holds a token: client configs and backups (`~/.claude.json`, `~/.cursor/mcp.json`, `~/.codeium/windsurf/mcp_config.json`, `$CODEX_HOME/config.toml`, `<file>.metiche-backup-<timestamp>`, `~/.metiche.metiche-backup-<timestamp>/env`, `config.toml.bak*`) | mode 0600 (no group or other bits), owned by you | error: "`<path>` holds a metiche token and is readable by others (mode `<m>`). Run: chmod 600 `<path>`" |

### 2.2.1 Binding and account checks

Inputs:
- every `.metiche` from the working directory up to `/`, the nearest first;
- the git root and normalized remote (§1.10.1);
- `list_teams`;
- `get_team_state scope=projects` for the bound team and, for the cross-team row, for each of your
  teams.

They run after the identity checks (§2.1). With `--offline` only the `binding.file` and
`binding.project_scope` rows run, and every server row is `skip`. Doctor still writes nothing:
every remediation is a command for the person.

| ID | mode | healthy | finding → remediation (exact) |
|---|---|---|---|
| `binding.file` | around | the nearest `.metiche` parses; has `team`; no key appears twice; it is a regular file | missing `team` → error: "`<path>` has no `team =` line, so agents ignore it. Run `metiche init --force` here." Duplicate key → warn: "`<path>` sets `<key>` twice; the last one wins." Unknown key named like a secret (`token`, `secret`, `code`, `key`, `password`) or any value containing `://…@` → error: "`<path>` holds something that looks like a credential (`<key>`). A .metiche must only name a team and a project. Remove it, and if it was ever committed, treat that credential as leaked." The value is never printed. |
| `binding.project_scope` | around | no `project` line in any `.metiche` above the git root | → warn: "`<path>` is above this repository (`<root>`) and sets `project = <key>`, which agents ignore: a project belongs to one repository. Remove that line; bind the repository itself with `metiche init`." |
| `binding.project` | server | the `project` in effect (from a file at or below the git root) is the bound team's project for this repository, or there is none yet | differs from the project whose `repo_url` is this repository → warn: "`<path>` says project `taqueria_tracker`, but this repository is project `taqueria` on taqueria-tracker. Agents land on `taqueria` (the repository wins, §1.10.4). Fix the file: `metiche init --force`." The key belongs to another repository → error: "Project `<key>` on `<team>` belongs to `<other repo>`, so start_session will refuse it here. Run `metiche init --force`." No project yet → info: "Project `<key>` will be created on the first start_session." No remote → `skip`: "no repository URL to compare". |
| `binding.split` | server | at most one active project on the bound team has this repository's normalized `repo_url` | ≥ 2 → **error**: "This repository is N projects on `<team>`: `taqueria` (2 live, 12s ago), `taqueria_tracker` (1 live, 3m ago). Collisions between sessions in different projects are never detected. Have the agents in `<the ones not named in .metiche>` end their sessions and start again from the repository root. There is no merge tool: let the stray project go idle (§10 Q13)." Projects with **no** `repo_url` whose key equals the derived key or the name of a directory between the git root and the working directory's parents → info, listing them: "These projects have no repository recorded and may be this repository from before it was recorded: `taqueria_tracker`." It is never an error, because that is a hint, not a match. |
| `binding.teams_split` | server | this repository is a project on at most one of your teams | ≥ 2 → warn: "Sessions from this repository go to N boards: `taqueria` on taqueria-tracker (live 12s ago), `taqueria` on hack-night (3d ago). If one is wrong, end its sessions and bind the repository: `metiche init --team <slug> --force`." |
| `account.duplicate_teams` | server | no two of your teams share a normalized name (`slugKey(name, 40)`) | → warn: "You are on N teams named "Hack Night": `hack-night` (member, 6 members), `hack-night-3f9a1c` (owner, 1 member). An agent asked to choose sees both. Keep one: bind repositories with `metiche init --team <slug>`, and leave the other with `metiche teams leave <slug>` once nothing runs there." Runs anywhere, not only in a repository. |

```
binding   /Users/me/work/taqueria  (origin github.com/mklfarha/taqueria)
  ✔ binding           .metiche names taqueria-tracker, one of your teams                       server
  ! project           .metiche says taqueria_tracker; this repository is project taqueria        server
                      → Agents land on taqueria (the repository wins). Fix the file: metiche init --force
  ✘ split             this repository is 2 projects on taqueria-tracker: taqueria, taqueria_tracker  server
                      → Collisions between them are never detected. End the sessions in taqueria_tracker
                        and start again from the repository root.
account
  ! duplicate teams   2 teams named "Hack Night": hack-night, hack-night-3f9a1c                  server
```

### 2.3 Claude Code

Data sources:
- `${CLAUDE_CONFIG_DIR:-$HOME}/.claude.json`: user scope `mcpServers`, and local scope
  `projects["<abs path>"].mcpServers`;
- `.mcp.json` files from cwd up to the git root (project scope);
- `~/.claude/plugins/installed_plugins.json`, `~/.claude/plugins/cache/metiche/metiche/<v>/`;
- `~/.claude/settings.json` `enabledPlugins`;
- the `claude` CLI.

| ID | mode | check | healthy | remediation (exact) |
|---|---|---|---|---|
| `claude.present` | around | `claude` on PATH; `claude --version` | found | absent → `skip` for all Claude checks |
| `claude.config` | around | `.claude.json` parses as JSON; mode | valid, 0600 | invalid → error: "~/.claude.json is not valid JSON; Claude Code will load no MCP servers from it. Do not edit it while Claude Code runs; restore it from ~/.claude/backups." |
| `claude.registrations` | around | every entry pointing at metiche, across local, project, user and plugin scopes | exactly one, user scope, named `metiche` | none → error: "Claude Code has no metiche server. Run the installer; it registers one at user scope with Claude Code's own token." Legacy `metiche-direct` → error: "Claude Code still has the hand-added `metiche-direct` entry, which fails on every start. Re-run the installer; it removes it and registers `metiche` with Claude Code's own token." Any other extra entry (a name the installer does not own) → error: "Claude Code has N metiche registrations: `<name> (<scope>)`, …. Remove the one you do not want: `claude mcp remove <name> -s <scope>`." (That is the removal command `claude mcp get` itself prints, and the claude CLI is the only safe writer of `~/.claude.json`.) A local or project entry shadowing user scope → warn: "In `<dir>` Claude Code uses the `<scope>` entry, not your user-scope one." A project `.mcp.json` using `${METICHE_TOKEN}` inside the metiche repository → info (deliberate shadow, see root `.mcp.json`). |
| `claude.headers` | around | header **names** on the effective entry; URL | `type: http`; URL = endpoint; header names exactly `[Authorization]` | extra `X-Metiche-Client-Key` → warn: "`<name>` also sends X-Metiche-Client-Key, a header from before per-agent tokens. The server ignores it and Claude Code does not reliably forward custom headers. Re-run the installer." Missing Authorization → error: "`<name>` sends no Authorization header, so every tool except health is refused. Re-run the installer." URL mismatch → error naming both URLs. `${…}` in Authorization at user scope → warn: "The token comes from an environment variable, which depends on how Claude Code was launched." |
| `claude.plugin` | around | installed metiche plugin version; whether the **active** install path ships `.mcp.json` defining a server; orphaned cache | not installed, or ≥ `minPluginVersion` (0.2.0) with no `.mcp.json` | active version < 0.2.0, or active `.mcp.json` defines metiche → error: "The metiche plugin `<v>` still ships its own server config, whose custom headers Claude Code drops, and Claude Code caches plugins by version. Update both: `claude plugin marketplace update metiche && claude plugin update metiche@metiche --scope user`, then restart Claude Code." Orphaned cache dir (`.orphaned_at`) → info: "An orphaned copy of plugin `<v>` remains in ~/.claude/plugins/cache/metiche; Claude Code does not load it." |
| `claude.connection` | **through** | `claude mcp get <name>` run in cwd, 30 s timeout, output captured and parsed. Only the `Status:` and `Issue:` lines are kept; header lines are dropped before any processing. **Never `claude mcp list`**, which contacts every third-party server. | `Status: ✔ Connected` | `✘` + `HTTP 401`, **and** `machine.endpoint` reports the database reachable → error: "Claude Code reached metiche and was refused (HTTP 401): the token in its config is dead. Tokens do not survive a server reset. Re-run the installer." (With the database down the same 401 is not a dead token, and is left to `machine.endpoint`.) `✘` + `MCP endpoint not found` → error: "Claude Code found no MCP endpoint at `<url>`. The path must be /v1/mcp. Re-run the installer." `✘` + `ENOTFOUND`/other → error with the redacted Issue text. `⏸ Pending approval` → warn: "A project .mcp.json here defines metiche and is waiting for your approval inside Claude Code; until then it does not connect." Unparseable → warn: "Could not read `claude mcp get` output from Claude Code `<v>`; run `claude mcp get <name>` yourself." |
| `claude.delivery` | **server** | `health` → `t0` (server clock); `claude.connection` probe; then `whoami` with the entry's token → any `recent_requests` entry with `user_agent` not starting `metiche-cli/` and `last_at ≥ t0` | a request carrying Claude Code's token arrived after `t0` | `✔ Connected` but no such request → **error**: "Claude Code reports Connected, but the server never received its token: Claude Code connected WITHOUT the Authorization header. This happens when the header comes from a plugin's .mcp.json. Re-run the installer, which registers the server with `claude mcp add --header`." |
| `claude.token` | around | `whoami` with the entry's token | accepted; `token_scope: agent`; `client_key` = `<machine_id>-claude` | 401 (not already reported) → error as for `connection`. `client_key` differs → warn: "Claude Code's token belongs to agent `<client_key>`, not `<expected>`: it was copied from another client or machine. Re-run the installer so Claude Code gets its own." `account` scope → warn (legacy). |

### 2.4 Cursor

Data sources:
- `~/.cursor/mcp.json`;
- `.cursor/mcp.json` from cwd up to the git root;
- `cursor-agent` (the CLI Cursor installs; it reads the same files);
- IDE logs: `~/Library/Application Support/Cursor/logs/<newest>/window*/exthost/anysphere.cursor-mcp/MCP user-<name>.*.log`
  on macOS, `~/.config/Cursor/logs/…` on Linux.

| ID | mode | check | healthy | remediation (exact) |
|---|---|---|---|---|
| `cursor.present` | around | `~/.cursor` exists, or `cursor` / `cursor-agent` on PATH | found | `skip` |
| `cursor.config` | around | JSON valid; mode | valid, 0600 | as Claude |
| `cursor.registrations` | around | entries pointing at metiche, global and project | exactly one global `metiche` | a project `.cursor/mcp.json` with a literal bearer → **error**: "`<path>` is inside a repository and holds a metiche token. Remove that entry; the installer configures Cursor globally in ~/.cursor/mcp.json." Duplicates → error, naming them. None → error: "Cursor has no metiche server. Run the installer." |
| `cursor.headers` | around | `url`; header names | URL = endpoint; `[Authorization]` | `${env:…}` in Authorization → warn: "Cursor reads this token from the environment, and a Cursor launched from the Dock does not have your shell's. Re-run the installer, which writes Cursor's own token literally." Other cases as Claude. |
| `cursor.connection` | **through** (Cursor's agent CLI, not the IDE process) | `cursor-agent mcp list-tools metiche`, 30 s. **Never `mcp list`**, which starts every configured server. Never `mcp enable` / `mcp login`. | `Tools for metiche (N):` with N > 0 | `MCP 'metiche' requires authentication.` → error: "Cursor could not authenticate to metiche. See cursor.token: either the token is dead or Cursor is not sending it." Any other output → error with the redacted first line. The label says "Cursor's agent CLI": the IDE process can still differ (approval state). |
| `cursor.delivery` | **server** | as `claude.delivery`, around the `list-tools` probe | request after `t0` | `list-tools` succeeded but no request carried Cursor's token → error: "Cursor connected without its token." Neither → covered by `connection`. |
| `cursor.log` | **through** (the IDE's own log) | newest session's `MCP user-metiche.*.log`: last 200 lines, `[error]` entries, redacted | no `[error]` since `mcp.json` last changed | error lines → warn with the latest redacted line and its time: "The Cursor IDE logged errors for metiche at `<time>`." Failure detector only: silence is not proof. Log format for an auth failure is unobserved (§10). |
| `cursor.token` | around | `whoami` | as Claude, expecting `<machine_id>-cursor` | as Claude |

### 2.5 Windsurf

Data source: `~/.codeium/windsurf/mcp_config.json`. **No through-the-client check exists**: the
docs describe no CLI, no logs and no status command, and Windsurf is not installed on this machine.
Doctor says so rather than inventing one.

| ID | mode | check | healthy | remediation (exact) |
|---|---|---|---|---|
| `windsurf.present` | around | `~/.codeium/windsurf` exists, or `windsurf` on PATH | found | `skip` |
| `windsurf.config` | around | JSON valid; mode | valid, 0600 | as Claude |
| `windsurf.registrations` | around | entries pointing at metiche | exactly one `metiche` | as Cursor |
| `windsurf.headers` | around | `serverUrl` or `url` (docs accept both); header names | exactly one URL key (or both equal) = endpoint; `[Authorization]` | both keys differing → error: "Windsurf's metiche entry has serverUrl and url set to different endpoints; which one Windsurf uses is undocumented. Re-run the installer." `${env:…}` → warn as Cursor. |
| `windsurf.connection` | — | none available | — | always `skip`: "Windsurf has no command that tests its own connection. Prove it with: `metiche doctor --prove windsurf`." |
| `windsurf.token` | around | `whoami` | as Claude, expecting `<machine_id>-windsurf` | as Claude |

### 2.6 `--prove <client>`: server evidence for GUI clients

For Windsurf, Codex.app and the Cursor IDE, the only through-the-artifact proof is the server
seeing the client's own request:

1. `health` → `t0` (server clock).
2. Print: "Restart `<client>` (or reload its MCP servers) now. Waiting up to 120 s…"
3. Every 3 s, `whoami` with that client's configured token, until some `recent_requests` entry has a
   `user_agent` not starting `metiche-cli/` and `last_at ≥ t0`.
4. Found → ok: "The server received `<client>`'s token at `<time>` from `<user_agent>`." Timeout →
   error: "`<client>` did not reach metiche with its token within 120 s. If it shows the server as
   failed, its token or URL is wrong (see above); if it shows it connected, it is not sending the
   Authorization header."

### 2.7 Codex

Data sources:
- `${CODEX_HOME:-~/.codex}/config.toml`, parsed read-only with go-toml;
- `codex mcp get <name> --json`;
- `${CODEX_HOME}/logs_*.sqlite` (highest number), opened `file:<path>?mode=ro`.

| ID | mode | check | healthy | remediation (exact) |
|---|---|---|---|---|
| `codex.present` | around | `codex` on PATH; `codex --version` | found | absent but `$CODEX_HOME` exists → warn (the installer's rule: without the CLI nothing can prove the entry) |
| `codex.config` | around | TOML parses (error with line and column); mode | valid, 0600 | parse error → error: "`<path>`:`<line>`:`<col>`: `<msg>`. Codex will not load this file." |
| `codex.registrations` | around | `[mcp_servers.*]` tables pointing at metiche | exactly one `metiche` | duplicates → error: "Codex has N metiche servers (`<names>`). Re-run the installer for `metiche`; delete any other stale table from `<path>` by hand. Do not use `codex mcp remove`, which rewrites your other tables." |
| `codex.headers` | around | `url`, `http_headers` keys, `bearer_token_env_var`, `env_http_headers` | URL = endpoint; `http_headers` keys exactly `[Authorization]`; no env-based auth | `bearer_token_env_var` or `env_http_headers` set → **error**: "Codex reads metiche's token from $`<VAR>`. Codex launched from the Dock or Finder does not read your shell profile, so the variable is missing and Codex refuses to start the server; its log says 'Environment variable `<VAR>` … is not set'. Re-run the installer; it writes Codex's own token into http_headers." Extra `X-Metiche-Client-Key` → warn (legacy, re-run the installer). |
| `codex.readback` | **through** (Codex's own config reader; *not* a connection test) | `codex mcp get metiche --json`: `enabled`, `disabled_reason`, `transport.type`, `transport.url`, header **names** (values discarded while decoding) | `enabled: true`, `streamable_http`, URL = endpoint, and it agrees with doctor's own parse | error → error: "Codex cannot read its metiche entry: `<redacted stderr>`." Disagreement → error: "Codex reads `<field>` as `<x>` but the file says `<y>`." `enabled: false` → error: "Codex has metiche disabled (`<reason>`)." |
| `codex.startup` | **through** (Codex's own log) | `SELECT ts, feedback_log_body FROM logs WHERE ts >= :config_mtime AND target = 'codex_mcp::rmcp_client' AND level = 'WARN' AND feedback_log_body LIKE 'MCP server startup failed%' AND feedback_log_body LIKE '%server_name="metiche"%' ORDER BY id DESC LIMIT 20`. The schema is checked first (table `logs` with those columns); rows older than `config.toml`'s mtime count only as history. | no failures since the config last changed | contains `Environment variable` … `is not set` → error, same text as `codex.headers` env. Contains `handshaking with MCP server failed` → error: "Codex tried to start metiche N times since config.toml last changed and the handshake failed (latest `<time>`). Usually a dead token or a wrong URL; see codex.token." Unknown schema or missing DB → `skip`: "Codex's log format is not one this doctor knows (Codex `<v>`)." Only older failures → info: "N startup failures predate your current config." Failure detector only: silence is not proof; use `--prove codex`. |
| `codex.token` | around | `whoami` | as Claude, expecting `<machine_id>-codex` | as Claude |
| `codex.agents_md` | around | `${CODEX_HOME}/AGENTS.md`: exactly one `<!-- metiche:start -->` … `<!-- metiche:end -->` block, ending within the first 32 KiB (Codex's `project_doc_max_bytes`, the installer's `AGENTS_MAX_BYTES`) | present once, inside the budget | missing → info: "Codex has no standing metiche instruction, so it will not call start_session unprompted. Re-run the installer." Past 32 KiB or duplicated markers → warn: "Codex truncates AGENTS.md at 32 KiB and will not read the metiche block. Re-run the installer." |

**Deliberately not used:** `codex doctor --json`. Its `mcp.config` check evaluates environment
variables in the terminal's environment, which is exactly the environment a GUI-launched Codex does
not have; it would pass on the machine where Codex.app fails. And `codex mcp list`, which adds
nothing over `get` for one server. The installer notes that the codex CLI writes into `CODEX_HOME`
even to read. That is Codex writing its own state, not the CLI; `--no-exec` avoids it.

### 2.8 Identity checks (across every token read)

Inputs: `whoami` for the anchor and for each client token that the server accepted.

| ID | mode | healthy | remediation (exact) |
|---|---|---|---|
| `identity.collapse` | server | every configured client resolves to a **distinct** agent | two clients, same agent → error: "`<A>` and `<B>` carry the same token, so they are ONE agent on the board and can end each other's sessions. Re-run the installer; it gives each client its own." |
| `identity.person` | server | every accepted token resolves to **one** `account_key` | several → error: "The tokens on this machine belong to N different accounts: `<client>` → `<account>` … A join without your anchor created a second person. Re-run the installer with METICHE_TOKEN set to the token of the account you want to keep." |
| `identity.anchor` | server | the anchor's agent is one of this machine's configured clients, or it is a legacy account token | otherwise → warn: "~/.metiche/env names agent `<client_key>`, which no client on this machine uses." |
| `identity.retired` | server | no accepted token names a retired agent | (a retired agent's token is already 401 at the edge, so this surfaces as 401 above) |

### 2.9 Every failure from that night → the check that catches it

| # | failure | check(s) | mode |
|---|---|---|---|
| 1a | token never reached the client (no profile line; `${METICHE_TOKEN}` in a shell-less client) | `machine.env.shell`, `claude.headers` / `cursor.headers` / `windsurf.headers` env warnings, `claude.delivery` | around + server |
| 1b | GUI Codex without the env var, refusing to start | `codex.headers` (env-based auth), `codex.startup` (the exact log line) | around + through |
| 2 | Claude Code drops custom headers from a plugin `.mcp.json` | `claude.plugin` (active plugin ships a server), `claude.delivery` (Connected, yet no request with the token) | around + server |
| 3 | wrong endpoint path | `machine.endpoint.url`, `machine.endpoint` (404), `claude.connection` ("MCP endpoint not found"), the `*.headers` URL mismatch | around + server + through |
| 4 | two agents collapsing into one identity | `identity.collapse`, `*.token` client_key mismatch | server |
| 5 | stale duplicate registration (`metiche-direct`) | `claude.registrations`, `codex.registrations`, `cursor.registrations` | around |
| 6 | plugin config cached by version | `claude.plugin` | around |
| 7 | tokens dead after a server reset | `machine.anchor.token`, `claude.connection` (401), `cursor.connection`, `codex.startup`, `*.token` | through + around |
| 8 | an agent cannot learn its own identity | not a doctor check: the `whoami` tool (§4.1), and `metiche status` | server |
| 9 | (2026-09-12, later) one repository split into two projects by two agents' different `project_key`s | `binding.split`, `binding.project`, `binding.project_scope`; prevented by the `start_session` fix and `metiche init` (§1.10) | server + around |
| 10 | a second team with the same name on one account | `account.duplicate_teams`; prevented by `create_team`'s guard (§4.8) | server |

---

## 3. Identity for the CLI itself

**Decision: the CLI authenticates with the anchor, and never becomes an agent.**

Credential resolution, in order:
1. `METICHE_TOKEN` from the environment. This is the variable `install.sh` honours, and it is never an argument.
2. `METICHE_TOKEN=` in `~/.metiche/env`, read as text and never sourced.
3. **Fallback, only when 1–2 are absent or rejected with 401:** the tokens in the four client
   configs, each checked with `whoami`. The fallback is used only if every accepted client token
   resolves to the same `account_key`. It prints on stderr: `credential: Codex's token (the anchor
   in ~/.metiche/env was rejected — run metiche doctor)`. If they resolve to different accounts, exit
   3: "the tokens on this machine belong to N accounts; run metiche doctor".

Why this and not the alternatives:

- **Everything the CLI calls is account- or member-scoped.**
  - `list_teams` resolves the account from any token.
  - `get_team_state` goes through `RequireTeam`, which checks membership only.
  - The invite tools (§4) resolve a member through `RequireTeam`.
  - Any token of the person sees the whole person, which is what `status` must show.
- **The CLI never touches a session**, so `RequireSession`'s cross-agent refusal
  (`session %q belongs to another of your agents …`) never applies. The CLI must not end, heartbeat
  or claim through anybody's session, and with a borrowed agent token it cannot reach another
  client's sessions anyway.
- **A `<machine_id>-cli` agent would cost more than it buys.** It needs a join and a token on disk
  that the installer must mint, rotate and remove. It also creates an agent row that never works,
  and that row shows in team state (and on the board, for a public team). What it would buy is attribution of invite actions to "the CLI", but
  invites are attributed to the **member** (`created_by_member_uuid`, `revoked_by_member_uuid`),
  which is the right unit.
- **The installer already owns the anchor** and keeps it live. The CLI adds no credential of its
  own, so there is nothing new to leak, rotate or uninstall.

`doctor` is the one command that also reads **each client's own token**. It uses each one only to
call `whoami` for that client, keeps it in memory for the run, and never uses it for anything else.

**When the anchor is dead.** "Dead" means a 401 while an anonymous `health` reports the database
reachable. A 401 with the database down is exit 4 ("metiche's database is down"), never a dead
token.
- `status`, `teams`, `open`, `init` and `invite` fall back as above, or exit 3 with: "no usable
  metiche token on this machine. Run `metiche doctor` to see why, then re-run the installer."
  `teams create` additionally needs an **agent** token: a legacy account anchor falls back to a
  client's agent token too (§1.4.1).
- `doctor` reports `machine.anchor.token` as an error and continues with every client token
  independently.
- Recovery is the installer's job. Re-running it with a rejected saved anchor starts a new identity
  (`dead_token_note`) and needs a join code or a team name. The doctor's remediation is always
  "re-run the installer", never a command that handles a token.

### What this asks of install.sh (owned there, not here)

Already in `11782d0`: `--uninstall`, legacy convergence (`metiche-direct`, `X-Metiche-Client-Key`,
`METICHE_CLIENT_KEY`), the managed env block, timestamped backups, and the 401-plus-health rule.
Still needed:

1. **Phase 3:** `install_cli` and the final `metiche doctor` run (§6.3). `--uninstall` already
   removes `~/.metiche`, and with it `~/.metiche/bin`. It must also remove `~/.local/bin/metiche`,
   but only when that is a symlink into `~/.metiche/bin`; otherwise the symlink is left dangling.
2. **Partial death.** Today a rejected saved anchor starts a new identity even when a client's own
   token is still accepted. For example: the anchor was Claude Code's token, and that token was
   later rotated by a join that did not carry it. The new identity is a second account, and
   `identity.person` would then fire. Before starting one, try each client's current token as the
   anchor, under the CLI's one-account rule, and start a new identity only if none is accepted.
   After a full server reset every token is dead at once, so that case does not change.

---

## 4. Server-side changes

Six tools, two scopes and one changed tool, plus guidance text. Every other gap is closed without a
new tool, for the reasons in §4.7. All code lives in `code/backend/metiche/app/mcp/` and is
registered in `server.go:newServer` next to `list_teams`. `AllowedRoutes` does not change. Errors
from the new and changed tools start with a stable code and a colon (`not_permitted: …`,
`not_found: …`, `invalid_argument: …`, `rate_limited: …`, `already_exists: …`), so the CLI maps them
to exit codes without matching prose.

| change | kind | who may call it | § | phase |
|---|---|---|---|---|
| `whoami` | new tool | anyone; token optional | 4.1 | 1 |
| `get_team_state scope=projects`, `team` block | new scope | live member | 4.2 | 1 |
| `get_team_state scope=members` | new scope | live member | 4.9 | 1 |
| `list_invites` | new tool | member (own invites), owner (all) | 4.3 | 2 |
| `create_invite` | new tool | member (capped), owner | 4.4 | 2 |
| `revoke_invite` | new tool | the invite's creator, owner | 4.5 | 2 |
| `create_team`: duplicate-name guard, `allow_duplicate_name` | changed tool | unchanged (unauthenticated allowed); the guard applies when a token is carried | 4.8 | **0** |
| `create_team`: identity defaults for an agent token | changed tool | unchanged | 4.8 | 2 |
| `rename_team` | new tool | **owner** | 4.10 | 2 |
| `leave_team` | new tool | **the member themself**, never another | 4.11 | 2 |
| guidance: `list_teams` note and description, `create_team` description, server instructions, skill, Codex block | text | — | 4.12 | **0** |
| `start_session` repository match (committed `1b44075`, owned elsewhere) | changed tool | unchanged | 4.12 | prerequisite |

### 4.1 `whoami`

The gap: an agent cannot learn its own identity, and a human cannot prove which headers a client
actually delivers.

| | |
|---|---|
| Params | `{}` |
| Annotations | `readOnly` (ReadOnlyHint true, DestructiveHint false, OpenWorldHint false) |
| Authorization | none required. Unauthenticated calls return `authenticated: false` and touch no database. A present but invalid token is refused with HTTP 401 before the tool runs (unchanged). |
| Rate limit | none: the unauthenticated path does no I/O; the authenticated path is the auth every tool already pays |
| Errors | none beyond transport |

```json
{
  "ok": true,
  "authenticated": true,
  "token_scope": "agent",
  "account_key": "AC-3f9c",
  "agent": {
    "key": "AG-12", "label": "claude on laptop", "client_key": "laptop-claude",
    "client_kind": "claude", "status": "active"
  },
  "teams": 2,
  "recent_requests": [
    {"user_agent": "claude-code/2.1.270", "last_at": "2026-09-20T18:02:11Z"},
    {"user_agent": "metiche-cli/0.1.0 (darwin/arm64)", "last_at": "2026-09-20T18:02:14Z"}
  ],
  "recent_requests_scope": "this server process, since 2026-09-20T09:00:00Z",
  "headers_received": ["Accept", "Authorization", "Content-Type", "Mcp-Protocol-Version", "Mcp-Session-Id", "User-Agent"],
  "server_time": "2026-09-20T18:02:14Z",
  "protocol_version": "1.0",
  "note": "Your token identifies agent laptop-claude. You never need client_key."
}
```

- `token_scope` is `agent` | `account` (legacy) | `none`. `agent` is null unless the scope is
  `agent`.
- **`headers_received`**: the header NAMES on this request, from `req.Extra.Header` (go-sdk
  `RequestExtra.Header`, the same source `safetool.go:logMissingIdentityHeader` uses), canonicalized
  and sorted. Ingress-added names (`X-Forwarded-*`, `X-Real-Ip`, `Forwarded`) are filtered so the
  list reflects what the *client* sent. **Values never.** It is the in-client version of the server
  log that proved the header drop: an agent inside a misbehaving client calls `whoami` and sees
  `authenticated: false` with no `Authorization` in the list.
- **`recent_requests`**: an in-process record kept in `authMiddleware` (`auth.go`) for every HTTP
  request whose token resolves.
  - Keyed by agent id (or account id for legacy tokens).
  - Up to 8 entries of `{user_agent truncated to 80 printable chars, last_at}`, deduplicated by
    user agent.
  - Agents unseen for an hour are swept.
  - Returned only to a token of the same agent or account.
  - This is what makes `*.delivery` and `--prove` possible: `initialize` and `tools/list` from a
    client's own health check pass through the middleware even though no tool runs.
- **Why in memory, not a column:** it needs no schema change and no DB write on the hot path, and
  `agent.last_seen_at` keeps its current meaning (it is written by `heartbeat`, `sessions.go`).
  v1 runs one pod (`METICHE_ROLE=all`), so the record is complete. `recent_requests_scope` says
  what it covers; if the deployment splits into several replicas, doctor reports delivery as
  "unproven" instead of "failed".
- `note`, for `none`: "No token arrived on this request. If your client's config has an
  Authorization header, the client did not send it." For `account`: "This is a pre-per-agent
  account token: it names a person, not this client. Re-run the installer."
- Description, for agents: "Who am I? Returns the agent your token names (label, client_key) and
  the header names this request carried. Call it instead of guessing an identity, and when a
  metiche call fails in a way that looks like authentication."

### 4.2 `get_team_state scope=projects`

The gap: nothing lists projects. **Decision: a scope, not a tool, and not `list_teams`.**
`list_teams` is read by a model deciding where private work goes, and its type comment deliberately
limits it to five fields. Projects are team state.

| | |
|---|---|
| Params | existing `GetTeamStateParams`; `scope` gains `projects`; `cursor` and `limit` apply; `since_sequence` is ignored |
| Annotations | unchanged (`readOnly`) |
| Authorization | unchanged: `RequireTeam` (live member) |
| Errors | the invalid-scope message becomes "scope must be one of sessions, events, me, projects" |

**Also: a `team` block on every `get_team_state` response, whatever the scope:**
`"team": {"slug": "…", "name": "…", "visibility": "private" | "public"}`.
- `RequireTeam` has already loaded the team row, so it costs no query.
- It is how `open` and `status` tell a board a browser can show from one that 404s.
- It stays out of `list_teams` for the same five-field reason as projects.
- Tests: a default-created team reports `private`; a team set public in the test database reports
  `public`, on every scope.

```json
{"ok":true,"key":"…","sequence":417,"revision":88,"pending":{…},
 "team":{"slug":"taqueria-tracker","name":"Taqueria Tracker","visibility":"private"},
 "scope":"projects",
 "projects":[{"key":"taqueria","name":"taqueria","repo_url":"https://github.com/…","default_branch":"main",
              "live_sessions":2,"last_activity_at":"2026-09-20T18:02:00Z"}],
 "next_cursor":"…"}
```

Only `project.status` active projects. Ordered by `last_activity_at` desc, then `key`.
`last_activity_at` is the maximum over the project's sessions of `last_heartbeat_at` and
`started_at`. `live_sessions` counts sessions with status `live`.

### 4.3 `list_invites`

| | |
|---|---|
| Params | `team_slug` (optional, `RequireTeam` rules), `include_inactive` bool (default false), `cursor`, `limit` (1–50, default 20) |
| Annotations | `readOnly` |
| Authorization | live member via `RequireTeam`. `member.role = owner` sees every invite; `member` sees only invites they created. |
| Errors | `RequireTeam`'s errors |

```json
{"ok":true,"team_slug":"taqueria-tracker",
 "invites":[{"invite_id":"9f2a61c0-…","label":"hack night","code_hint":"7QX2",
             "status":"active","uses":2,"max_uses":5,"expires_at":"2026-09-14T21:00:00Z",
             "created_by":"Mark","created_at":"…","last_used_at":"…",
             "revoked_at":null,"revoked_by":null,"mine":true}],
 "next_cursor":null,
 "note":"Codes are shown only when an invite is created. To share a code you no longer have, create a new invite and revoke the old one."}
```

- **The code is never returned.**
- `code_hint` is the last 4 characters, omitted when the code is shorter than 12.
- `status` is *effective*: an `active` row whose `expires_at` has passed reports `expired`, and one
  whose `uses ≥ max_uses` reports `exhausted`. This is the same predicate `redeemInvite` applies.

### 4.4 `create_invite`

| | |
|---|---|
| Params | `team_slug` (optional); `label` (≤ 80, optional); `max_uses` (1–1000, optional = unlimited); `expires_in_hours` (1–720, optional = never); `idempotency_key` (required, ≥ 8 chars) |
| Annotations | `additive` (DestructiveHint false, IdempotentHint false, OpenWorldHint false): two keys make two invites; the key makes a *retry* safe |
| Authorization | live member via `RequireTeam`. **Owner:** unrestricted. **Member:** `max_uses` ≤ 25 and `expires_in_hours` ≤ 168 are required; omitted values default to 10 and 72 (decided, §10 Q1). |
| Rate limit | new `createInviteLimit`, keyed by account id; `METICHE_CREATE_INVITE_PER_HOUR`, default 30 |
| Errors | `invalid_argument: …` (bounds, missing key), `not_permitted: members may create invites with at most 25 uses and 7 days`, `rate_limited: …`, `RequireTeam`'s errors |

```json
{"ok":true,"team_slug":"taqueria-tracker","invite_id":"9f2a61c0-…",
 "join_code":"<the code>","join_code_note":"Shown once. Share it with the people you want on this team; anyone holding it can join until it expires, is used up, or is revoked.",
 "label":"hack night","max_uses":5,"expires_at":"2026-09-14T21:00:00Z","status":"active","created":true}
```

- **Idempotency without a schema change:** the invite id is a UUIDv5 of (member id, idempotency
  key), the same trick `create_team` uses to derive its primary key. A retry hits the primary key
  and returns the stored row with `created: false`.
- The code is minted with `MintJoinCode` and inserted like `ensureFirstInvite` does.
- **No `team_event`:** `EventKind` has no invite kinds (adding one is a nuzur schema change), invites
  are not board state, and the invite row already records who created and revoked it.
- **Not enforced:** plan limits. Nothing in `app/` reads `plan.max_members` today (verified), and
  this tool does not start.
- The code is never logged. It is not in any event, so there is no snapshot to redact.

### 4.5 `revoke_invite`

| | |
|---|---|
| Params | `team_slug` (optional), `invite_id` (full uuid) |
| Annotations | DestructiveHint **true**, IdempotentHint true, ReadOnlyHint false, OpenWorldHint false. A revocation cannot be undone, and anyone holding the code loses the ability to join, so honest clients should confirm. |
| Authorization | live member via `RequireTeam`. Owner: any invite on the team. Member: only invites they created. |
| Errors | `not_found: no invite <id> on <team>` (also for an id on another team), `not_permitted: only the team's owner or the invite's creator can revoke it` |

The update:

```sql
UPDATE invite SET status = REVOKED, revoked_at = now, revoked_by_member_uuid = :member, updated_at = now
WHERE id = :id AND team_uuid = :team AND revoked_at IS NULL
```

When it affects zero rows and the row exists with `revoked_at` set, the result is `already_revoked:
true` and `ok`. Revocation takes effect on the next redemption: `redeemInvite` already requires
`revoked_at IS NULL AND status = ACTIVE` and reads with `WithSkipCache`.

```json
{"ok":true,"invite_id":"9f2a61c0-…","status":"revoked","already_revoked":false,"uses":2,
 "note":"Members who already joined keep their access. Nobody new can join with this code."}
```

### 4.6 Tests the server changes must carry

- **`whoami`:**
  - no token → `authenticated: false`, no DB call;
  - agent token → agent fields;
  - legacy account token → `account` scope;
  - a retired agent's token → HTTP 401;
  - `headers_received` never contains a value: a canary header value `CANARY-…` sent with a real
    request is absent from the whole response;
  - through the real transport (`httptest` + SDK client, as in `identity_integration_test.go`):
    requests with user agents `A` and `B` both appear in `recent_requests`, and a request with no
    token appears in nobody's.
- **`scope=projects`:** a project with and without live sessions; ordering; pagination.
- **Invites:**
  - owner and member visibility;
  - **the code is absent from every `list_invites` response** (canary);
  - member caps refused, with `not_permitted` / `invalid_argument` prefixes;
  - idempotent create returns the same id and code;
  - revoke is idempotent;
  - a member revoking the owner's invite is refused;
  - **create → `join_team` with the code succeeds → revoke → `join_team` fails with
    `ErrInviteNotUsable`**, end to end through the real transport.
- **Team writes, members, and the `create_team` guard:** the lists in §4.8–§4.11.
- Run with `-p 1` and `METICHE_TEST_MYSQL_DSN` (`…?parseTime=true&interpolateParams=true`), as the
  existing suite does.

### 4.7 Deliberately not added

| candidate | why not |
|---|---|
| a separate identity-debug tool | folded into `whoami` (`headers_received`, `recent_requests`): one question, "what does the server see of me" |
| `list_projects` | a scope on `get_team_state` (§4.2) |
| `list_agents` / `retire_agent` | doctor detects collapse and stale identities from per-token `whoami`. Retiring stale server-side agents is a real gap, left for later (§10 Q4) |
| REST routes for the CLI | the REST API is default-deny by design; the MCP endpoint is the one public door |
| rotating a token from the CLI | tokens are minted and kept by `join_team`, which the installer drives. A second writer of client tokens is exactly what `IDENTITY.md` removed. |
| `list_members` | a scope on `get_team_state` (§4.9) |
| `set_team_visibility`, `remove_member`, `set_member_role`, `archive_team` (for now) | acts on exposure or on other people, on anonymous accounts. They come after board login's deploy, which fixes the board serving a team made private (F2) and keeps the board itself read-only. Owner-only MCP tools then (§1.4.5, §10). |
| `merge_projects`, to heal a split repository | the `start_session` fix stops new splits. Folding sessions, claims and conflicts from one project into another is a data migration that needs owner judgement, not a CLI verb. Doctor reports splits (§2.2.1); no merge tool now (§10 Q13). |
| the server reading `.metiche`, or deriving a **team** from `repo_url` | the server never reads the disk (`PLAN.md:303-304`), and a team inferred from a URL is the guess `PLAN.md` forbids. A *project* matched within the chosen team is not that guess (§1.10.4). |

The surface goes from 13 registered tools (8 in `server.go`, 3 work tools, 2 instruction tools) to 19:
six new tools. The two scopes and the `create_team` change add none.

### 4.8 `create_team`: refuse a second team with the same name, and mint nothing for a caller who already has an agent

The gap is in Context: dedupe is by idempotency key only. Agents choose a fresh key per attempt, so
an agent that retries "create a team called X" after a lost response, or in a new conversation,
makes a second X.

| | |
|---|---|
| Params | existing `CreateTeamParams`, plus `allow_duplicate_name` bool (default false). With an **agent token**, `client_key`, `member_name` and `agent_label` become optional (below). |
| Annotations | unchanged (`additive`) |
| Authorization | unchanged: unauthenticated calls are allowed. The guard applies only when the request carries a token, because only then is there an account with teams. |
| Errors | new `already_exists: …`; existing validation messages gain the `invalid_argument:` prefix |

**The guard, in order inside `CreateTeam`**, after the rate limit and validation
(`createteam.go:102-127`):
1. Derive `teamID` as today (`createteam.go:129-130`).
2. **Replay first.** If a team exists at `teamID`, return it as today (`created: false`), with no
   duplicate check. Otherwise a retry of the very call that created the team would be refused by its
   own result. Replay-safe by construction.
3. **Then the guard.** The request carries a token, `allow_duplicate_name` is false, and this
   account has a live membership (`revoked_at IS NULL`, status active) in an **active** team whose
   `slugKey(name, 40)` equals `slugKey(team_name, 40)`. Then refuse:

   `already_exists: you are already on a team named "Hack Night" (slug: hack-night). Use it: pass team_slug="hack-night" on your calls; to attach another client, call join_team with that team_slug. Only if the person explicitly wants a second team with this name, call create_team again with allow_duplicate_name: true.`

   Every matching slug is listed. Only teams the caller is a live member of are ever named, so the
   refusal leaks nothing. A same-named team you are not on, have left, or that is inactive does not
   block.
4. **Concurrency.** The check and the insert run under a lock on the caller's `account` row
   (`SELECT id FROM account WHERE id = ? FOR UPDATE`, in one transaction with `ensureTeam`'s
   insert), so two concurrent calls with different keys serialize and the second is refused. If
   `teammod`'s insert cannot join that transaction without invasive change, the race is accepted for
   v1: `account.duplicate_teams` (§2.2.1) is the backstop, and the concurrency test names which
   behaviour shipped.

Normalization is `slugKey`, the function that makes slugs (`handler.go:285-305`). So "Hack Night",
"hack-night" and " HACK  night " are one name, exactly when they would have produced the same slug
base. `install.sh:1351-1352` compares `ascii_downcase` of the raw name and should switch to the same
rule (owned there). Its check stays, because it refuses before anything is written.

**Identity defaults for an agent token.** `metiche teams create` needs these (§1.4.1); they are
independent of the guard.
- When the token names an agent: `client_key` defaults to that agent's, and a *different* one is
  refused with `RequireAgent`'s message (`auth.go:673-677`). `member_name` defaults to the account's
  display name (`ensureMember` already falls back to `acct.DisplayName`, `join.go`), and
  `agent_label` to the agent's label. `joinAs` then keeps the token (`token_kept: true`,
  `join.go:162-171`), and no agent or token is minted.
- With no token, or a legacy account token, the three stay required as today
  (`createteam.go:115-121`).

**Description.** This replaces the first sentence of `server.go:186-189`; the rest is unchanged:
"Create a new metiche team and join it. **Call list_teams first**: if you are already on a team
with this name, use its slug instead. create_team refuses a second team with the same name on your
account unless you pass allow_duplicate_name: true, which you should do only when the person
explicitly asked for a second team. If you were given a join code, use join_team instead."

Tests:
- same name, a different idempotency key, same token → `already_exists:` naming the existing slug,
  and no team row created;
- **replay**: the call that created the team, retried → `created: false`, not refused;
- `allow_duplicate_name: true` → created, with the `-xxxxxx` slug suffix;
- normalization table: "Hack Night", "hack-night" and " HACK  night " refused; "Hack Nights"
  allowed;
- a same-named team the account is **not** on, has left, or that is inactive → allowed, and a canary
  slug for that team never appears in any error;
- unauthenticated first contact → allowed;
- two concurrent calls with different keys → exactly one team, or, if the lock was not taken, a
  test named for the accepted race that asserts doctor's warning instead;
- agent token with no `client_key`, `member_name` or `agent_label` → `token_kept: true`, no `token`
  in the response, agent-row count unchanged; a different `client_key` → refused.

### 4.9 `get_team_state scope=members`

The gap: nothing lists who is on a team and which agents they run there, which `metiche teams show`
needs. **A scope, not a tool**, for §4.2's reason.

| | |
|---|---|
| Params | existing; `scope` gains `members`; `cursor` and `limit` apply |
| Annotations | unchanged (`readOnly`) |
| Authorization | unchanged: `RequireTeam` (live member). Members see members, as the board already shows them. |
| Errors | the invalid-scope message becomes "scope must be one of sessions, events, me, projects, members" |

```json
{"ok":true,"scope":"members","team":{"slug":"taqueria-tracker","name":"Taqueria Tracker","visibility":"private"},
 "members":[{"key":"mark","display_name":"Mark","role":"owner","joined_at":"…","last_seen_at":"…","mine":true,
             "agents":[{"key":"AG-12","label":"claude on laptop","client_kind":"claude","status":"active","last_session_at":"…"}]}],
 "next_cursor":null}
```

- Live members only (`revoked_at IS NULL`, status active); owners first, then by `joined_at`.
- **`agents` lists only agents with at least one session on this team.** Agents are account-wide
  (`agent.account_uuid`, no team column), so listing every one would show teammates the labels of
  clients you use on *other* teams. Your own row (`mine: true`) lists all your active agents.
- No `client_key`, no account key, nothing derived from a token.
- Tests: a revoked member is absent; an agent used only on another team is absent from a
  teammate's view and present in your own; ordering; pagination.

### 4.10 `rename_team`

| | |
|---|---|
| Params | `team_slug` (optional, `RequireTeam` rules); `name` (1–120, at least one letter or digit, as `create_team`); `allow_duplicate_name` bool |
| Annotations | `idempotent`: the same name twice changes nothing |
| Authorization | live member via `RequireTeam` **and** `member.role = owner`; otherwise `not_permitted: only an owner can rename a team` |
| Errors | `invalid_argument`; `not_permitted`; `already_exists` (§4.8's guard, over the caller's *other* teams); `RequireTeam`'s errors |

- Updates `team.name` and `updated_at` under the team row lock. **Never `slug`**: `.metiche` files,
  board URLs and installs name the slug, and a rename that moved it would unbind every clone
  silently.
- Result: `{"ok":true,"team_slug":"…","previous_name":"…","name":"…","changed":true}`. The same name
  gives `changed: false`.
- **Event.** `EventKind` has no rename kind (`enums/event_kind.go:15-37`). The decision (§10 Q10) is to
  add `team_renamed` in the nuzur model in phase 2, as a structural event, so `board_revision` moves
  and boards re-render the title. Until it exists, the board shows the new name on its next full load.
- Tests: an owner renames; a member is refused; the slug is unchanged; a duplicate is refused, and
  allowed with the flag; idempotent.

### 4.11 `leave_team`

| | |
|---|---|
| Params | `team_slug` (optional, `RequireTeam` rules) |
| Annotations | DestructiveHint **true**, IdempotentHint true: the caller loses access, and getting it back takes a new invite |
| Authorization | the caller's own membership only. There is no member argument: removing someone else waits for board login (§1.4.5). |
| Errors | `not_permitted: …` for each refusal below; `RequireTeam`'s errors |

Refusals, checked under the team row lock:
1. The account has sessions on the team with status `live` or `stale` →
   `not_permitted: end your sessions on <slug> first: S-41 (claude on laptop, live 12s ago), …`. The
   server does not end them itself: a session ended by anyone but its agent confuses that agent, and
   the sweeper already handles abandoned ones.
2. The caller is the team's only owner and other live members remain →
   `not_permitted: you are the only owner of <slug>; ownership cannot be transferred yet`.
3. The caller is the only live member →
   `not_permitted: you are the last member of <slug>; leaving would orphan it and its invites`.

- Otherwise: `member.revoked_at = now`, status inactive. That is the soft removal
  `liveMemberships` already honours (`auth.go:718-720`). Already revoked → `already_left: true`.
- Your invites stay valid: they belong to the team, and an owner can revoke them. A leaver who
  redeems a still-valid invite is reinstated (`ensureMember`, `join.go:339-360`), which by design is
  the invite issuer's decision.
- **Event.** The decision (§10 Q10) is to add `member_left` in the nuzur model in phase 2, as a
  structural event, since a lane disappears. Until it exists, the lane goes on the next full load.
- **`RequireTeam`'s error for a former member.** When the slug names a team where the caller has a
  *revoked* membership row, the message is `not_found: you left <slug> on <date>` instead of the
  generic one. It reveals the team's existence only to a former member, and it is how `doctor`'s
  `machine.binding` explains why a `.metiche` stopped working.
- Tests: leave → `list_teams` no longer lists the team → `get_team_state` refused with the
  former-member message; each refusal; an idempotent re-leave; a leaver cannot `start_session` on
  the team.

### 4.12 What this plan needs from the `start_session` fix, and the guidance text

The fix is committed (`1b44075`) and owned elsewhere (`sessions.go`, `server.go`, the skill). It matches projects by
normalized `repo_url`, derives the key when `project_key` is omitted, backfills `repo_url`, refuses a
key bound to another repository, and notes existing projects when it creates one. `metiche init` and
doctor depend on it for four things:
1. **`scope=projects` returns the normalized `repo_url`.** §4.2's example already carries the
   field.
2. **The normalization and the key derivation are published as a vector table** (input URL →
   normalized URL → derived key). It is checked into the backend's tests and mirrored under
   `code/cli/testdata/`, with a CLI test that fails on drift. At minimum these four must normalize
   alike:
   - `git@github.com:mklfarha/taqueria.git`
   - `https://github.com/mklfarha/taqueria`
   - `ssh://git@github.com/mklfarha/taqueria.git`
   - `https://<user>:<token>@github.com/mklfarha/taqueria.git` — this one proves `userinfo` is
     stripped and never stored.
3. **Precedence as in §1.10.4: the repository first** (decided, §10 Q5; `1b44075` implements it).
   When the passed `project_key` differs from the matched project's key, the note names both.
   **`1b44075` does not do this yet** (§1.10.4, "Gap"), so it is still owed by the server's owner.
4. **The refusal names the other repository's normalized URL**, so `init` and the agent can say
   which repository holds the key.

The guidance text is written by the owners of the server and the skill. This plan fixes only what it
must say (§1.10.5):
- `list_teams` note (`listteams.go:173-197`): write `.metiche` **at the git root**, not the working
  directory, and prefer `metiche init --team <slug>` when it is available.
- `list_teams` description (`server.go:239-242`): `.metiche`'s `team` wins; `project` counts only
  from a file at or below the git root.
- `create_team` description: "call list_teams first" (§4.8).
- Server instructions (`server.go:115-160`): one line before step 2: "Find your team first: the
  nearest .metiche file's team, else list_teams. Pass repo_url to start_session."
- The skill and its plugin copy: a "Which team, which project" section carrying §1.10.5 items 1–4.
  The worked example's `project:` becomes `project_key:` and gains `repo_url:`.
- The installer's Codex `AGENTS.md` block (`install.sh:2120-2141`): one step before `start_session`
  with the same rule, keeping the block well inside 32 KiB.

---

## 5. Code layout

```
code/cli/
  go.mod                      module github.com/mklfarha/metiche/cli   (go version = code/backend/metiche/go.mod)
  main.go                     os.Exit(cmd.Main(os.Args[1:], os.Stdout, os.Stderr))
  .goreleaser.yaml            §6.1
  internal/
    buildinfo/                Version, Commit, Date — set by -ldflags -X; "dev" otherwise
    cmd/                      dispatcher (stdlib flag, one FlagSet per command) + status.go teams.go (list, create,
                              show, rename, leave) open.go init.go invite.go doctor.go version.go uninstall.go;
                              exit-code mapping in one place; prompt.go (TTY-only numbered choice, no default)
    config/                   endpoint (--url > METICHE_MCP_URL > default), board base, HOME / CLAUDE_CONFIG_DIR /
                              CODEX_HOME resolution, machine_id (install.sh's algorithm)
    secret/                   type Secret; String/GoString/MarshalJSON/Format all yield "<redacted>"; Reveal() used
                              only by mcpclient's RoundTripper. Redactor seeded with every secret read this run.
    credential/               anchor (env, ~/.metiche/env), client-token fallback, one-account rule
    mcpclient/                Dial(ctx, url, Secret) over go-sdk; typed calls; error classification
    wire/                     result structs mirroring the tools the CLI calls (never imports the backend)
    binding/                  .metiche nearest-wins reader (team =, project =, # comments; project ignored above
                              the git root) and the ONE file writer in the binary (atomic rename, in-place line
                              rewrite keeping comments, symlink and ownership refusal, canary-tested)
    gitx/                     git root and remote through execx; URL sanitizing (userinfo stripped, raw value
                              seeds the redactor), normalization and key derivation from §4.12's vectors
    clients/
      client.go               interface: Name, Detect, Registrations, Probe, Logs
      claude/ cursor/ windsurf/ codex/   config readers, CLI output parsers, log readers
    execx/                    run a client CLI: fixed argv allowlist, context timeout, stdin closed,
                              output capped at 1 MiB, returned only to a parser
    doctor/                   Check model, runner (ordering rules §2.1), identity checks, render
    render/                   tables, relative times, JSON envelope, colour policy
  testdata/                   fixtures (§8)
```

**MCP client.** `mcp.NewClient(&mcp.Implementation{Name: "metiche-cli", Version: buildinfo.Version}, nil)`,
then `client.Connect(ctx, &mcp.StreamableClientTransport{…}, nil)` with:
- `Endpoint: url`;
- `HTTPClient: &http.Client{Transport: authRT, CheckRedirect: refuseCrossHost, Timeout: …}`;
- `MaxRetries: -1`, since a doctor must see the first failure rather than retry past it;
- `DisableStandaloneSSE: true`, since the CLI wants no server push.

`authRT` wraps `http.DefaultTransport`: it sets `Authorization: Bearer <Reveal()>` (omitted for the
unauthenticated `health` / `whoami` probes) and `User-Agent`, and records the last HTTP status. That
status is how 401 and 404 (exit 4, "wrong path") are told apart from the SDK's wrapped errors. A
401 is classified as a rejected token (exit 3) only after an anonymous `health` on a fresh
connection reports `ok` and `database: reachable`, which is `install.sh`'s `token_is_dead` rule.
Otherwise it is exit 4. Results are decoded from the tool's JSON content into `wire` structs. Production decoding
tolerates unknown fields; tests decode strictly (§8).

**Why not share types with the backend:** `github.com/mklfarha/metiche/backend` drags in fx, grpc,
the MySQL driver and generated code. The contract is the tool's JSON, and a strict-decode
integration test guards drift.

**Config readers:**
- **JSON** (Claude, Cursor, Windsurf): `encoding/json` into minimal structs. For example,
  `struct{ MCPServers map[string]json.RawMessage; Projects map[string]struct{ MCPServers map[string]json.RawMessage } }`
  for the 155 KB `.claude.json`. Header maps are decoded as `map[string]string` and **values are
  moved into `Secret` immediately**.
- **TOML** (Codex): `github.com/pelletier/go-toml/v2`. It is pure Go, decodes into `map[string]any`
  including inline tables (`http_headers = { … }`), and its `DecodeError` carries line and column,
  which is what `codex.config`'s remediation prints.
- **SQLite** (Codex logs): `modernc.org/sqlite`, pure Go. The release is `CGO_ENABLED=0` like
  nuzur-cli, which rules out `mattn/go-sqlite3`; shelling out to `sqlite3` is not guaranteed on
  Linux. Opened `file:<path>?mode=ro`. It costs a few MB of binary.
- **CLI framework:** none. Nine commands, two with subcommands, and a handful of flags fit stdlib
  `flag`, keeping the dependency list to the SDK, go-toml and sqlite.

**Client CLI allowlist** (`execx`, enforced by a test):
- `claude --version`, `claude mcp get <name>`;
- `codex --version`, `codex mcp get <name> --json`;
- `cursor-agent --version`, `cursor-agent mcp list-tools <name>`;
- `git rev-parse --show-toplevel`, `git config --get remote.origin.url`, `git remote`,
  `git check-ignore -q .metiche`, `git ls-files --error-unmatch .metiche`, each run with its working
  directory set to the directory being bound. Read-only: never `add`, `commit` or `config` writes.
  `GIT_TERMINAL_PROMPT=0`, and no network subcommand.

`<name>` must match `^[A-Za-z0-9._-]{1,64}$` and come from a registration already read.

---

## 6. Release pipeline

Based on `/Users/mklfarha/Dropbox/nuzur-24/code/nuzur-cli/.goreleaser.yaml`,
`/Users/mklfarha/Dropbox/nuzur-24/code/nuzur-cli/.github/workflows/release.yml`,
`.github/workflows/go.yml` and its `install.sh`.

What is kept from nuzur-cli:
- GoReleaser `version: 2`;
- `CGO_ENABLED=0`;
- the `uname`-compatible archive name template;
- `release.mode: replace` (a partially failed release can be re-run);
- `setup-go` with `go-version-file`, never `stable`: nuzur's release broke when `stable` floated to
  Go 1.27;
- `goreleaser-action@v6` with `version: "~> v2"`;
- an installer that resolves the tag through the GitHub API, not `releases/latest/download`, because
  a release exists before its assets finish uploading;
- sha256 verification with `sha256sum` or `shasum -a 256`;
- no sudo.

What changes, and why:

| nuzur-cli | metiche | why |
|---|---|---|
| version is a Go constant (`constants.CLI_VERSION`) | ldflags `-X …/buildinfo.Version={{.Version}}` | the tag is the single source; no constant to forget |
| `before: go mod tidy; go generate` | `before: go mod download` | a release must not mutate the tree it builds |
| linux, darwin, **windows** | linux, darwin | the installer is POSIX sh only, and doctor's config paths are unverified on Windows (later) |
| brews + scoops taps | none | `install.sh` is the one owner of the binary and of uninstall; a brew-owned copy would be a second owner |
| workflow on `tags: "*"` **and** `pull_request` | release on `tags: v*` only; a separate CI workflow for PRs | a release job should never run on a PR |
| changelog from git, whole repo | `changelog.disable: true`, hand-written release notes | this monorepo's commits are mostly not the CLI; path-filtered changelogs are GoReleaser Pro |
| no signing, no SBOM | none in phase 3 (later: cosign keyless on the checksums) | parity with nuzur-cli; the trust root is GitHub over HTTPS either way |

### 6.1 `code/cli/.goreleaser.yaml`

```yaml
version: 2
project_name: metiche

before:
  hooks:
    - go mod download

builds:
  - id: metiche
    main: .
    binary: metiche
    env: [CGO_ENABLED=0]
    goos: [linux, darwin]
    goarch: [amd64, arm64]
    flags: [-trimpath]
    mod_timestamp: "{{ .CommitTimestamp }}"
    ldflags:
      - -s -w
      - -X github.com/mklfarha/metiche/cli/internal/buildinfo.Version={{ .Version }}
      - -X github.com/mklfarha/metiche/cli/internal/buildinfo.Commit={{ .ShortCommit }}
      - -X github.com/mklfarha/metiche/cli/internal/buildinfo.Date={{ .CommitDate }}

archives:
  - formats: [tar.gz]
    # identical scheme to nuzur-cli: `uname -s` / mapped `uname -m` produce these names
    name_template: >-
      {{ .ProjectName }}_
      {{- title .Os }}_
      {{- if eq .Arch "amd64" }}x86_64
      {{- else }}{{ .Arch }}{{ end }}
    files: [LICENSE*]

checksum:
  name_template: "{{ .ProjectName }}_{{ .Version }}_checksums.txt"
  algorithm: sha256

release:
  github: { owner: mklfarha, name: metiche }
  mode: replace
  prerelease: auto

changelog:
  disable: true
```

Assets per release `vX.Y.Z`: `metiche_Darwin_arm64.tar.gz`, `metiche_Darwin_x86_64.tar.gz`,
`metiche_Linux_arm64.tar.gz`, `metiche_Linux_x86_64.tar.gz`, `metiche_X.Y.Z_checksums.txt`.

**Tags.** The repo has no tags today (verified), and images are tagged by short commit
(`deploy/scripts/build-images.sh`), so `v*` is free for the CLI. GoReleaser OSS has no monorepo tag
prefix. (Decided: bare `v*` tags are reserved for CLI releases, §10 Q2.)

### 6.2 Workflows

`.github/workflows/cli-release.yml`:

```yaml
name: cli-release
on:
  push:
    tags: ["v*"]
permissions:
  contents: write
jobs:
  goreleaser:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with: { fetch-depth: 0 }
      - uses: actions/setup-go@v5
        with: { go-version-file: code/cli/go.mod, cache-dependency-path: code/cli/go.sum }
      - run: go test ./...
        working-directory: code/cli
      - uses: goreleaser/goreleaser-action@v6
        with:
          distribution: goreleaser
          version: "~> v2"
          args: release --clean
          workdir: code/cli
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
```

`.github/workflows/cli-ci.yml`, on pushes and PRs touching `code/cli/**`, `install.sh`,
`code/frontend/static/install.sh`:
- `go vet`, `gofmt -l`, `go test ./...` on `ubuntu-latest` **and** `macos-latest` (the two sha256
  tools and the two `/bin/sh`s, nuzur's reason);
- `goreleaser check`;
- `goreleaser build --snapshot --clean` (the whole matrix compiles);
- `sh -n install.sh` and `dash -n install.sh`;
- `cmp install.sh code/frontend/static/install.sh`.

### 6.3 How `install.sh` gets the binary

New in `install.sh` (mirrored to `code/frontend/static/install.sh`), as one `install_cli` step that
runs **after** client configuration and before `handle_profile`. A failed download is a warning,
never a failed install: the joins are the product.

- Flags and env: `--no-cli` skips the step. `METICHE_CLI_VERSION` pins a version (`v` optional). Both
  go on the `sh` side of the pipe, nuzur's documented pitfall.
- **OS:** `uname -s` → `Darwin` or `Linux`, which GoReleaser's `title .Os` already renders. Anything
  else → `info "the metiche CLI has no build for <os>; skipping"`.
- **Arch:** `uname -m` → `x86_64|amd64` → `x86_64`, `aarch64|arm64` → `arm64`; else skip.
- **Version:** pinned, or `tag_name` from `https://api.github.com/repos/mklfarha/metiche/releases/latest`
  read with `sed` (no jq dependency for this step). Failure →
  `warn "could not resolve the latest metiche CLI (GitHub API: <url>); set METICHE_CLI_VERSION=vX.Y.Z to pin one"`.
- **Idempotence:** if `~/.metiche/bin/metiche version --short` equals the version, print
  `metiche CLI <v> already installed` and stop.
- **Download** with `curl -fsSL --retry 3` into `mktemp -d` (removed by `trap … EXIT`):
  - `https://github.com/mklfarha/metiche/releases/download/v<V>/metiche_<OS>_<ARCH>.tar.gz`
  - `https://github.com/mklfarha/metiche/releases/download/v<V>/metiche_<V>_checksums.txt`
- **Verify:**
  - `want=$(grep " metiche_<OS>_<ARCH>.tar.gz\$" checksums.txt | cut -d' ' -f1)`;
  - `got` from `sha256sum`, else `shasum -a 256`;
  - neither tool present → refuse ("cannot verify, not installing");
  - empty `want` or `want ≠ got` → refuse, naming both URLs. Nothing is written.
- **Install:**
  - `tar -xzf … -C "$tmp" metiche` (single member);
  - `mkdir -p ~/.metiche/bin`, then `install -m 0755 "$tmp/metiche" ~/.metiche/bin/metiche`;
  - `~/.metiche` stays 0700.
- **PATH:**
  - If `~/.local/bin` is on `$PATH` and `~/.local/bin/metiche` is absent or already a symlink into
    `~/.metiche/bin`, run `ln -sf ~/.metiche/bin/metiche ~/.local/bin/metiche`.
  - A non-symlink file there → `warn "~/.local/bin/metiche exists and is not ours; leaving it"`.
  - `~/.local/bin` not on PATH → print `export PATH="$HOME/.metiche/bin:$PATH"` as advice. The
    installer's single profile loader line stays the only profile edit.
  - Scan `$PATH` for another `metiche` that shadows this one and say so (nuzur's check).
- **`--dry-run`:** print the two URLs and the destination; fetch nothing.
- **Final proof step:** after everything is written, run
  `~/.metiche/bin/metiche doctor --client <the clients configured this run> --json` and print one
  line per non-ok check plus the summary. The exit status is reported, not fatal: the writes already
  passed the installer's own proofs, and doctor is the through-the-client second opinion.
- **`--uninstall`:** `uninstall_env_dir` already removes `~/.metiche`, the binary included, after
  copying it to `~/.metiche.metiche-backup-<timestamp>`. Add removal of `~/.local/bin/metiche`, only
  under the symlink rule above.

### 6.4 Tag-to-binary proof

`metiche version` on a downloaded binary prints the tag's version and commit. The phase 3 proof
compares these with `git rev-parse --short v<X.Y.Z>`.

### 6.5 Self-update: deferred

- **The installer is already the update, and it does more.** Re-running it converges client
  configs, the plugin and the anchor as well as the binary. A self-updated doctor would diagnose
  stale configs that only the installer fixes.
- **No signatures yet.** A binary that replaces itself using checksums fetched from the same host
  adds a code-execution path without adding trust over re-running the installer.
- **The CLI's behaviour is not server-driven** the way nuzur-cli's deploys are (nuzur's
  `selfupdate.go` rationale). Server capabilities arrive as tools, and the CLI degrades per missing
  tool (`whoami` absent → identity checks `skip` with "server too old").

`metiche version --check` covers the "you are behind" signal. **Revisit** when release signing
lands, or when the CLI gains behaviour that drifts with the server.

---

## 7. Security

- **Never print a token.** Every token is a `secret.Secret` from the moment it is read. Its
  `String`, `GoString`, `Format` and `MarshalJSON` yield `<redacted>`. Only `mcpclient`'s
  RoundTripper calls `Reveal()`.
- **Redaction at every boundary with foreign text.** Output of client CLIs, Codex log bodies, Cursor
  log lines and server error details pass through a redactor. It is seeded with **every secret
  value read in this run** (exact-match replacement, robust to token formats) plus the pattern
  `Bearer <non-space>`. `claude mcp get` prints header values (verified), so its output is parsed in
  memory, and only `Status:` / `Issue:` lines survive.
- **Never print a join code**, except the `create_invite` result (`invite create`, including its
  `--json`) and the `create_team` result (`teams create`, including its `--json`, but not with
  `--quiet`). `list_invites` does not return codes at all: the server enforces it, not the CLI.
- **Repository URLs can carry credentials** (`https://<user>:<token>@host/…`). `gitx` strips
  `userinfo` before any other use, seeds the redactor with the raw value, never prints it, and never
  writes any URL into `.metiche`. A test runs `init` and `doctor` against a remote with a canary in
  its userinfo and asserts the canary appears nowhere: stdout, stderr, JSON, or the written file.
- **`.metiche` is the one file the binary writes**, and only from `metiche init`. It is written
  atomically at mode 0644, never through a symlink, and never outside the chosen directory. It holds
  only `team` and `project`.
- **Other tools' files are read-only.** Opened with `os.Open`. The CLI never writes, chmods, renames
  or locks them. SQLite is opened `mode=ro`. Permission problems are findings with the exact
  `chmod` for the person to run.
- **File permission findings** (`machine.files.perms`): any file holding a metiche token with group
  or other bits set, or owned by another user, is an error. `~/.metiche` must be 0700.
- **Network.** The CLI itself connects only to:
  1. the MCP endpoint;
  2. `api.github.com` for `version --check`.

  Redirects to another host are refused, so the bearer never follows a redirect. The client probes
  make the *clients* connect, and only for the metiche entry (`get <name>` / `list-tools <name>`,
  never the list forms, which contact every configured server). No telemetry.
- **HTTPS** is required, except `localhost` / `127.0.0.1` with a warning.
- **The binary must never:**
  - write any file other than a `.metiche` from `metiche init`. Board login will add one more
    exception, its sign-in redirect file (`docs/BOARD_LOGIN.md` §5.3);
  - run sudo, or any `git` subcommand outside the §5 allowlist;
  - modify a client config, or run `claude mcp add|remove`, `codex mcp add|remove`,
    `cursor-agent mcp enable|login` or `claude plugin …`;
  - call `join_team`, `start_session`, `end_session`, `heartbeat`, `declare_intent`,
    `update_intent`, `check_paths`, `get_instructions` or `report_back`;
  - call `create_team` anywhere but `teams create`, or with no agent token, or with `client_key`,
    `member_name` or `agent_label` (§1.4.1). Any of those would mint an agent or an account;
  - accept a secret as an argument;
  - pass a token to a child process (argv or env it adds);
  - log to disk;
  - execute a downloaded script.

  The MCP tool allowlist (`health`, `whoami`, `list_teams`, `get_team_state`, `list_invites`,
  `create_invite`, `revoke_invite`, `create_team`, `rename_team`, `leave_team`) is a compile-time
  list with a test.

---

## 8. Tests and verification

### 8.1 Unit: one test file per checker, fixtures under `code/cli/testdata/`

Fixture configs, including the exact legacy shapes seen on this machine (header **names** as
observed, token values replaced by canaries `mtk_CANARY_<client>_0123456789`):

| fixture | shape |
|---|---|
| `claude/healthy.json` | user `metiche`: `type: http`, `/v1/mcp`, `Authorization` only |
| `claude/legacy-direct.json` | user `metiche-direct` with `Authorization` + `X-Metiche-Client-Key`, **no** `metiche` (this machine) |
| `claude/duplicate.json` | `metiche` and `metiche-direct` both at user scope |
| `claude/local-shadow.json` | `projects["/work/x"].mcpServers.metiche` with another URL |
| `claude/wrong-path.json` | URL `https://mcp.metiche.xyz/mcp` |
| `claude/env-placeholder.json` | `Authorization: Bearer ${METICHE_TOKEN}` at user scope |
| `claude/invalid.json` | truncated JSON |
| `claude-plugins/active-0.1.0/` | `installed_plugins.json` lists `metiche@metiche` 0.1.0; cache `.mcp.json` with both header names |
| `claude-plugins/orphaned-0.1.0/` | cache dir with `.orphaned_at`, not in `installed_plugins.json` (this machine) |
| `codex/healthy.toml`, `codex/legacy-client-key.toml` (this machine), `codex/bearer-env-var.toml`, `codex/env-http-headers.toml`, `codex/duplicate.toml`, `codex/malformed.toml` | as named |
| `codex/logs_2.sqlite` | built by the test with the real `logs` schema; rows with the two observed bodies, one older and one newer than the config mtime; a row for another server |
| `codex/logs-unknown-schema.sqlite` | `logs` without `feedback_log_body` |
| `cursor/healthy.json`, `cursor/env-interp.json`, `cursor/project-literal-token/.cursor/mcp.json` | as named |
| `windsurf/serverUrl.json`, `windsurf/url.json`, `windsurf/both-differ.json` | as named |
| `metiche/env-block` (the managed `# >>> metiche >>>` block), `metiche/env-block-with-user-lines`, `metiche/env-legacy-client-key` (the pre-v4 header shape on this machine), `metiche/env-0644`, `metiche/env-garbage` | as named |
| `cli-output/claude-get-401.txt`, `claude-get-notfound.txt`, `claude-get-enotfound.txt`, `claude-get-connected.txt`, `claude-get-pending.txt`, `claude-get-absent.txt` | the verbatim texts observed with Claude Code 2.1.270, header lines kept **with canary values** so the parser's dropping of them is tested |
| `cli-output/cursor-list-tools-ok.txt`, `cursor-list-tools-auth.txt` | observed texts (cursor-agent 2026.07.01) |
| `cli-output/codex-get.json` | observed key structure, canary values |

Stub client CLIs: `testdata/bin/{claude,codex,cursor-agent}` are sh scripts that print a chosen
fixture, so parsers run through `execx` exactly as in production.

**Required unit tests:**
- every row in §2.2–§2.8 has a passing fixture and a failing fixture;
- **the redaction canary**: every command runs against every fixture with `--json` and without,
  and no canary appears in stdout, stderr or the JSON;
- the `execx` argv allowlist and the MCP tool allowlist;
- `machine_id` parity: runs `install.sh`'s `machine_id` in `sh` with a set of `METICHE_MACHINE_ID`
  and hostname inputs and compares;
- `minPluginVersion` equals `plugin/.claude-plugin/plugin.json`'s version;
- endpoint resolution and exit-code mapping tables;
- `open` and `status` against a stub server: a public team opens or prints its URL; a private team
  opens nothing, prints no URL, exits 1 and gives `board_url: null` in JSON (the stub opener records
  that it was never called).
- **`init`**, in scratch git repositories against a stub server:
  - team resolution: flag, inherited, only team, TTY prompt (Enter alone is not accepted), no TTY →
    exit 2 with `candidates`;
  - placement: default, `--here` below the root, `--parent`, `--parent --project` → 2, `--here
    --parent` → 2, no git → cwd with team only;
  - project sources: flag, server repository match, several matches → prompt or 2, derived from the
    remote, derived from the directory name, a key bound to another repository → 6;
  - a different binding → 6 with the diff and no write; `--force` keeps comments and unknown keys;
  - a re-run leaves the file byte-identical with its mtime unchanged;
  - a symlinked `.metiche` is refused; `--dry-run` writes nothing;
  - the reader ignores `project` above the git root.
- **The written `.metiche` never contains a canary**: a token, a join code, or URL userinfo, under
  any input.
- **Repository-URL vector parity** with the backend (§4.12), like `machine_id` parity.
- **`teams`:**
  - duplicate-name marking;
  - `teams create` sends none of `client_key`, `member_name`, `agent_label` (the stub server records
    the arguments), refuses without an agent token (3), refuses when the schema still requires them
    (1), and its `--quiet` output contains no code;
  - `teams leave` with no TTY and no `--yes` exits 2 and makes no call.
- **Doctor §2.2.1 rows**: each has a passing and a failing fixture, including the incident's
  two-projects-one-repository state.

### 8.2 Integration: against a locally built backend

`code/cli/integration_test.go`, gated on `METICHE_TEST_MYSQL_DSN` like the backend suite:
- apply `core/repository/sql/schema/create.sql` and `deploy/sql/seed-default-plan.sql`;
- `go build` `code/backend/metiche` and start it on a free port with `METICHE_ROLE=all`;
- point `METICHE_MCP_URL` at it.

1. Mint two agents with `create_team` / `join_team` through the SDK, as the installer does.
2. `metiche status --json` with each token as `METICHE_TOKEN`: the same account and teams; the
   projects and sessions from a `start_session` appear; **strict decoding** of every tool result
   (`DisallowUnknownFields`), so a server change the `wire` structs do not know fails here.
3. `metiche invite create` → `join_team` with the code succeeds → `metiche invite revoke` →
   `join_team` fails with the invite-not-usable error → `metiche invite list --all` shows `revoked`
   and no code anywhere in its output.
4. Visibility: the team `create_team` made reports `private` in `get_team_state`, and
   `metiche open --print` exits 1 with no URL. After the test sets it public in the database, it
   prints `<board>/t/<slug>`.
5. Exit codes: an unknown token → 3; `METICHE_MCP_URL` ending `/mcp` → 4 with the path message; a
   member revoking the owner's invite → 5.
6. Binding, in a scratch git repository with remote `https://github.com/example/demo.git`:
   - `metiche init` (one team) writes `team` and `project = demo`, and a re-run is `unchanged`;
   - `start_session` with that `repo_url` and no `project_key` lands on project `demo` (needs the
     fix);
   - a second project with the same `repo_url`, inserted in the test database → `metiche doctor
     --json` reports `binding.split` as an error;
   - the file rewritten to `project = other` → `binding.project` warns.
7. Teams:
   - `teams create "Demo"` → created; again → exit 6 naming the slug; `--allow-duplicate-name` → a
     second slug;
   - `doctor` then reports `account.duplicate_teams`, and `whoami`'s agent is unchanged throughout;
   - `teams rename` by a member → 5;
   - `teams leave` with a live session → 5; after `end_session` it succeeds;
   - `open`, run inside the directory whose `.metiche` names the team just left, exits 5 with the
     former-member message.

### 8.3 End to end: doctor must catch every failure from that night

`deploy/scripts/smoke-doctor.sh` (POSIX sh, reusing `lib.sh`) runs on top of the installer smoke
test from `IDENTITY.md` Step 7 (`deploy/scripts/smoke-install.sh`, which does not exist yet):
- start the local backend;
- set `HOME`, `CLAUDE_CONFIG_DIR` and `CODEX_HOME` to scratch directories;
- run `install.sh` for every client it can;
- **assert `metiche doctor` exits 0**;
- then, for each row below: reproduce the failure, assert `metiche doctor --json` reports the
  listed check with the listed status, restore, and assert it is clean again.

| # | reproduction in the scratch HOME | must report | real client needed |
|---|---|---|---|
| 1a | project `.mcp.json` with `${METICHE_TOKEN}`, env unset | `machine.env.shell` warn | no |
| 1b | Codex table rewritten to `bearer_token_env_var = "METICHE_TOKEN"`; the recorded startup-failure row inserted into a scratch `logs_2.sqlite` after the config mtime | `codex.headers` error **and** `codex.startup` error (env class) | codex CLI for `readback` |
| 2 | Claude user-scope entry with its `Authorization` header removed (the client now connects tokenless, as with a dropped plugin header) | `claude.connection` **ok** and `claude.delivery` **error** (the case where ✔ lies) | **claude** |
| 2b | scratch plugin install at 0.1.0 whose `.mcp.json` defines metiche | `claude.plugin` error | no |
| 3 | every entry's URL changed to `/mcp` | `claude.connection` error ("MCP endpoint not found"), `*.headers` URL error; `--url …/mcp` → `machine.endpoint` error | claude for `connection` |
| 4 | Cursor's token copied into Codex's config | `identity.collapse` error, `codex.token` client_key warn | no |
| 5 | `claude mcp add --scope user metiche-direct …` beside `metiche` | `claude.registrations` error naming `metiche-direct` | claude (to add) |
| 6 | as 2b, with `installed_plugins.json` at 0.1.0 | `claude.plugin` error with the update command | no |
| 7 | drop and recreate the database (server reset) | `machine.anchor.token` error, `claude.connection` error (401), every `*.token` error | claude for `connection` |
| 7b | stop MySQL with the backend still running (every token now gets 401) | `machine.endpoint` error ("database is not"); **no** dead-token errors | claude for `connection` |
| 8 | (server) `whoami` with each client's token returns that client's `client_key` | covered by §4.6 and step 2 of §8.2 | no |
| + | `chmod 644` on `~/.metiche/env` and on `$CODEX_HOME/config.toml` | `machine.anchor.file` error, `machine.files.perms` error | no |
| + | a second account: a client joined with no anchor | `identity.person` error | no |

Rows needing a real `claude` or `codex` binary run where one is installed (the owner's machine, a
macOS CI runner with Claude Code installed) and are **reported as not run** elsewhere, never
silently passed. The script prints a table of row, ran or not, and result.

### 8.4 Mutation checks: a check that cannot fail is decoration

For each row in §8.3 the proof includes one deliberate break of the checker, the failing row, and
the restore:
- skip header-name comparison → row 5's legacy header passes → the test fails;
- treat `✔ Connected` as delivery → row 2 passes → fails;
- drop the `ts >= config_mtime` filter → a clean config after a fix reports old failures → the
  "restore, assert clean" step fails;
- compare `client_key` instead of `agent_key` for collapse → row 4 passes → fails;
- drop the `health` confirmation from 401 classification → row 7b reports dead tokens → fails;
- remove the value-seeded redactor → the canary test fails;
- remove `Authorization` from `authRT` → integration step 2 exits 3.

On the server:
- `list_invites` returning `code` → the canary fails;
- `revoke_invite` not setting `revoked_at` → the join-after-revoke test succeeds and the test fails;
- `recent_requests` recorded after the tool instead of in the middleware → the `claude.delivery`
  row fails (tokenless connect and token connect become indistinguishable).

### 8.5 The owner's machine as a live fixture

This machine is currently in the pre-v4 state recorded in the Context table. The phase 1 proof runs
`metiche doctor` here **before** the installer is re-run, and it must report at least:
- `metiche-direct` stale and 401;
- Codex's and `~/.metiche/env`'s legacy client key;
- Codex's startup failures.

After re-running the installer it must be clean. A doctor that is green on this machine today is
broken.

---

## 9. Phases

Each phase follows `IDENTITY.md`'s execution rules:
- the coordinator writes no code;
- file ownership is explicit;
- every report carries the commands and their pasted output;
- every proof is re-run independently before commit;
- commits are authored by the owner, with no trailers;
- no credential in any file.

**Ordering, and why.** Phase 0 is server-only and small, and it stops new damage now (duplicate
teams). The `start_session` repository fix, committed elsewhere as `1b44075`, stops new splits and is a
prerequisite for `init`'s project resolution. Phase 1 gives a person the reads and the one local
write (`init`) that repairs bindings. Phase 2 adds every server write the CLI makes (invites, team
create, rename, leave), so all mutating tools ship and are proven together. Phase 3 releases.

### Phase 0 — stop duplicate teams (server and guidance only, now)

**Scope.**
- `create_team`'s duplicate-name guard and `allow_duplicate_name` (§4.8, the guard half), with its
  tests.
- The guidance text in §4.12, except what depends on `init` existing (the "prefer `metiche init`"
  clause is added in phase 1).
- `install.sh`'s duplicate check switches to `slugKey` normalization, mirrored to
  `code/frontend/static/install.sh`.

**Coordination.** The `start_session` fix that changed `server.go`, `sessions.go` and the skill is
committed (`1b44075`). Phase 0 starts `createteam.go` immediately, and its text changes to
`server.go`, the skill and `listteams.go`'s note build on that commit.

Agents with distinct files:
- A · `createteam.go` and its tests;
- B · text in `server.go`, `listteams.go`, the skill and its plugin copy, and `install.sh` (the
  block and the check), on top of the fix (`1b44075`).

**Done when:**
1. §4.8's guard tests are green through the real transport, output pasted, including the replay
   that must not be refused and the concurrency test.
2. Against a local backend: create "Phase Zero"; a second `create_team` with another key is refused
   naming the slug; `allow_duplicate_name` succeeds. On production, only the refusal half is run, on
   a team the owner already has, since it creates nothing.
3. A fresh agent session, told only "set up a metiche team called `<a name the account already
   has>`", uses the existing slug: it calls `list_teams` first, or is refused and follows the
   refusal. Transcript pasted.

### Phase 1 — server read tools, then `version`, `status`, `teams`, `teams show`, `open`, `init`, `doctor` (no server writes)

**Scope.**

Server (`app/mcp/`):
- `whoami`, with the `recent_requests` record in `authMiddleware`;
- `get_team_state scope=projects` (with the normalized `repo_url` from the fix) and
  `scope=members`;
- tests per §4.6 and §4.9;
- a deploy.

CLI:
- `code/cli` module, §5 layout;
- `version`, `status`, `teams`, `teams show`, `open`, `uninstall` (pointer only), `doctor` with
  every check in §2, including §2.2.1, and `--prove`;
- `init` (§1.10): the one local write, and the fix for the incident on the person's side;
- `--json` and exit codes;
- built with `go build`, not released.

**Prerequisite for `init`'s project half:** the `start_session` fix is deployed and its vector table
exists (§4.12). Until then `init` may ship writing `team` and a derived `project` with a warning,
but proof 9 below needs the fix.

Agents with distinct files:
- A · server: `whoami.go`, `auth.go` middleware, `state.go` (both scopes), tests;
- B · CLI core: `cmd` (except `init.go`), `config`, `secret`, `credential`, `mcpclient`, `wire`,
  `render`;
- C · doctor clients: `clients/*`, `execx`, fixtures;
- D · doctor runner: `doctor/*` (including §2.2.1), `deploy/scripts/smoke-doctor.sh`;
- E · binding: `binding/` (reader and writer), `gitx/`, `cmd/init.go`, the URL vectors, and the
  guidance clause "prefer `metiche init`" (§4.12).

C, D and E are written against B's interfaces; D and E wait for A's deploy for the server rows.

**Done when:**
1. The backend `go test -p 1 ./app/...` is green, with the new tests' output pasted.
2. `cd code/cli && go test ./...` is green, including the canary and allowlist tests.
3. The §8.2 integration test output is pasted.
4. The §8.3 table is printed with every row caught, and the not-run rows named.
5. §8.4 mutation runs are shown for at least rows 2, 4 and 7 and the redactor.
6. §8.5: `metiche doctor` on the owner's machine before and after re-running the installer, both
   outputs pasted.
7. `metiche status` on the owner's machine lists their teams, projects and live sessions.
8. Through the artifact, against production: for every board URL `status` prints, `curl -s -o
   /dev/null -w '%{http_code}'` returns 200. For every private team of the owner's, the
   `<board>/t/<slug>` URL the CLI withheld returns 404. A 200 for a private team means the premise
   has changed (a board token was configured, with no viewer gate), and it is a stop-the-line
   finding, not a CLI bug.
9. In the owner's `taqueria` clone, `metiche init` run from the git root and again from a
   subdirectory writes **one** `.metiche`, at the git root, with `project = taqueria`. The second
   run reports `unchanged`. Outputs pasted.
10. `metiche doctor` in that clone reports the existing `taqueria` / `taqueria_tracker` split as
    `binding.split` until the owner resolves it. A green doctor there before that is broken.
11. Through the artifact: two agent sessions started in that clone, one from the git root and one
    from its parent folder, both reading the committed `.metiche`, land on **one** project, and a
    deliberate overlapping `declare_intent` on one file raises a conflict. That is the incident,
    replayed and caught.

### Phase 2 — invites and team writes

**Scope.**
- Server:
  - `list_invites`, `create_invite`, `revoke_invite`, and `METICHE_CREATE_INVITE_PER_HOUR`;
  - `rename_team`, `leave_team` (with the former-member `RequireTeam` message);
  - `create_team`'s identity defaults for an agent token (§4.8);
  - the `team_renamed` and `member_left` event kinds (nuzur model change approved, §10 Q10).
- CLI: `invite list|create|revoke`; `teams create|rename|leave`.
- Docs:
  - `docs/PLAN.md` tool table (19 tools);
  - `skill/metiche-teamwork/SKILL.md` and its plugin copy ("call `whoami` instead of guessing your
    identity; invite tools only when the person asks").

**Done when:**
1. The §4.6 invite tests are green through the real transport, including create → join → revoke →
   join refused, and the list-never-contains-code canary.
2. §8.2 step 3 is green.
3. On production, the owner creates an invite with the CLI, a scratch client joins with it, the
   owner revokes it, and a second join is refused, with the outputs pasted (the code redacted in
   the report).
4. No SQL is run by hand.
5. §4.8's identity-default tests and §4.10–§4.11's tests are green; §8.2 step 7 is green.
6. On production: `metiche teams create` makes a scratch team, and `whoami` shows the same agent and
   the same agent count before and after. A second create is refused with exit 6. `teams rename`
   works, and `teams leave` by the owner is refused (only owner). Outputs pasted, the code redacted.
   The scratch team stays until archiving exists (§10); the leave path is proven on the local
   backend (§8.2 step 7).

### Phase 3 — release pipeline and installer download

**Scope.**
- `code/cli/.goreleaser.yaml`, `cli-release.yml`, `cli-ci.yml`.
- `install.sh`: `install_cli`, `--no-cli`, `METICHE_CLI_VERSION`, the post-install doctor run, the
  `--uninstall` removal of the binary and symlink, mirrored to `code/frontend/static/install.sh`.

**Done when:**
1. `goreleaser check` and `goreleaser build --snapshot --clean` run locally, with the artifact list
   pasted.
2. The tag `v0.1.0-rc.1` produces a GitHub prerelease with the 5 assets.
3. `install.sh` in a scratch HOME on darwin/arm64 **and** in a `debian` container (dash, linux/amd64)
   downloads, verifies and installs it, and `metiche version` shows the tag's commit.
4. A tampered checksum (a local HTTP fixture) makes the installer refuse and write nothing.
5. `--dry-run` fetches nothing (the proxy log is empty).
6. A re-run downloads nothing.
7. `--uninstall --dry-run`, then `--uninstall`, removes the binary and only our symlink.
8. The installer's final doctor summary appears in a real install.

### Later

- Self-update (§6.5).
- Windows builds and paths.
- Cosign keyless signing of the checksums, and an SBOM.
- `retire_agent` and `metiche agents` (§10 Q4: later, not phase 2).
- `metiche open` for private teams, once the board service has a viewer gate (browser login,
  `docs/BOARD_LOGIN.md`). Until then `open` declines private teams (§1.5).
- After board login's deploy: `set_team_visibility`, `remove_member`, `set_member_role` (ownership
  transfer, which unblocks `teams leave` for a sole owner) and `archive_team`, as owner-only MCP
  tools with `metiche teams` subcommands (§1.4.5). The board stays read-only.
- `merge_projects`, or a one-off migration, for repositories split before the fix, only if splits recur after the fix (§10 Q13).
- Several projects per repository (monorepos), if the server ever supports it (§1.10.4).
- Delivery evidence across several replicas, if the deployment splits.

---

## 10. Decisions (owner-approved 2026-09-13)

1. **Invites by plain members:** members may create invites, capped (`max_uses` ≤ 25, `expires_in_hours` ≤ 168, defaults 10 and 72) and revoking only their own; owners are unrestricted (§4.4). Adding a teammate need not wait on the owner, and the caps bound what a member's invite can admit.
2. **Tags:** bare `v*` tags are reserved for CLI releases. The repo has no tags, images are tagged by short commit, and GoReleaser OSS has no monorepo tag prefix (§6.1).
3. **The board viewer gate:** a separate board-service plan, `docs/BOARD_LOGIN.md` (sign-in links minted by `open_board`, no board token; owner-approved 2026-09-13, not implemented), not a phase here; until it ships `metiche status` is the private-team view and `open` declines private teams, and its §5.3 lists what then changes here. The gate must exist before any private board is served, and that design serves them without ever configuring a board token.
4. **Stale server-side agents:** `retire_agent` and `metiche agents list|retire` are left for later, not phase 2 (§4.7, "Later"). Doctor already detects stale and collapsed identities from per-token `whoami`, and retiring is a destructive server write that needs its own design.
5. **`project` in `.metiche` versus the repository URL:** the repository wins (§1.10.4): the server matches by normalized `repo_url`, a disagreeing `project_key` is reported, never used, and the key decides only without a remote. This agrees with commit `1b44075` on precedence and refusal; the report in the note is not implemented yet, the sent key still reaches the event summary, and the key also names the project when no project has the repository yet (§1.10.4, "Gap"). One stale or copied file must not recreate the split for every clone.
6. **`project` in a parent-directory `.metiche`:** ignored with a doctor warning, `init --parent` refuses to write one, and `PLAN.md`'s example drops its `project` line and says "defaults to the name the server derives from the repository's remote" (owned in `PLAN.md`). A parent file covers several repositories; a project is one.
7. **Should `init` suggest committing `.metiche`:** yes, print-only, never running git, with the caveat that a public repository publishes the slug, which grants no access (`PLAN.md:284-285`). Committing binds every teammate's clone with no question, and the private-by-default board makes a visible slug harmless.
8. **Duplicate-name guard:** refuse, with `allow_duplicate_name: true` as the explicit override, normalized by `slugKey` (§4.8). A warning in a successful response is what a model skims past, and a second team confuses every later "which team?".
9. **Leaving as the sole owner, or the last member:** refuse both for now (§4.11); ownership transfer and archiving come later as owner-only tools after board login (§1.4.5). Auto-promotion would make someone an owner who never asked, and a last-member leave orphans a team whose first invite never expires.
10. **Event kinds for rename and leave:** add `team_renamed` and `member_left` to the nuzur model in phase 2, both structural. A team's name and its member lanes are board state; invites are not.
11. **Visibility, member removal and roles:** owner-only MCP tools with CLI subcommands after board login's deploy, each with a structural event, not board controls (§1.4.5). The board stays read-only against the backend, and board login's F2 fix and per-request membership checks make the change take effect everywhere a person can look.
12. **Do agents write `.metiche` themselves:** they may, but prefer `metiche init --team <slug>` when `metiche` is on `PATH`, always at the git root (§1.10.5). The CLI keeps placement and format consistent, which is exactly what went wrong from the parent folder.
13. **Repositories already split** (`taqueria` / `taqueria_tracker`): no merge tool now; doctor reports the split, the owner ends the stray project's sessions and lets it go idle, and `merge_projects` is revisited only if splits recur. The fix's repository match and backfill keep new sessions on one project.
14. **Should the installer run `metiche init`:** no; it ends by printing "in each repository: `metiche init`". The installer is machine-global and runs outside any repository, and `list_teams`'s note covers repositories nobody bound.

Known unknowns that are not questions, verified during phase 1 on real installs:
- Cursor's `list-tools` and IDE-log text on an HTTP 401;
- whether Windsurf reads `url`, which the installer writes, as the docs say, or only `serverUrl`;
- how Claude Code names plugin-provided servers in `claude mcp get`.

---

## Verification, end to end

1. `go test -p 1 ./app/...` in `code/backend/metiche` and `go test ./...` in `code/cli` are green;
   `go vet` and `gofmt -l` are clean; the §8.2 integration test passes against a local backend.
2. `deploy/scripts/smoke-doctor.sh` passes, with every §8.3 row caught (or named as not run), and
   each §8.4 mutation turns its row red.
3. On the owner's machine, `metiche doctor` reports the real pre-v4 failures before re-running the
   installer and is clean after. `metiche status` shows every team, project and live session, private
   teams included, without an agent in the loop. It prints board URLs only for public teams, each of
   which answers 200, while every private team's withheld URL answers 404 (phase 1 proof 8).
4. An invite is created, used, revoked and refused through the CLI on production, with no SQL.
5. `curl -fsSL https://metiche.xyz/install.sh | sh` on a fresh machine installs a checksum-verified
   `metiche` from a tagged GitHub release, ends with a clean doctor summary, and
   `install.sh --uninstall` leaves nothing behind.
6. A repository bound with `metiche init` sends every agent session, from the root or from a parent
   folder, to one project on one team, and an overlapping edit between two of them is detected
   (phase 1 proof 11). `metiche doctor` reports a split repository, a stale `project`, a `project`
   above the git root, and a `.metiche` naming a team you are not on.
7. No account can gain a second team with a name it already has without saying
   `allow_duplicate_name`, whether through the CLI (exit 6) or an agent calling `create_team`
   directly (`already_exists`). `teams create` mints no agent and no token.
