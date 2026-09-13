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
| Scope | Read-mostly. The only writes are invite create and revoke. The CLI never touches sessions and never writes a file. |
| Doctor | Through-the-client checks where a client offers one; server evidence via `whoami`; around checks labelled as such. Never modifies anything. |
| Server additions | `whoami` (includes recent-request evidence and received header names), `get_team_state scope=projects`, `list_invites`, `create_invite`, `revoke_invite`. Four new tools, one new scope. |
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
| 2 | usage error: unknown command or flag, ambiguous team with no `--team` |
| 3 | no usable credential: none found, or every candidate rejected. A 401 counts as a rejection only when an anonymous `health` then reports the database reachable; a 401 with the database down is exit 4. |
| 4 | metiche unreachable: DNS, TLS, connect, timeout, HTTP 404 at the endpoint (wrong URL), 5xx, or `health` reporting the database unreachable |
| 5 | refused by the server: `not_permitted` or `not_found` from a tool (§4 error codes) |

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

### 1.4 `metiche teams`

The cheap, scriptable list: one `list_teams` call.

```
$ metiche teams
SLUG              NAME              ROLE    MEMBERS  YOU LIVE
taqueria-tracker  Taqueria Tracker  owner   3        yes
hack-night        Hack Night        member  6        no
```

No BOARD column: `list_teams` does not carry visibility, and printing a URL that 404s for every
private team would be wrong more often than right. `metiche status` and `metiche open <slug>` show
the board where one is viewable. Zero teams prints the server's `note` (create or join) and exits 0.

### 1.5 `metiche open`

```
metiche open [<slug>] [--print]
```

The team is resolved in this order, never guessed:
1. the argument;
2. the nearest `.metiche` walking up from the working directory (`team = <slug>`, the rule in
   `PLAN.md`);
3. `list_teams` with exactly one team.

With several teams and no binding it exits 2 and lists the slugs.

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

---

## 2. `doctor` in depth

### 2.1 The check model

```go
type Check struct {
    ID          string            // stable: "claude.connection"
    Client      string            // "machine" | "claude" | "cursor" | "windsurf" | "codex" | "identity"
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
3. Identity checks last, across every token that was read.
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
| `machine.binding` | around + server | nearest `.metiche` up from cwd; `list_teams` | the named slug is one of your teams | not a member → error: "`<path>` binds this repo to team `<slug>`, which you are not on. Fix the file or join that team." No file and several teams → info: "No .metiche here and you are on N teams; your agent will ask which one." |
| `machine.files.perms` | around | every file doctor read that holds a token: client configs and backups (`~/.claude.json`, `~/.cursor/mcp.json`, `~/.codeium/windsurf/mcp_config.json`, `$CODEX_HOME/config.toml`, `<file>.metiche-backup-<timestamp>`, `~/.metiche.metiche-backup-<timestamp>/env`, `config.toml.bak*`) | mode 0600 (no group or other bits), owned by you | error: "`<path>` holds a metiche token and is readable by others (mode `<m>`). Run: chmod 600 `<path>`" |

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
- `status`, `teams`, `open` and `invite` fall back as above, or exit 3 with: "no usable metiche
  token on this machine. Run `metiche doctor` to see why, then re-run the installer."
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

Four tools and one scope. Every other gap is closed without a new tool, for the reasons in §4.7.
All code lives in `code/backend/metiche/app/mcp/` and is registered in `server.go:newServer` next to
`list_teams`. `AllowedRoutes` does not change. Errors from the new tools start with a stable code
and a colon (`not_permitted: …`, `not_found: …`, `invalid_argument: …`, `rate_limited: …`), so the
CLI maps them to exit codes without matching prose.

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
| Authorization | live member via `RequireTeam`. **Owner:** unrestricted. **Member:** `max_uses` ≤ 25 and `expires_in_hours` ≤ 168 are required; omitted values default to 10 and 72 (owner question, §10). |
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
- Run with `-p 1` and `METICHE_TEST_MYSQL_DSN` (`…?parseTime=true&interpolateParams=true`), as the
  existing suite does.

### 4.7 Deliberately not added

| candidate | why not |
|---|---|
| a separate identity-debug tool | folded into `whoami` (`headers_received`, `recent_requests`): one question, "what does the server see of me" |
| `list_projects` | a scope on `get_team_state` (§4.2) |
| `list_agents` / `retire_agent` | doctor detects collapse and stale identities from per-token `whoami`. Retiring stale server-side agents is a real gap, but a separate decision (§10) |
| REST routes for the CLI | the REST API is default-deny by design; the MCP endpoint is the one public door |
| rotating a token from the CLI | tokens are minted and kept by `join_team`, which the installer drives. A second writer of client tokens is exactly what `IDENTITY.md` removed. |

The surface goes from 13 registered tools (8 in `server.go`, 3 work tools, 2 instruction tools) to 17.

---

## 5. Code layout

```
code/cli/
  go.mod                      module github.com/mklfarha/metiche/cli   (go version = code/backend/metiche/go.mod)
  main.go                     os.Exit(cmd.Main(os.Args[1:], os.Stdout, os.Stderr))
  .goreleaser.yaml            §6.1
  internal/
    buildinfo/                Version, Commit, Date — set by -ldflags -X; "dev" otherwise
    cmd/                      dispatcher (stdlib flag, one FlagSet per command) + status.go teams.go open.go
                              invite.go doctor.go version.go uninstall.go; exit-code mapping in one place
    config/                   endpoint (--url > METICHE_MCP_URL > default), board base, HOME / CLAUDE_CONFIG_DIR /
                              CODEX_HOME resolution, machine_id (install.sh's algorithm)
    secret/                   type Secret; String/GoString/MarshalJSON/Format all yield "<redacted>"; Reveal() used
                              only by mcpclient's RoundTripper. Redactor seeded with every secret read this run.
    credential/               anchor (env, ~/.metiche/env), client-token fallback, one-account rule
    mcpclient/                Dial(ctx, url, Secret) over go-sdk; typed calls; error classification
    wire/                     result structs mirroring the tools the CLI calls (never imports the backend)
    binding/                  .metiche nearest-wins reader (team =, project =, # comments)
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
- **CLI framework:** none. Seven commands and a handful of flags fit stdlib `flag`, keeping the
  dependency list to the SDK, go-toml and sqlite.

**Client CLI allowlist** (`execx`, enforced by a test):
- `claude --version`, `claude mcp get <name>`;
- `codex --version`, `codex mcp get <name> --json`;
- `cursor-agent --version`, `cursor-agent mcp list-tools <name>`.

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
prefix. (Owner question, §10.)

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
  `--json`). `list_invites` does not return codes at all: the server enforces it, not the CLI.
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
  - write any file;
  - run sudo;
  - modify a client config, or run `claude mcp add|remove`, `codex mcp add|remove`,
    `cursor-agent mcp enable|login` or `claude plugin …`;
  - call `join_team`, `create_team`, `start_session`, `end_session`, `heartbeat`, `declare_intent`,
    `update_intent`, `check_paths`, `get_instructions` or `report_back`;
  - accept a secret as an argument;
  - pass a token to a child process (argv or env it adds);
  - log to disk;
  - execute a downloaded script.

  The MCP tool allowlist (`health`, `whoami`, `list_teams`, `get_team_state`, `list_invites`,
  `create_invite`, `revoke_invite`) is a compile-time list with a test.

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

### Phase 1 — server read tools, then `version`, `status`, `teams`, `open`, `doctor` (read-only)

**Scope.**

Server (`app/mcp/`):
- `whoami`, with the `recent_requests` record in `authMiddleware`;
- `get_team_state scope=projects`;
- tests per §4.6;
- a deploy.

CLI:
- `code/cli` module, §5 layout;
- `version`, `status`, `teams`, `open`, `uninstall` (pointer only), `doctor` with every check in §2
  and `--prove`;
- `--json` and exit codes;
- built with `go build`, not released.

Agents with distinct files:
- A · server: `whoami.go`, `auth.go` middleware, `state.go`, tests;
- B · CLI core: `cmd`, `config`, `secret`, `credential`, `mcpclient`, `wire`, `render`, `binding`;
- C · doctor clients: `clients/*`, `execx`, fixtures;
- D · doctor runner: `doctor/*`, `deploy/scripts/smoke-doctor.sh`.

C and D are written against B's interfaces; D waits for A's deploy for the server rows.

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

### Phase 2 — invites

**Scope.**
- Server: `list_invites`, `create_invite`, `revoke_invite`, and `METICHE_CREATE_INVITE_PER_HOUR`.
- CLI: `invite list|create|revoke`.
- Docs:
  - `docs/PLAN.md` tool table (17 tools);
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
- `retire_agent` and `metiche agents` (if §10 says so).
- `metiche open` for private teams, once the board service has a viewer gate (browser login).
  Until then `open` declines private teams (§1.5).
- Delivery evidence across several replicas, if the deployment splits.

---

## 10. Open questions for the owner

1. **Invites by plain members.** Should members create invites (capped at 25 uses and 7 days, and
   revoking only their own), or only owners? The spec above allows capped member invites.
2. **Tags.** May bare `v*` tags be reserved for CLI releases in this monorepo? GoReleaser OSS has no
   tag prefix, and nothing uses tags today.
3. **The board viewer gate on the roadmap.** Today a private team has no browser view at all: the
   board runs without a board token, so `/t/<private-slug>` is a 404, and `metiche open` declines
   private teams (§1.5). A viewer gate (browser login, e.g. GitHub OAuth) is a board-service backlog
   item. It must exist before a board token is ever configured; without it, discovery would render
   any team that token can read to anyone who knows the slug.

   Should the gate join this roadmap as a phase after 3, so `open` can serve private teams? Or should
   it stay a separate board-service project, with `metiche status` as the private-team view for now?
4. **Stale server-side agents.** Old per-machine agents and agents holding account tokens linger in
   team state (and on public teams' boards). Add `retire_agent` plus `metiche agents list|retire` in phase 2, or leave it for later?

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
