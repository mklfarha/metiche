# Board login: a sign-in link from the terminal

Status: **design for owner review. Nothing here is implemented.** The schema in §3 is a proposal to be
modelled in nuzur, reviewed and approved by the owner **before** any codegen.

## Why

The board has no idea who is looking at it. Private teams now 404 in a browser, which is correct and
leaves the owner with no way to see their own board:

- `code/frontend/cmd/metiche-web/main.go` reads an optional service credential from
  `METICHE_BOARD_TOKEN` (`-backend-token-env`). Production runs without one: the process logs "no
  backend token; only PUBLIC teams can be shown" (also recorded in `docs/CLI.md`, Context).
- `app/authz/authz.go` `Guard.Authorize` short-circuits `team.Public` and otherwise needs a bearer
  whose account is a live member. With no bearer, a private team is `ErrDenied`, rendered
  byte-identically to "no such team" (`webapi.API.notFound`, and the stream's `NewServer` NotFound).
- The board's discovery (`code/frontend/internal/web/discovery.go`) probes
  `GET /v1/teams/{slug}` with the board's own credential (`feed.ProbeTeam`). With none, it finds
  public teams only, and remembers every 404 for `NotFoundTTL`.

So the owner ran the installer, got a private team (`createteam.go` writes
`enums.TEAM_VISIBILITY_PRIVATE`), and could not see it. The installer's final "Done" block
(`install.sh` `main`) prints no board URL at all. `docs/CLI.md` §1.5 had to make `metiche open`
decline private teams, and §10 Q3 names the missing viewer gate as the blocker.

**The direction:** a short-lived, single-use sign-in link, minted with the credential the machine
already holds (a per-agent bearer token) and exchanged by the board for a browser session for that
**account**. The browser then sees the private boards of every team that account is a live member
of. There is no sign-up and no password. GitHub OAuth later adds a second way into the same browser
session.

---

## 0. What the code does today (verified while writing this)

These facts shape the design. Several of them are hazards the design has to close, not just
background.

| # | finding | where |
|---|---|---|
| F1 | The board's POST controls `resolve`, `nudge` and `cadence` are **unauthenticated**, and `hub.Inject` fabricates an event into the in-memory hub, which broadcasts it to every viewer of that board. That includes live public teams. Once a cookie exists, these become CSRF targets as well. | `internal/web/server.go` (`resolve`, `nudge`, `cadence`), `internal/hub/hub.go` `Inject` |
| F2 | A team discovered while **public** and then made private **keeps being served to everyone**. `resolveTeam` returns any registered slug without asking the backend. `feed.Live` treats a non-200 as a reconnect error (`live.go`, the two `resp.StatusCode != http.StatusOK` branches) and never unregisters the team, so the last folded state stays up. | `internal/web/discovery.go` `resolveTeam`, `internal/feed/live.go` |
| F3 | The backend's generated request logger is chi `middleware.Logger`, which logs the full request URI **including the query string**. Any secret in a backend query string lands in the pod log. | `rest/server/middleware_common.go` (DO NOT EDIT) |
| F4 | `clientIP` honours `X-Forwarded-For` only when `METICHE_TRUST_PROXY` is set, and production does not set it. So every MCP rate limit keys on the ingress pod's address, and the whole internet shares one `join_team` bucket (`defaultJoinTeamPerHour = 60`) and one `create_team` bucket (`5`). When the variable IS set, `clientIP` takes the **leftmost** XFF entry. That is spoofable if the ingress appends to a client-supplied header instead of overwriting it. | `app/mcp/ratelimit.go` `clientIP` |
| F5 | The generated CORS middleware answers `Access-Control-Allow-Origin: *` on the backend. Harmless for header-borne credentials. It is one more reason a backend credential must never ride on a cookie. | `rest/server/middleware_common.go` `CORS` |
| F6 | Nothing retires an agent or revokes a membership except SQL. `AGENT_STATUS_RETIRED` is honoured by `IdentityByToken` and by `RequireAgent`/`RequireSession`, but no tool sets it (see `docs/CLI.md` §10 Q4). | `app/mcp/export_authz.go`, `auth.go` |
| F7 | The only public doors on the backend are `/v1/mcp` (Prefix) on `mcp.metiche.xyz` and `/v1/metrics/mcp` (Exact) on `api.metiche.xyz`. `/v1/teams/...` is reachable only in-cluster, and the board is its only consumer. `metiche.xyz` routes `/` (Prefix) to the board. | `deploy/.helm/metiche/values.yaml`, `templates/ingress.yaml`, `deploy/.helm/metiche-web/values.yaml` |
| F8 | `app/authz` takes the credential from headers only (`extractToken`), by design. Its package comment already says a private SSE stream "needs fetch-based SSE, a cookie session, or a short-lived ticket". | `app/authz/authz.go` package comment |
| F9 | The installer's `have_tty` is `(: < /dev/tty)`, so `curl … \| sh` still counts as interactive. Backups are named `<file>.metiche-backup-<TS>` (`backup_file`), and the env file is `~/.metiche/env` (`ENV_FILE`). | `install.sh` |

---

## 1. Goals, non-goals, threat model

### Goals

1. From a machine where metiche is installed, a person can open **their** private boards in a
   browser in one step, with nothing to type.
2. The installer ends with a board URL that works: a sign-in link it offers to open, or a plain URL
   for a public team.
3. An agent can do the same on request ("open the metiche board").
4. The backend remains the **only** authority on "may this viewer read this team". It keeps one
   identity implementation (`IdentityByToken`) and one membership predicate (live member, not
   revoked).
5. 404 stays indistinguishable for anonymous visitors, for signed-in non-members, and for
   nonexistent slugs.
6. The browser session is an account-level object, independent of how it was obtained, so GitHub
   OAuth can create the same session later.
7. A browser session can be revoked from the browser itself, from an agent, and by retiring the
   agent that created it.

### Non-goals

- No sign-up, password, email or magic-link email.
- No write actions on the backend from the board. The board stays read-only against the backend.
  Its three local controls are dealt with in §4.6, not extended.
- No public REST surface. No backend route added here is routed by any ingress.
- No multi-replica board session affinity. The board is `replicaCount: 1`, and sessions live in the
  database anyway.
- No GitHub OAuth in this plan. Only the seams it needs (§3, `auth_method`).

### Threat model

| threat | mitigation | residual |
|---|---|---|
| **Link in shell history** | Nobody types it. The installer and the CLI open a local redirect file (§5.1), so the secret is never on a command line. The link is single-use and expires after 10 min. | A user who pastes it into `open '<link>'` records a dead link. |
| **Link in terminal scrollback / screenshots** | Single use and 10 min TTL. The printed text says both, and says not to share the link. | A screenshot shared within 10 min of an unredeemed link gives the recipient a session. Stated in the output. |
| **Link in browser history** | The secret is in the URL **fragment** (`/signin#…`). The page's first script calls `history.replaceState` to strip it, then POSTs it. Once consumed, a history entry is dead. | Some browsers record the pre-replace URL in global history; it is already consumed. |
| **Link in Referer** | Fragments are never sent in Referer. `/signin` also sends `Referrer-Policy: no-referrer` and loads only same-origin static assets. | none |
| **Link in ingress / board / backend access logs** | A fragment is never transmitted, so no access log can contain it. The exchange is a POST body (neither nginx's default format nor chi `middleware.Logger` logs bodies). Board-to-backend calls carry it in a JSON body, never a query (F3). Verified by a log canary (§8). | none |
| **Link prefetch by scanners / unfurlers** (chat, mail, corporate proxies) | Consumption needs JavaScript to run and POST. A GET of `/signin` consumes nothing. | A full headless browser that executes JS could consume it. The legitimate user then sees "already used", and the account page shows the session (§5.4). |
| **Shared machine (another local user)** | The secret never appears in `argv`. The redirect file is `0600` inside `~/.metiche` (`0700`) and is deleted after 120 s and on the next run. | root on that machine is out of scope. |
| **Link or session in agent transcripts** (MCP tool) | `open_board` returns a link that is dead after one use or 10 min. Its description forbids writing it to files, commits or chat channels. | A transcript on disk holds a dead link. |
| **Stolen cookie** | `__Host-` cookie: `Secure`, `HttpOnly`, `SameSite=Lax`, host-only on `metiche.xyz`, never sent to `api.` or `mcp.`. Idle expiry 7 d, absolute 30 d. Revocation from `/account`, from the `sign_out_browsers` tool, and by retiring the originating agent. The account page lists user-agent hints. | A thief with the cookie reads that account's boards until the session is revoked or expires. No IP binding (it breaks on mobility). |
| **CSRF** | `SameSite=Lax`, plus a synchronizer token derived from the session on every state-changing POST, plus an `Origin`/`Sec-Fetch-Site` check. `/signin` (pre-session) requires same-origin `Origin`. §6.3. | none known |
| **Login CSRF** (attacker makes the victim sign in as the attacker) | The topbar always shows "signed in as <name>". The victim would see the attacker's boards, not the reverse. | Low impact, accepted. Open question 5 offers an interstitial. |
| **Session fixation** | A new session is always minted at exchange. Any cookie already present is ignored, and its session is revoked best-effort. | none |
| **Retired agent** | Its bearer is dead at the MCP edge (`IdentityByToken`), so it cannot mint. A link it minted earlier is refused at exchange. Sessions created from it stop validating (the session check joins `agent.status`). | none |
| **Removed member / left team** | The membership check is per request and uncached (`Guard.isLiveMember`). An open stream is re-authorized every 60 s (§4.4). | Up to 60 s of events on an already-open stream. |
| **Account deactivated** | The session check requires `account.status = ACTIVE`. | none |
| **Enumeration via the new endpoints** | `/v1/teams/{slug}/access` uses `Guard` and the same `notFound`. The board renders one NotFound for all causes, for a given viewer. The link failure page is one message for unknown, used and expired. | none |
| **Brute force of link or session secrets** | 256-bit `crypto/rand` secrets (as `MintToken`). Only sha256 is stored, checked with `VerifyToken`-style constant-time compare. Rate limits bound load, not guessing (§6.2). | none |
| **DB outage** | Everything fails closed. The session check returns 503, never 401, so the board does not clear the cookie. Private pages show the existing 503 "unavailable" page, not a 404. Demo and already-registered public boards keep serving. The exchange is one transaction, so a failed exchange consumes nothing. | During an outage a member sees "unavailable" rather than their board. |
| **A god credential on the board** (`docs/CLI.md` §10 Q3's warning) | Removed from the design. The board never holds a credential that can read a private team without a viewer: private boards are read with the viewer's own session (§4.2). `METICHE_BOARD_TOKEN` is retired (open question 7). | Board process compromise exposes the sessions of viewers active on it. |

---

## 2. The flow, end to end

### 2.1 Components and hosts

| step | component | host / path | credential |
|---|---|---|---|
| mint | MCP tool `open_board` (new), `app/mcp/openboard.go` | `mcp.metiche.xyz/v1/mcp` (already public) | the agent's bearer, resolved by `authMiddleware` + `authTool` |
| open | installer / `metiche open` / an agent | local browser | none |
| page | board `GET /signin` | `metiche.xyz` (already routed by `/` Prefix) | none |
| exchange | board `POST /signin` → backend `POST /v1/browser/sessions` | board: public; backend: in-cluster only | link secret in the body |
| use | board `/t/{slug}…` → backend `/v1/teams/{slug}…` with `X-Metiche-Browser-Session` | board: public; backend: in-cluster only | cookie → header |

**Why an MCP tool mints, not a REST route:**
- `docs/CLI.md` already settled that every client-facing capability is an MCP tool, and the MCP
  endpoint is the one public door.
- The installer already speaks MCP (`mcp_connect`, `mcp_call`).
- Agents need the same tool anyway.
- The agent token already authenticates there, through the path that has been hardened
  (`authTool` re-reads the header per call).

**Why the backend exchanges and the board only relays:** the database, `HashToken`/`VerifyToken`
and the membership predicate all live in the backend. The board has no database (`metiche-web`
values: "It holds no state and needs no database").

### 2.2 Sequence

```mermaid
sequenceDiagram
    participant T as installer / CLI / agent
    participant M as backend MCP (mcp.metiche.xyz)
    participant B as browser
    participant W as board (metiche.xyz)
    participant A as backend REST (in-cluster)
    participant D as MySQL

    T->>M: tools/call open_board {team_slug} (Authorization: Bearer <agent token>)
    M->>D: IdentityByToken (agent ACTIVE) + RequireTeam (live member)
    M->>D: INSERT board_login_link (secret_hash, account, agent, redirect_path, expires_at = now+10m)
    M-->>T: {board_url, login_url: https://metiche.xyz/signin#mbl_..., login_expires_at}
    T->>B: open ~/.metiche/signin.html (0600; location.replace(login_url))
    B->>W: GET /signin   (fragment not sent)
    W-->>B: static page, no-store, no-referrer, CSP script-src 'self'
    B->>B: read location.hash, history.replaceState("/signin")
    B->>W: POST /signin {link} (same-origin fetch, Origin checked)
    W->>A: POST /v1/browser/sessions {link_secret, user_agent, ip_hint}
    A->>D: BEGIN; UPDATE board_login_link SET consumed_at=now WHERE secret_hash=? AND consumed_at IS NULL AND expires_at>now; check agent+account ACTIVE; INSERT browser_session; COMMIT
    A-->>W: 201 {session_secret, session_key, expires_at, redirect_path, display_name}
    W-->>B: Set-Cookie __Host-metiche_session=...; {redirect: "/t/<slug>"}
    B->>W: GET /t/<slug> (cookie)
    W->>A: GET /v1/browser/session (X-Metiche-Browser-Session)
    W->>A: GET /v1/teams/<slug>/access (X-Metiche-Browser-Session)
    A->>D: session valid + Guard.Authorize (private: live member)
    A-->>W: 200 {visibility: private}
    W->>A: snapshot + stream for a viewer-scoped hub (same header)
    W-->>B: board HTML (Cache-Control: private, no-store); SSE /t/<slug>/stream (cookie)
```

### 2.3 Minting: `open_board`

- **Authentication:** `requireAccount`, then **an agent token is required**:
  `IdentityFromContext(ctx).Agent != nil`. A legacy account token is refused with
  `not_permitted: open_board needs this client's own token; re-run the metiche installer`
  (open question 3). The agent is re-read and must be `AGENT_STATUS_ACTIVE`, as in `RequireAgent`.
- **Team:**
  - `team_slug` given → `RequireTeam` (live member), `redirect_path = /t/<slug>`.
  - Empty and exactly one live membership → that team.
  - Empty and several memberships → `redirect_path = /teams`, the signed-in "your teams" list. It
    does not error, because "open the board" with several teams is a reasonable request.
  - Zero memberships → `redirect_path = /teams`.
- **What the link contains:** `<board_base>/signin#<secret>`, where
  `secret = "mbl_" + base64url(32 bytes crypto/rand)`.
  - The prefix makes a leaked link greppable, the same reasoning as `tokenPrefix = "mtk_"`.
  - The fragment carries **only** the secret. The destination is stored server-side in
    `redirect_path`, validated as exactly `/`, `/teams` or `/t/<slug>` with `web.ValidSlug`'s
    pattern. So there is no open-redirect parameter.
- **Board base:** `METICHE_BOARD_BASE_URL` on the backend (default `https://metiche.xyz`; must be
  `https://`, except `http://localhost*` / `http://127.0.0.1*`). The server builds the URL, so the
  installer and CLI need no board configuration.
- **TTL:** 10 minutes, fixed in code as `boardLinkTTL`. That is long enough to copy a link from an
  SSH session to a laptop, and short enough that scrollback is mostly dead (open question 1).
- **Single use:** the conditional UPDATE in §2.4.
- **Limits:**
  - `openBoardLimit`: a `RateLimiter` keyed by **account id**, `METICHE_OPEN_BOARD_PER_HOUR`,
    default 30.
  - At most 5 unconsumed, unexpired links per account (one indexed count). The 6th is refused with
    `rate_limited:`. No IP is involved, so F4 does not affect minting.
- **Stored:** only `sha256(secret)`, via the same definition as `HashToken`.
- **Never logged:** neither the secret nor the link.
- **No `team_event`:** it is not board state. The same reasoning `docs/CLI.md` §4.4 gives for
  invites.
- **Result** (standard `Envelope` plus):

  ```json
  {"ok":true,
   "team":{"slug":"taqueria-tracker","name":"Taqueria Tracker","visibility":"private"},
   "board_url":"https://metiche.xyz/t/taqueria-tracker",
   "login_url":"https://metiche.xyz/signin#mbl_<redacted>",
   "login_expires_at":"2026-09-20T18:12:14Z",
   "single_use":true,
   "note":"Give login_url to the person once, or open it for them. It signs ONE browser in as this account, works once, and expires at login_expires_at. Do not write it to a file, a commit, an issue or a chat channel."}
  ```

- **Annotations:** `additive` (a new row per call). `DestructiveHint` false, `IdempotentHint`
  false, `OpenWorldHint` false.

### 2.4 Exchange: `POST /v1/browser/sessions` (backend, in-cluster)

Request body (JSON): `{"link_secret":"…","user_agent":"…","ip_hint":"…"}`. The board fills
`user_agent` (truncated to 200 printable characters) and `ip_hint` (§3.3) from the browser request.

One transaction:

1. `UPDATE board_login_link SET consumed_at = :now, updated_at = :now WHERE secret_hash = :h AND
   consumed_at IS NULL AND expires_at > :now`. `RowsAffected == 1` or refuse. This is the same
   atomic-check-and-spend pattern as `redeemInvite`.
2. `SELECT` the link row with `VerifyToken`-style constant-time comparison of the hash. Join
   `agent` (`status = ACTIVE`) and `account` (`status = ACTIVE`). Refuse otherwise.
3. Mint the session secret (`"mbs_" + base64url(32 bytes)`) and `INSERT browser_session`
   (§3.2) with `auth_method = TERMINAL_LINK`, `created_from_agent_uuid`, `expires_at = now + 30d`
   and `last_seen_at = now`.
4. COMMIT, then return `201 {session_secret, session_key, expires_at, redirect_path, account_key,
   display_name}`.

Every refusal is one response: `404 {"title":"not found","detail":"no such sign-in link"}`, via
`writeProblem`. Unknown, used, expired, retired agent and inactive account are indistinguishable.

A database error is `503` and consumes nothing, because the transaction rolls back.

A process-wide limiter protects the database: `RateLimiter.Allow("browser-exchange")`,
`METICHE_BROWSER_EXCHANGE_PER_HOUR`, default 600. Per-IP limiting lives at the ingress and the
board (§6.2).

### 2.5 The redemption page, and why fragment + POST

| option | leaks to logs | consumed by GET-scanners | needs JS | chosen |
|---|---|---|---|---|
| `GET /signin?t=<secret>` → 303 that strips it | **yes**: the ingress access log and, for any backend hop, chi `middleware.Logger` (F3). Also the browser history entry of the first URL | **yes** | no | no |
| `GET /signin/<secret>` path segment | yes (the same logs) | yes | no | no |
| **`GET /signin#<secret>` → JS `replaceState` → same-origin `POST /signin`** | **no**: a fragment is never transmitted | **no** | yes | **yes** |
| device-code (the browser shows a code, the terminal confirms) | no | no | no | rejected: more steps, and "ask your agent to open the board" becomes a two-sided dance |

The board requires JavaScript anyway (htmx + SSE), so needing it here costs nothing. `<noscript>`
on `/signin` says so and suggests `metiche open` again.

`GET /signin` (board) serves a static templ page. Its script is `/static/signin.js`, with no inline
script. Headers:
- `Cache-Control: no-store`
- `Referrer-Policy: no-referrer`
- `Content-Security-Policy: default-src 'self'; script-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'`
- `X-Content-Type-Options: nosniff`

`signin.js`:
1. Reads `location.hash`.
2. Calls `history.replaceState(null, "", "/signin")` **before** any network call.
3. With an empty hash, shows the explanation page ("sign in from your terminal: `metiche open`, or
   ask your assistant to open the metiche board") and stops.
4. Otherwise sends `fetch("/signin", {method:"POST", credentials:"same-origin",
   headers:{"Content-Type":"application/x-www-form-urlencoded"}, body:"link=" + secret})`.
5. On `200 {"redirect": "/t/<slug>"}` calls `location.replace(redirect)`. On anything else, shows
   the one failure message.

`POST /signin` (board):
- Requires `Origin` equal to the board origin, and `Sec-Fetch-Site: same-origin` when that header
  is present.
- Ignores any existing cookie. If one is present, it calls `DELETE /v1/browser/session` for it
  (best-effort).
- Relays the secret to §2.4, sets the cookie (§2.6), and answers `200 {"redirect": …}` with
  `Cache-Control: no-store`.
- Failure → `200 {"error":"link"}` with one message: "This sign-in link has already been used or has
  expired. Links work once, for 10 minutes. Get a new one with `metiche open`, or ask your assistant
  to open the metiche board."
- Backend 503 → `{"error":"unavailable"}`: "metiche is unavailable right now. If you have not used
  this link yet, reload this page within its 10 minutes." The fragment is gone after
  `replaceState`, so the page keeps the secret in a `sessionStorage` slot until retry, and clears
  it on success or failure.

### 2.6 The cookie

```
Set-Cookie: __Host-metiche_session=<mbs_…>; Path=/; Secure; HttpOnly; SameSite=Lax; Max-Age=<seconds until expires_at>
```

- **`__Host-` prefix:** the browser refuses it without `Secure` and `Path=/`, or with any `Domain`.
  So the cookie is host-only on `metiche.xyz` and is never sent to `api.metiche.xyz` or
  `mcp.metiche.xyz`.
- **`SameSite=Lax`, not `Strict`:** a board URL pasted into chat and clicked is a top-level
  cross-site navigation. `Strict` would show that signed-in member a 404. `Lax` still withholds the
  cookie from cross-site POSTs.
- **Lifetime:**
  - Absolute 30 days (`browser_session.expires_at`).
  - Idle 7 days: `last_seen_at`, written at most once per 10 minutes by the session check, so
    browsing does not write a row per request.
  - Open question 2.
- **Rotation:**
  - A new secret at every sign-in. Sessions are never extended by replacing the secret.
  - Periodic in-place rotation is **deferred** (open question 12). With a 256-bit secret that never
    reaches a log or a URL, rotation buys little over idle and absolute expiry, and it costs a
    grace-window column and races between concurrent tabs and the SSE stream.
  - GitHub linking later rotates by minting a new session and revoking the old one.
- **Local development:** browsers other than Safari treat `http://localhost` as secure, so
  `__Host-` works there. The board gets `-dev-insecure-cookie`, which uses the name
  `metiche_session` without `Secure` and is **refused unless the board's own base URL is
  `http://localhost*` / `http://127.0.0.1*`**.

### 2.7 Using the session

On each board request the board does the following (§4 has the details).

1. **Resolve the viewer.** No cookie → anonymous. Otherwise
   `GET /v1/browser/session` with header `X-Metiche-Browser-Session: <secret>`:
   - `200 {session_key, account_key, display_name, expires_at}` → a signed-in viewer.
   - `401` → invalid, expired, revoked, retired origin agent or inactive account. The board clears
     the cookie (`Max-Age=0`) and continues anonymously.
   - `503` or a timeout → viewer **unavailable**. The cookie is kept.

   A positive answer is cached in board memory for **15 s**, keyed by `sha256(secret)`, never the
   secret itself.
2. **Authorize the team, per request, uncached.** `GET /v1/teams/{slug}/access` with the same
   header → `200 {visibility, role}` or the standard 404.

### 2.8 Sign out and revocation

| action | where | backend | effect |
|---|---|---|---|
| sign out (this browser) | board `POST /signout` (CSRF) | `DELETE /v1/browser/session` | `revoked_at = now`, `end_reason = SIGNED_OUT`; the cookie is cleared; redirect to `/` |
| sign out everywhere | board `POST /account/signout-all` (CSRF) | `DELETE /v1/browser/sessions` | every live session of the account, `SIGNED_OUT_EVERYWHERE` |
| revoke one other browser | board `POST /account/sessions/{key}/revoke` (CSRF) | `DELETE /v1/browser/sessions/{key}` (must belong to the caller's account; otherwise the same 404) | `REVOKED` |
| an agent revokes | MCP `sign_out_browsers {session_key?}` | direct SQL in the tool | one, or all, of the caller's account; `REVOKED_BY_AGENT` |
| retire the agent that minted it | (SQL today, F6; `retire_agent` later) | no write | the session check joins `agent.status`, so sessions `created_from_agent_uuid` that agent stop validating at once |
| leave a team / membership revoked | (SQL today) | no write | `/access` is per request → 404 on the next page; an open stream closes within 60 s (§4.4) |
| account deactivated | — | no write | the session check requires `account.status = ACTIVE` |
| session list | board `GET /account` (signed-in only) | `GET /v1/browser/sessions` | key, created, last seen, user-agent hint, "this browser"; never a secret or a hash |

**Membership: re-checked per request, not cached.** `Guard.Authorize` stays uncached, for the
reason its own comment gives: "A cache here would outlive a membership revocation". The only cached
thing on the board is the 15 s session validation. That bounds revocation latency at 15 s for
pages, and at 60 s for a stream that is already open.

---

## 3. Data model proposal — FOR OWNER REVIEW BEFORE CODEGEN

**Nothing in this section may be generated until the owner approves it in nuzur.**

The process mirrors `docs/IDENTITY.md` Step 1:
- `searchProjectsByName("metiche")`;
- find the latest **published** version (do not trust remembered names or uuids; it follows
  `v4-agent-tokens`);
- `createProjectVersionDraft` `v5-browser-login` based on it;
- `sendProjectVersionForReview`, then **stop and wait**;
- after approval, `nuzur-cli` codegen into `code/backend/metiche` with
  `code/backend/nuzur-codegen.json`.

Generated files (`entity/`, `core/`, `enums/`, `create.sql`) are never hand-edited.

Conventions (from `docs/PLAN.md` "Data model"):
- `id` is uuid (1).
- `created_at`/`updated_at` are datetime (26), generated.
- Every datetime that is absent until it happens, and every explicitly set expiry, gets
  `no_default_current_timestamp: true`.
- Enums (22) start at 1; codegen adds `INVALID` at 0.

### 3.1 Entity `board_login_link` — "a single-use, short-lived sign-in link minted by an agent"

| field | type | req | notes |
|---|---|---|---|
| `id` | uuid (1) | yes | PK |
| `account_uuid` | uuid (1) | yes | relationship → `account` (ON DELETE CASCADE). The account the browser will be signed in as. |
| `agent_uuid` | uuid (1) | yes | relationship → `agent` (ON DELETE CASCADE). The agent whose token minted it; must still be ACTIVE at exchange. |
| `secret_hash` | varchar (7) `max_size: 64` | yes | sha256 hex of the link secret (`HashToken` definition). The plaintext is shown once and never stored. |
| `redirect_path` | varchar (7) `max_size: 120` | yes | `/`, `/teams` or `/t/<slug>`; validated at mint; never taken from the browser |
| `requested_via` | enum `board_link_source` | yes | `INSTALLER=1`, `CLI=2`, `AGENT=3`; client-reported, for the account page and debugging only |
| `expires_at` | datetime (26) | yes | `no_default_current_timestamp: true`; mint time + 10 min |
| `consumed_at` | datetime (26) | no | `no_default_current_timestamp: true`; set by the atomic exchange |
| `created_at`, `updated_at` | datetime (26) | yes | generated |

Indexes:
- `uq_board_login_link_secret_hash` unique `[secret_hash]` — the exchange lookup.
- `idx_board_login_link_account` `[account_uuid, consumed_at, expires_at]` — the 5-outstanding cap.
- `idx_board_login_link_expires` `[expires_at]` — the sweeper.

### 3.2 Entity `browser_session` — "a signed-in browser for one account; how it was obtained is `auth_method`"

| field | type | req | notes |
|---|---|---|---|
| `id` | uuid (1) | yes | PK |
| `key` | varchar (7) `max_size: 32` | yes | short public handle (`BS-` + 10 base32 chars) for the account page and `sign_out_browsers`; not a credential |
| `account_uuid` | uuid (1) | yes | relationship → `account` (ON DELETE CASCADE) |
| `secret_hash` | varchar (7) `max_size: 64` | yes | sha256 hex of the cookie value |
| `auth_method` | enum `browser_auth_method` | yes | `TERMINAL_LINK=1`, `GITHUB=2` (reserved for OAuth; unused in this plan) |
| `created_from_agent_uuid` | uuid (1) | no | relationship → `agent` (ON DELETE CASCADE). Set for `TERMINAL_LINK`; the session is valid only while this agent is ACTIVE. NULL for `GITHUB`. |
| `user_agent` | varchar (7) `max_size: 200` | no | truncated, printable-only hint captured at exchange |
| `ip_hint` | varchar (7) `max_size: 64` | no | truncated prefix only (IPv4 /24, IPv6 /48), at exchange (open question 6) |
| `expires_at` | datetime (26) | yes | `no_default_current_timestamp: true`; absolute, exchange + 30 d |
| `last_seen_at` | datetime (26) | no | `no_default_current_timestamp: true`; written at most every 10 min; idle expiry = +7 d |
| `revoked_at` | datetime (26) | no | `no_default_current_timestamp: true` |
| `end_reason` | enum `browser_session_end_reason` | no | `SIGNED_OUT=1`, `SIGNED_OUT_EVERYWHERE=2`, `REVOKED=3`, `REVOKED_BY_AGENT=4`, `REPLACED=5` (a new sign-in in the same browser) |
| `created_at`, `updated_at` | datetime (26) | yes | generated |

Indexes:
- `uq_browser_session_secret_hash` unique `[secret_hash]` — the per-request lookup.
- `uq_browser_session_key` unique `[key]`.
- `idx_browser_session_account` `[account_uuid, revoked_at, expires_at]` — the list, and sign out
  everywhere.
- `idx_browser_session_agent` `[created_from_agent_uuid]` — "sessions from this agent", for a
  future `retire_agent` report.
- `idx_browser_session_expires` `[expires_at]` — the sweeper.

Entity descriptions to update:
- `account`: "a person; may hold browser sessions".
- `agent`: "its token may mint board sign-in links; retiring it ends the browser sessions it
  created".

**Deliberately not modelled:**
- **No link ↔ session foreign key.** The sweeper deletes links on their own schedule, and a cascade
  in either direction would delete the wrong thing. `created_from_agent_uuid` plus `created_at` is
  the audit trail.
- **No `previous_secret_hash`** for rotation (§2.6, deferred).
- **No `team_uuid` on either table.** A session is for an account. The team is authorized per
  request, so a membership change needs no session change.
- **`account` needs no new field.** `identity_provider` (`NONE`, `GITHUB`), `identity_subject`,
  `identity_handle` and `uq_account_identity` already exist for the GitHub claim.

### 3.3 Retention and the sweeper

A new file, `app/sweeper/logins.go`, runs as step 5 of `Sweeper.RunOnce`, after retention.

- Delete `board_login_link` rows with `expires_at < now - 24h`, consumed or not.
- Delete `browser_session` rows with `expires_at < now - 7d` OR `revoked_at < now - 7d` OR
  `last_seen_at < now - 7d - 7d` (idle-expired a week ago).
- The 7-day tail keeps "why was I signed out" answerable on the account page for a week.
- Bounded batches (`LIMIT 500`, looped), autocommit, and never the team row lock. These are the
  same rules as `retention.go` rules 4 and 5.
- **Not** gated on `Options.RetentionEnabled`. Retention rule 1 protects team *history*, and these
  rows are credentials whose only safe state after expiry is gone. Own switch
  `Options.LoginSweepEnabled`, default **true** (open question 11).
- `Report` gains `Logins.LinksDeleted`, `Logins.SessionsDeleted`.
- The session check never relies on the sweeper. Expiry is enforced in the query, just as claim
  TTL is enforced lazily (PLAN.md: "lazy filter is authoritative").

`ip_hint` is computed by the **board**. It is the only component that could see the client address,
and today it sees only the ingress's. It is recorded only when the board runs with
`-trust-proxy-hops=1` (§6.2); otherwise it is NULL.

### 3.4 Migration (applied BEFORE the new backend binary)

- Two new tables, no ALTER. `deploy/scripts/apply-schema.sh` applies the regenerated `create.sql`
  (all `CREATE TABLE IF NOT EXISTS`), which creates them.
- A reviewed `deploy/sql/2026-09-browser-login.sql` holding exactly those two `CREATE TABLE IF NOT
  EXISTS` statements (copied from the generated `create.sql`) is committed next to
  `2026-09-agent-token-hash.sql` for the record.
- Verify with `SHOW CREATE TABLE board_login_link` / `browser_session` before the binary ships.
- An old binary on the new schema is fine: nothing reads the tables. A new binary on the old schema
  fails `open_board`, and every session check fails closed.

---

## 4. Authorization: how the viewer reaches the API

### 4.1 One credential shape in `app/authz`

`authz.Authorizer.Authorize(ctx, teamRef, token string)` becomes
`Authorize(ctx, teamRef string, cred Credential)`, where `Credential` holds `Bearer` and
`BrowserSession`. `extractToken` becomes `extractCredential`, still **headers only**:
- `Authorization: Bearer` / `X-Metiche-Token` → `Bearer`, unchanged.
- `X-Metiche-Browser-Session` → `BrowserSession`.

`Guard.Authorize`, private branch:

1. Bearer present → `metichemcp.AccountByToken`, unchanged.
2. Else, a browser session present → `browser.AccountBySession(ctx, db, secret)`. This is the same
   predicate as `GET /v1/browser/session`: hash lookup, constant-time verify, `revoked_at IS NULL`,
   `expires_at > now`, idle limit, `account.status = ACTIVE`, and (when `created_from_agent_uuid`
   is set) `agent.status = ACTIVE`. Every failure is one opaque error.
3. Both present → deny. A request is one principal. The board never sends a bearer (§4.2).
4. Then `isLiveMember`, unchanged. Every refusal is still `ErrDenied`, so the 404 bytes do not
   change.

The public short-circuit stays first: a public team never looks at either credential.

**Where the session code lives: a new package `app/browser`.**
- It holds mint, exchange, validate, list and revoke, plus the REST handlers for `/v1/browser/*`.
- It imports **no** `app/mcp`. It defines its hashing with `crypto/sha256` and `crypto/subtle`, and
  an external test (`package browser_test`) asserts `browser.Hash(x) == mcp.HashToken(x)` and the
  same verify semantics.
- `app/mcp` (the tool) and `app/authz` both import `app/browser`. The one-way rule in
  `export_authz.go` ("Nothing in app/mcp may import app/authz") still holds, and no cycle is
  introduced.

### 4.2 The board reads private teams with the viewer's own session (viewer-scoped hubs)

Two designs were weighed:

| | A. shared hub per team, read with a privileged board credential | **B. private teams read with the viewer's session, one hub per (browser session, team)** |
|---|---|---|
| credential the board holds | one that can read **every** team | only the sessions of viewers currently using it |
| who decides access to data | the board (a bug serves any private team to anyone) | the backend, on every upstream request |
| revocation | a board-side re-check only | the backend refuses the upstream; board re-checks too |
| cost | one upstream stream per team | one upstream stream per (active browser session, private team); teams are ≤10 people (PLAN.md), so tens of streams against a 2000-SSE tripwire |
| matches `docs/CLI.md` §10 Q3 ("must exist before a board token is ever configured") | no: it is exactly that token | yes: no board token at all |

**Chosen: B.** Public and demo teams keep today's shared, anonymous hubs untouched.

Board changes (`code/frontend/internal/web/`):

- **`viewer.go` (new):** `type Viewer struct{ SessionKey, AccountKey, DisplayName string; secret
  string }`.
  - `viewerFromRequest(r)` implements §2.7 step 1, with the 15 s cache.
  - The secret is never logged, never rendered, and never in `feed.Live.Name()`.
- **`viewerboards.go` (new):** a map keyed by `(SessionKey, slug)` → `*Team`, **separate from
  `Server.teams`**. It is never consulted for an anonymous request and never listed by `Teams()`,
  `demoTeams()` or `/healthz`.
  - Each hub runs a `feed.Live` whose new `BrowserSession` field is sent as the header on snapshot
    and stream.
  - Evicted when:
    - no subscriber for 2 minutes (the page load → SSE connect gap needs a grace);
    - the upstream answers 401 or 404;
    - the viewer signs out;
    - the cap `-max-viewer-boards` (default 200) is reached — the least recently used idle hub goes
      first, and a new one is refused with the existing 503 page if none is idle.
- **`Server.resolveTeam` becomes `resolveTeam(ctx, viewer, slug)`:**
  1. A registered **demo** or **public live** team in `Server.teams` → serve it. This is unchanged,
     but see F2 below.
  2. A signed-in viewer → `GET /v1/teams/{slug}/access` with the session:
     - `200 public` → register through normal discovery as today (shared, anonymous);
     - `200 private` → get or create the viewer hub;
     - `404` → NotFound (§4.3);
     - `503` → unavailable.
  3. Anonymous → today's discovery.
- **The negative cache (`discovery.remember`) holds anonymous answers only.** A signed-in member
  must not be shown a 404 cached from an anonymous probe, and nothing learned from a signed-in
  request may enter a structure an anonymous request reads. Signed-in 404s are not cached: they
  cost one indexed read at human speed.
- **F2 fix.** When a shared live hub's upstream returns 404 (the team went private or was deleted),
  the team is removed from `Server.teams` and its feed cancelled (`Team.cancel`). The next
  anonymous request then gets a real 404, and a signed-in member gets a viewer hub. `feed.Live`
  must surface "404 from upstream" as a distinct, terminal error instead of backing off forever.
- **Responses** for a viewer hub, and every response rendering a signed-in indicator:
  `Cache-Control: private, no-store` and `Vary: Cookie`.
- **`METICHE_BOARD_TOKEN`:** with login enabled, the board **refuses to start** when a board token
  is configured (`registerTeams`). Two credentials with different reach are how a private team ends
  up in the shared map. Open question 7 recommends deleting `-backend-token-env` outright.

Backend changes:

- `webapi.PathAccess = "/v1/teams/{slug}/access"`, registered in `API.RegisterOn` behind
  `a.guard.Wrap`, like every other route there. Its handler reads the `authz.Team` that `Wrap` puts
  on the context (a new `authz.TeamFromContext`) and returns
  `{"visibility":"public"|"private","role":"owner"|"member"|null}`. `role` is null for a public
  team read without a membership.
- `webapi` and `stream` routes need no other change: they inherit the credential shape through
  `Guard`.

### 4.3 404 semantics

- The backend is unchanged: `ErrDenied` → `notFound`, byte-identical, for `/access` as well.
- On the board, `view.NotFound(slug)` is the one page for unknown slug, private team (anonymous),
  private team (signed-in non-member), and an invalid slug.
  - It gains one hint line, shown **identically on every 404 under `/t/`**: "If this is your team's
    board, sign in from a terminal where metiche is installed: `metiche open`, or ask your assistant
    to open the metiche board."
  - For a signed-in viewer it also shows the signed-in indicator. That is identical across the
    three not-found causes for that viewer, so it tells them nothing about the slug.
- A test compares full response bodies and status codes pairwise for the same viewer (§8).

### 4.4 SSE gating

- **Browser → board (`/t/{slug}/stream`):** a same-origin `EventSource` sends the cookie, so no
  query-string credential is ever needed. That is the design decision F8 asked for.
  - The handler resolves exactly like the page.
  - For a viewer hub it re-runs session validation and `/access` **every 60 s**, and ends the
    stream on anything but 200.
  - A 404 at reconnect makes `EventSource` stop retrying; it does not retry non-200 responses.
- **Board → backend (`/v1/teams/{slug}/stream`):** `authz.Middleware.Wrap` authorizes once, before
  the first byte. `stream.Server.handle` gains a re-authorization tick (`reauthInterval`, 60 s,
  a field for tests like `keepalive`):
  - it re-runs `s.guard.Authorizer.Authorize` with the credential `Wrap` placed on the context
    (`authz.CredentialFromContext`), and returns on failure;
  - a public team costs one indexed read per minute per stream.

  Together these close "revoked member keeps an open stream" on both hops.

### 4.5 What each kind of viewer sees

| | demo | public live team | private team, member | private team, non-member | nonexistent |
|---|---|---|---|---|---|
| anonymous | unchanged; "DEMO · a recording." banner (`view.WithDemo`) | unchanged, shared hub | 404 + hint | 404 + hint | 404 + hint |
| signed in | unchanged + indicator | unchanged shared hub + indicator | **board**, viewer hub, `private, no-store` | 404 + hint + indicator | 404 + hint + indicator |

The public pages (`/`, `/teams`, `/join`, `/healthz`) still **never name a live team to an
anonymous visitor** (`privacy_test.go` stays as is). For a signed-in viewer, `/teams` adds a
"Your teams" section from `GET /v1/browser/teams` (slug, name, visibility, role — the
`list_teams` fields plus visibility), rendered `private, no-store`. `/healthz` never changes.

### 4.6 The board's local controls (F1)

`resolve`, `nudge` and `cadence` inject fabricated events into a hub.
- On a **demo** board that is the point of the recording.
- On a **live** board it lets anyone paint fake "resolved from the board" or "nudge queued" events
  into every viewer's timeline.

Before cookies exist:
- demo → unchanged, and no CSRF token is needed, because demo hubs hold no private state;
- live → `404` (the same NotFound) until these controls call real backend tools.

Signed-in members do **not** get them either: an event that exists only in one board process is a
lie on a live board (open question 8). If the owner prefers members-only, they need the CSRF token
from §6.3.

---

## 5. Surfaces

### 5.1 Installer (`install.sh`, mirrored byte-for-byte to `code/frontend/static/install.sh`, pinned by `internal/web/install_sh_test.go`)

A new `board_step`, called from `main` after `note_unverified`, printed as its own step after
"Done". A failure is a **warning**, never a failed install: the joins are the product.

**Which token:** `tok_get "$FIRST_CLIENT"`. It is an agent token proved moments ago by
`verify_token`. The anchor may be a legacy account token, which `open_board` refuses.

**Flags and environment** (both on the `sh` side of the pipe):
- `--open` mints and opens without asking.
- `--no-open` never mints.
- `METICHE_OPEN_BOARD=ask|yes|no` (default `ask`).

**Decision table:**

| situation | behaviour |
|---|---|
| `--dry-run` | "would mint a sign-in link with open_board as `<client>` for team `<slug>`, and offer to open it". No network. |
| `CI` set, or `have_tty` false, or `no` | **no link is minted** (a secret has no business in CI logs). Prints the board URL; for a private team adds "private: sign in with `metiche open`, or ask your assistant to open the metiche board, or re-run this installer in a terminal". |
| TTY, `ask` | `Open your board in a browser now, signed in? [Y/n]` read from `/dev/tty`. |
| yes + a local GUI (`uname` Darwin with `open`; Linux with `xdg-open` and `DISPLAY` or `WAYLAND_DISPLAY`) and **not** SSH (`SSH_CONNECTION`/`SSH_TTY` unset) | `mcp_call "$tok" open_board '{"team_slug":…,"requested_via":"installer"}'`; write `~/.metiche/signin.html` with `umask 077` (a 5-line page whose only content is a `location.replace` to the link and a meta refresh); `open`/`xdg-open` **the file path**; `(sleep 120; rm -f ~/.metiche/signin.html) &`; print the link too, with its lifetime. |
| yes + SSH or no GUI | mint and print the link: "open it in a browser on your own machine within 10 minutes; it works once". |

**Never on `argv`:** the secret is never passed as an argument to `open`, `xdg-open` or anything
else. `ps` shows every user's argv on macOS and on Linux without `hidepid`. The redirect file is
the carrier.

**Stale file:** `~/.metiche/signin.html` left over from an earlier run is removed at the start of
every run. `--uninstall` already removes `~/.metiche`.

**Output** (private team, local GUI):

```
==> Your board
    taqueria-tracker is private: its board is for members, after signing in.
    Open it in your browser now, signed in? [Y/n] y
    opened https://metiche.xyz/t/taqueria-tracker in your browser.

    If nothing opened, this sign-in link does the same in any browser. It works ONCE and
    only until 18:12 (10 minutes), and it signs that browser in as you, so do not share it:

      https://metiche.xyz/signin#mbl_<secret>

    Later: metiche open, or ask your assistant to "open the metiche board".
```

For a public team the plain board URL is printed first ("anyone can open it"), and signing in is
offered as optional.

**Tokens are still never printed.** The link secret is a separate, single-use, account-scoped
credential that cannot call a tool. That is why it may be printed and a token may not. The output
says what it does and how long it lives.

**`dead` / `unavailable` handling:** `mcp_call` failure → `warn "could not create a sign-in link: <redacted error>"`, print the board URL, and exit 0.

### 5.2 Installer follow-up: a stale `METICHE_TOKEN` in the same shell

**The failure:**
1. A run starts a new identity: a dead saved anchor, or a first create.
2. It rewrites `~/.metiche/env`; `backup_file` keeps the old one as
   `~/.metiche/env.metiche-backup-<TS>`.
3. The shell that ran it still exports the **old** value, because the profile line loaded it at
   shell start.
4. A re-run in that shell reaches `anchor_token`: `$METICHE_TOKEN` differs from `existing_token`, so
   it takes the `TOKEN_EXPLICIT=1` path. The token is treated as **passed on purpose**:
   - it outranks the saved token, so `METICHE_JOIN_CODE`/`METICHE_TEAM_NAME` are ignored
     (`choose_identity`);
   - when rejected, `resolve_team` refuses: "refusing to configure anything with a token that does
     not authenticate".

**Proposal: a token that is or was this machine's saved token is never "passed".**

1. **Detect, in `anchor_token`:** when `$METICHE_TOKEN` is set and differs from `existing_token`,
   read `METICHE_TOKEN=` (the way `existing_token` does: `sed`, never sourced) from each
   `~/.metiche/env.metiche-backup-*` and from `~/.metiche.metiche-backup-*/env` (written by
   `--uninstall`). On an exact match, set `TOKEN_STALE_SHELL=1` and **do not** set
   `TOKEN_EXPLICIT`. Values are compared in-process and never printed.
2. **Behave, in `choose_identity` / `resolve_team`:**
   - If a current saved token exists, the saved token is the anchor. Say: "your shell still has
     the METICHE_TOKEN from before this machine's last install (it matches a backup of
     ~/.metiche/env); using the current saved token. Open a new terminal, or run
     `. ~/.metiche/env`."
   - If no saved token exists (after `--uninstall`), the stale value is treated exactly like
     `TOKEN_FROM_PROFILE`: an anchor candidate whose confirmed death may start a new identity.
3. **Prevent, at the end of `main`:** when `${METICHE_TOKEN:-}` is non-empty and differs from the
   anchor just written, print "this terminal still has the previous METICHE_TOKEN. Open a new
   terminal (or run `. ~/.metiche/env`) before re-running the installer or using the metiche
   repository's .mcp.json." The value is never shown.
4. **Unchanged:** a `$METICHE_TOKEN` that matches neither the saved token nor any backup is still a
   passed token with today's refuse-on-rejection rule.

Tests (in `smoke-board-login.sh`, §8):
- create identity A; kill A's token in the database; re-run in the same environment with the old
  `METICHE_TOKEN` exported and `METICHE_TEAM_NAME` set → a new identity is created, and the output
  names the stale shell;
- a foreign `METICHE_TOKEN` (no backup match) that is rejected → still refused, nothing written;
- after a successful run where the anchor changed → the end-of-run note appears.

Mutation: drop the backup match → the first test refuses → fails.

### 5.3 CLI `metiche open` (`docs/CLI.md` §1.5, once `code/cli` exists)

- Team resolution is unchanged: argument → nearest `.metiche` → `list_teams` with exactly one.
- It calls `open_board` (`requested_via: "cli"`) with the anchor credential. `docs/CLI.md` §3's
  rule stands, except that a legacy account anchor falls back to a client's agent token under the
  one-account rule, because `open_board` needs an agent.
- Public team: it opens `board_url` as today (no secret involved), and `--signin` also signs in.
- Private team: it mints, writes the redirect file into `os.MkdirTemp` (`0700`), opens the file,
  waits 60 s, removes the file, and exits.
  - `--print` prints the link and its expiry, and opens nothing.
  - `--json` gives `login_url` and `login_expires_at`.
- A new `metiche signout [--all | <session-key>]` calls `sign_out_browsers`.

`docs/CLI.md` must change when this lands (not edited by this plan):
- §1.5 drops "Private team: opens nothing".
- §7 gains one named exception to "never write any file": the `0600` redirect file in a private
  temp dir, removed before exit.
- The §7 tool allowlist gains `open_board` and `sign_out_browsers`.
- §10 Q3 is answered.
- The phase 1 proof 8 premise changes. Anonymous private URLs still 404, and a 200 for an
  **anonymous** request is still stop-the-line.

### 5.4 MCP tools an agent can call

Registered in `server.go` `newServer` next to `list_teams`, through `addTool`, so `authTool` and
`recoverTool` apply.

| tool | params | annotations | authorization | result |
|---|---|---|---|---|
| `open_board` | `team_slug?`, `requested_via?` (`agent` default) | additive | agent token (§2.3), `RequireTeam` when a slug is given | §2.3 |
| `sign_out_browsers` | `session_key?` (empty = all) | DestructiveHint **true**, IdempotentHint true | `requireAccount`; only the caller's account's sessions; another account's key → `not_found:` | `{"ok":true,"revoked":N,"note":"…"}` |

`open_board` description (for models):

> "Open the metiche board for the person. Returns login_url, a sign-in link that signs ONE browser
> in as this person, works once and expires in 10 minutes. If the person asked you to open the
> board and you can run a command, write it into a private temporary file and open that file with
> the OS opener — never put the link itself on a command line. Otherwise show it to them once.
> Never write it into the repository, a commit, an issue, a PR or a chat channel. board_url is the
> plain address to share."

Server instructions (`serverInstructions`) gain one sentence: "If the person asks to see the board,
call open_board." The skill (`skill/metiche-teamwork/SKILL.md` and its plugin copy) gets the same
line.

A `list_browser_sessions` tool is deferred; the board's `/account` page lists sessions.

### 5.5 Board UI (`code/frontend/internal/view/`)

- **`layout.templ` `topbar`:**
  - Signed in: "signed in as <display name>" linking to `/account`, and a "Sign out" button (a POST
    form with the CSRF token).
  - Anonymous: "Sign in", linking to `/signin` (the explanation page when there is no fragment).
  - The CSRF token is in `<meta name="csrf-token">`, and `hx-headers` on `<body>` sends it as
    `X-CSRF-Token` on htmx requests.
- **`signin.templ` (new):** the `/signin` page — progress, failure and explanation states.
- **`account.templ` (new):** `/account` lists the sessions (§2.8) with revoke buttons and "Sign out
  everywhere". Anonymous → redirect to `/signin`.
- **`join.templ`:** `NotFound` gets the hint line (§4.3). `JoinPage` (`/teams`) gets "Your teams"
  for a signed-in viewer, with a board link per team.
- **`static/signin.js` (new).** No inline scripts anywhere.

---

## 6. Ingress, rate limits, CSRF, headers

### 6.1 Ingress and allowlists, per host

| host | change | why |
|---|---|---|
| `mcp.metiche.xyz` | **none** (`/v1/mcp` Prefix already routes the new tools) | tools, not routes |
| `api.metiche.xyz` | **none**; `/v1/browser/*` and `/v1/teams/{slug}/access` are **not** added to `ingress.hosts[api].paths` | in-cluster only, as `/v1/teams/...` is (F7) |
| `metiche.xyz` | `/` Prefix already routes `/signin`, `/signout`, `/account`. **Add** a second Ingress in `deploy/.helm/metiche-web/templates/ingress.yaml`, rendered from a new `ingress.signin` value: path `/signin` Exact with `nginx.ingress.kubernetes.io/limit-rpm: "20"` and `limit-burst-multiplier: "2"` | per-client-IP limiting where the real client address is known (the ingress), independent of F4 |

Backend `app/rest.go` `AllowedRoutes` gains, with comments "board only; NOT routed by any ingress":
- `webapi.PathAccess`;
- `browser.PathSessions` (`/v1/browser/sessions`: POST exchange, GET list, DELETE all);
- `browser.PathSession` (`/v1/browser/session`: GET current, DELETE current);
- `browser.PathSessionKey` (`/v1/browser/sessions/{key}`: DELETE);
- `browser.PathTeams` (`/v1/browser/teams`: GET).

`DenyUnlisted` keeps unregistered methods at 404. `app/rest_deny_test.go` extends its walk to
assert the new patterns answer only their registered methods.

A chart test (in the deploy agent's proof) runs `helm template deploy/.helm/metiche` and asserts no
`/v1/browser` or `/access` string appears in any rendered Ingress.

Backend env: `METICHE_BOARD_BASE_URL` in `deploy/.helm/metiche/values.yaml` `env`, and
`deploy/prod.yaml.example` notes it. It is not a secret.

### 6.2 Rate limits and the trust-proxy issue

| limit | key | where | default |
|---|---|---|---|
| mint (`open_board`) | account id | backend `openBoardLimit` | 30/h, plus ≤5 outstanding links |
| exchange, per client | client IP | ingress `limit-rpm` on `/signin` Exact; board `signinLimit` keyed by trusted client IP | 20/min; board 30 per 10 min |
| exchange, global backstop | constant key | backend `browserExchangeLimit` | 600/h |
| `sign_out_browsers` | account id | backend | 60/h |
| viewer hubs | process | board `-max-viewer-boards` | 200 |

**Trust proxy.** Two separate problems (F4):

1. **The board** gets `-trust-proxy-hops=N` (default 0). With N=1 it takes the **rightmost** entry
   of `X-Forwarded-For`, the one the ingress appended, and never the leftmost, which the client
   controls. With 0 it uses `RemoteAddr` (the ingress), and `signinLimit` degrades to a shared
   bucket. That is tolerable, because the ingress `limit-rpm` is the real per-IP layer.
2. **The backend `clientIP`** is not needed by this design: minting keys on the account, and the
   exchange backstop is global. Its existing flaw is still worth fixing in the same deploy, as a
   separate small change (owner question 9):
   - verify on the box how the microk8s ingress sets `X-Forwarded-For`
     (`kubectl -n ingress get cm -o yaml`; `use-forwarded-headers`, `compute-full-forwarded-for`)
     and whether a load balancer sits in front;
   - change `clientIP` to the rightmost trusted hop;
   - set `METICHE_TRUST_PROXY` in values.

   Until then `join_team` and `create_team` share one internet-wide bucket.

### 6.3 CSRF

- **Cookie:** `SameSite=Lax`, so no cross-site POST carries the session.
- **Synchronizer token:**
  - `csrf = base64url(HMAC-SHA256(key = session secret, msg = "metiche-csrf-v1"))`. It is
    stateless, needs no storage, and dies with the session.
  - Rendered in the meta tag and sent as `X-CSRF-Token` (htmx) or a `csrf` form field (plain
    forms).
  - Compared in constant time on `POST /signout`, `/account/signout-all` and
    `/account/sessions/{key}/revoke`.
- **Origin check on every POST:** `Origin`, when present, must equal the board origin, and
  `Sec-Fetch-Site`, when present, must be `same-origin`. Otherwise 403.
- **`/signin` POST:** there is no session yet, so the Origin/Sec-Fetch-Site check alone applies.
  Login CSRF is covered in the threat model.
- **GET is never state-changing.** `/signout` has no GET handler; `DenyUnlisted`-style behaviour on
  the board is a 405→404.

### 6.4 Headers

| response | headers |
|---|---|
| `GET /signin`, `POST /signin` | `Cache-Control: no-store`; `Referrer-Policy: no-referrer`; `Content-Security-Policy` (§2.5); `X-Content-Type-Options: nosniff`; `X-Frame-Options: DENY` |
| private board pages and streams; any page with the signed-in indicator; `/account`; `/teams` when signed in | `Cache-Control: private, no-store`; `Vary: Cookie`; `Referrer-Policy: same-origin`; `X-Frame-Options: DENY` |
| every board response | `Referrer-Policy: same-origin` (board URLs name slugs, which should not leak cross-site); `X-Content-Type-Options: nosniff` |
| backend `/v1/browser/*` | `Cache-Control: no-store` |

The board stream keeps `Cache-Control: no-cache, no-transform` plus `private` for viewer hubs, and
keeps `X-Accel-Buffering: no`.

---

## 7. Deliberate interactions with existing rules

- **`app/authz` "a token never comes from the URL":** preserved. The link secret enters through a
  POST body at the board and a JSON body at the backend. The session always travels as a header to
  the backend.
- **"One implementation of whose token is this":** `IdentityByToken` stays the only bearer path.
  `app/browser` is the only session path, and parity tests pin its hashing to `HashToken`.
- **The installer's "never passes a credential on a command line":** extended to the link secret
  (the redirect file).
- **`docs/IDENTITY.md` backlog "Frontend viewer gate":** this plan is that item.

---

## 8. Implementation plan

The execution rules of `docs/IDENTITY.md` apply:
- the coordinator writes no code;
- explicit file ownership, and two agents never share a file;
- every report pastes the commands and their real output;
- mutation runs where a test's value is not obvious;
- every proof is re-run independently before commit;
- commits are authored by the owner with no trailers;
- no credential in any file, no hand edit of a `DO NOT EDIT` file, and the forbidden names rule.

### Wave 0 — schema (gate: owner approval)

1. The coordinator models §3 as the nuzur draft `v5-browser-login` and sends it for review.
   **Stop.**
2. Owner approves (or edits) in nuzur.
3. One agent runs codegen and reports the generated diff: expected `entity/board_login_link`,
   `entity/browser_session`, three enums, `core/module/...`, and `create.sql`. It also writes
   `deploy/sql/2026-09-browser-login.sql` from the generated DDL.

### Wave 1 — parallel, after codegen

| agent | owns (exclusively) | proof |
|---|---|---|
| **A · browser core** | `app/browser/*` (new: `link.go`, `session.go`, `rest.go`, `hash.go`, tests incl. `hash_parity_test.go` in `package browser_test`), `app/rest.go`, `app/rest_deny_test.go` | `go test -p 1 ./app/browser/ ./app/` with `METICHE_TEST_MYSQL_DSN` (`parseTime=true&interpolateParams=true`); exchange: reuse refused, expired refused, retired agent refused, inactive account refused, two concurrent exchanges of one link → exactly one 201; session: idle and absolute expiry, revoked, origin agent retired → 401, DB closed → 503 |
| **B · authz + read API + stream** | `app/authz/authz.go`, `app/authz/*_test.go`, `app/webapi/api.go`, `app/webapi/access.go` (new), `app/webapi/webapi_mysql_test.go`, `app/stream/sse.go`, `app/stream/sse_test.go` | private team: member session 200, non-member session 404 byte-identical to unknown slug, bearer+session together 404; public short-circuit ignores a garbage session; `/access` bodies; stream re-auth: a revoked member's open stream ends within `reauthInterval` (set to 50 ms in the test) |
| **C · MCP tools** | `app/mcp/openboard.go` (new), `app/mcp/openboard_test.go`, `app/mcp/server.go` (registration + one instructions sentence), `app/mcp/ratelimit.go` (new limiters only) | through the real transport (`httptest` + SDK client, as `identity_integration_test.go`): agent token mints, legacy account token refused, retired agent 401 at the edge, the link's hash stored and never its plaintext (canary absent from `board_login_link` and `team_event`), a 6th outstanding link refused, `redirect_path` for 0/1/several teams, `sign_out_browsers` cannot touch another account |
| **D · sweeper** | `app/sweeper/logins.go` (new), `app/sweeper/sweeper.go` (step 5, `Options.LoginSweepEnabled`, `Report.Logins`), a test in `app/sweeper/integration_test.go` section it adds | expired/revoked rows past the tail are deleted in batches; a live session and an unexpired link survive; runs with `RetentionEnabled=false` |
| **E · board server** | `code/frontend/internal/web/server.go`, `discovery.go`, `viewer.go` (new), `viewerboards.go` (new), `signin.go` (new: `/signin`, `/signout`, `/account*`, CSRF, headers), their tests incl. `privacy_test.go` additions, `internal/feed/live.go` (`BrowserSession` field, terminal 404), `internal/feed/probe.go`, `internal/feed/browser.go` (new: `/v1/browser/*` + `/access` client), `cmd/metiche-web/main.go` (flags, refuse board token) | `go test ./...` against a stub backend: the §4.5 matrix; anonymous negative cache never shown to a member; a private hub never in `Teams()`; F2 eviction; F1 controls 404 on live boards; CSRF/Origin refusals; cookie attributes; cache headers |
| **F · board views** | `internal/view/layout.templ`, `join.templ`, `signin.templ` (new), `account.templ` (new) and their `_templ.go` (regenerated with `templ generate`, never hand-edited), `static/signin.js` (new), `static/app.css` additions | `templ generate` clean; rendered HTML from fixtures: indicator, hint line, "Your teams", no inline script, `signin.js` strips the fragment before its first `fetch` (a headless-browser test with a request recorder) |
| **G · installer** | `install.sh`, `code/frontend/static/install.sh` | `sh -n`, `dash -n`, `cmp` of the two files; `--dry-run` output; §5.2 tests; a real run against a local backend (below) |
| **H · deploy** | `deploy/.helm/metiche-web/values.yaml`, `templates/ingress.yaml`, `deploy/.helm/metiche/values.yaml` (env only), `deploy/prod.yaml.example`, `deploy/scripts/preflight.sh` (print how the ingress sets `X-Forwarded-For`) | `helm template` for both charts: the `/signin` Exact ingress with its limit annotations; no `/v1/browser` or `/access` in any Ingress; the existing path guards still fail on `path: /` |

E and F code against A/B's endpoint shapes (§2, §4). C codes against A's package API. All are
re-verified once A and B land.

### Wave 2 — after Wave 1

- **I · smoke** `deploy/scripts/smoke-board-login.sh` (POSIX sh, `lib.sh` helpers), the proof below.
  It runs on top of `smoke-install.sh` from `docs/IDENTITY.md` Step 7 if that exists; otherwise it
  starts its own backend.
- **J · docs:**
  - `docs/CLI.md` (§1.5, §3, §7, §10 Q3, Later);
  - `docs/IDENTITY.md` backlog;
  - `docs/ONBOARDING.md` (the installer's last step);
  - `docs/PLAN.md` (tool table, pages);
  - `skill/metiche-teamwork/SKILL.md` + plugin copy.
- **K · CLI `open`/`signout`**, when `code/cli` exists (CLI phase 1 or later).

### Wave 3 — deploy (order is load-bearing)

1. Tables: `apply-schema.sh` with the new `create.sql`, then paste `SHOW CREATE TABLE
   board_login_link` and `browser_session`.
2. Backend image with `METICHE_BOARD_BASE_URL`. Verify:
   - the tool list includes `open_board`;
   - `curl -s -o /dev/null -w '%{http_code}' https://api.metiche.xyz/v1/browser/session` → 404
     (not routed);
   - the same through `kubectl port-forward` without a header → 401.
3. Frontend image, with **no** `METICHE_BOARD_TOKEN`. The board refuses to start if one is set.
4. Serve the new `install.sh` (the embedded copy ships in the frontend image).
5. The owner re-runs the installer. The final proof is their private board open in their browser
   without flipping it public, and an incognito window on the same URL showing 404.

An old board on the new backend works: nothing it calls changed. A new board on an old backend
fails closed: `/access` is 404, so a signed-in member sees a 404 until the backend lands. That is
why the backend goes first.

### Verification — through the real artifacts

`smoke-board-login.sh`, against a fresh local database (`create.sql` +
`deploy/sql/seed-default-plan.sql`), a locally built backend (`METICHE_ROLE=all`,
`METICHE_BOARD_BASE_URL=http://localhost:8787`, the backend's stdout captured to a file), and a
locally built board (`-backend http://127.0.0.1:8080 -dev-insecure-cookie`, stdout captured). An
`open` shim on `PATH` records its argv and copies the file it was given.

1. **Minted by the installer.** Scratch `HOME`, `METICHE_TEAM_NAME=…`,
   `--url http://127.0.0.1:8080/v1/mcp --only cursor --open`.
   - The shim was called with **a file path**, not a URL, and no argv contains `mbl_`.
   - The file is mode `0600`.
   - stdout contains the link exactly once, and no `mtk_` anywhere.
   - Extract the link from the copied file.
2. **Redeemed by a real HTTP client.**
   - `curl` `GET /signin` → 200, with `no-store`, `no-referrer` and CSP.
   - `POST /signin` with `Origin: http://localhost:8787` and the secret → `Set-Cookie` asserted
     attribute by attribute: name, `HttpOnly`, `SameSite=Lax`, `Path=/`, no `Domain`, `Max-Age` ≈
     30 d. A separate unit test runs in `Secure`/`__Host-` mode.
   - `GET /t/<slug>` with the cookie → 200, the team's name in the body,
     `Cache-Control: private, no-store`.
3. **Redeemed by a real browser.** Headless Chrome opens the copied redirect file.
   - It lands on `/t/<slug>`, signed in.
   - `location.href` has no `#`.
   - The request recorder shows no request whose URL contains `mbl_`.
   - The board's SSE connects, and a `start_session` made with the member's agent token appears on
     the board.
4. **Private board visible to the member, 404 to others.**
   - A second identity (another scratch HOME, its own team) redeems its own link.
   - `GET /t/<slug>` and `/t/<slug>/stream` with its cookie → 404, **byte-identical** to
     `/t/no-such-team-7q` with the same cookie.
   - Anonymous → 404, byte-identical to anonymous `/t/no-such-team-7q`.
   - Directly against the backend: `/v1/teams/<slug>` and `/access` with no header, with the
     non-member's session, and with a garbage session → the same 404 bytes.
5. **Reuse refused:** POST the same link again → the one failure body, no `Set-Cookie`.
6. **Expiry refused:** mint, then `UPDATE board_login_link SET expires_at = NOW() - INTERVAL 1 SECOND`
   in the scratch database → refused.
7. **Retired agent.**
   - Mint with client X, set X's agent to `RETIRED` in the scratch database, redeem → refused.
   - Separately: redeem, then retire → the next page is 404 and the response clears the cookie.
   - `open_board` with X's token → HTTP 401 at the MCP edge.
8. **Removed member:** set `member.revoked_at` → the next page is 404, and an open board stream ends
   within the re-auth interval (the board run with `-viewer-reauth=2s`, a test-only flag with a
   floor of 1 s).
9. **Sign out:**
   - without CSRF → 403;
   - with it → the cookie is cleared, and replaying the old cookie → anonymous (404 on the private
     board);
   - `sign_out_browsers` via MCP revokes a second browser's session.
10. **No secret in logs:** grep the backend log, the board log and (in a kind cluster with
    ingress-nginx, when one is available) the ingress controller log for every `mbl_` and `mbs_`
    value used in the run → **zero hits**. Rows needing a cluster are reported as "not run", never
    as passed.
11. **DB outage:** stop MySQL.
    - A signed-in private board page → 503 (not 404), and the response carries **no** cookie
      deletion.
    - The demo board still 200.
    - Restart → the board is back with the same cookie.
12. **Visibility flip (F2):** make a team public, open it anonymously (discovered), set it private →
    anonymous 404 within one upstream reconnect, and the member (signed in) still sees it.
13. **Installer re-run in the same shell (§5.2)**, as specified there.
14. **Unit/integration suites green:** backend `go test -p 1 ./app/...` with the DSN flags, `go
    vet`, `gofmt -l` empty; frontend `go test ./...`, `templ generate` clean.

**Mutation checks** (each: break, show the named test failing, restore):

| break | must fail |
|---|---|
| drop `consumed_at IS NULL` from the exchange UPDATE | reuse test (A, smoke 5) |
| drop `expires_at > now` | expiry test (A, smoke 6) |
| drop the origin-agent `status = ACTIVE` join in session validation | retired agent test (A, smoke 7) |
| skip `isLiveMember` for the browser-session branch in `Guard` | non-member 404 (B, smoke 4) |
| `extractCredential` also reads `?session=` | the authz "never from the URL" test (B) |
| return 403 instead of `notFound` for a non-member session | byte-identical 404 test (B, E) |
| register a private viewer hub in `Server.teams` | anonymous-after-member test (E): anonymous gets 200 → fails |
| key the negative cache by slug for signed-in requests | member-after-anonymous test (E): the member gets a cached 404 → fails |
| remove `history.replaceState` before `fetch` in `signin.js` | the recorder test (F) sees `#mbl_` in the post-load URL |
| drop `HttpOnly` or `SameSite` | cookie attribute test (E, smoke 2) |
| skip the CSRF compare | sign-out CSRF test (E, smoke 9) |
| remove the stream re-auth tick (backend or board) | revoked-member stream test (B, smoke 8) |
| pass the link on `open`'s argv in the installer | smoke 1 argv assertion |
| flip the sweeper cutoff sign | "live session survives" (D) |
| drop the backup match in `anchor_token` | §5.2 stale-shell test |

---

## 9. Open questions for the owner (each with a recommendation)

1. **Link TTL.**
   *Recommend 10 minutes.* Five is tight for copying out of an SSH session. Thirty makes scrollback
   and screenshots live much longer.
2. **Session lifetime.**
   *Recommend absolute 30 days, idle 7 days.* A board is checked daily during a hackathon and then
   not for weeks. Re-signing in costs one command.
3. **Legacy account tokens minting links.**
   *Recommend agent tokens only.* The retired-agent revocation path needs an agent, and legacy
   tokens are being converged away by re-running the installer. The refusal message says exactly
   that.
4. **Retiring the minting agent ends its browser sessions.**
   *Recommend yes.* A lost laptop is handled by retiring its agents, and the browser on that laptop
   is the likeliest thing to be stolen with it. The cost: a future stale-agent cleanup also signs
   out the browsers those agents created, and the account page shows why.
5. **Confirmation interstitial on `/signin`** ("Sign in as Mark? [Continue]").
   *Recommend no.* The link is opened by the person's own terminal. The interstitial mainly defends
   against login CSRF, which is low impact here and is visible through the signed-in indicator.
   Revisit if links start being shared.
6. **Storing IP hints.**
   *Recommend a truncated prefix (IPv4 /24, IPv6 /48) plus the user agent, deleted with the row.*
   It is enough for "is that browser mine?" without keeping precise addresses.
7. **`METICHE_BOARD_TOKEN` / `-backend-token-env`.**
   *Recommend deleting it* (keeping at most a local-dev flag that refuses to run with login
   enabled). Viewer-scoped reads make it unnecessary, and it is exactly the credential `docs/CLI.md`
   §10 Q3 warned about.
8. **The board's local controls (`resolve`, `nudge`, `cadence`) on live boards.**
   *Recommend 404 for live boards now* (demo unchanged), and real backend-backed controls later as
   their own plan. Fabricated events on a real team's timeline are misinformation, signed in or
   not.
9. **Fix `clientIP` / `METICHE_TRUST_PROXY` in the same deploy.**
   *Recommend yes, as a separate small change:* verify the ingress's `X-Forwarded-For` behaviour
   (`preflight.sh`), take the rightmost trusted hop, and set the variable. Today `join_team` and
   `create_team` share one internet-wide bucket.
10. **Installer default when a TTY is present.**
    *Recommend asking, default Yes, and never minting without a TTY or under `CI`.* The person
    running an interactive install is standing right there; a CI log is the worst place for a
    secret.
11. **Login sweep independent of `RetentionEnabled`.**
    *Recommend always on* (`LoginSweepEnabled` default true). Expired credentials are not history,
    and the self-hosting promise in `retention.go` rule 1 is about history.
12. **Periodic session-secret rotation.**
    *Recommend deferring.* The secret never reaches a URL or a log. Idle and absolute expiry plus
    revocation cover theft. Rotation brings multi-tab and SSE races and an extra column. Revisit
    with GitHub linking.
13. **Viewer-scoped hubs (B) vs a shared hub with a privileged board credential (A).**
    *Recommend B* (§4.2). The extra upstream streams are small at this team size, and the backend
    stays the only authority.
14. **Entity names `board_login_link` / `browser_session`.**
    *Recommend these.* `session` already means a piece of agent work, and `browser_session` stays
    correct once GitHub creates the same rows.
15. **Show "Your teams" on `/teams` for a signed-in viewer, or only on `/account`.**
    *Recommend `/teams`.* It is where `open_board` sends a person on several teams, and it stays
    `private, no-store` and invisible to anonymous visitors.
