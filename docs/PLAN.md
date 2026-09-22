# metiche — build plan

## Context

Working in a small team (hackathons especially) collapses into a queue: everyone waits on one
person, usually the frontend, because nobody can see what anyone else is doing until it lands in
git. The work is parallelizable; the *awareness* is not.

**metiche** (Mexican slang for "nosy") is an MCP server every teammate's coding agent connects to.
Each agent continuously reports what it is **about to do** and what it is **doing right now**. The
server keeps that as shared live team state, detects collisions deterministically, and hands the
other agents enough context that their own model spots the semantic ones. A Go web board shows the
whole team in realtime.

Hackathons are the inspiration; the product is "any small team working in parallel on one repo."
A team may be one human with three agents, or five humans with two agents each — both from day one.

Public GitHub repo. **No third-party credential may ever be required to run the server.** That one
constraint rules out a server-side LLM judge and shapes the entire detection design.

---

## Decisions settled

| | |
|---|---|
| Backend | nuzur-generated Go, MCP server hand-written in `app/` |
| Frontend | Go: **templ + htmx + SSE** |
| nuzur home | new team **`metiche`**, project `metiche`, module `github.com/mklfarha/metiche` |
| Identity | team **join code** → server-minted per-agent token; agent self-declares person + label |
| Git facts | **agent self-reported** (branch, base commit, paths). No CLI, no hooks, no server-side clone. |
| Detection | **hybrid** — server deterministic only; the caller's own LLM judges semantics |
| Conflict kinds v1 | path overlap · contract mismatch · decision contradiction · duplicate work |
| Claims | **advisory + TTL**. Never block. Overlap is allowed and raises a conflict immediately. |
| Human control | nudges (stop/steer/ask) + conflict resolution from the board |
| Database | **MySQL**, reuses its proven deploy and codegen path (see Realtime) |
| Domain | **metiche.xyz** — `metiche.xyz` (board), `api.metiche.xyz`, `mcp.metiche.xyz`, all pointed at the nuzur box |
| Deployment | **the existing nuzur Linode box**, microk8s, its own `metiche` namespace; Helm, hostPath config overlay, no secrets in repo |
| Onboarding | `metiche-teamwork` skill + one-command join + Claude Code plugin |
| v1 target | end-to-end demo first, harden after |

> **"No credentials" ≠ "no auth."** anyone who can reach its MCP
> endpoint can write. metiche must not copy that: a public repo means the endpoint is public, and a
> team's board is not. Join code → server-minted agent bearer token. Say this in the README or
> someone will helpfully delete it.

---

## Realtime: is MySQL enough?

**Yes. No Redis, no NATS, no second datastore.** The reasoning, because it is worth being explicit:

**There are two latency paths and only one of them is a database question.**

*Agent → agent.* MCP is request/response. An agent cannot be pushed to; it learns things when it
makes its next call. The floor here is **the agent's call interval — tens of seconds** — and no
datastore changes that by a microsecond. `pending_instructions` count riding on every
response is the mitigation, and metiche inherits it. The one hard requirement is that detection runs
*synchronously inside* the declaring call, so the agent creating the collision is told in the same
response. That is a query-latency requirement (target p99 < 30ms), not a store-choice one.

*Server → browser.* tails the event table every 400ms and fans out over SSE: ~400–500ms
end-to-end, which reads as instant. metiche improves it for free:

- **In-process fan-out on commit** (≈2ms) **plus** the DB tailer as the cross-pod backstop.
  Subscribers already dedupe by `sequence`, so the two paths cannot double-deliver.
  deliberately skipped this for behavioral uniformity; we keep the tailer for exactly that reason
  and just add the fast path.
- **Run `METICHE_ROLE=all` in one pod for v1**, so the fast path covers 100% of writes. The
  role split exists for when agent load needs isolating — not yet.
- Tail interval **250ms**, and only for teams that currently have a subscriber.

**Capacity, concretely.** A team is ≤10 humans × ≤5 agents. Event-producing writes run **1–5/sec at
peak** (heartbeats, the dominant call, are deliberately outside the lock and produce no event). The
detection query is one index range scan over a few hundred live `claim_path` rows. The single
serialization point is `SELECT team FOR UPDATE`, held ~5–15ms, giving a **ceiling near 70–200
writes/sec per team against an actual 1–5**. Twenty to a hundred times headroom on one MySQL pod.

**The rule that protects that headroom:** nothing inside the team lock may do I/O or loop over an
unbounded row set. Ship a `metiche_team_lock_hold_ms` histogram from day one so we find out before
users do.

**Tripwires for "now we need another layer"** — none of which are v1:
`p99 lock hold > 25ms` or `>200 writes/sec on one team` → shard the sequence per-project before
reaching for Redis. `>2000 concurrent SSE connections` → move fan-out to a real bus. Multi-region →
then, and only then, a message broker.

**The one MySQL tax to remember:** it has no partial unique indexes, so "exactly one active
assertion per `(contract, session, role)`" is enforced with a generated `active_marker` column that
is `1` when active and `NULL` otherwise (MySQL lets NULLs repeat in a unique index). Document it at
the column, because it looks like a mistake to anyone reading the schema cold.

---

## Repository layout

```
metiche/
  README.md                 the pitch, the protocol, how to join
  CLAUDE.md                 rules for agents working ON metiche
  .gitignore                credential-proof: overlay, credentials, keys, kubeconfigs
  .mcp.json                 metiche pointed at itself (dogfooding)
  code/
    backend/
      nuzur-codegen.json
      metiche/              generated tree, committed
        app/                HAND-WRITTEN ZONE, survives regeneration
          rest.go
          mcp/              tool surface, response envelope, SSE, web API
          coordination/     paths.go · detect.go · contracts.go · tokens.go · sweeper.go
          queries/          the five hand-written sqlc queries
    frontend/               Go: templ + htmx + SSE
  skill/metiche-teamwork/SKILL.md
  plugin/                   Claude Code plugin wrapping the skill + mcp config
  deploy/                   Helm charts, scripts, schema.sql, prod.yaml.example
  docs/
```

---

## Data model

Modeled in nuzur (new team `metiche`, project `metiche`, first version `v1-coordination`). All
`id` PKs are **uuid (1)**; `created_at`/`updated_at` are **datetime (26) generated**; every optional
"absent until it happens" datetime (`ended_at`, `resolved_at`, `released_at`, `delivered_at`) sets
`no_default_current_timestamp: true` — otherwise a row inserted without the column is born already
ended.

Human/agent-facing short keys (`S-17`, `INT-83`, `CF-14`, `#auth-jwt-cookie`) are what appear in tool
params and prose. UUIDs are for joins only.

### Identity and scope
- **`team`** — name, slug, `join_code` (unique, rotatable), **`sequence`** (monotonic, every event —
  the repaint cursor), **`board_revision`** (structural changes only — the re-layout cursor),
  `settings` json. *The team row is the sequence lock.*
- **`member`** — human. `team_uuid`, display_name, role, last_seen_at.
- **`agent`** — one agent instance. `member_uuid`, label, client_kind, `token_hash`, `client_key`
  (idempotent re-join), status. Unique `(member_uuid, client_key)`.
- **`project`** — one repo. `key`, repo_url, default_branch, `ignore_patterns`, `hotspot_patterns`.
  Claims are project-scoped or a poly-repo team gets cross-repo false positives.
- **`session`** — one bounded piece of work by one agent: branch, base_commit, head_commit, goal,
  `status_line` (the "right now" line), status `live|stale|ended|abandoned`, heartbeat timestamps.
  This is also the **run record** — history is the event log filtered by `session_uuid`.

### Coordination
- **`intent`** — the semantic surface. `summary` varchar(280) hard cap, kind, status
  `declared|active|done|abandoned|superseded`, `external_ref` (issue id — the highest-signal
  duplicate-work key because it is an exact match), **`revision`** (bumped on material edits; drives
  re-judging of decision pairs), and **`wording_revision`** (bumped only when the summary's wording or
  the `external_ref` materially changes; duplicate-work pairs key on it, so a path edit never re-asks).
  An **`intent_token`** side table was planned for retrieval; it stays in the model unused, because
  duplicate work scores the project's live summaries in Go at declare time (docs/DUPLICATES.md §1.3).
- **`claim`** + **`claim_path`** — the mechanical surface. `mode` (`write|read|structural`), TTL,
  `hard_expires_at` (4h cap that heartbeats cannot push past). `claim_path` carries **denormalized**
  `project_uuid/session_uuid/member_uuid/mode/status/expires_at` plus computed `prefix`, `kind`,
  `depth`, `ext` so the hot query is one index scan with zero joins.
- **`contract`** + **`contract_assertion`** + **`contract_field`** — one assertion table with a
  `role` enum (`produces|consumes`), so a mismatch is a self-join. `contract_field` carries a
  **`direction`** (`in|out`), which is load-bearing: a missing *out* field breaks the consumer, a
  missing *in* field breaks the producer.
- **`decision`** — `key` (`#auth-jwt-cookie`), `statement` varchar(400) hard cap (it ships inline in
  review blocks), `always_show` flag, scope. Plus `decision_token` / `decision_path` side tables.
- **`conflict`** + **`conflict_participant`** — participants is a child table, not two columns:
  three agents in one directory is normal, and "conflicts involving my session" is the notification
  join. Unique `(team_uuid, dedupe_key)` so re-detection bumps a counter instead of spamming.
- **`judgement`** — the anti-re-judging ledger, unique on `(team_uuid, pair_key)`.
- **`instruction`** — the push-without-push mechanism (nudges, conflict notices, and the question
  metiche puts to a person when two agents have not settled a decision contradiction). A pair
  waiting to be judged is **not** an instruction: it rides on `pending.reviews` and is read with
  `get_review_context` (docs/DECISIONS.md §1.3).
  Index `(target_session_uuid, status)` backs the `pending` count on every response.
- **`event`** — one append-only team-scoped log. Unique `(team_uuid, sequence)` and
  `(team_uuid, idempotency_key)`; `response_snapshot` json replays a retried call verbatim.

**Design rule that keeps the model honest:** real table if it must be indexed, joined, counted, or
mutated independently — `claim_path`, `contract_field`, `conflict_participant`, `instruction`,
`judgement`, `*_token`. Dependent JSON entity otherwise — `event.payload`, `conflict.evidence`,
`contract_assertion.shape`.

---

## Detection

### Path overlap — deterministic
Normalize on insert: reject absolute/`..`, collapse separators, trailing `/` → `**`, a wildcard-free
last segment with no `.` → treated as a directory. Drop anything matching `ignore_patterns`; defaults
include `**/*.gen.go` and `vendor/**`, **which matters enormously here because nuzur generates most
of the Go** and without it every agent collides with every other agent on generated CRUD.

Compute `prefix` = longest literal prefix truncated to the last `/` before the first wildcard. Two
patterns can overlap only if one prefix is an ancestor of the other, so candidate selection is one
sargable query on `(project_uuid, status, prefix, expires_at)`: `prefix IN (:ancestors)` (≤8 point
lookups) `OR prefix LIKE CONCAT(:my_prefix,'%')` (left-anchored range scan). **No trie needed —
path segments give the ancestor set for free.** Confirm the tens of candidates in Go with
`bmatcuk/doublestar/v4` (stdlib `path.Match` has no `**`).

Severity: read×read is **never** a conflict; write×read `medium`; write×write glob `high`; identical
exact path `critical`; `structural` (rename/move/delete) against anything `high`, because renames
break readers silently. Then deterministic adjusters: −1 same member, −1 same branch, capped at
`low` if either claim is over-broad (depth ≤ 1), +1 on hotspot paths (lockfiles, migrations,
`go.mod`) which carry a canned "regenerate after merge, don't hand-resolve" note.

### Contract mismatch — deterministic core
Server canonicalizes every submitted shape (sorted keys, closed type vocabulary, descriptions
stripped) and hashes it; **agents never compute the hash**, or two agents describing the same shape
produce different bytes and dedupe evaporates. Fast path is `COUNT(DISTINCT shape_hash)` — one
distinct hash means done in ~0.1ms, which is most calls. Otherwise a four-branch directional
anti-join over `contract_field`: required *out* fields the consumer needs and the producer omits;
required *in* fields the producer needs and the consumer omits; type disagreements on shared paths;
naming variants (same `path_snake`, different literal — `low`, usually a serializer detail).

Two free sub-kinds worth as much as the main one: **`contract_unclaimed`** — a `consumes` assertion
with no active `produces` for >5 minutes, i.e. *"Bob is coding against an endpoint nobody is
building"*, which in a hackathon is the single highest-value signal in the system; and key-level
Levenshtein ≤2 on `key_norm`, catching `/api/session` vs `/api/sessions`.

### The semantic half — context return, then verdict report
After deterministic detection, `declare_intent` returns a **review block**: top ~3 candidate
decisions (always-show ones, scope overlap via the same prefix query against `decision_path`, then
token overlap) and at most 2 plans that may be the same work (the same `external_ref` anywhere on the
team first, then summaries in the same project that name the same thing, with overlapping claims
lowering the bar; docs/DUPLICATES.md §4.2). Each carries a server-computed **`pair_key`**.

**Budget: hard cap 700 tokens on the whole response, typical steady state ~90**, because suppression
means the block is usually empty. Cut order when over: full contract shapes → pointer; extra
candidates; path lists → counts; statements → titles; and finally **drop the block entirely**, set
`review_pending`, and let the agent fetch it with the read-only `get_review_context`. That last step
is the escape hatch that makes the budget safe — the feature degrades to zero inline tokens without
being lost, and can ship disabled per-team.

`pair_key = sha256(kind | sorted(a.uuid@a.revision, b.uuid@b.revision))`. Server-computed, symmetric,
and **revision-scoped** — which answers "how do you stop every agent re-judging the same pair
forever": judged once, never surfaced again, unless a subject materially changes, which mints a new
key and earns exactly one more look. On first surfacing the server inserts a `judgement` row
assigned to the plan's own agent with a 15-minute window, re-armed at most three times; the unique
index makes it insert-first-wins, so **N agents never burn N× tokens on one pair**, and a pair whose
judge goes quiet is re-armed or expired by the sweeper (docs/DECISIONS.md §1.3, §4.4).

`report_judgement(pair_key, verdict, severity, confidence, rationale)` closes the loop. `no_conflict`
kills the pair permanently and costs one tiny event — and that is 80% of cases, so it must be cheap.

### Races
There is no simultaneity: detection and insertion happen **inside the same transaction as the
sequence lock**. Check overlap outside the lock and you get exactly the TOCTOU race where both agents
see a clean world and both insert. Nobody is ever blocked — ordering assigns *responsibility*, not
permission: the **later declarer is told synchronously** (they have the info in hand and haven't
started), the **earlier declarer asynchronously** via an instruction that surfaces on their next
call, including a bare heartbeat. For duplicate work the earlier declarer is told only if the
conflict is still open after a short grace, so a later declarer that yields at once interrupts
nobody (docs/DUPLICATES.md §4.5).

### Noise control — non-negotiable
A conflict system that cries wolf gets ignored, and an LLM that learns to ignore your tool output is
unrecoverable. The rules: read×read never conflicts; generated code never enters `claim_path` at all;
broad claims are capped at `low` and instead get a "narrow this" nudge; one human's two agents get
severity −1 and no interrupt; one conflict per pair forever with re-notify only on escalation;
**record floor `low` but notify floor `medium`**; LLM verdicts interrupt only at `confidence ≥ 0.7`
because models asked "is this a conflict?" have a strong yes-bias; ≤3 review items per session per
minute; and **never surface a conflict without a suggested next action** — *"you and Alice both hold
auth.go"* is noise, *"Alice holds auth.go (write, 4m, feat/auth); consider consuming her
POST /api/login contract instead"* is signal.

Plus a self-tuning loop: `resolve_conflict(status='dismissed', reason)` tracks per-rule dismissal
rates over a trailing 20, and a rule exceeding 50% `false_positive` on a project auto-demotes to
record-only with a one-click re-enable on the board. ~30 lines, no ML, and a rule that is wrong for
your repo stops shouting within an afternoon.

---

## Binding a repo to a team — ask, never infer

One person is on several teams: a personal project and a hackathon, at least. So every
repo has to say which team its work belongs to, and **getting that wrong is the worst
failure this system has** — it puts private file paths, branch names and decisions on a
board other people can read.

The rule is therefore: **the agent asks, and never guesses.**

- A `.metiche` file names the team. The agent walks **up** from the working directory and
  takes the nearest one, the way `.gitignore` and `.editorconfig` resolve — so a folder
  holding five hackathon repos needs one file, not five.

  ```
  # ~/work/hackathon/.metiche
  team = hackathon-2026        # required: the team slug
  project = orbital-freight    # optional: defaults to the repo directory name
  ```

  Two placements, both useful. Inside a repo it is committed, so every teammate who clones
  gets the same binding and nobody is asked at all. In a parent directory it covers every
  repo beneath it and belongs to you alone, never entering a repository. A file deeper in
  the tree overrides a shallower one, which is how one repo inside `hackathon/` opts out.

  **Never a credential.** The slug is not access; the token stays in `~/.metiche/env`. A
  `.metiche` file is safe to commit precisely because holding it grants nothing.
- No `.metiche` and the account is on exactly one team → use it, and **say so once**.
  With one team there is nothing to disambiguate, so asking would be friction for no
  information. But the exposure has not gone away: if your one team is the hackathon team
  and you open a private repo, it binds silently and your private work lands on a board
  those people can read. So the binding is automatic and *announced* — one line the first
  time a repo is bound, never again. Automatic is fine; silent is not.
- No `.metiche` and the account is on more than one → **stop and ask the person**, then
  write the answer to `.metiche` so it is asked once per repo, not once per session.
- **Never derive a team from the directory name**, the repo name, or the remote URL. A
  plausible guess that is wrong is worse than a question, because nobody reviews a guess.

What limits the blast radius if it still goes wrong: a slug is not access. Boards are
private by default and reading requires membership, so a mis-bound or leaked `.metiche`
exposes nothing to somebody who is not already in that team. The exposure that matters is
declaring private work into a team you really are a member of, and that is precisely the
case the question prevents.

This is a skill and convention change, not a schema one: the server never reads your disk.
The agent reads `.metiche` and passes `team_slug`.

## MCP tool surface — 15 tools

Bias: collapse verbs onto few nouns. Agents pick correctly when each tool maps to something they
were already going to say in prose.

| | tool | when |
|---|---|---|
| 1 | `join_team` | once |
| 2 | `start_session` | once per session |
| 3 | `end_session` | once per session |
| 4 | **`heartbeat`** — alive, extend claims, get pending counts | **every ~60s** |
| 5 | **`declare_intent`** — what I'm about to do + the paths I'm taking | **every loop; the main tool** |
| 6 | **`update_intent`** — status / scope / what I'm doing right now | **every loop** |
| 7 | `check_paths` — who else is in these files (readOnly, no commitment) | before exploring |
| 8 | `publish_contract` — I produce *or* consume this shape (`role` param) | occasional |
| 9 | `record_decision` | rare |
| 10 | `get_review_context` (readOnly) | when deferred |
| 11 | `report_judgement` — my LLM checked a pair, here's the verdict | occasional |
| 12 | `resolve_conflict` | occasional |
| 13 | `get_instructions` — **not readOnly: reading is the delivery receipt** | when pending > 0 |
| 14 | `report_back` | after acting |
| 15 | `get_team_state` (readOnly, scoped, cursor-paginated) | session start |

**There is no user-facing claim verb.** `declare_intent(summary, paths[], mode)` creates the intent
and its claims in one transaction; `update_intent(add_paths, drop_paths, status)` absorbs what would
otherwise be `record_activity`, `release_claim`, `extend_claim` and `complete_intent`. Agents learn
one concept, not two.

`heartbeat` is the only tool with a **time obligation** — an agent can edit for 20 minutes without
any other call and its claims would lapse. It takes no team lock, writes no event, and is the
cheapest carrier of the pending counts.

Every response, without exception:
```json
{"ok":true,"key":"...","sequence":417,"revision":88,
 "pending":{"instructions":0,"conflicts":0,"reviews":0},"note":"..."}
```
plus `conflicts[]` and `review` where applicable. **Never the board, never everybody's claims.**

Annotations matter: MCP's `destructiveHint` **defaults to true when omitted**, so every tool declares
`readOnly` / `idempotent` / `additive` explicitly or clients gate the whole surface.

**Board sign-in tools** (`docs/BOARD_LOGIN.md` §2.3, §5.4). These are account-scoped and outside
the loop above. Their results are not the `Envelope`: there is no team sequence to report.

| tool | when | note |
|---|---|---|
| `open_board` — `team_slug?`, `requested_via?` (`agent` default, `cli`, `installer`) | the person asks to see the board | additive. Needs an **agent** token; a legacy account token is `not_permitted`. Returns `login_url` (`<board>/signin#mbl_…`, single use, 10 min) and `board_url`. The link lands on `/t/<slug>` for a given slug or the only team, and `/teams` for none or several. 30/hour and ≤5 outstanding links per account. |
| `sign_out_browsers` — `session_key?`, `all?` | the person asks to see or end their browser sessions | destructive, idempotent. **No arguments only lists**; `session_key` revokes one, `all=true` revokes every one. 60/hour per account. |

---

## Frontend — Go, templ + htmx + SSE

The frontend is its own Go service that consumes the backend's JSON SSE and re-broadcasts **rendered
HTML fragments**, so the browser does no templating.

The two cursors map directly onto two htmx behaviors: **`sequence`** appends to the timeline
(`sse-swap` + `hx-swap="beforeend"`), **`board_revision`** re-renders the board
(`hx-swap="outerHTML"`). One stream, two event names.

**The landing page is part of the product, not marketing afterthought.** `metiche.xyz` is where
somebody who has never heard of this understands what it is, why parallel agents collide, and how
to get their own team on it in about a minute. It has to carry the idea — two agents' paths crossing
into an eye, which is the mark — and end in a join flow that actually works. It is the page that
decides whether anyone ever sees the board.

Pages: `/` landing + join · `/t/{slug}` the live board (a lane per member → their agents → current intent,
status line, held paths, live conflict badges) · `/t/{slug}/conflicts` · `/t/{slug}/contracts` (the
produces/consumes matrix — this is the view that shows the bottleneck) · `/t/{slug}/decisions` ·
`/t/{slug}/runs/{session_key}` history. Humans raise nudges and resolve conflicts from the board.

Sign-in pages (`docs/BOARD_LOGIN.md`, built, not yet deployed):
- `/signin`: redeems a link from `open_board` (`/signin#mbl_…`), and explains how to get one when
  there is none.
- `/account`: the viewer's signed-in browsers, with sign out and sign out everywhere.
- `/teams`: gains "Your teams" for a signed-in viewer.
- `POST /signout`: signs this browser out.

A private team's board is served only to a signed-in live member, read with that viewer's own
session. For everyone else it is the same 404 as a team that does not exist. Until backend-backed
controls exist, a live board's nudge, resolve and cadence controls answer 404; they still work on
demo boards (§4.6 there).

---

## Build phases

1. **Scaffold** — repo layout, `.gitignore`, README, CLAUDE.md.
2. **Schema** — nuzur team + project + `v1-coordination`, all entities/enums/indexes above, publish,
   `nuzur-codegen`, local MySQL, `create.sql` applied.
3. **Core write path** — team lock, event log, idempotency + `response_snapshot` replay, response
   envelope with pending counts. `join_team` / `start_session` / `heartbeat` / `end_session`.
   Panic-recovery tool wrapper `safetool.go`.
4. **Intents and claims** — `declare_intent` / `update_intent` / `check_paths`, path normalization,
   the candidate scan, severity matrix, dedupe key, conflict + participants + instructions.
5. **Contracts** — canonicalization, hashing, the anti-join, `contract_unclaimed` sweeper rule.
6. **Semantic loop** — review block + budget/cut order, `pair_key`, judge assignment,
   `report_judgement`, `get_review_context`, `resolve_conflict`, noise rules.
7. **Sweeper + SSE** — TTL expiry (lazy filter is authoritative; sweeper is for *visibility*),
   in-process fan-out + 250ms DB tail, `/v1/teams/{slug}/stream`.
8. **Frontend** — board, conflicts, contracts matrix, decisions, run history, nudges.
9. **Skill + plugin** — `metiche-teamwork/SKILL.md`, `.mcp.json`, plugin manifest; point metiche at
   itself.
10. **Deploy** — onto the existing nuzur Linode box under a new `metiche` namespace (no new host, no new cluster bootstrap). Helm charts and scripts, `METICHE_ROLE=all` for v1.

---

## Execution strategy — parallel, with proof gates

**Step 0 (me, first):** write this plan to `docs/PLAN.md` in the repo, plus the scaffold it
describes (`.gitignore`, `README.md`, `CLAUDE.md`). Everything else
keys off that file, including the subagents.

**Step 1 — the critical path is the schema, so unblock it immediately.** I create the nuzur team,
project and `v1-coordination` version myself and run codegen. Nothing that touches generated code
can start before this.

**Wave A — starts now, in parallel, needs nothing from the schema.** The highest-value
parallelization is that the two hardest pieces are *pure functions* with zero dependency on
generated code:

| agent | scope | proof required |
|---|---|---|
| A1 | `coordination/paths.go` — normalization, ancestor/prefix computation, doublestar overlap, severity matrix + adjusters, dedupe keys | `go test ./coordination/ -run Path -v` output pasted, covering every case named in the Verification section |
| A2 | `coordination/contracts.go` — shape canonicalization, stable hashing, field flattening with `direction`, `pair_key` | same, plus a test proving canonical bytes are stable across key order and whitespace |
| A3 | `code/frontend/` shell — templ + htmx + SSE skeleton against a fixture feed, board/conflicts/contracts/decisions/runs pages | `go build ./...` and a screenshot of the board rendering from fixtures |
| A4 | `skill/metiche-teamwork/SKILL.md`, `plugin/`, `.mcp.json`, `deploy/` Helm charts | the skill read back in full; `helm template` output for each chart |

**Wave B — after codegen lands.** B1 core write path (team lock, event log, idempotency +
`response_snapshot` replay, response envelope, tools 1–4 and 15). B2 the sweeper + SSE fan-out.
These two share `app/mcp/` so they are sequenced, not parallel.

**Wave C — integration, me.** Wire tools 5–14 onto B1's core using A1/A2's pure functions. This is
the part where the seams show, so I do it rather than delegate it.

**Proof gates — what I will not accept from a subagent.** Every agent reports: files written, the
actual command it ran, and the actual output. I then verify independently — run the tests myself,
read the diff. A report of "tests pass" with no pasted output is treated as untested work and sent
back. Nothing merges on a subagent's say-so.

**Hard rules handed to every subagent:** no credential, token or connection string in any file; no
outbound HTTP from `app/coordination/`; never edit a file carrying the nuzur generated-code marker
(schema changes go to nuzur and get regenerated).

---

## Verification

**Unit (Go, table-driven, no DB):**
- `paths_test.go` — normalization and overlap. Must cover `src/**` vs `src/api/user.go`,
  `src/api/*_test.go` vs `src/api/user_test.go`, glob×glob, ignore-list rejection, the full severity
  matrix and every adjuster.
- `contracts_test.go` — canonical bytes are stable across key order and whitespace; the four
  anti-join branches each fire on a crafted pair; `direction` flips which side is at fault.
- `pairkey_test.go` — symmetry, and that a revision bump mints a new key while a cosmetic edit does
  not.

**Integration (real MySQL):**
- Idempotency: the same call twice returns byte-identical responses and writes one event.
- Concurrency: N goroutines `declare_intent` on overlapping paths simultaneously → exactly one
  conflict row, `sequence` gapless, no lost update. This is the test that proves the lock.
- TTL: freeze a session's heartbeat → claims drop out of detection lazily before the sweeper runs.
- Assert `metiche_team_lock_hold_ms` p99 stays under 25ms under that load.

**End to end — the real test:** two Claude Code sessions in two terminals, both with the skill and
both pointed at a local metiche, told to work on the same repo. Walk the scenario: A declares the
login endpoint and publishes the producer contract → B declares the login UI, collides on
`auth.go`, and is told **in its own tool response** → B's model judges the localStorage plan against
the recorded cookie decision and reports the verdict → B publishes a consumer contract whose
response shape doesn't match → A learns on its next heartbeat, never having polled → A re-publishes
→ the conflict auto-resolves as `converged`. Watch the board the whole time; every step must land
in under a second. Count the interruptions to A: **one**, and it was actionable. That ratio is the
product.

**Security gate before first push:** confirm `.gitignore` covers the overlay and credentials, that
no token or connection string is in any committed file, and add a CI grep forbidding outbound
`net/http` client calls from `app/coordination/` — that constraint is the one a well-meaning
contributor will break first ("let's just call an LLM to rank the decisions").
