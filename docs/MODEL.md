# The data model, in one page

24 entities: 21 tables and 3 dependent shapes (typed JSON columns, not tables of their own).
This is the "what is each thing for" summary. The full spec is in [PLAN.md](PLAN.md); the
authoritative definitions, with every field described, live in the nuzur project.

---

## Who is involved

| | |
|---|---|
| **`team`** | A group working together, and the scope of everything else. It is also the **sequence lock** — every write that produces an event takes a row lock on it, which is what gives the event log a total order and makes collision detection race-free. Holds the two cursors (`sequence`, `board_revision`) and the join code. |
| **`member`** | A human. |
| **`agent`** | One agent instance belonging to a member. A person can run several at once, which is why this is separate from `member` — and why several detection rules soften when two colliding sessions turn out to belong to the same person. Holds the hash of its bearer token, never the token. |
| **`project`** | One repository. Claims are scoped to it, or a team working across several repos would collide with itself for no reason. Also carries the per-repo tuning: which paths to ignore, which are hotspots, and the **cadence** (hackathon / sprint / steady) that sets how urgent things are and how the copy reads. |
| **`session`** | One bounded piece of work by one agent on one branch. This is also the **run record** — a run's history is just the event log filtered by `session_uuid`, which is why there is no separate graph of steps. Carries the one-line "what I'm doing right now" the board shows. |

## What people are doing

These two are deliberately separate, and it is the most important split in the model.

| | |
|---|---|
| **`intent`** | The **semantic** surface: what an agent is about to do, in prose. Judged by other agents' own models, retrieved by word overlap, long-lived. |
| **`claim`** | The **mechanical** surface: an advisory, expiring hold on part of the repo. Short TTL, refreshed by heartbeat. Never blocks anyone — overlap is allowed, it just raises a conflict. |

They are separate because a claim has to be able to expire without abandoning the plan. An agent
thinking for twenty minutes should drop its holds, not its intent. Ergonomically they are still
one call: agents declare an intent with paths and never learn a second concept.

| | |
|---|---|
| **`claim_path`** | One normalized path or glob inside a claim, and the hottest table in the system. Six columns are copied down from `claim` so the overlap scan that runs inside *every* intent declaration is a single index range scan with no joins. |
| **`intent_token`** | The intent's wording, tokenized, so a similar intent elsewhere can be found by overlap. Tokenized in Go rather than with MySQL full-text search, whose defaults silently swallow exactly the words that matter here — `jwt`, `api`, `ui`. |

## What the team has agreed

| | |
|---|---|
| **`contract`** | An interface two sessions have to agree on: an endpoint, a shared type, a table, an env var, a component prop. Identity is the *normalized* key, so the agent building the route and the agent calling it land on the same row even when they spell the path differently. |
| **`contract_assertion`** | One session's claim about a contract — either it **produces** that shape or it **consumes** it. Both roles share one table, which is what turns a mismatch into a self-join instead of two tables somebody has to keep in sync. |
| **`contract_field`** | One leaf of a canonicalized shape, flattened into a row so the mismatch check is plain SQL. Each row knows its **direction**: a missing required *out* field breaks the consumer, a missing required *in* field breaks the producer. Without that, the comparison blames the wrong person. |
| **`decision`** | A team decision of record — "auth is a JWT in an httpOnly cookie". The kind of drift that never shows up as a merge conflict. A few are flagged always-show, because the decisions people violate are the ones nobody thought to repeat. |
| **`decision_path`** | A decision's scope in the same normalized form as `claim_path`, so "which decisions touch what this intent is claiming" reuses the identical query rather than a second implementation that could drift from it. |
| **`decision_token`** | The decision's wording tokenized, mirroring `intent_token`, so it can surface even when no paths intersect. |

## What went wrong, and who was told

| | |
|---|---|
| **`conflict`** | A detected collision. Deduped hard: re-detecting the same pair bumps a counter instead of inserting, and re-notifies only if it got worse. Carries a **suggested action**, because "you and Ana both hold auth.go" is noise and "Ana owns this handler, consume her endpoint instead" is signal. |
| **`conflict_participant`** | A session caught up in a conflict. A child table rather than two columns, because three agents in one directory is ordinary — and because "conflicts involving my session" is the query behind the count on every tool response. |
| **`judgement`** | The ledger that stops every agent re-judging the same question forever. A pair is shown to exactly one session, answered once, and never raised again — unless one side materially changes, which mints a new key and earns it exactly one more look. |
| **`instruction`** | Something waiting for an agent — a person's stop or steer, a conflict notice, a request to judge a pair — or, since v2, something waiting for a **person**. This is the **entire push mechanism**: MCP cannot push, so a count of these rides on every tool response and the agent fetches the contents only when the count is non-zero. |

## Reaching a person

| | |
|---|---|
| **`notification_channel`** | An optional outbound channel per team — Slack, Discord, or a plain webhook — for the people who are not sitting at their terminal. Entirely opt-in, because the primary way somebody hears about a conflict is their own agent telling them, which needs no configuration at all. Tracks its own delivery health so a team learns their webhook died from the board rather than from the silence, and switches a persistently failing channel off instead of retrying forever. |

The `target_url` on that row is the first real secret in the system: anyone
holding an incoming-webhook URL can post into the team's channel. It is
write-only from the outside — never returned by an API, never rendered on the
board, never logged, never in an event payload. Even `last_error` is capped to
a status line, because a response body can echo the URL back.

## The record

| | |
|---|---|
| **`team_event`** | Append-only, never updated, and the only source of truth. Everything else is a materialized view of it. Per-team monotonic `sequence` lets a browser resume an SSE stream at an exact cursor; a unique idempotency key plus a stored copy of the response lets a retried tool call replay verbatim instead of applying twice. |

## The three dependent shapes

Not tables — typed JSON columns. They exist so the generated code has a real struct instead of
an opaque blob, without paying for a table nothing queries independently.

| | |
|---|---|
| **`team_settings`** | On `team`. Detection tuning a team can change without a deploy: TTLs, which rules have been auto-demoted for crying wolf, and the two notify floors. There are three thresholds in total and they are deliberately different — what gets **recorded**, what interrupts another **agent**, and what is worth pulling a **person** away from what they are doing. The human floor sits highest: somebody interrupted as often as an agent stops reading the interruptions. |
| **`conflict_evidence`** | On `conflict`. What the two sides actually said, frozen — so the call can still be judged later, after both subjects have moved on. |
| **`event_payload`** | On `team_event`. The delta an event carries, with the fields every client needs typed and one escape hatch for the long tail. |

---

## Three shapes that look odd on purpose

**`claim_path` repeats six columns from `claim`.** Normalization would cost a join on the
query that runs inside every single intent declaration. This is the one place the model trades
tidiness for a measured read path, and it is the right trade.

**`contract_assertion.active_marker` is `1` or `NULL` and nothing else.** MySQL has no partial
unique index, so this column plus its unique index is the only way to say "at most one active
assertion per contract, session and role" — a unique index ignores repeated NULLs. It looks like
a mistake. It is load-bearing.

**Five `*_uuid` columns have no relationship.** `conflict_participant.subject_uuid`,
`judgement.subject_a_uuid` and `subject_b_uuid`, `instruction.ref_uuid`, and
`team_event.subject_uuid` are polymorphic: each is paired with a `*_kind` enum and points at a
different table depending on its value. A foreign key cannot express that. Every other `*_uuid`
in the model has an edge.
