# Identity redesign — the token names the agent

## Why

The first live demo failed on onboarding, not on the product. The collision loop works end to
end and has been verified repeatedly against production. What broke, every single time, was
**how an agent identifies itself**:

- The bearer token identifies the **person** (`account.token_hash`). One person runs several
  agents, so the server needs a `client_key` to tell them apart — and **an agent cannot learn
  its own `client_key`**. It is chosen by the installer and stored nowhere the agent can read.
  One agent guessed four times and was rejected four times; the other correctly refused to guess,
  because a wrong guess files work under someone else's agent on a shared board.
- The fix-of-the-night added a second channel, `X-Metiche-Client-Key`. The server log then
  proved **Claude Code drops custom headers from a plugin's `.mcp.json`** (only `Authorization`
  arrives), and `bearer_token_env_var` for Codex fails for a GUI-launched app: no shell
  environment, so Codex refuses to start the server, shows `failed (0 tools)`, and puts the real
  reason in a SQLite log nobody reads.
- The installer defaulted `client_key` to `metiche-install-<hostname>` — per **machine** — so two
  clients on one laptop collapsed into one agent unless the user typed overrides by hand.
- The Claude Code plugin caches config by version; `claude plugin update` said "already latest"
  and shipped nothing.

Every failure was on the second channel. `Authorization` is the one header every MCP client
reliably forwards. The structural fix is to make **the token name the agent**, so identity rides
on the one channel that works, and to **generate** agent identity per (machine, client) so nobody
types it.

Decisions taken: per-agent tokens (a schema change; the model is reviewed before codegen);
Claude Code is registered by the installer via `claude mcp add`; the plugin becomes skill-only;
identity is generated, never passed; and several terminals of one client must work at once.

## The model

| layer | row | identified by | lifetime |
|---|---|---|---|
| person | `account` | (legacy) `account.token_hash` | forever; may be claimed via GitHub later |
| **client install** | `agent` | **`agent.token_hash`** (new), one per (account, `client_key`); `client_key = <machine_id>-<client>`, generated | until rotated by a join that does not carry it, or retired |
| **terminal / piece of work** | `session` | `session_key` from `start_session` | until `end_session` or the sweeper abandons it |

Invariants:

- The bearer on every call names an **agent** (new installs) or an **account** (legacy). Never
  anything else. `Mcp-Session-Id` is transport state, regenerated on reconnect, and is never
  consulted for identity.
- A token minted for agent X is rotated only by a join that does **not** carry X's own token. The
  account token is never rotated again.
- An agent may hold N concurrent live sessions — one per terminal. Nothing above the session
  layer distinguishes terminals, and nothing needs to.

The principle for verification, learned the hard way: **verify through the artifact you
produced, not around it.** The installer already proves "the token works" with its own HTTP
client. That caught nothing, because the failures were in what each *client* did with the file
it was handed.

---

## Step 1 — nuzur schema draft, review, then codegen

**No codegen until the model is approved.**

- Find the project with `searchProjectsByName("metiche")`, its published base version
  (`v3-plans`) and the `agent` entity. Do not trust remembered uuids.
- `createProjectVersionDraft` `v4-agent-tokens` based on `v3-plans`.
- `addField` on `agent`: `token_hash`, varchar (7) `max_size: 64`, **not required (nullable)**.
  Description: "sha256 hex of this agent's bearer token; NULL on agents joined before v4, which
  authenticate with the account token".
- `addIndex` on `agent`: `uq_agent_token_hash`, unique, `[token_hash]`. MySQL/InnoDB permits any
  number of NULLs in a unique index, so legacy rows coexist.
- Update entity descriptions: `agent` no longer "holds no credential of its own";
  `account.token_hash` becomes "legacy anchor; new accounts carry an unusable hash".
- `sendProjectVersionForReview`. **Stop and wait.** Then `nuzur-cli` codegen into
  `code/backend/metiche` with `code/backend/nuzur-codegen.json`.
- Expected generated output: `entity/agent.Agent.TokenHash null.String`,
  `core/module/agent/FetchAgentByTokenHash`, insert/update carrying the column, `create.sql`.

## Step 2 — prod migration, applied BEFORE the new binary ships

`deploy/scripts/apply-schema.sh` is `CREATE TABLE IF NOT EXISTS` only. Add
`deploy/sql/2026-09-agent-token-hash.sql`:

```sql
ALTER TABLE `agent`
  ADD COLUMN `token_hash` VARCHAR(64) NULL AFTER `client_key`,
  ADD UNIQUE INDEX `uq_agent_token_hash` (`token_hash`);
```

Order is load-bearing. The old binary on the new schema is fine (its INSERT does not name the
column). The new binary on the old schema fails every `join_team`. Zero data migration. Verify
with `SHOW COLUMNS FROM agent` before deploying the binary.

## Step 3 — backend (`code/backend/metiche/app/mcp/`)

**3a `export_authz.go` — one resolver, two lookups.**
`type Identity struct { Account; Agent *agent_entity.Agent }`.
`IdentityByToken(ctx, db, token)`: hash; `SELECT agent JOIN account WHERE agent.token_hash = ?`
→ `VerifyToken` → **refuse if `agent.status != ACTIVE`** (a retired agent's token is dead at the
edge, MCP and board alike) → `Identity{Account, &Agent}`. On no row, the existing account SELECT
→ `Identity{Account, nil}`. Every failure is the one opaque `ErrUnauthenticated`.
`AccountByToken` delegates; **`app/authz` needs no code change** (update its comment). Keep
`WithClientKey` / `ClientKeyFromContext` as an override path.

**3b `auth.go`.** `resolveToken` returns `Identity`; add `WithIdentity` / `IdentityFromContext`;
keep `WithAccount` / `AccountFromContext` as thin wrappers so existing tests compile.
`RequireAgent`: after `RequireTeam`, if the token named an agent → use it (re-check status; if an
explicit `client_key` names a *different* agent, refuse: "this connection is agent %q"). Done —
no header, no argument. Otherwise the existing chain (argument → header → single agent → error),
with the multi-agent error rewritten: "re-run the metiche installer so each client gets its own
token". `RequireSession`: if the token names an agent and `sess.AgentUUID != agent.ID`, refuse
("belongs to another of your agents") — today any agent of the account can end any other's
session. Two terminals of the *same* client share a token and may still reach each other's
sessions; that is by design. `mintAccount`: mint and **discard** the plaintext (the column is
NOT NULL; nothing can authenticate with it). Package comment: identity comes from the
`Authorization` header of the call being served, and from nothing else on the transport.

**3c `join.go` — mint per agent.**
- `JoinTeamParams` gains `team_slug` (optional): with a token whose account is a live member,
  name the team by slug and attach another agent **without spending an invite use**. Forced by
  one-join-per-client — a `max_uses=1` invite would otherwise admit one client. No token, or not
  a member → the same opaque error as an unusable invite. Rate-limited either way.
- `joinAs` decides whether to mint: no token → `mintAccount` + mint; account token (legacy) →
  mint; agent token with the same `client_key` → **keep** (`token_kept`); agent token with a
  different `client_key` → mint/rotate that other agent only. Set `TokenHash` on the inserted or
  updated row; the update path re-reads the row inside the transaction so a kept hash survives.
- `JoinTeamResult`: keep `token`; add `token_scope: "agent"`, `token_kept`, `client_key`. Rewrite
  `token_note`: "identifies THIS agent; every client gets its own; re-joining with it keeps it".
- Snapshot redaction and the nonce idempotency key are unchanged: the token never reaches
  `team_event`.

**3d `createteam.go`.** Inherits via `joinAs`. Leave the primary-key derivation from `client_key`
alone; the installer absorbs the consequence (Step 5: `create_team` once, others join by slug).

**3e `safetool.go:authTool`.** Resolve → `WithIdentity`. Keep reading the client-key header. The
INFO "no agent identity header" log (deployed, currently uncommitted) must be gated on *"account
token AND no header"*, or it fires on every call once the header is gone. Commit it.

**3f `sessions.go:StartSession` — N terminals per agent.** Inside the locked `Apply`: count this
agent's live and stale sessions; refuse above `maxLiveSessionsPerAgent = 8`, listing them;
otherwise insert and set the note *"you already have S-7 live on feat/a (12m ago); this is S-9 —
if S-7 was this terminal's earlier run, end_session it"*, built inside `Apply` so replay matches.
The sweeper already walks live → stale → abandoned for terminals that vanish.

**3g `server.go` instructions and the `client_key` schema on `StartSessionParams`:** "Your token
identifies THIS agent. You never need client_key; omit it. Several terminals of one client are
several sessions of one agent."

## Step 4 — board: one lane per live session (`code/frontend/`)

`internal/state/derive.go`: `AgentLane` holds exactly one `Session`, and the lane builder keeps
one session per agent, so a second terminal is invisible. Replace with
`liveByAgent map[string][]*Session` (sorted by start) → one `AgentLane` per live session, plus one
lane with the latest finished session for an idle agent; `Lane.Active()` counts distinct agents;
add `Multi bool` on the lane. `internal/view/board.templ`: show `Session.Key` on the label when
`Multi`; `templ generate`. `internal/model` and `app/webapi/snapshot.go` need nothing — the API
already emits every live and stale session. Add a derive test: two live sessions on one agent →
two lanes, `Active()==1`.

## Step 5 — installer (`install.sh`, mirrored to `code/frontend/static/install.sh`)

Restructure `main` from *decide → join once → write everything* to
*decide → detect clients → per client: identity → join → verify → write*.

- `detect_clients()` → ordered `claude cursor windsurf codex`, filtered by `want` and presence.
  The fixed order matters for create.
- `client_identity <client>`: `CLIENT_KEY="$(machine_id)-$client"`,
  `AGENT_LABEL="$client on $(machine_id)"`, `CLIENT_KIND="$client"`. `METICHE_CLIENT_KEY` and
  `METICHE_AGENT_LABEL` are honoured only under `--only <one client>`; add `METICHE_MACHINE_ID`.
- `client_current_token <client>` (read-only, never printed): jq on `~/.cursor/mcp.json`,
  `~/.codeium/windsurf/mcp_config.json` and `~/.claude.json` (verify the user-scope key path
  against a real file first); `codex mcp get metiche --json` for Codex.
- `anchor_token()`: `$METICHE_TOKEN`, else `existing_token()` from `~/.metiche/env`.
- `resolve_team()`: create → `create_team` **once**, as the first client, carrying the anchor;
  first check `list_teams` and refuse to create a team whose name the account already has. Join →
  the first client redeems the code. Token → `list_teams`; one team → use it; several → require
  `METICHE_TEAM_SLUG`, never guess.
- `join_client <client>`: bearer = that client's current token if present (the server keeps it),
  else the anchor (the server mints); arguments `{team_slug, member_name, agent_label,
  client_key, client_kind}`; never the join code after the first call. Then `verify_token` on a
  fresh connection **and** assert the expected slug is in `list_teams`. Store per-client tokens
  with a small `tok_set` / `tok_get` (POSIX sh, no arrays).
- `server_json` takes the token as an argument; **drop `X-Metiche-Client-Key`** everywhere,
  including the no-jq manual snippet and `manual_mcp_note`.
- `install_claude`: plugin install (skill only), then `claude mcp remove metiche --scope user`
  (ignore failure) and `claude mcp add --transport http --scope user metiche "$URL" --header
  "Authorization: Bearer <that client's token>"` — **only when the read shows a different URL or
  no token**, so re-runs are no-ops with no argv exposure. Edit the header comment's "never passes
  a credential on a command line" to name this one exception and why: the CLI is the only safe
  writer of `~/.claude.json` while Claude Code is running.
- `install_codex`: `http_headers = { Authorization = "Bearer <token>" }` only; the "already
  registered" check compares the URL **and** the token's presence, not `CLIENT_KEY`.
- `write_env_file`: `METICHE_TOKEN=<anchor>` — the legacy account token if one verified, else
  Claude Code's agent token if present, else the first client's. Comment: "the anchor: sent on
  later runs so new clients attach to the same person; each client's own token is in its own
  config". **Drop `METICHE_CLIENT_KEY`.**
- `--dry-run`: one block per client (identity, what it would carry, what it would write); for
  create: "create_team once as claude, then join by slug for cursor, codex". No network mutation.
- The `idempotency_key` seed uses `machine_id`, not `CLIENT_KEY`. Usage and header text: "one
  join per client".
- The `METICHE_TOKEN` path goes through the same loop. Today it skips `resolve_agent_identity`
  and leaves `CLIENT_KEY` empty — a latent bug.

## Step 6 — plugin and dogfood config

Delete `plugin/.mcp.json`. Bump `plugin/.claude-plugin/plugin.json` **and** `marketplace.json`
to `0.2.0` — they currently disagree (0.1.1 vs 0.1.0). Rewrite `plugin/README.md`: "skill only;
the server is registered by install.sh with a per-agent token". Root `.mcp.json`: drop the
client-key header, keep `${METICHE_TOKEN}`; note that it deliberately shadows the user-scope
entry inside this repo so `METICHE_MCP_URL` still redirects local development.

## Step 7 — verification through the artifact

**New `deploy/scripts/smoke-install.sh`** (POSIX sh, `set -eu`, lib.sh helpers): start a backend
on a fresh database (`create.sql` + `deploy/sql/seed-default-plan.sql`, DSN with
`interpolateParams=true`), point `HOME` / `CODEX_HOME` at scratch directories, run `install.sh`
for every client it can, then **for each config file the installer wrote, extract the URL and
the `Authorization` value and make a real `start_session` with nothing else** → assert distinct
`agent_uuid`s across clients and no `client_key` anywhere. Where a client CLI can probe its own
config (`claude mcp get metiche` → "✔ Connected"), run it. Re-run the installer and assert every
config is byte-identical and no token rotated. Feed it a corrupted anchor and assert nothing is
written. This is the test that did not exist.

Keep the server-side "which headers arrived" log, gated as in 3e. Add the same at the board gate
(`app/authz`) for identity refusals.

**Backend tests.** Invert `TestIntegrationJoinTeamNeverPersistsAToken`: `agent.token_hash`
exists, is 64 lowercase hex, equals `HashToken(returned token)`, never carries `mtk_`, differs
from `account.token_hash`, and neither the plaintext nor the hash appears in `team_event`. Adjust
`TestIntegrationRejoinIsSameAgent` (a rejoin carrying its own token → `token_kept`, hash
unchanged). Route the `clientkey_integration_test.go` cases through a new `hs.legacyCtx`
(account-only context) — with agent tokens the "several agents, no argument" failure no longer
exists, which is the point.

New tests: `TestIntegrationTwoAgentsTwoTokensNoHints` — the end-to-end proof through the **real
transport** (`httptest` + the SDK client): two `start_session`s with only `Authorization`,
distinct agents, and A's token refused on B's session. `RotateOneAgentNotThePerson`.
`LegacyAccountTokenStillWorks`. `RetiredAgentTokenIsDead` (MCP and authz).
`AgentTokenWithForeignClientKeyIsRefused`. `JoinBySlugNeedsAToken` / `NeedsMembership`.
`SecondTerminalIsSecondSession` (the note names the first; the cap refuses the ninth). Fix the
dangling reference: `safetool.go` cites `TestAuthIsPerCallNotPerSession`, which does not exist.
Run with `-p 1` and the production DSN flags.

## Step 8 — docs, then deploy

`docs/PLAN.md` "Identity and scope" still describes **v1** (`team.join_code`, `agent.token_hash`
under `member`). Rewrite it to the three-layer model above and add `account`, `invite`,
`create_team`, `list_teams`. `docs/ONBOARDING.md`: "one join per machine" → per client; remove the
stale "client_key on start_session". `docs/DEMO.md` setup. `README.md`.
`skill/metiche-teamwork/SKILL.md` and its `plugin/skills/` copy: "you never pass client_key;
several terminals of one client are several sessions of one agent; `start_session` tells you about
your other live sessions".

Deploy order: ALTER (Step 2) → backend image → frontend image → serve the new `install.sh` → the
owner re-runs the installer. Existing installs keep working until then: an account token falls
back to today's behaviour.

## Execution strategy — orchestrate only

The rule for building this: the coordinator writes no code. It briefs, reviews, validates, and
demands proof. Nothing merges on an agent's report; every proof is re-run independently before
commit, and commits are authored by the owner with no trailers.

**Proof gate for every agent.** The report must contain: files written, the exact commands run,
and their actual pasted output. "Tests pass" with no output is treated as untested and sent
back. Where a test's value is not obvious, a mutation run is required: break the code on
purpose, show the test fail, restore. File ownership is explicit in every brief — two agents in
one file is how `auth.go` was clobbered once already — and every brief carries the hard rules:
no credential in any file, never edit a `DO NOT EDIT` generated file, the forbidden names.

**Wave 0 — schema (Step 1, then owner approval).** The nuzur draft is modelling, not code; the
coordinator creates it and sends it for review. The owner approves. One agent then runs codegen
and reports the generated diff.

**Wave 1 — parallel, after codegen lands.** Distinct files per agent:

| agent | owns | proof |
|---|---|---|
| A · identity | `export_authz.go`, `auth.go`, `join.go`, `createteam.go`, `safetool.go`, `server.go`, `deploy/sql/2026-09-agent-token-hash.sql`, tests for 3a–3e/3g | full `-p 1` suite with the production DSN flags; `TwoAgentsTwoTokensNoHints` through the real transport; `LegacyAccountTokenStillWorks` |
| B · sessions | `sessions.go` + `SecondTerminalIsSecondSession` | the cap refuses the ninth; the note names the first; replay byte-identical |
| C · board | `code/frontend/internal/state/derive.go`, `internal/view/board.templ` (+ regenerated) | derive test: two live sessions → two lanes, `Active()==1`; rendered HTML from a fixture with two sessions on one agent |
| D · installer + plugin | `install.sh`, `code/frontend/static/install.sh`, `plugin/`, root `.mcp.json` | `sh -n`; `--dry-run` for a two-client machine; a real run against a local server for cursor/windsurf/codex into scratch `HOME`/`CODEX_HOME`, each config's own bearer making a real `start_session`; re-run byte-identical; a corrupted anchor writes nothing |

D is written against the `team_slug` join spec and re-verified once A lands.

**Wave 2 — after A and D.** E · smoke test (`deploy/scripts/smoke-install.sh`, Step 7) — the
proof is the script passing, AND failing when the per-machine `client_key` default is
deliberately reintroduced. F · docs (Step 8), including the `docs/PLAN.md` v1→v3 rewrite.

**Wave 3 — deploy.** One deploy agent: ALTER first with `SHOW COLUMNS` pasted, then the backend
image, the frontend image, `install.sh`. Verified from outside: the production tool surface, a
real two-token `start_session` pair, the log free of "no agent identity" lines. The owner re-runs
the installer; the final proof is their two clients on the board without typing an identity.

## Backlog surfaced by the demo, deliberately NOT in this plan

- Tools 8–12 (`publish_contract`, `record_decision`, `get_review_context`, `report_judgement`,
  `resolve_conflict`): contract mismatch and the model-judged checks do not fire yet.
- Frontend viewer gate: the board service authenticates to the API, but nothing gates who may
  load `metiche.xyz/t/<slug>`. The demo team is public for exactly that reason.
- The kind-gate worktree (19 uncommitted `deploy/` files: helm test hooks,
  `local-cluster-test.sh`, `--atomic`) overlaps `values.yaml`, `lib.sh` and `helm-deploy.sh` on
  main — a reconcile, not a merge.
- `build-images.sh` runs on neither machine; nothing applies `seed-default-plan.sql`; the nuzur
  Dockerfile `ENTRYPOINT` names the identifier rather than the module basename (worked around in
  the chart; the real fix is upstream).
- A Claude Code `SessionStart` hook and an installer-written `~/.codex/AGENTS.md` block, so
  agents call `start_session` without being told. The installer writing `.metiche`.
- The board shows `branch: NULL` when the agent's working directory is above the git root.

## Verification, end to end

1. `go test -p 1 ./app/...` green with `interpolateParams=true`; `go vet`; `gofmt -l` empty;
   frontend `go test ./...` and `templ generate` clean.
2. `deploy/scripts/smoke-install.sh` passes locally: N clients, N tokens, N agents, one account,
   no `client_key` in any request, and a re-run is a byte-identical no-op.
3. On the owner's machine after re-running the installer: Claude Code and Codex both
   `start_session` with no argument and resolve to distinct agents; two Claude Code windows
   appear as two lanes on `metiche.xyz/t/taqueria-tracker`; the collision on `app/rest.go` fires
   between them with `same_member_concurrent` recorded.
4. The production log shows no "no agent identity" lines for agent-token calls.
