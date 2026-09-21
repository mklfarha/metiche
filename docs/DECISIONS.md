# Decisions: record them, and let each agent's own model judge its plan against them

Status: **design approved 2026-09-15, not yet built.** The data model is published in nuzur as
`v7-decisions` (`f73e4dfa-8d6c-49f2-825c-da7c1e756995`). Codegen and the production migration are
being done now (Wave 0, §8). The owner's answers are folded in as decisions (§10). Build agents
implement straight from this file. §9 "Frozen interfaces" is the contract between them; change it
only through the coordinator.

This covers PLAN's tools 9–11: `record_decision`, `get_review_context` and `report_judgement`, and
the `decision_contradiction` conflict kind. `resolve_conflict` (tool 12) and duplicate work come
next. Nothing here boxes them in (§4.6, §3.3 "dispatched by kind").

## Why

Path overlap and contract mismatch catch collisions a machine can see. The third kind in PLAN is
the one nobody sees until review: the team settled something ("auth is a JWT in an httpOnly
cookie"), and an agent that was not in that conversation writes a token to localStorage. That is
correct from everything it can see. The landing page promises this ("The decision you made at nine
is gone by eleven"), and the Decisions tab is empty because nothing can record one.

**The direction.** Agents record decisions. When a live plan (an intent) touches a decision, by its
files, its wording, or because the decision is flagged always-show, the server pairs them. It hands
the pair to **the plan's own agent**, whose model judges it and reports a verdict. The server never
calls a model. It picks candidates, keeps the ledger that stops re-judging, raises and settles the
conflict, and asks a person only when the agents did not settle it in time.

---

## 0. What the code does today (verified while writing this)

| # | finding | where |
|---|---|---|
| F1 | The envelope already carries `review` (unused) and `pending.reviews`, counted from `judgement` rows assigned to the session. The pending note says judging is not available. | `app/mcp/envelope.go` `Envelope.Review`, `Pending`, `pendingCounts`, `NoteForPending` |
| F2 | No hand-written code writes `decision`, `decision_path`, `decision_token`, `intent_token` or `judgement`. Only `webapi/decisions.go` reads decisions and their paths. | grep over `app/` |
| F3 | `app/coordination` has `contracts.go`, `pairkey.go` and `paths.go`. `PairKey` is built and tested (symmetric, revision- and kind-scoped). There is no tokenizer; the `tokens.go` PLAN lists does not exist. | `app/coordination/` |
| F4 | Every write goes through `commit`: team row lock, replay check, Apply, Detect, pending counts, one event. A Detect hook may set `m.Envelope` fields and they land in the stored snapshot. The handler-wide detector is installed once in `app/rest.go` (`SetDetector(NewPathDetector(...))`). | `app/mcp/sequence.go` `commit`; `app/rest.go` |
| F5 | `update_intent` bumps `intent.revision` on a changed summary or changed paths, "which mints a new pair_key and earns the pair exactly one more look". A status line alone does not. | `app/mcp/intents.go` (`material`) |
| F6 | Contract conflicts are the pattern to copy. Dedupe on sessions, not revisions. A conflict metiche closed reopens; one a person closed is silenced (counted, never re-announced). The other side is re-notified only when the conflict is fresh or got worse. Settlement is a conditional UPDATE plus a derived `conflict_resolved` event that advances `tc.Sequence` and `tc.Revision`. | `contractdetect.go` `recordContractConflict`; `contractresolve.go`; `conflictresolve.go` `appendConflictResolvedEvent` |
| F7 | `get_instructions` truncates `what` and `suggested_action` to 200 characters (`instructionTextChars`). It splits only `conflict_notice` bodies, on the first `": "`. A `question` body is shown whole, up to 200. | `app/mcp/instructions.go` `renderInstructions`, `splitNoticeBody` |
| F8 | Nothing writes `instruction.target_member_uuid`, and a live board's controls answer 404. **A person can only be reached through their own agent.** | grep; `docs/PLAN.md` Frontend |
| F9 | `uq_contract_field_path` was `(assertion_uuid, path)`, so a path present in both request and response kept one row. `insertContractFields` skips the second. **Fixed in v7** (§2); the skip goes away. | `app/mcp/publishcontract.go` `insertContractFields` |
| F10 | Board: `decisions.templ` renders an empty state. `feed/wire.go` maps `accepted` to `active`. `state/store.go` folds `decision_updated`, which the backend never emits. `model.Decision.SessionKey` is never filled. `web/history.go` says Contracts and Decisions have no history. `state/graph.go` draws decision nodes from `Conflict.DecisionKey`, which no wire carries. | `code/frontend/internal/...` |
| F11 | The conflict history's kind filter (`liveConflictKinds`) excludes `decision_contradiction`. | `app/webapi/conflicthistory.go` |
| F12 | Retention never deletes decisions, their paths or tokens: they are standing agreements. It deletes closed sessions, and children cascade. | `app/sweeper/retention.go` rule 3 |
| F13 | The sweeper writes events through `appendEvent` with a locked `extra` hook, and returns `errNothingToSay` to roll back without consuming a sequence number. | `app/sweeper/conflicts.go` |

---

## 1. What PLAN and MODEL specify

### 1.1 The server never calls a model

This is not ambiguous in PLAN, and it is not a choice this design makes.

- PLAN:18-19: "No third-party credential may ever be required to run the server. That one constraint rules out a server-side LLM judge."
- PLAN:32: "Detection | hybrid — server deterministic only; the caller's own LLM judges semantics."
- PLAN:473-475, the end-to-end test: "B's model judges the localStorage plan against the recorded cookie decision and reports the verdict." B is the agent whose plan it is.
- PLAN:483-484: a CI check forbids outbound HTTP clients in `app/coordination/` ("let's just call an LLM to rank the decisions").
- nuzur `decision` description: "The server cannot tell whether an intent contradicts one, so it retrieves the candidates and the calling agent's own model judges."
- `landing.templ` footer: "the judgement calls coming next will run in your own agent's model."

### 1.2 What they specify

- **Decisions.** PLAN:158-159; MODEL:44-46. `key` (`#auth-jwt-cookie`), `statement` varchar(400) as a hard cap because it ships inline, `always_show`, scope, with `decision_token` and `decision_path` side tables. `decision_path` uses the same normalized prefix form as `claim_path`, so the same query finds it.
- **The review block.** PLAN:211-222.
  - `declare_intent` returns up to ~3 candidate decisions: always-show ones, then scope overlap through the prefix query against `decision_path`, then token overlap.
  - Each carries a server-computed `pair_key`.
  - The whole response has a hard cap of 700 tokens, typically ~90. The cut order ends with dropping the block, which the agent then fetches with read-only `get_review_context`.
  - The block can be switched off per team.
- **The pair key.** PLAN:224-229. `pair_key = sha256(kind | sorted(a.uuid@a.revision, b.uuid@b.revision))`.
  - On first surfacing, a `judgement` row is assigned to the caller with a 120s window.
  - The unique index makes the first insert win.
- **The verdict.** PLAN:231-232. `report_judgement(pair_key, verdict, severity, confidence, rationale)`. `no_conflict` kills the pair for good and "costs one tiny event".
- **Noise rules.** PLAN:247-251.
  - Model verdicts interrupt only at confidence ≥ 0.7.
  - At most 3 review items per session per minute.
  - Record floor `low`, notify floor `medium`.
  - Never surface a conflict without a suggested next action.
- **Later.** PLAN:253-256: `resolve_conflict(status='dismissed', reason)`, dismissal rates per rule, auto-demotion.
- **The judgement ledger.** MODEL:54. A pair is shown to exactly one session, answered once, and never raised again unless a side materially changes.
- **Pinning.** nuzur `judgement.pinned`: "Set when a human dismisses the conflict. Suppresses regeneration even across revision bumps." This belongs to `resolve_conflict`.
- **Revisions.** nuzur `decision.revision`: "Baked into pair_key, so amending a decision re-opens it for judgement against intents already cleared against the old wording."
- **Three floors.** MODEL:82: recorded, interrupts an agent, worth a person. "The human floor sits highest." `team_settings` already has `review_block_enabled`, `judge_window_seconds`, `max_reviews_per_minute` and `human_notify_floor`.

### 1.3 Where the code and PLAN disagree, and what this spec does

1. **Judge requests.** PLAN:164 lists "judge requests" as instructions. The built envelope has a separate `pending.reviews` count. Using both would double-interrupt. The instruction's at-most-once delivery also does not fit a judge who must re-read. **Decided:** judge requests travel only as `pending.reviews` and `get_review_context`. No `judge_request` instruction is written. The fallback text for that kind points at `get_review_context`, for any stray row.
2. **`review_pending`.** PLAN:220's flag is the built `pending.reviews > 0`.
3. **The 120s window.** PLAN:228 designed the window for "whoever surfaces it first judges it". For a decision-and-plan pair the only sensible judge is the plan's owner, so reassigning it elsewhere makes no sense. **Decided:** a 15-minute window, re-armed for the same session up to 3 times (§4.4).
4. **A person as a target.** MODEL:55 says an instruction can wait for a person "since v2". Nothing delivers one (F8). **Decided:** a person is asked through their own agent, with a `question` instruction (§4.7).

### 1.4 Where PLAN is silent, decided here

| Silence | Decision | § |
|---|---|---|
| Decisions recorded after plans already exist | `record_decision` pairs itself with live plans that touch it | 3.1 |
| Review on `update_intent` | the reviewer runs on declare and on non-terminal update; unchanged revisions mint no new pairs | 3.2 |
| Who judges | the plan's own agent, always | 10 |
| `unsure` | recorded as a `low` conflict: on the board, interrupts nobody | 4.2 |
| Fault | always the plan; changing the decision is a separate, permissioned act | 4.2 |
| Reopening | contract rule: a conflict metiche closed reopens, one a person closed is silenced | 4.6 |
| Changing another person's decision | needs `person_confirmed: true` | 3.1, 10 |
| `proposed` | not used in v1; everything is recorded `accepted` | 10 |
| Asking a person | sweeper rule, time-based, once per conflict | 4.7 |

### 1.5 Stale text that changes in the same release

- `skill/metiche-teamwork/SKILL.md` and `plugin/skills/metiche-teamwork/SKILL.md`: "Coming next" (tools list) and the `reviews` bullet.
- `app/mcp/envelope.go` `NoteForPending` reviews case.
- `app/mcp/instructions.go` `fallbackAction` for `INSTRUCTION_KIND_JUDGE_REQUEST`.
- `app/mcp/server.go` `serverInstructions`.
- `code/frontend/internal/view/landing.templ`: the file comment, "Tools to record decisions" in the tool group, the "Not built yet" heading and `collideDecision`, and the footer's "coming next".
- `code/frontend/internal/view/docs_pages.templ`: the board page's "Other tabs" note, and the "Not built yet" list item "Decisions".
- `code/frontend/internal/web/history.go` comment on Contracts and Decisions history.
- `app/webapi/conflicthistory.go` `liveConflictKinds`.

---

## 2. Data model — approved and published in nuzur 2026-09-15 (`v7-decisions`)

### 2.1 What already exists (unchanged)

| Table / enum | Used for |
|---|---|
| `decision` (key 80, title 140, statement 400, rationale TEXT, status, always_show, supersedes_uuid, superseded_by_uuid, decided_by_member_uuid, decided_at, revision) with `uq_decision_team_key`, `idx_decision_always_show (team_uuid, status, always_show)` | the decision row |
| `decision_path` (pattern, pattern_norm, kind, prefix, depth, project_uuid nullable) with `idx_decision_path_scan (team_uuid, prefix)` | scope, found by the prefix scan |
| `decision_token` (token 32, weight) with `idx_decision_token_lookup (team_uuid, token)` | wording overlap |
| `intent_token` | **not written by this feature.** Left for duplicate work (§3.1 explains why it is not needed here) |
| `judgement` (pair_key, kind, subject_a/b kind, uuid, revision, status, verdict, severity, confidence, rationale 400, judge_session_uuid, judging_expires_at, conflict_uuid, pinned) with `uq_judgement_pair`, `idx_judgement_assignment` | the ledger |
| `conflict`, `conflict_participant`, `instruction`, `team_event` | as for contracts |
| `conflict_kind.decision_contradiction`; `event_kind` `decision_recorded` (14), `decision_superseded` (15), `conflict_escalated` (17), `judgement_reported` (19); `judgement_status` pending/judged/expired; `judgement_verdict` conflict/no_conflict/unsure; `decision_status` proposed/accepted/superseded/revoked; `subject_kind` intent/decision; `detected_by.agent`; `conflict_resolution` converged/superseded/coordinated; `instruction_kind` question/conflict_notice | **no enum changes** |

`conflict_evidence` does not change either. A decision conflict reuses `overlap_path` for the
decision key, the way contract conflicts use it for the contract key (§3.3).

### 2.2 The v7 changes

1. **`contract_field.uq_contract_field_path`** is now `(assertion_uuid, direction, path)`. `insertContractFields` keys its de-duplication on direction plus path, and the limitation comment is deleted.
2. **`decision.recorded_by_session_uuid`**: uuid, nullable, FK `session_has_recorded_decisions` → `session(id)` ON DELETE SET NULL. It is the session that last recorded, revised, superseded, revoked or reinstated the decision. It serves two purposes:
   - the reviewer skips pairing a decision with plans of the session that just recorded it;
   - a dispute is routed to that session while it is live.
3. **`judgement.judged_at`**: datetime, nullable, no default. Set when a verdict arrives; `updated_at` moves on later pinning.
4. **`judgement.assignment_count`**: smallint, default 0. Times the pair was armed for its judge. The sweeper stops re-arming at 3.
5. **`idx_judgement_subject (team_uuid, subject_a_uuid, subject_b_uuid)`**. Convention: for `decision_contradiction`, subject a is **always** the decision and b the intent. The key stays symmetric; storage order is fixed. It serves the Decisions tab counts, the pinned check across revisions, and "the latest conflict verdict on this pair".
6. **`idx_judgement_team_open (team_uuid, status, judging_expires_at)`**. The sweeper uses it for pending rows that are unassigned (NULL expiry sorts first) or expired.
7. **`conflict.escalated_at`**: datetime, nullable, no default. When metiche asked a person because the agents did not settle it. Cleared when a conflict metiche had closed reopens.

### 2.3 Migration (applied BEFORE the new backend binary)

The SQL lives at **`deploy/sql/2026-09-decisions.sql`**, written by the codegen agent. It is
authoritative; nothing in this file restates it. What it must do, and what build agents may rely on:

- **Order is load-bearing.**
  - An old binary on the new schema is fine. It never writes an in and an out row for the same path, and never names the new columns.
  - A new binary on the old schema fails. `publish_contract` inserts both rows for a shared path, which the old index refuses. Every `record_decision`, review assignment and escalation names a missing column.
- **No data migration.** Existing rows get NULL or 0. Widening a unique index cannot fail on existing data.
- **The index swap must never leave `contract_field` without an index leading with `assertion_uuid`.** The FK `assertion_has_fields` needs one; MySQL refuses otherwise with error 1553. So: add the new index under a temporary name, drop the old one, rename.
- **Verify afterwards** with `SHOW CREATE TABLE` for `contract_field`, `decision`, `judgement` and `conflict`, and compare column order, index names and the FK name with the regenerated `core/repository/sql/schema/create.sql`.
- `deploy/scripts/apply-schema.sh` only runs `CREATE TABLE IF NOT EXISTS`. That file is how production gets these changes.

---

## 3. Tools

### 3.0 The shared pattern, and the limits

Every tool follows `publish_contract`:

- **Resolve the caller.** `who := h.RequireSessionOnTeam(ctx, args.SessionKey, args.TeamSlug)`. `record_decision` also calls `requireWorkableSession`. `get_review_context` and `report_judgement` accept live and stale sessions; `get_review_context` also accepts ended ones, like `get_instructions`.
- **Validate outside the lock.** All validation and normalization, including path normalization with the project's ignore patterns and case setting, and tokenizing.
- **Write through `commit(Mutation{Apply, Detect})`.** Apply re-reads the session status under the lock. Derived `conflict_resolved` events go through `appendConflictResolvedEvent`.
- **JSON goes in as a string.** Every JSON column is written as `string(...)`, never `[]byte` (see detector.go's note on MySQL 3144 with `interpolateParams=true`).
- **Every query inside the lock is bounded.** Each is a point lookup or an index range scan with an explicit `LIMIT`.
- **Free text is sanitized.** Anything an agent typed that reaches a board note or an event goes through `sanitizeNoteText`.

| Constant | Value | Meaning |
|---|---|---|
| `MinDecisionKeyChars` / `MaxDecisionKeyChars` | 3 / 60 | after normalization, without `#` |
| `MaxDecisionTitleChars` | 140 | `decision.title` |
| `MinDecisionStatementChars` / `MaxDecisionStatementChars` | 20 / 400 | runes |
| `MaxDecisionRationaleChars` | 2000 | runes, stored in TEXT |
| `MaxDecisionScopePaths` | 16 | |
| `MaxAlwaysShowPerTeam` | 5 | accepted always-show decisions |
| `decisionMaxTokens` | 32 | per text, from `coordination.Tokenize` |
| `decisionMinSharedTokens` | 2 | words signal |
| `decisionNearDuplicateTokens` | 5 | the "looks like #x" note |
| `decisionMaxCandidatesPerRecord` | 12 | pairs one `record_decision` creates |
| `decisionMaxLiveIntentsScanned` | 100 | per project |
| `decisionMaxProjectsScanned` | 8 | team-wide decisions |
| `reviewMaxPathsScanned` | 16 | caller's paths scanned against `decision_path` |
| `reviewDecisionPathLimit` | 200 | rows per scan |
| `reviewTokenCandidateLimit` | 10 | decisions from the token query |
| `reviewMaxInlinePairs` | 3 | |
| `reviewInlineChars` | 1200 | rendered `review` block |
| `ReviewContextDefaultLimit` / `ReviewContextMaxLimit` | 3 / 3 | |
| `reviewContextCharBudget` | 2200 | like `instructionsResponseCharBudget` |
| `judgeRateWindow` | 60s | with `team_settings.max_reviews_per_minute`, default 3 |
| `decisionJudgeWindow` | 15m | `team_settings.judge_window_seconds` overrides |
| `judgeMaxAssignments` | 3 | |
| `coordination.JudgeMinConfidence` | 0.70 | inclusive |
| `judgeRationaleChars` | 400 | `judgement.rationale` |
| `decisionBoardRationaleChars` | 600 | rationale on the board |
| escalation budgets | hackathon 10m, sprint 30m, steady 2h | §4.7 |
| human floor default | `high` | when `team_settings.human_notify_floor` is unset |

Pair key and dedupe key:

```go
pairKey := coordination.PairKey("decision_contradiction",
    coordination.PairSubject{Kind: "decision", UUID: decisionUUID, Revision: decisionRevision},
    coordination.PairSubject{Kind: "intent", UUID: intentUUID, Revision: intentRevision})

// Not revision-scoped: one disagreement stays one row, which is what lets convergence close it.
func DecisionContradictionDedupeKey(decisionUUID, intentUUID string) string // sha256("decision_contradiction|" + decision + "|" + intent), hex
```

Detector rules on `conflict.detector_rule`: `decision_contradiction.judged` for a `conflict` verdict,
`decision_contradiction.unsure` for `unsure`.

### 3.1 `record_decision`

**Annotation: idempotent**, non-destructive (a revoke can be undone by recording the key again).
Registered in a new `RegisterDecisionTools(s, h, logger)`.

Description:

> Record a decision the team has settled that the rest of the code must obey — how auth works, an error format, a library choice, who owns a module — so other agents' plans are checked against it. Call it after the decision is agreed, never as a proposal, and not for task plans (those are intents). Give a short kebab-case key (cited as #key), a statement another agent can check a plan against, and the paths it governs as scope. Recording the same key again revises it; supersedes replaces another decision; revoke withdraws one. Changing a decision somebody else recorded needs your person's agreement, passed as person_confirmed. metiche asks the agents whose live plans touch the decision to judge them against it; this call returns no conflicts.

```go
type RecordDecisionParams struct {
	SessionKey string   `json:"session_key" jsonschema:"The session_key start_session gave you."`
	TeamSlug   string   `json:"team_slug,omitempty" jsonschema:"The team this session is on, by slug: the team_slug start_session returned. Optional on one team; pass team_slug when you are on more than one team, because session keys are per team."`
	Key        string   `json:"key" jsonschema:"The decision's short name, kebab-case, the way a teammate would cite it: 'auth-jwt-cookie' (a leading # is fine). Lowercase letters, digits and dashes, max 60 characters. Recording the same key again revises that decision instead of adding a second one."`
	Title      string   `json:"title,omitempty" jsonschema:"A few words naming what was decided: 'Auth is a JWT in an httpOnly cookie'. Max 140 characters. Required unless revoke is true."`
	Statement  string   `json:"statement,omitempty" jsonschema:"The rule the code must follow, in one or two plain sentences another agent can check its plan against: 'Sessions are a signed JWT in an httpOnly, Secure cookie. Never store tokens in localStorage.' Max 400 characters. This is exactly what other agents' models read. Required unless revoke is true."`
	Rationale  string   `json:"rationale,omitempty" jsonschema:"Why, for the people on the board. Never sent to other agents inline. Max 2000 characters. No secrets."`
	Scope      []string `json:"scope,omitempty" jsonschema:"Repo-relative paths or globs the decision governs, written like claims: ['internal/auth/**', 'web/src/api/**']. An agent whose claimed files touch the scope is asked to judge its plan against the decision. Leave empty for a decision about wording rather than files; it is still matched by its words. Max 16."`
	TeamWide   bool     `json:"team_wide,omitempty" jsonschema:"true when the decision holds for every repository on the team, not only this session's project. Default false."`
	AlwaysShow bool     `json:"always_show,omitempty" jsonschema:"true only when your person says this decision is load-bearing enough to check against every plan in the project, whatever its files. At most 5 per team."`
	Supersedes string   `json:"supersedes,omitempty" jsonschema:"The key of a decision this one replaces. The old one is marked superseded and stops being checked."`
	Revoke     bool     `json:"revoke,omitempty" jsonschema:"true to withdraw the decision named by key without replacing it. Only key and rationale (why) are read."`

	PersonConfirmed bool   `json:"person_confirmed,omitempty" jsonschema:"Pass true ONLY when your person explicitly agreed to revise, supersede or revoke a decision somebody else recorded. Never on your own judgement."`
	IdempotencyKey  string `json:"idempotency_key,omitempty" jsonschema:"Pass a key of your own and retrying this exact call returns the exact same answer instead of recording again."`
}
```

#### Validation (pure, before the lock)

- **key.** Trim, strip one leading `#`, lowercase, spaces and underscores become `-`, repeated dashes collapse, leading and trailing dashes are trimmed. The result must match `^[a-z0-9-]{3,60}$`. It is stored as `#` + key.
- **revoke.** Only `key` and `rationale` are read; `title`, `statement` or `supersedes` alongside it is an error.
- **Otherwise:**
  - `title` 1-140, `statement` 20-400 runes, `rationale` ≤ 2000, no control characters in key, title or statement;
  - `scope` ≤ 16, each through `coordination.NormalizePath(p, project.IgnorePatterns, project.CaseInsensitivePaths)`, where a pattern the ignore list drops is silently dropped, as for claims;
  - `supersedes` normalized like `key`, and not equal to it.
- **Tokens.** `coordination.Tokenize(title, 32)` gets weight 2; statement tokens not already in the title get weight 1; at most 32 rows.
- **Content hash.** `coordination.DecisionContentHash(title, statement, scopeNorm, teamWide)`.

Errors, returned as tool errors with exactly these messages (`%` values filled in):

| When | Message |
|---|---|
| key empty | `key is required — a short kebab-case name like 'auth-jwt-cookie', cited as #auth-jwt-cookie` |
| key invalid | `key must be 3-60 lowercase letters, digits and dashes (got %q)` |
| title missing | `title is required — a few words naming what was decided` |
| title long | `title is %d characters and the limit is 140` |
| statement missing | `statement is required — the rule the code must follow, in one or two sentences another agent can check its plan against` |
| statement short | `statement is %d characters; write the rule itself in at least 20, not a label` |
| statement long | `statement is %d characters and the limit is 400 — it ships inline to other agents; put the reasoning in rationale` |
| rationale long | `rationale is %d characters and the limit is 2000` |
| control character | `%s contains a control character` |
| scope too long | `scope has %d paths and the limit is 16 — name the folders the decision governs, not every file` |
| scope path invalid | `%w — scope paths are repo-relative, like internal/auth/** or web/src/api/**` |
| revoke combined | `revoke withdraws a decision by key; do not send title, statement or supersedes with it` |
| supersedes itself | `a decision cannot supersede itself` |

#### Apply (under the lock)

Re-read the session status (`requireWorkableSession`). Read the decision by `(team_uuid, key)`.
Then take the first row that matches:

| Existing row | Call | Outcome |
|---|---|---|
| none | `revoke` | error `no decision %s on this team to revoke` |
| none | `supersedes` set | the target must exist and be accepted (below); **insert** the new decision, then **supersede** the target |
| none | plain | **insert**: `status=accepted`, `revision=1`, `decided_by_member_uuid` and `recorded_by_session_uuid` = caller, `decided_at=now`; insert `decision_path` and `decision_token` rows. Outcome `new` |
| accepted | `supersedes` set | error `supersedes names the decision this new key replaces; %s already exists — revise it, or pick a new key` |
| accepted | `revoke` | permission check; `status=revoked`, recorder = caller; settle its open conflicts. Outcome `revoked` |
| accepted | same content hash | if `always_show` and `rationale` are unchanged, outcome `unchanged`, nothing written. Otherwise permission check, update those two columns and the recorder with **no revision bump** (cosmetic for judging). Outcome `updated` |
| accepted | changed content | permission check; `revision+1`, recorder = caller; delete and reinsert paths and tokens. Outcome `revised` |
| revoked | `revoke` | error `%s is already revoked` |
| revoked | plain | permission check; `status=accepted`, `revision+1`, `decided_at=now`, recorder = caller; rewrite paths and tokens. Outcome `reinstated` |
| superseded | anything | error `%s was superseded by %s at %s; record a new key or revise %s` |

Supersede target checks, inside Apply:
- missing: `no decision %s on this team to supersede — check the key on the Decisions tab, or record without supersedes`;
- not accepted: `%s is already %s; only an accepted decision can be superseded`;
- permission check against the target.

Superseding sets `old.status=superseded`, `old.superseded_by_uuid=new`, `new.supersedes_uuid=old`,
and settles the old decision's open conflicts (§4.5).

**Permission check.**
- **Rule.** When `decision.decided_by_member_uuid` is not the caller's member and `person_confirmed` is false, refuse with code `not_permitted`, the same coded-error path `open_board` uses. This applies to any write to an existing decision, including a cosmetic one.
- **Message when the decider has a live or stale session on the team** (the recorder session if still live, otherwise their most recent heartbeat):
  `%s was recorded by %s. Changing it changes what %s agreed to: settle it with %s's agent (%s is live) or ask your person, then call again with person_confirmed: true.`
- **Message when they have none:**
  `%s was recorded by %s. Changing it changes what %s agreed to: ask your person, then call again with person_confirmed: true.`
- **On success** with `person_confirmed`, `decided_by_member_uuid` becomes the caller's member. The decision now belongs to whoever changed it with their person's agreement.

**always_show cap.** When the result would have more than 5 accepted always-show decisions on the
team: `the team already has 5 always-show decisions (%s); an always-show decision is checked against every plan, so turn one off before adding another`.

**Near duplicate.** For `new` only: an accepted decision on the team sharing ≥ 5 tokens (the
`decision_token` lookup, excluding the superseded target) adds a line to the note.

#### Detect (pairs the decision with live plans)

Runs for outcomes `new`, `revised`, `reinstated`, and for a superseding `new`. It collects candidate
intents on the decision's project, or on the team's projects (≤ 8) for a team-wide decision:

1. **scope.** For each normalized decision path, the claim-path candidate scan from `detector.go` (`project_uuid, status held, prefix IN ancestors OR prefix LIKE mine%`, `expires_at > now`, LIMIT 200). Keep `write` and `structural` claims only. Confirm with `coordination.PathsOverlap`. Map `claim_path.claim_uuid` → `claim.intent_uuid`.
2. **live intents.** `idx_intent_live`: `intent.status IN (declared, active)`, not expired, joined to a live or stale session, `ORDER BY declared_at DESC LIMIT 100`.
   - **words:** tokenize each summary in Go; ≥ 2 tokens shared with the decision's tokens.
   - **always_show:** when the decision is always-show, every one of these intents.
3. **drop:** intents of the recording session; intents whose `(decision, intent)` has a `pinned` judgement at any revision (`idx_judgement_subject`); pairs whose exact `pair_key` already exists in any status.
4. **rank** with `coordination.RankDecisionCandidates`: scope 3 > always_show 2 > words 1, then shared tokens descending, then intent key ascending. Cap at 12.

Why no `intent_token`: step 2 reads at most 100 live summaries per project and tokenizes them in Go,
inside the budget. Writing `intent_token` would change `declare_intent`'s Apply for no gain here.
Duplicate work will need it and will add it.

For each surviving candidate, insert a judgement:

```sql
INSERT INTO `judgement` (`id`,`team_uuid`,`pair_key`,`kind`,`subject_a_kind`,`subject_a_uuid`,`subject_a_revision`,
  `subject_b_kind`,`subject_b_uuid`,`subject_b_revision`,`status`,`judge_session_uuid`,`judging_expires_at`,
  `assignment_count`,`pinned`,`created_at`,`updated_at`)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,0,?,?)
ON DUPLICATE KEY UPDATE `id` = `id`
```

- `kind=decision_contradiction`, a = decision at its current revision, b = intent at its current revision, `status=pending`.
- **Assigned** (`judge_session_uuid` = the intent's session, `judging_expires_at = now + window`, `assignment_count = 1`) when that session has had fewer than `max_reviews_per_minute` assignments in the last 60s: `SELECT COUNT(*) FROM judgement WHERE judge_session_uuid = ? AND status = pending AND updated_at >= now - 60s`, on `idx_judgement_assignment`.
- **Otherwise unassigned** (`judge_session_uuid`, `judging_expires_at` NULL, `assignment_count = 0`). The sweeper assigns it later (§4.4).

Open conflicts on a revised decision stay open. Their plan's new pair is among the candidates, and
the verdict settles or keeps them (§4.5).

#### Envelope

```json
{"ok":true,"key":"#auth-jwt-cookie","sequence":412,"revision":90,
 "pending":{"instructions":0,"conflicts":0,"reviews":0},
 "note":"#auth-jwt-cookie recorded (revision 1, 2 scope path(s)); 3 live plan(s) will be checked against it"}
```

`conflicts[]` is never set. Notes, exactly:

| Outcome | Note |
|---|---|
| new, with pairs | `%s recorded (revision 1, %d scope path(s)); %d live plan(s) will be checked against it` |
| new, no pairs | `%s recorded (revision 1, %d scope path(s)); no live plan touches it yet` |
| revised | `%s revised to revision %d; %d live plan(s) will be checked against the new wording` |
| updated | `%s updated (revision %d unchanged: rationale or always_show only)` |
| unchanged | `%s: already recorded with this wording and scope (revision %d); nothing changed` |
| reinstated | `%s reinstated as revision %d; %d live plan(s) will be checked against it` |
| revoked | `%s revoked; %d conflict(s) on it settled` |
| suffix: superseded | `; superseded %s, %d conflict(s) on it settled` |
| suffix: near duplicate | `; looks like %s (%d shared words): if it is the same decision, revise that key or pass supersedes` |

#### Idempotency, events

- **Idempotency key.** `decision_recorded:<session uuid>:<caller key>` when the caller passes one, else `decision_recorded:<nonce>`. An `unchanged` call still writes its one event and replays byte for byte, like a contract publish.
- **Events.** One per call, `Structural: true`, because the Decisions tab and the conflict list change shape and the board refetches on a structural frame. `SubjectKind: decision`, `SubjectKey` = key.

| Outcome | Kind | Summary | Payload |
|---|---|---|---|
| new | `decision_recorded` | `%s recorded %s` (agent label, key) | `message` = statement, `detail` = `r1`, `paths` = scope |
| new + supersedes | `decision_recorded` | `%s recorded %s, superseding %s` | `detail` = `r1 supersedes #old` |
| revised | `decision_recorded` | `%s revised %s (r%d)` | `detail` = `r%d revised` |
| updated / unchanged | `decision_recorded` | `%s updated %s` / `%s recorded %s again (unchanged)` | `detail` = `r%d` |
| reinstated | `decision_recorded` | `%s reinstated %s (r%d)` | `previous_status` revoked, `new_status` accepted |
| revoked | `decision_superseded` | `%s revoked %s` | `previous_status` accepted, `new_status` revoked, `message` = rationale (sanitized, clipped 400) |

Plus one derived `conflict_resolved` (structural) per settled conflict.

### 3.2 `get_review_context`, and the inline review block

**Annotation: readOnly.** No `commit`, no event, no write of any kind.

Description:

> Read the pairs metiche has asked you to judge: a recorded team decision next to your own plan, with why they were paired. Call it when pending.reviews is above zero, or when a response's review block did not carry everything you need, then answer each pair with report_judgement. Read-only: it changes nothing and can be called again. metiche never judges anything itself — your model does.

```go
type GetReviewContextParams struct {
	SessionKey string `json:"session_key" jsonschema:"The session_key start_session gave you. You only ever see pairs assigned to your own session."`
	TeamSlug   string `json:"team_slug,omitempty" jsonschema:"The team this session is on, by slug: the team_slug start_session returned. Optional on one team; pass team_slug when you are on more than one team, because session keys are per team."`
	PairKey    string `json:"pair_key,omitempty" jsonschema:"One pair, by the pair_key a review block or an earlier call gave you. Omit it to get the pairs waiting for you, oldest first."`
	Limit      int    `json:"limit,omitempty" jsonschema:"How many pairs, 1-3. Defaults to 3. Anything left over is reported as more_waiting."`
}

type ReviewContextResult struct {
	Envelope
	Reviews     []ReviewItem `json:"reviews"`
	MoreWaiting int          `json:"more_waiting,omitempty"`
}

type ReviewItem struct {
	PairKey    string         `json:"pair_key"`
	Kind       string         `json:"kind"`     // "decision_contradiction"; duplicate_work later
	Question   string         `json:"question"` // "Would %s, as planned, break decision %s?"
	Why        []string       `json:"why"`
	Decision   *ReviewDecision `json:"decision,omitempty"`
	Plan       ReviewPlan     `json:"plan"`
	AnswerWith string         `json:"answer_with"` // "report_judgement(pair_key, verdict, confidence, severity, rationale)"
	ExpiresAt  *string        `json:"expires_at,omitempty"`
}
type ReviewDecision struct {
	Key       string   `json:"key"`
	Title     string   `json:"title"`
	Statement string   `json:"statement"`
	Scope     []string `json:"scope"`
	DecidedBy string   `json:"decided_by,omitempty"`
	Revision  int64    `json:"revision"`
}
type ReviewPlan struct {
	Key      string   `json:"key"`
	Summary  string   `json:"summary"`
	Paths    []string `json:"paths"` // at most 8 of the intent's held paths
	Revision int64    `json:"revision"`
}
```

**Reading.** One read-only transaction, `sql.TxOptions{ReadOnly: true}`.
- **Without `pair_key`:** `judgement` rows with `judge_session_uuid = caller`, `status = pending` and `judging_expires_at > now` (the same set `pending.reviews` counts), oldest `updated_at` first, on `idx_judgement_assignment`.
- **With `pair_key`:** that row by `uq_judgement_pair`, if pending and assigned to the caller, **even when its window has lapsed**. The caller is still its only judge.
- **Then:** the decision and the intent by primary key, the intent's held paths (LIMIT 8), and `pendingCounts`.

Rules for the returned fields:
- `rationale` is never included.
- `why` strings, exactly:
  - `your claim %s is in its scope`
  - `the team checks every plan against it`
  - `your summary shares words with it: %s` (at most 4 tokens, comma-joined)
- **Budget.** 2200 characters of rendered items, the first always delivered, the rest counted in `more_waiting`.

Notes: `%d pair(s) to judge: read each, then report_judgement`, with `; %d more waiting — call get_review_context again` when some are left. With nothing waiting: `nothing waiting for you to judge`.

Errors:
- unknown pair: `pair_key %s is not a pair on this team — call get_review_context to see what is waiting for you`
- another session's pair: `this pair is %s's to judge; nothing for you here` (session key)
- already judged: `pair %s was already judged %s at %s`
- expired: `this pair expired unanswered; call get_review_context for what is waiting now`
- limit out of range: clamp silently to 1..3, like `get_instructions`.

Example:

```json
{"ok":true,"key":"S-22","sequence":415,"revision":90,"pending":{"instructions":0,"conflicts":0,"reviews":1},
 "note":"1 pair(s) to judge: read each, then report_judgement",
 "reviews":[{"pair_key":"9c1e5f0a…","kind":"decision_contradiction",
   "question":"Would INT-91, as planned, break decision #auth-jwt-cookie?",
   "why":["your claim web/src/auth/session.ts is in its scope"],
   "decision":{"key":"#auth-jwt-cookie","title":"Auth is a JWT in an httpOnly cookie",
     "statement":"Sessions are a signed JWT in an httpOnly, Secure cookie. Never store tokens in localStorage or sessionStorage.",
     "scope":["internal/auth/**","web/src/auth/**"],"decided_by":"Ana","revision":1},
   "plan":{"key":"INT-91","summary":"store the session token in localStorage after login","paths":["web/src/auth/session.ts"],"revision":1},
   "answer_with":"report_judgement(pair_key, verdict, confidence, severity, rationale)",
   "expires_at":"2026-09-15T14:20:00Z"}]}
```

#### The inline review block (on `declare_intent` and `update_intent`)

Built by `NewDecisionReviewer` (§9), chained after the path detector in `app/rest.go`. It runs inside
the same Detect step, so the block and the judgement rows it assigns are in the stored snapshot and
a replay returns the same pair keys.

- **When.** The mutation kind is `intent_declared`, or `intent_updated` that is not terminal, and the context carries a `pathDetectionRequest` with an intent.
- **Fast path.** `SELECT 1 FROM decision WHERE team_uuid = ? AND status = accepted LIMIT 1` on `idx_decision_always_show`. With no rows, return immediately. The response bytes are exactly today's.
- **Intent state.** Read the intent's current `revision` and `summary` by primary key. For an update, read its held paths (claim → claim_path, LIMIT 64). For a declaration, `req.Declared`. Keep write and structural paths for the scope signal.
- **Candidates:**
  1. **scope.** For up to 16 paths:
     ```sql
     SELECT dp.`decision_uuid`, dp.`pattern_norm`
     FROM `decision_path` dp
     WHERE dp.`team_uuid` = ? AND (dp.`prefix` IN (<ancestors>) OR dp.`prefix` LIKE ?)
       AND (dp.`project_uuid` = ? OR dp.`project_uuid` IS NULL)
     LIMIT 200
     ```
     Rebuild each pattern with `coordination.NormalizePath(pattern_norm, nil, project.CaseInsensitivePaths)` (the table has no suffix or ext columns) and confirm with `PathsOverlap`.
  2. **words.**
     ```sql
     SELECT dt.`decision_uuid`, COUNT(DISTINCT dt.`token`) AS shared, SUM(dt.`weight`) AS score
     FROM `decision_token` dt
     WHERE dt.`team_uuid` = ? AND dt.`token` IN (<intent tokens, ≤32>)
       AND (dt.`project_uuid` = ? OR dt.`project_uuid` IS NULL)
     GROUP BY dt.`decision_uuid` HAVING shared >= 2
     ORDER BY score DESC, dt.`decision_uuid` LIMIT 10
     ```
  3. **always_show.** `idx_decision_always_show` `(team, accepted, 1)`, LIMIT 5, project or team-wide.
  4. Keep only accepted decisions, by an `IN` point read. Drop decisions whose `recorded_by_session_uuid` is the caller, pinned subjects, and existing pair keys.
  5. `RankDecisionCandidates`, top 3.
- **Assignment.** Insert each as in §3.1, assigned to the caller under the per-minute cap. Pairs past the cap: scope and words candidates are inserted unassigned (backlog). **always_show candidates past the cap are dropped**; they come back on the plan's next revision.
- **The block.**
  ```json
  {"pairs":[{"pair_key":"9c1e5f0a…","decision":"#auth-jwt-cookie","statement":"Sessions are a signed JWT in an httpOnly, Secure cookie. Never store tokens in localStorage or sessionStorage.","why":"scope"}],
   "more":0,"answer_with":"report_judgement"}
  ```
  - `why` is `scope`, `always_show` or `words`.
  - `more` counts pairs assigned this call but not listed.
  - Pairs assigned to the caller earlier are not listed; `pending.reviews` counts them.
- **Cut order** when the rendered block exceeds 1200 characters:
  1. drop `statement` from every pair;
  2. drop pairs from the end, counting them in `more`;
  3. drop the block.
- **Opt-out.** With `team_settings.review_block_enabled = false`, assignment still happens and only the block is omitted.
- **Note.** Whenever pairs were assigned, append to the envelope note: `; judge %d pair(s) against your plan before you edit: see review, then report_judgement`, or `…: call get_review_context, then report_judgement` when there is no block.

### 3.3 `report_judgement`

**Annotation: idempotent.** No `idempotency_key` parameter: the key is built from the judgement and
the verdict, like `report_back`.

Description:

> Give your verdict on a pair from get_review_context or a review block: would your plan, as written, break the recorded decision? 'no_conflict' is the usual answer; 'conflict' only when doing the plan would break the statement; 'unsure' when the wording does not settle it. Include an honest confidence and a one-line rationale; both are shown on the board. A conflict comes back in conflicts[] with what to do, and the decision's author's agent is told. Change your plan and update_intent, and you will be asked once more; a no_conflict then settles it. Safe to retry: the same verdict on the same pair returns the first answer.

```go
type ReportJudgementParams struct {
	SessionKey string   `json:"session_key" jsonschema:"The session_key start_session gave you."`
	TeamSlug   string   `json:"team_slug,omitempty" jsonschema:"The team this session is on, by slug: the team_slug start_session returned. Optional on one team; pass team_slug when you are on more than one team, because session keys are per team."`
	PairKey    string   `json:"pair_key" jsonschema:"The pair_key from a review block or get_review_context, exactly as given. metiche computed it; never make one up."`
	Verdict    string   `json:"verdict" jsonschema:"'conflict' if carrying out your plan as written would break the decision; 'no_conflict' if it would not (the usual answer: unrelated or compatible); 'unsure' if the decision's wording does not settle it."`
	Confidence *float64 `json:"confidence,omitempty" jsonschema:"How sure you are, 0 to 1. Be honest: a conflict below 0.7 is recorded on the board but interrupts nobody. Required for conflict."`
	Severity   string   `json:"severity,omitempty" jsonschema:"For a conflict: 'low', 'medium' (default) or 'high', meaning how much breaks if the plan goes ahead. Ignored otherwise."`
	Rationale  string   `json:"rationale,omitempty" jsonschema:"One line naming what in your plan meets what in the decision: 'plan stores the token in localStorage; #auth-jwt-cookie forbids it'. Max 400 characters. Required for conflict and unsure. Shown on the board; no secrets."`
}
```

`Confidence` is a pointer so a missing value is distinguishable from 0.

#### Validation

Outside the lock:
- `verdict` in `conflict | no_conflict | unsure`;
- `confidence` present for conflict and in [0,1] when present;
- `severity` in `low | medium | high`, default `medium`;
- `rationale` present for conflict and unsure, then `sanitizeNoteText` and clipped to 400;
- read the judgement by `(team, pair_key)` and check `judge_session_uuid` = caller.

Under the lock, in Apply, re-read:
- the judgement: `pending`, or `judged` with the **same** verdict, which falls through to commit's replay;
- the decision: accepted, with revision equal to `subject_a_revision`;
- the intent: declared or active, with revision equal to `subject_b_revision`.

| When | Message |
|---|---|
| pair_key empty | `pair_key is required — send back the pair_key a review block or get_review_context gave you` |
| verdict invalid | `verdict must be conflict, no_conflict or unsure (got %q)` |
| confidence missing on conflict | `confidence is required with a conflict verdict — how sure you are, 0 to 1` |
| confidence out of range | `confidence must be between 0 and 1 (got %v)` |
| severity invalid | `severity must be low, medium or high (got %q)` |
| rationale missing | `rationale is required with a %s verdict — one line naming what in your plan meets what in the decision` |
| not found | `pair_key %s is not a pair on this team — call get_review_context` |
| not yours | `this pair is %s's to judge, not yours` |
| stale plan | `%s is now revision %d and you judged revision %d: call get_review_context for the current pair` (intent key) |
| stale decision | `%s is now revision %d and you judged revision %d: call get_review_context for the current pair` (decision key) |
| judged differently | `already judged %s at %s; a changed plan earns a new pair when you update_intent` |
| plan ended | `%s is %s; nothing to judge` |
| decision ended | `%s was %s; nothing to judge` |
| expired | `this pair expired unanswered; call get_review_context for what is waiting now` |

Idempotency key: `judgement_reported:<judgement uuid>:<verdict>`. The same verdict with a different
confidence or rationale replays the first answer.

#### Apply

```sql
UPDATE `judgement` SET `status` = judged, `verdict` = ?, `severity` = ?, `confidence` = ?, `rationale` = ?,
  `judged_at` = ?, `updated_at` = ? WHERE `id` = ? AND `status` = pending
```

`severity` is stored only for `conflict`.

#### Detect, dispatched on `judgement.kind`

This spec implements `decision_contradiction`. The dispatch table is where `duplicate_work` goes later.

- **`no_conflict`.** If a conflict with `DecisionContradictionDedupeKey(decision, intent)` is open or acknowledged, settle it (§4.5, release `DecisionReleaseJudged`). Otherwise nothing more.
- **`unsure`.** Raise or reopen the conflict at `low`, rule `decision_contradiction.unsure`, detected_by `agent`, with `confidence` if given. No notice to anyone; the caller gets no `conflicts[]` entry because it is below the notify floor.
- **`conflict`.** Raise or reopen the conflict, rule `decision_contradiction.judged`, detected_by `agent`, `confidence`, severity from §4.2.

Raising or reopening, following `recordContractConflict`:

- **conflict row.** On insert: `key = conflictKey(tc, n)`, `kind = decision_contradiction`, `dedupe_key`, `severity`, `status = open`, `detected_by = agent`, `detector_rule`, `confidence`, `evidence`, `suggested_action` (the plan owner's action, §4.8), `suggested_yield_session_uuid` = the intent's session, `suggested_yield_reason = "plan contradicts #key"`, `occurrence_count = 1`, first and last detected now.
- **evidence.**
  - `overlap_path` = decision key;
  - `a_label` = `decision: #key (Ana)`, `a_summary` = statement, `a_pattern` = the decision path that overlapped, or the first scope path, or null;
  - `b_label` = `describeHolder` of the plan owner, `b_summary` = intent summary, `b_pattern` = the plan's path that overlapped, or its first held path, or null;
  - `field_issues` = [`%s's model (%.2f): %s`] (session key, confidence, rationale), where an unsure verdict without a confidence renders `(unsure)`;
  - `detail` = rule.
- **existing open or acknowledged row.** Bump `occurrence_count`, set `last_detected_at`, `severity`, `evidence`, `suggested_action`, `detector_rule`, `confidence`.
- **existing row resolved with `resolved_by_member_uuid` NULL** (metiche closed it). Reopen, exactly like contracts, and also set `escalated_at = NULL`. It is news again.
- **existing row resolved or dismissed by a person.** Silenced: bump the count only, no participants touched, no notice, no `conflicts[]` entry.
- **`judgement.conflict_uuid`** = the conflict id.
- **participants.**
  - The plan owner's session: `subject_kind = intent`, `subject_uuid` = intent, role `initiator`.
  - The **decider**, when the decider member differs from the plan owner's member: the recorder session if live or stale, otherwise that member's most recent live or stale session on the team by `last_heartbeat_at`. `subject_kind = decision`, `subject_uuid` = decision, role `incumbent`. With no such session, there is no second participant.
  - Upsert by `(conflict, session)`; an existing participant's `subject_uuid` is refreshed.
- **notice to the decider participant.** When severity ≥ `medium`, and the conflict is fresh or its severity rose above `max_severity_notified`: an instruction `source = server`, `kind = conflict_notice`, `ref_kind = conflict`, `ref_uuid` = conflict, `requires_report = 0`, `expires_at = now + 4h`. The body is `%s (%s) on %s: %s` (conflict key, severity, decision key, the decider action from §4.8), so `splitNoticeBody` separates the action. Then set `notified_at` and `max_severity_notified`.
- **caller.** When severity ≥ `medium` and the conflict is not silenced, append a `ConflictNotice` and stamp the caller participant's `notified_at`.

#### Envelope

```json
{"ok":true,"key":"CF-31","sequence":418,"revision":91,"pending":{"instructions":0,"conflicts":1,"reviews":0},
 "note":"judged INT-91 against #auth-jwt-cookie: conflict (0.90) — read conflicts[] before you edit",
 "conflicts":[{"key":"CF-31","kind":"decision_contradiction","severity":"medium","with":"Ana (backend)",
   "decision":"#auth-jwt-cookie","at_fault":"plan",
   "suggested_action":"Your plan INT-91 breaks #auth-jwt-cookie (Sessions are a signed JWT in an httpOnly, Secure cookie. Never store tokens in localStorage…). Change the plan to follow it and update_intent with the new summary, which asks you to judge again. If the decision itself is wrong, settle that with Ana's agent (S-17); only Ana or your person can change it."}]}
```

- `Envelope.Key` is the conflict key when a conflict was raised, reopened or settled, else the intent key.
- `ConflictNotice` gains `Decision string json:"decision,omitempty"`. `AtFault` is `"plan"`.
- `With` is `describeHolder` of the decider participant, or the decider member's display name when there is no participant.

| Verdict | Note |
|---|---|
| no_conflict | `judged %s against %s: no conflict (%.2f)` (the confidence part is omitted when not sent), plus `; %s settled` when a conflict closed |
| conflict ≥ 0.7 | `judged %s against %s: conflict (%.2f) — read conflicts[] before you edit` |
| conflict < 0.7 | `judged %s against %s: conflict (%.2f), recorded low on the board; below 0.7 it interrupts nobody` |
| unsure | `judged %s against %s: unsure, recorded low on the board` |
| silenced | `judged %s against %s: conflict (%.2f); a person already settled this one, so it stays closed` |

#### Events

- One `judgement_reported`: `SubjectKind: decision`, `SubjectUUID` = decision, `SubjectKey` = decision key.
- Summary: `%s judged %s against %s: %s` (agent label, intent key, decision key, verdict with `_` shown as a space).
- Payload: `intent_uuid`, `conflict_uuid` when there is one, `severity`, `detail` = verdict, `message` = sanitized rationale, `paths` = [decision key].
- `Structural: true` exactly when a conflict was raised or reopened. Otherwise false: PLAN's "one tiny event".
- A settlement adds its own structural `conflict_resolved`.

### 3.4 Changes to existing tools and files

| File | Change |
|---|---|
| `app/mcp/server.go` | `RegisterDecisionTools(server, h, logger)` after contracts; the `serverInstructions` paragraph in §6.2 |
| `app/mcp/envelope.go` | `ConflictNotice.Decision`; `NoteForPending` reviews case: `%d pair(s) to judge against your plan — call get_review_context, then report_judgement` |
| `app/mcp/instructions.go` | `fallbackAction` for `JUDGE_REQUEST`: `judge it with get_review_context, then report_judgement; then report_back('%s', 'done')` |
| `app/mcp/intents.go` | on terminal `update_intent`, inside Apply after `settleAfterRelease`: expire the intent's pending judgements (`UPDATE judgement SET status = expired WHERE judge_session_uuid = ? AND status = pending AND subject_b_uuid = ?`), then settle its open decision conflicts with `DecisionReleaseIntentEnded` |
| `app/mcp/sessions.go` | in `EndSession` Apply, after the contract settle: `ExpireJudgementsOfSession`, then settle `OpenDecisionConflictsOfSession` with `DecisionReleaseSessionEnded`. **Decisions are not withdrawn** (F12) |
| `app/mcp/conflictresolve.go` | `ConflictResolvedSummary` gains the decision case (§4.8) |
| `app/mcp/publishcontract.go` | `insertContractFields`: `seen` keyed on `direction + "|" + path`; delete the limitation comment |
| `app/rest.go` | `handler.SetDetector(metichemcp.ChainDetectors(metichemcp.NewPathDetector(coreImpl, logger), metichemcp.NewDecisionReviewer(coreImpl, logger)))`; add the history route to the allowed-routes map |

---

## 4. Detection and settlement

### 4.1 What the server decides, and what it never decides

**Deterministic, on the server:**
- candidacy: scope overlap by the claims' prefix algorithm, `always_show`, wording overlap of ≥ 2 shared tokens;
- the rate cap;
- staleness, by revisions;
- severity adjusters, fault, the notify and human floors;
- settlement and escalation.

**Never on the server:** whether a plan contradicts a decision. No deterministic contradiction rule is
claimed; any rule shape (forbidden paths, keyword bans) cries wolf, and PLAN:247 warns models already
lean towards "yes". Every candidate pair goes to **the plan's own agent**. It knows its plan, and it
is the side that must act.

### 4.2 Severity, fault and notify (pure: `coordination.JudgedSeverity`)

| Step | Rule |
|---|---|
| 1 | `unsure` → `low`. Stop. |
| 2 | `conflict` with confidence < 0.70 → `low`. Stop. |
| 3 | Start from the requested severity, default `medium`, clamped to [`low`, `high`]. An agent verdict never reaches `critical`. |
| 4 | Plan owner's member = decider member → −1, floor `low`. |
| 5 | Decision is always-show → floor `medium`. |

- **Notify floor:** `medium` (`notifySeverityFloor`). Below it, the conflict is recorded and on the board, and nobody is interrupted.
- **Human floor:** `team_settings.human_notify_floor`, default `high` (§4.7).
- **Fault:** always the plan: `at_fault: "plan"`, yield session = plan owner. A decision is a standing agreement; changing it is a separate act with its own permission (§3.1).

### 4.3 Notices

| Who | How |
|---|---|
| The judge (plan owner) | `pending.reviews` on every response, a bare heartbeat included; the inline `review` block on declare and update |
| The plan owner, on a conflict | `conflicts[]` in its own `report_judgement` response |
| The decider's agent | `conflict_notice` → `pending.instructions` → `get_instructions`; `pending.conflicts` as a participant |
| Escalation | `question` instruction, `requires_report = 1`, to the plan owner and the decider participant (§4.7) |

### 4.4 Judge window, backlog and re-arming (sweeper, no events)

- **Window.** `team_settings.judge_window_seconds` when set, otherwise 15 minutes.
- **The pass.** Each sweeper pass, per team, reads `idx_judgement_team_open`: `status = pending AND (judging_expires_at IS NULL OR judging_expires_at < now)`, LIMIT 64.
- **Subjects still valid** means: decision accepted at `subject_a_revision`, intent declared or active at `subject_b_revision`, intent's session live or stale.
- **An assigned pair whose window lapsed:**
  - subjects valid and `assignment_count < 3` → re-arm: `judging_expires_at = now + window`, `assignment_count + 1`, `updated_at = now`;
  - otherwise `status = expired`.
- **An unassigned pair:**
  - subjects valid and the intent's session under the per-minute cap → assign it to that session: expiry now + window, `assignment_count = 1`;
  - subjects not valid → `status = expired`;
  - otherwise leave it for the next pass.
- **No lock needed.** Every write is a single conditional UPDATE (`WHERE id = ? AND status = pending AND <the state it read>`), so no team lock is taken. A race can overshoot the rate cap by one, which is acceptable.
- **Quiet.** An expired, unanswered pair asks no person and writes no event. Unjudged is not a contradiction.

### 4.5 Settlement

All of it runs on the transaction that holds the team lock. Each settlement is a conditional
`UPDATE conflict SET status = resolved, resolution, resolution_note, resolved_at WHERE id = ? AND status IN (open, acknowledged)`
plus one derived structural `conflict_resolved` event (`appendConflictResolvedEvent`). A conflict
metiche closes keeps `resolved_by_member_uuid` NULL.

`decideDecisionSettlement` is pure. The first rule that fits wins:

| # | State | Resolution |
|---|---|---|
| 1 | decision is superseded or revoked | SUPERSEDED |
| 2 | intent is done, abandoned or superseded, or its row is gone | SUPERSEDED |
| 3 | intent's session is ended or abandoned, or gone | SUPERSEDED |
| 4 | the pair at the current revisions of both subjects is judged `no_conflict` | CONVERGED |
| – | anything else | stays open |

A chosen resolution becomes **COORDINATED** when a participant answered this conflict's notice with
`report_back` done or acknowledged and a note (`loadSettleNotes`), unless the release kind is
`DecisionReleaseSessionAbandoned`.

| Release | Called from |
|---|---|
| `DecisionReleaseJudged` | `report_judgement` no_conflict |
| `DecisionReleaseDecisionChanged` | `record_decision` supersede or revoke |
| `DecisionReleaseIntentEnded` | `update_intent` terminal |
| `DecisionReleaseSessionEnded` | `end_session` |
| `DecisionReleaseSessionAbandoned` | sweeper, next to `settleContractConflicts` in `settleAfterAbandon` |

- **Revisions do not close anything.** When the plan or the decision is revised, the conflict stays open. The new pair goes to the plan owner, and its next verdict converges or keeps it open.
- **Superseding.** The new decision has a new uuid: a new pair and, if needed, a new conflict.
- **Decisions are never withdrawn when a session ends or is abandoned.** Only that session's pending judgements expire, and its plans' conflicts settle SUPERSEDED.

### 4.6 Reopen and silence (and room for `resolve_conflict`)

- **Reopen.** A conflict metiche closed reopens on a new `conflict` or `unsure` verdict for the same dedupe key: status open, resolution fields cleared, `notified_at`, `max_severity_notified` and `escalated_at` cleared, count bumped.
- **Silence.** A conflict a person resolved or dismissed (`resolved_by_member_uuid` set, or status dismissed) is only counted.
- **Pinned pairs.** The reviewer never assigns a pair whose subjects have a `pinned` judgement at any revision.
- **Room for `resolve_conflict`.** It will set `resolved_by_member_uuid`, `dismiss_reason` and `judgement.pinned`, and needs nothing else from this design. Duplicate work reuses the ledger, `get_review_context` items (`kind`) and `report_judgement`'s dispatch.

### 4.7 Asking a person — only when the agents did not settle it

This is a sweeper rule, per team per pass, run after the backlog step:

```sql
SELECT c.`id` FROM `conflict` c
WHERE c.`team_uuid` = ? AND c.`status` IN (open, acknowledged) AND c.`severity` >= ?   -- human floor
  AND c.`kind` = decision_contradiction AND c.`escalated_at` IS NULL
ORDER BY c.`first_detected_at` LIMIT 16
```

It uses `idx_conflict_open (team_uuid, status, severity)`. For each row, through `appendEvent` with the
locked `extra` hook, re-read and decide with `coordination.ShouldEscalate`:

- the conflict is still open or acknowledged, `escalated_at` is NULL, severity ≥ floor;
- the plan's intent is declared or active and its session live or stale;
- `now − openSince ≥ EscalationBudget(cadence of the conflict's project)`, where `openSince = GREATEST(COALESCE(last_detected_at, first_detected_at), MAX(participant.notified_at))`. A fresh verdict or a fresh notice restarts the clock, so both agents always get a full budget after their last turn;
- budgets: hackathon 10m, sprint 30m, steady 2h.

When it fires:
1. `UPDATE conflict SET escalated_at = now, updated_at = now WHERE id = ? AND escalated_at IS NULL`.
2. A `question` instruction to the plan owner's session, and one to the decider participant's session if it is still live or stale: `source = server`, `ref_kind = conflict`, `requires_report = 1`, `expires_at = now + 4h`. Bodies in §4.8. They must fit 200 characters (F7).
3. Event `conflict_escalated`: `Structural: true`, subject conflict, summary `%s: a person was asked about %s` (conflict key, decision key), payload `conflict_uuid`, `severity`, `message` = the plan owner's question body. Idempotency key `sweep:conflict_escalated:<conflict id>:<occurrence_count>`. If the check fails inside the lock, return `errNothingToSay`.

Nothing else in this feature interrupts a person. The only other path to a person is agent-initiated:
`record_decision`'s `not_permitted` message tells the agent to settle it with the decider's agent or
ask its person.

### 4.8 Exact wording

`{decision}` is a key like `#auth-jwt-cookie`. `{intent}` is `INT-91`. A session name is
`settleSide.name()`, like `S-22 (ui)`. `{time}` is `clock()`, like `14:05 UTC`. `{statement…}` is
`clip(statement, 120)`. Everything goes through `clip` to its column or field limit.

**Suggested action for the plan owner** (`conflict.suggested_action` and `ConflictNotice.SuggestedAction`, ≤ 400):

| Case | Text |
|---|---|
| decider is another member with a live agent | `Your plan {intent} breaks {decision} ({statement…}). Change the plan to follow it and update_intent with the new summary, which asks you to judge again. If the decision itself is wrong, settle that with {decider}'s agent ({decider session key}); only {decider} or your person can change it.` |
| decider is another member without a live agent | `Your plan {intent} breaks {decision} ({statement…}). Change the plan to follow it and update_intent with the new summary, which asks you to judge again. If the decision itself is wrong, ask your person: only {decider} or your person agreeing can change it.` |
| decider is the plan owner's own member | `Your plan {intent} breaks {decision} ({statement…}), which your own person decided. Change the plan to follow it and update_intent with the new summary, or revise {decision} with record_decision if the decision is what changed.` |
| unsure | `The wording of {decision} does not settle whether {intent} breaks it. Nothing to do now; if it matters, ask {decider}'s agent what it means, then revise the plan or the decision.` |

**Decider's notice action** (after `%s (%s) on %s: `; `get_instructions` shows ≤ 200):
`{owner name} judged its plan {intent} breaks your decision: "{rationale clipped 60}". They were told to follow it. If the decision should change, revise it with record_decision.`

**Escalation question bodies** (≤ 200, `{summary…}` = `clip(intent summary, 40)`):
- plan owner: `{conflict} on {decision}, unsettled {budget}. Ask your person: follow the decision, or agree with {decider} to change it? Plan: "{summary…}". Then report_back their answer.`
- decider: `{conflict} on {decision}, unsettled {budget}. Ask your person: keep the decision, or change it for {owner member}'s plan "{summary…}"? Then report_back their answer.`

`{budget}` renders as `10m`, `30m` or `2h`.

**Resolution notes** (`conflict.resolution_note`, ≤ 400, free text sanitized):

| Case | Note |
|---|---|
| CONVERGED, plan revised | `Settled by the agents: {judge} revised {intent} at {time} and judged it no longer contradicts {decision} (r{decision revision}): "{rationale}".` |
| CONVERGED, decision revised | `Settled by the agents: {recorder} revised {decision} to r{revision} at {recorded time}, and {judge} judged {intent} no longer contradicts it: "{rationale}".` |
| SUPERSEDED, superseded | `Settled by the agents: {actor} superseded {decision} with {new decision} at {time}, so {intent} no longer breaks a standing decision.` |
| SUPERSEDED, revoked | `Settled by the agents: {actor} revoked {decision} at {time}, so {intent} no longer breaks a standing decision.` |
| SUPERSEDED, intent ended | `Settled by the agents: {owner} marked {intent} {status} at {time}.` |
| SUPERSEDED, session ended | `Settled by the agents: {owner} ended its session at {time}{outcomeSuffix}, ending {intent}.` |
| SUPERSEDED, abandoned | `Cleared by metiche: {owner} was abandoned at {time} after no heartbeat, ending {intent}.` |
| COORDINATED | the sentence of what closed it, then ` {S-x} said: "{note}"` exactly as `buildResolutionNote` quotes |
| any, after escalation | append ` A person was asked at {escalated time}.` |

When the rationale is empty, the `: "{rationale}"` part is dropped. "Plan revised" and "decision
revised" are told apart by the latest `conflict` verdict on `(decision, intent)` (`idx_judgement_subject`,
`ORDER BY judged_at DESC LIMIT 1`): if its `subject_a_revision` is below the decision's current
revision, the decision was revised.

**Timeline summary** (`ConflictResolvedSummary`, decision case):
`%s settled (%s): the plan no longer contradicts %s` (conflict key, resolution, decision key).

---

## 5. Board

### 5.1 Backend reads (`app/webapi`, owner B2)

- **`GET /v1/teams/{slug}/decisions?limit=N`.** Default and max 300, as today. **Now returns accepted decisions only**, always-show first, then `updated_at` desc, then key. Wire in §9.2.
  - `judged` counts **one entry per plan**: the state of that plan's **latest** judgement on the decision's **current revision**. `no_conflict`, `conflict` and `unsure` count plans whose latest row is judged with that verdict; `pending` counts plans whose latest row is pending, assigned or not. The four numbers never add up to more than the number of plans paired with this revision.
    - **Latest** is, among the plan's rows at `subject_a_revision` = the decision's revision whose status is not `expired`, the one with the highest `subject_b_revision` (the id breaks a tie, which the pair key rules out). Revisions only grow, so that is the row on the plan's current revision when one exists, otherwise the newest row the plan has.
    - A plan judged `conflict`, then revised (a new pair key) and judged `no_conflict`, counts once, as `no_conflict`. A plan judged `conflict` whose revision is still waiting to be judged counts as `pending`, not `conflict`.
    - Expired rows are never counted and never the latest: a plan whose re-check expired unanswered still counts by its last verdict; a plan whose only pair expired counts nothing.
    - A plan that has ended (done, abandoned, superseded) still counts by its last verdict. The card says "checked against N plans", a statement of what was checked, and `open_conflicts` — not `judged.conflict` — is what says a contradiction is live. Ending a plan expires its pending rows, so an ended plan is never counted as waiting.
    - Served by `idx_judgement_subject` (`team_uuid`, `subject_a_uuid`, `subject_b_uuid`), filtered by `subject_a_revision` and status, with `ROW_NUMBER()` over `(subject_a_uuid, subject_b_uuid)` picking the latest row; no join to `intent`.
  - `open_conflicts` lists open or acknowledged decision conflicts on this decision by key. The query goes `conflict_participant` `subject_kind = decision` → conflict, plus conflicts whose evidence `overlap_path` = key, de-duplicated. Since a decider participant may not exist, **the evidence path is authoritative**: `idx_conflict_open` over the team's open conflicts, filtered by kind and `JSON_VALUE(evidence, '$.overlap_path')`.
- **`GET /v1/teams/{slug}/decisions/history?status=&cursor=&limit=&key=`.** New, wire in §9.3.
  - `status` is empty (both), `superseded` or `revoked`.
  - The cursor is opaque, over `(updated_at DESC, key ASC)`.
  - `limit` defaults to 50, max 100.
  - `key`, when set, returns that one decision in any status with its `revisions` from the event log: `kind IN (decision_recorded, decision_superseded) AND subject_key = ?`, newest first, LIMIT 20. This walks the team's sequence index backwards, the same cost profile `events.go` accepts for a kind filter.
  - Revisions older than event retention are gone; the current wording never is (§10).
- **`GET /v1/teams/{slug}/conflicts`** and the history. `conflictColumns` gains `c.escalated_at` and `JSON_VALUE(c.evidence, '$.field_issues[0]')`. `conflictWire` gains `decision_key`, `judge_note` and `escalated_at` (§9.4). For `decision_contradiction`, `paths` is `[b_pattern, a_pattern]` without nulls, and `overlap_path` goes to `decision_key` only.
- **`liveConflictKinds`** gains `decision_contradiction`.
- **`app/rest.go`**: the new route name in the allowed-routes map, e.g. `webapi.PathDecisionHistory: "board decision history"`.

### 5.2 Frontend (`code/frontend`, owner A5)

- **`model.Decision`** gains `Revision`, `DecidedBy`, `ProjectKey`, `Supersedes`, `SupersededBy`, `Rationale`, `Judged {NoConflict, Conflict, Unsure, Pending}`, `OpenConflicts []string`, `UpdatedAt`, `EndedAt`. `Scope` stays a joined string for the card.
- **`model.Conflict`** gains `JudgeNote string` and `EscalatedAt time.Time`. `DecisionKey` is now filled from the wire, which lights up `state/graph.go`'s decision nodes with no change there.
- **`feed/wire.go`** maps §9 exactly; keep `accepted → active`.
- **`state/store.go`** folds `decision_superseded` (`new_status` superseded or revoked sets the status) and removes the never-emitted `decision_updated` case. The board refetches on structural frames anyway.
- **`view/decisions.templ`**, accepted cards:
  - the key, then badges: `active`, `always-show`, `team-wide` or the project key;
  - title (bold), statement;
  - scope chips;
  - `r{revision} · decided by {decided_by} · {ago} ago`, with `revised {ago} ago` when revision > 1;
  - a judged line: `checked against {n} plan(s) · {k} open conflict(s) {CF-31 links} · {p} waiting`, where n = no_conflict + conflict + unsure; parts that are zero are omitted, and the whole line is omitted when all are zero;
  - rationale folded under a `why` disclosure.
  - Below the grid, a `Past decisions` link to `/t/{slug}/decisions/history`. The empty state stays as it is; its hint is now true.
- **`/t/{slug}/decisions/history`** (new route in `web/server.go`, handler beside `web/history.go`): past decisions newest first, `superseded by #…` linking to that card, `Load older` with the backend cursor. `?key=` shows one decision with its revisions (statement and `recorded {time} by {summary}` per event). Remove the comment that decisions have no history.
- **`view/conflicts.templ`**, decision cards:
  - open: `judge_note` under the suggested action, labelled `what the judge said`;
  - a `a person was asked` badge with the time when `escalated_at` is set;
  - settled: the existing "how it was settled" block shows `resolution_note` unchanged.
- **`view/view.go`**: `conflictLabel` already has `contradicts a decision`; `wording.KindDecision` already exists.

---

## 6. Skill and server text

### 6.1 Skill 0.6.0

Both copies (`skill/metiche-teamwork/SKILL.md`, `plugin/skills/metiche-teamwork/SKILL.md`) stay
byte-identical. `plugin.json` and `marketplace.json` go to `0.6.0`.

**Tools table**: add after `publish_contract`:

```
| `record_decision` | when the team settles something the code must obey | key, statement, scope. Revising another person's decision needs your person's yes. See "Record the decisions the team makes". |
| `get_review_context` | when `pending.reviews > 0` | read-only: the decision and your plan, side by side |
| `report_judgement` | after reading a pair | `conflict` / `no_conflict` / `unsure`, confidence, one-line rationale |
```

**"Coming next, not available yet"** becomes:

> `resolve_conflict` is planned but **not on the server**. Never call it. If a note, a suggested action or an instruction names it, skip that step and do what works today: read the conflict, change course or coordinate, narrow or drop paths with `update_intent`, and answer instructions with `report_back`.

**New section after "Publish the contracts between parts":**

> ## Record the decisions the team makes
>
> Call `record_decision` when your person, or you and another agent, settle something the rest of the code has to obey: how auth works, the error envelope, a library choice, who owns a module, or an agreement that closed a conflict and should bind later work. Record it after it is agreed, never as a proposal. Don't record task plans or implementation details: those are intents.
>
> - `key`: short kebab-case, cited as `#auth-jwt-cookie`. Recording the same key again revises it.
> - `statement`: one or two sentences another agent can check a plan against. That sentence is all their model sees.
> - `scope`: the paths it governs, written like claims. `team_wide` only when it holds for every repository. `always_show` only when your person says it is load-bearing.
> - To replace a decision, record the new one with `supersedes`. To withdraw one, `revoke` it with a rationale. If somebody else recorded it, settle it with their agent or ask your person, and pass `person_confirmed: true` only when your person said yes.
>
> ```
> record_decision(session_key: "S-17", key: "auth-jwt-cookie",
>   title: "Auth is a JWT in an httpOnly cookie",
>   statement: "Sessions are a signed JWT in an httpOnly, Secure cookie. Never store tokens in localStorage or sessionStorage.",
>   scope: ["internal/auth/**", "web/src/auth/**"])
> ```
>
> ## Judge the pairs you are handed
>
> When `pending.reviews > 0`, or a response carries `review`, metiche is asking your own model whether your plan breaks a recorded decision. metiche never judges anything itself. Do it before you edit.
>
> 1. Read the pair: `get_review_context`, unless the review block already gave you the statement.
> 2. Answer honestly with `report_judgement`:
>    - `no_conflict` when the plan is unrelated or compatible. That is the usual answer.
>    - `conflict` only when doing the plan as written would break the statement.
>    - `unsure` when the wording doesn't settle it.
>
>    Give a real `confidence` and a one-line `rationale` naming the part of the plan and the part of the decision. The board shows both.
> 3. On a conflict, follow the decision: change the plan and `update_intent` the summary. You'll be asked once more, and a `no_conflict` closes it. If the decision is wrong, settle that with its author's agent; ask your person only if you can't.
>
> Never answer `no_conflict` to avoid work.
>
> If you recorded a decision and get a `conflict_notice` about it: do nothing if the decision stands, or revise it with `record_decision` if the team changed its mind, then `report_back` what you did. If an instruction asks you to ask your person, ask them that exact question and `report_back` their answer.

**The `pending` bullets**: replace the `reviews` bullet with:

> - **`reviews > 0` → judge now.** `get_review_context`, then `report_judgement` for each pair. Do it before you edit the files the plan names.

**Worked example**: add a step after step 3 (the agents are A and B, the people Ana and Bob):

> **3b. B's plan meets a decision.** Ana's agent had recorded `#auth-jwt-cookie` with scope `web/src/auth/**`. B's next declaration comes back with a review:
>
> ```
> B → declare_intent(session_key: "S-22", summary: "Keep the session token in localStorage after login",
>       paths: ["web/src/auth/session.ts"], mode: "write")
>     ← {"ok": true, "key": "INT-92", "review": {"pairs": [{"pair_key": "9c1e…", "decision": "#auth-jwt-cookie",
>          "statement": "Sessions are a signed JWT in an httpOnly, Secure cookie. Never store tokens in localStorage…",
>          "why": "scope"}]}, "pending": {"instructions": 0, "conflicts": 0, "reviews": 1}}
> B → report_judgement(session_key: "S-22", pair_key: "9c1e…", verdict: "conflict", confidence: 0.9,
>       rationale: "plan stores the token in localStorage; #auth-jwt-cookie forbids it")
>     ← {"conflicts": [{"key": "CF-31", "kind": "decision_contradiction", "decision": "#auth-jwt-cookie",
>          "at_fault": "plan", "suggested_action": "Your plan INT-92 breaks #auth-jwt-cookie … Change the plan …"}]}
> B → update_intent(session_key: "S-22", intent_key: "INT-92",
>       summary: "Read the session from the httpOnly cookie the login endpoint sets")
>     ← {"pending": {"reviews": 1}}
> B → report_judgement(session_key: "S-22", pair_key: "4d7a…", verdict: "no_conflict", confidence: 0.95,
>       rationale: "plan now relies on the httpOnly cookie")
>     ← {"note": "judged INT-92 against #auth-jwt-cookie: no conflict (0.95); CF-31 settled"}
> ```
>
> A is told once, through `get_instructions`, and does nothing because the decision stands. Nobody's person was interrupted.

**Quick reference, Do**: add `Record a decision once it is agreed, and judge every pair you are handed before you edit.`

### 6.2 Server instructions (`serverInstructions`, after the contracts paragraph)

```
When the team settles something the code must obey (how auth works, an error format, a library, who owns a
module), record it with record_decision: a short key, a statement another agent can check a plan against, and the
paths it governs. When pending.reviews is above zero, or a response carries review, metiche is asking your own
model whether your plan breaks a recorded decision: read the pair with get_review_context and answer with
report_judgement. no_conflict is the usual answer; conflict only when doing the plan as written would break the
statement. On a conflict, change the plan and update_intent; if the decision is wrong, settle it with its author's
agent, and ask your person only if you can't.
```

### 6.3 Landing and docs (owner: coordinator, Wave C)

- `collideDecision` becomes a live card ("works today") in the present tense: an agent records a decision, another agent's plan touches it, that agent's own model judges it, and the conflict settles itself when the plan changes.
- The tool group line lists decisions; the footer drops "coming next" for judgement calls.
- `docs_pages.templ`:
  - a "Decisions" tool group with the three rows;
  - the board page describes the Decisions tab, its history and the decision conflict card;
  - "Decisions" leaves the not-built list, which keeps duplicate work and dismissals.
- `landing_test.go` and `docs_test.go` change with it.

---

## 7. Test plan

Integration tests need `METICHE_TEST_MYSQL_DSN` with `parseTime=true&interpolateParams=true` and run
with `go test -p 1`.

### 7.1 Unit (pure, no database)

- **`app/coordination/tokens_test.go`:**
  - keeps `jwt`, `api`, `ui`, `db`;
  - splits `camelCase`, `snake_case`, `kebab-case` and `path/segments`;
  - drops stopwords and single characters;
  - lowercases; de-duplicates keeping first-seen order; caps at the limit; returns the same output for the same input.
- **`app/coordination/decisions_test.go`:**
  - `NormalizeDecisionKey` on `#Auth JWT_cookie` → `#auth-jwt-cookie`, and the rejects;
  - `DecisionContentHash` is stable across scope order and changes with team_wide;
  - `RankDecisionCandidates` order and cap;
  - `JudgedSeverity`, the full §4.2 table: 0.69 vs 0.70, same member −1, always_show floor, the high clamp, unsure;
  - `EscalationBudget` per cadence;
  - `ShouldEscalate`: floor, budget edge, intent not live, already escalated.
- **`app/coordination/importguard_test.go`:** parse the imports of `app/coordination/*.go`, and of `app/mcp/{recorddecision,decisionreview,reviewcontext,reportjudgement,decisionresolve}.go`. Fail on `net/http` in coordination, and on any known model SDK import path anywhere in that list.
- **`app/coordination/pairkey_test.go`:** add a decision and intent pair; symmetric; an intent revision bump changes the key; the same subjects under `duplicate_work` give a different key.
- **`app/mcp/decisions_test.go`:**
  - `parseRecordDecision`, every row of the §3.1 error table;
  - `decideDecisionSettlement`, every row of §4.5 plus coordinated;
  - every note in §4.8 fits its limit and masks `mtk_…`, `bearer …` and `password=…`;
  - the review block cut order at 1200 characters;
  - `NoteForPending` reviews text;
  - `DecisionContradictionDedupeKey` ignores revisions.
- **`app/mcp/publishcontract_test.go`:** a shape with `id` in both request and response yields two field rows (keyed on direction and path).

### 7.2 Integration, `app/mcp` (`decisions_integration_test.go`)

- **`record_decision`:**
  - new, unchanged (one event each, the second call's bytes identical under the same idempotency_key), updated (no revision bump), revised (revision 2, paths and tokens rewritten), reinstated;
  - supersede and revoke settle open conflicts with the §4.8 notes;
  - another member's key refused with `not_permitted`, then accepted with `person_confirmed` (and `decided_by` moves);
  - superseded key refused;
  - always-show cap;
  - a new decision assigns pairs to other sessions' live plans under the cap, with the rest unassigned;
  - the recording session's own plans are skipped.
- **`declare_intent` and `update_intent`:**
  - inline block with ≤ 3 pairs;
  - a team with no decisions returns exactly the previous bytes;
  - `review_block_enabled = false` assigns and omits the block;
  - a status-line-only update assigns nothing new;
  - a summary change mints a new pair;
  - a replayed declaration returns the same pair keys.
- **Concurrency:** 8 goroutines declare onto one decision's scope → exactly one judgement per pair key, a gapless `team.sequence`, and the `metiche_team_lock_hold_ms` p99 stays under 25ms.
- **`get_review_context`:** row counts of `judgement`, `instruction` and `team_event`, and `team.sequence`, are unchanged after the call; only your own pairs; `pair_key` of another session refused; budget and `more_waiting`; works on an ended session.
- **`report_judgement`:**
  - `no_conflict`: non-structural event, no conflict row;
  - `conflict` 0.9: the conflict row with the §3.3 evidence, participants (intent/initiator, decision/incumbent), a notice to the decider only when a different member, caller `conflicts[]`, structural event;
  - `conflict` 0.6: low, no notice, no `conflicts[]`;
  - `unsure`: low, rule `.unsure`;
  - same verdict replayed byte for byte, a different verdict refused;
  - stale after `update_intent`, not assigned, ended intent, superseded decision: all refused.
- **Settlement:**
  - plan revised then `no_conflict` → CONVERGED with the note;
  - decision revised then `no_conflict` → the "decision revised" note;
  - update_intent done → SUPERSEDED, the intent's pending judgements expired;
  - end_session → SUPERSEDED, `ExpireJudgementsOfSession`;
  - reopen after metiche closed it (`escalated_at` cleared);
  - silenced after `resolved_by_member_uuid` is set by hand.
- **Session keys across teams:** a key present on two teams, for all three tools, like `sessionkey_integration_test.go`.

### 7.3 Integration, sweeper and webapi

- **`app/sweeper` (`decisions_test` with MySQL):**
  - abandoned session → SUPERSEDED decision conflicts;
  - backlog assigned when under the cap;
  - lapsed window re-armed to `assignment_count` 3, then expired;
  - stale subjects expired without re-arming;
  - escalation on a test clock: nothing at budget − 1s, fires at budget, once (a second pass writes nothing), `question` bodies ≤ 200, `conflict_escalated` structural, never below the human floor, never for a finished intent.
- **`app/webapi` (MySQL):**
  - `/decisions` fields and judged counts on the current revision only, accepted only;
  - `/decisions/history` status filter, cursor paging with no duplicates or gaps, `?key=` revisions;
  - `/conflicts` `decision_key`, `judge_note`, `escalated_at` and paths without the key;
  - the history kind filter accepts `decision_contradiction`.

### 7.4 End to end, through the real transport (`app/decisions_e2e_mysql_test.go`)

Two agents with their own tokens over `/v1/mcp` (`inviteWorld.connect`, `tool`). Every name is a test name.

1. Ana's agent starts S-17 on `shop`, records `#auth-jwt-cookie` with scope `internal/auth/**`, `web/src/auth/**`.
2. Bob's agent starts S-22 and declares "store the session token in localStorage after login" on `web/src/auth/session.ts`. The response has `review` with one pair; `pending.reviews` is 1.
3. Bob calls `get_review_context` (one item) and `report_judgement` conflict 0.9. `conflicts[]` has CF with `at_fault: "plan"`, `decision: "#auth-jwt-cookie"`.
4. Ana heartbeats: `pending.instructions` 1, `pending.conflicts` 1. `get_instructions` delivers the notice with `ref` = CF.
5. Bob `update_intent`s the summary to "read the session from the httpOnly cookie": `pending.reviews` 1, a new pair key. `report_judgement no_conflict` → the note says CF settled; the conflict is CONVERGED with the §4.8 note.
6. REST: `/decisions` shows `judged.conflict` 0 and `no_conflict` 1 for revision 1 (the judgement on the pair at plan revision 2), `open_conflicts` empty. Conflict history carries the note. `/events` has two `judgement_reported`.
7. Interruptions to Ana: exactly one instruction over the whole run.

Variant A: Ana's agent revises the decision instead; Bob's re-judge converges with the "decision revised" note.
Variant B: nobody acts, severity `high`, project cadence hackathon; the sweeper at clock + 10m escalates, and Bob's agent receives the question.

### 7.5 Frontend

- `feed/decisions_wire_test.go` decodes the §9 examples verbatim.
- `state/store_test.go` folds `decision_superseded`.
- `view/decisions_test.go`: active card with every line, zero parts omitted, empty state, past link.
- `view/conflicts_test.go`: judge note, escalated badge, settled note.
- `web/history_test.go`: decisions history page and `Load older`.
- `web/docs_test.go`: the tool list equals the registered tools.
- `view/landing_test.go`: the decision card is live.

### 7.6 Mutation targets

Each of these, flipped, must turn at least one test red:

- `confidence >= 0.70` → `>`; the notify-floor and human-floor comparisons;
- `judge_session_uuid == caller` removed;
- either revision staleness compare removed;
- `ON DUPLICATE KEY UPDATE id = id` → plain INSERT;
- the reopen guard `resolved_by_member_uuid IS NULL` removed;
- `escalated_at IS NULL` removed from the escalation UPDATE;
- `escalated_at = NULL` removed from reopen;
- the always-show cap `>` → `>=`;
- the per-minute cap count;
- `team_slug` dropped from `RequireSessionOnTeam` in any of the three tools;
- any `Structural` flag inverted;
- the idempotency key losing the session (record) or the verdict (judgement);
- `sanitizeNoteText` on the rationale removed;
- `seen` keyed on path only;
- any write inside `get_review_context`;
- the recorder-session skip removed;
- the pinned skip removed;
- each `LIMIT` in the new queries removed;
- the settlement rule order swapped (rule 4 before 1);
- the always-show drop-past-cap → backlog.

---

## 8. Build plan

**The rules for every agent** (docs/LEARNINGS.md §8):
- Every brief names the files the agent owns, and two agents never share one.
- Generated code, `create.sql` and `deploy/` are off limits.
- Proof is the command that was run and its output.
- Commits are in the owner's name with no trailers.

### Wave 0 — schema: **in progress**

- nuzur `v7-decisions` (`f73e4dfa-8d6c-49f2-825c-da7c1e756995`) is **published** with exactly §2.2.
- Codegen and `deploy/sql/2026-09-decisions.sql` are being produced by the codegen agent.
- §9 is frozen by this document.

### Wave A — parallel, no codegen needed

| Agent | Owns | Proof |
|---|---|---|
| A1 coordination | `app/coordination/tokens.go`, `tokens_test.go`, `decisions.go`, `decisions_test.go`, `importguard_test.go`, additions to `pairkey_test.go` | `go test ./app/coordination/ -v` output |
| A5 frontend | `internal/model/*`, `internal/feed/wire.go` + tests, `internal/state/store.go` + tests, `internal/view/decisions.templ`, `conflicts.templ`, `view.go`, `internal/web/server.go`, `internal/web/history.go` (+ the new decisions history handler and template), generated `_templ.go`, tests; builds against §9's examples | `templ generate`, `go test ./...` in `code/frontend`, a screenshot of the Decisions tab from the fixture |
| A6 skill | both `SKILL.md` copies, `plugin.json`, `marketplace.json` (0.6.0) | the two files `diff`ed identical; merged only after Wave B lands |

### Wave B — after codegen

| Agent | Owns | Needs |
|---|---|---|
| B1 `app/mcp` (the seams: one owner) | new `recorddecision.go`, `decisionreview.go`, `reviewcontext.go`, `reportjudgement.go`, `decisionresolve.go`, `decisions_test.go`, `decisions_integration_test.go`; edits to `server.go`, `envelope.go`, `instructions.go`, `intents.go`, `sessions.go`, `conflictresolve.go`, `publishcontract.go`, `publishcontract_test.go`, `contracts_integration_test.go`, `server_test.go` | A1's functions (§9.1) |
| B2 webapi + wiring | `app/webapi/decisions.go` (+ history), `conflicts.go`, `conflicthistory.go`, `wire.go`, `api.go`, `app/rest.go` (chained detector, allowed routes), webapi MySQL tests | B1's `NewDecisionReviewer` and `ChainDetectors` compiling (§9.1) |
| B3 sweeper | `app/sweeper/decisions.go` (backlog, re-arm, expire, escalate) + tests, edits to `sweeper.go` (`RunOnce` step) and `conflicts.go` (`settleAfterAbandon`) | B1's `OpenDecisionConflictsOfSession`, `SettleDecisionConflict`, `ExpireJudgementsOfSession`; A1's `ShouldEscalate`, `EscalationBudget` |

B2 and B3 can start on B1's frozen signatures with stub bodies.

### Wave C — the coordinator

- `app/decisions_e2e_mysql_test.go` (§7.4);
- `landing.templ`, `docs_pages.templ`, `landing_test.go`, `docs_test.go` (§6.3);
- the full backend and frontend suites;
- the lock-hold p99 under the concurrency test;
- the migration applied to a copy of the current schema, `SHOW CREATE TABLE` compared with `create.sql`;
- the deploy runbook: migration first, then the backend, then the board, then the skill.

### Risks

1. **Lock hold.** The reviewer adds queries to every `declare_intent` on a team with decisions. Mitigated by the fast path, 16 scanned paths, 32 tokens and LIMITs. The histogram decides.
2. **Self-judging bias.** The owner may answer `no_conflict` to avoid work. The rationale is on the board; that is the mitigation chosen (§10).
3. **always_show fan-out.** Capped at 5 per team and lowest priority inside the per-minute cap.
4. **Migration order and error 1553** (§2.3).
5. **Must ship together.** `docs_test.go` fails unless docs change with the tools. The skill must not advertise tools before they are deployed.
6. **Old wording** ages out with event retention (§10).
7. **Orphaned judgements.** Rows whose intent was deleted with its session stay. A batched retention cleanup is follow-up work.
8. **Question bodies over 200 characters** would be cut by `get_instructions` (F7); §7.3 tests the bound.

---

## 9. Frozen interfaces

### 9.1 Go

`queryer` is the package's existing unexported interface. Callers outside the package pass a
`*sql.Tx` or `*sql.DB`, as the sweeper already does with `SettleContractConflict`.

```go
// app/mcp/decisionresolve.go — owner B1, used by B3

type DecisionReleaseKind int

const (
	DecisionReleaseJudged           DecisionReleaseKind = iota + 1 // report_judgement no_conflict
	DecisionReleaseDecisionChanged                                 // record_decision supersede or revoke
	DecisionReleaseIntentEnded                                     // update_intent done/abandoned/superseded
	DecisionReleaseSessionEnded                                    // end_session
	DecisionReleaseSessionAbandoned                                // the sweeper writing a session off
)

type DecisionRelease struct {
	TeamUUID    uuid.UUID
	SessionUUID uuid.UUID // whose call or lapse caused it
	Kind        DecisionReleaseKind
	At          time.Time // the transaction's clock
}

// OpenDecisionConflictsOfSession lists the open or acknowledged decision_contradiction
// conflicts a session takes part in, oldest first, at most settleMaxConflicts.
// Driven by idx_participant_session.
func OpenDecisionConflictsOfSession(ctx context.Context, q queryer, teamUUID, sessionUUID uuid.UUID) ([]uuid.UUID, error)

// SettleDecisionConflict re-evaluates one decision_contradiction conflict (§4.5) and closes it
// when a rule fits. It returns false and writes nothing when the conflict is closed already or
// still stands. Must run on the transaction holding the team lock. The caller appends the
// conflict_resolved event (ConflictResolvedSummary / ConflictResolvedPayload handle the kind).
func SettleDecisionConflict(ctx context.Context, q queryer, conflictID uuid.UUID, rel DecisionRelease) (SettledConflict, bool, error)

// ExpireJudgementsOfSession marks every pending judgement assigned to the session expired.
// Driven by idx_judgement_assignment. No event.
func ExpireJudgementsOfSession(ctx context.Context, q queryer, sessionUUID uuid.UUID, now time.Time) (int64, error)

// app/mcp/decisionreview.go — owner B1, used by B2 in app/rest.go

// NewDecisionReviewer returns the Detect hook that pairs a declared or updated intent with
// candidate decisions, assigns the judgements and writes the inline review block (§3.2).
// It returns no ConflictNotices.
func NewDecisionReviewer(coreImpl *core.Implementation, logger *zap.Logger) DetectHook

// ChainDetectors runs hooks in order on the same TxContext and Mutation, skipping nil hooks,
// concatenating their notices and returning the first error.
func ChainDetectors(hooks ...DetectHook) DetectHook
```

```go
// app/coordination — owner A1, used by B1 and B3. Pure: no database, no clock, no network.

func Tokenize(text string, max int) []string
func NormalizeDecisionKey(raw string) (string, error) // "#auth-jwt-cookie"
func DecisionContentHash(title, statement string, scopeNorm []string, teamWide bool) string // hex sha256

type CandidateSignal int

const (
	SignalWords      CandidateSignal = 1
	SignalAlwaysShow CandidateSignal = 2
	SignalScope      CandidateSignal = 3
)

type DecisionCandidate struct {
	DecisionUUID string
	DecisionKey  string
	IntentUUID   string
	IntentKey    string
	Signal       CandidateSignal // the strongest signal for this pair
	SharedTokens int
	Why          string // the ReviewItem.why text
}

func RankDecisionCandidates(in []DecisionCandidate, max int) []DecisionCandidate

const JudgeMinConfidence = 0.70

type JudgedSeverityInput struct {
	Verdict    string   // "conflict" | "unsure"
	Confidence float64
	Requested  Severity // SeverityLow..SeverityHigh; zero means medium
	SameMember bool
	AlwaysShow bool
}

func JudgedSeverity(in JudgedSeverityInput) Severity

// cadence is enums.ProjectCadence.String(): "hackathon" 10m, "sprint" 30m, "steady" 2h; anything else 30m.
func EscalationBudget(cadence string) time.Duration

func ShouldEscalate(severity, humanFloor Severity, openSince, now time.Time, cadence string, intentLive, alreadyEscalated bool) bool
```

### 9.2 `GET /v1/teams/{slug}/decisions`

Rules:
- `scope`, `open_conflicts` and `judged` are always present.
- `judged` counts each plan once, by its latest judgement on the current revision (§5.1); in the example, eight plans were paired with revision 2: five judged `no_conflict`, one `conflict` (CF-31), two still waiting.
- `decided_by`, `decided_at`, `updated_at`, `project_key`, `rationale`, `supersedes` and `superseded_by` are omitted when empty.
- `status` is always `accepted` here.
- Times are RFC 3339 UTC.

```json
{
  "sequence": 431,
  "board_revision": 92,
  "team": {"key": "shop-hack", "name": "Shop hackathon", "sequence": 431, "board_revision": 92},
  "decisions": [
    {
      "key": "#auth-jwt-cookie",
      "title": "Auth is a JWT in an httpOnly cookie",
      "statement": "Sessions are a signed JWT in an httpOnly, Secure cookie. Never store tokens in localStorage or sessionStorage.",
      "status": "accepted",
      "always_show": true,
      "revision": 2,
      "decided_by": "Ana",
      "decided_at": "2026-09-15T13:02:11Z",
      "updated_at": "2026-09-15T13:40:05Z",
      "scope": ["internal/auth/**", "web/src/auth/**"],
      "project_key": "shop",
      "rationale": "One session mechanism for the web app and the API; a cookie is out of reach of injected scripts.",
      "supersedes": "#auth-bearer-header",
      "judged": {"no_conflict": 5, "conflict": 1, "unsure": 0, "pending": 2},
      "open_conflicts": ["CF-31"]
    }
  ]
}
```

```go
type decisionWire struct {
	Key           string             `json:"key"`
	Title         string             `json:"title"`
	Statement     string             `json:"statement"`
	Status        string             `json:"status"`
	AlwaysShow    bool               `json:"always_show"`
	Revision      int64              `json:"revision"`
	DecidedBy     string             `json:"decided_by,omitempty"`
	DecidedAt     *string            `json:"decided_at,omitempty"`
	UpdatedAt     *string            `json:"updated_at,omitempty"`
	EndedAt       *string            `json:"ended_at,omitempty"` // history only
	Scope         []string           `json:"scope"`
	ProjectKey    string             `json:"project_key,omitempty"` // empty = team-wide
	Rationale     string             `json:"rationale,omitempty"`   // clipped 600
	Supersedes    string             `json:"supersedes,omitempty"`
	SupersededBy  string             `json:"superseded_by,omitempty"`
	Judged        decisionJudgedWire `json:"judged"`
	OpenConflicts []string           `json:"open_conflicts"`
}

type decisionJudgedWire struct {
	NoConflict int64 `json:"no_conflict"`
	Conflict   int64 `json:"conflict"`
	Unsure     int64 `json:"unsure"`
	Pending    int64 `json:"pending"`
}
```

### 9.3 `GET /v1/teams/{slug}/decisions/history`

Query: `status` (empty, `superseded`, `revoked`), `cursor`, `limit` (default 50, max 100), `key`.

- `ended_at` is the row's `updated_at` once terminal (a terminal decision is never written again).
- `next_cursor` is present only when there is an older page.
- `revisions` is present only when `key` is given; there `decisions` holds that one decision in any status, and `next_cursor` is absent.
- A bad `status` or `cursor` is `400`, as in conflict history.

```json
{
  "sequence": 431,
  "board_revision": 92,
  "team": {"key": "shop-hack", "name": "Shop hackathon", "sequence": 431, "board_revision": 92},
  "status": "",
  "statuses": ["superseded", "revoked"],
  "decisions": [
    {
      "key": "#auth-bearer-header",
      "title": "Auth is a bearer token in the Authorization header",
      "statement": "Clients send the session JWT as Authorization: Bearer on every API call.",
      "status": "superseded",
      "always_show": false,
      "revision": 1,
      "decided_by": "Bob",
      "decided_at": "2026-09-15T11:20:00Z",
      "updated_at": "2026-09-15T13:02:11Z",
      "ended_at": "2026-09-15T13:02:11Z",
      "scope": ["internal/auth/**"],
      "project_key": "shop",
      "superseded_by": "#auth-jwt-cookie",
      "judged": {"no_conflict": 3, "conflict": 0, "unsure": 1, "pending": 0},
      "open_conflicts": []
    }
  ],
  "next_cursor": "ZDF8MjAyNi0wOS0xNVQxMzowMjoxMVp8I2F1dGgtYmVhcmVyLWhlYWRlcg",
  "revisions": [
    {
      "sequence": 402,
      "kind": "decision_recorded",
      "summary": "backend revised #auth-jwt-cookie (r2)",
      "statement": "Sessions are a signed JWT in an httpOnly, Secure cookie. Never store tokens in localStorage or sessionStorage.",
      "occurred_at": "2026-09-15T13:40:05Z"
    }
  ]
}
```

The example shows `next_cursor` and `revisions` together only to freeze both shapes; a real response
carries one or the other.

```go
type decisionHistoryResponse struct {
	cursors
	Team       teamRef                 `json:"team"`
	Status     string                  `json:"status"`
	Statuses   []string                `json:"statuses"`
	Decisions  []decisionWire          `json:"decisions"`
	NextCursor string                  `json:"next_cursor,omitempty"`
	Revisions  []decisionRevisionWire  `json:"revisions,omitempty"`
}

type decisionRevisionWire struct {
	Sequence   int64   `json:"sequence"`
	Kind       string  `json:"kind"`
	Summary    string  `json:"summary"`
	Statement  string  `json:"statement,omitempty"` // payload.message
	OccurredAt *string `json:"occurred_at,omitempty"`
}
```

### 9.4 Conflict wire: the new fields

`conflictWire` gains three fields, on `/conflicts`, `/conflicts/history`, the session page and run history:

```go
DecisionKey string  `json:"decision_key,omitempty"` // decision_contradiction only: evidence overlap_path
JudgeNote   string  `json:"judge_note,omitempty"`   // decision_contradiction only: evidence field_issues[0]
EscalatedAt *string `json:"escalated_at,omitempty"` // any kind; set when metiche asked a person
```

A full open decision conflict:

```json
{
  "key": "CF-31",
  "kind": "decision_contradiction",
  "severity": "high",
  "status": "open",
  "detected_by": "agent",
  "detector_rule": "decision_contradiction.judged",
  "suggested_action": "Your plan INT-91 breaks #auth-jwt-cookie (Sessions are a signed JWT in an httpOnly, Secure cookie. Never store tokens in localStorage…). Change the plan to follow it and update_intent with the new summary, which asks you to judge again. If the decision itself is wrong, settle that with Ana's agent (S-17); only Ana or your person can change it.",
  "occurrence_count": 1,
  "first_detected_at": "2026-09-15T14:02:40Z",
  "last_detected_at": "2026-09-15T14:02:40Z",
  "paths": ["web/src/auth/session.ts", "web/src/auth/**"],
  "decision_key": "#auth-jwt-cookie",
  "judge_note": "S-22's model (0.90): plan stores the token in localStorage; #auth-jwt-cookie forbids it",
  "escalated_at": "2026-09-15T14:12:40Z",
  "participants": [
    {"session_key": "S-22", "member_name": "Bob", "agent_label": "ui", "role": "initiator", "subject_kind": "intent"},
    {"session_key": "S-17", "member_name": "Ana", "agent_label": "backend", "role": "incumbent", "subject_kind": "decision"}
  ]
}
```

Settled, the same object carries `"status": "resolved"`, `"resolution": "converged"`,
`"resolved_at": "2026-09-15T14:15:02Z"` and
`"resolution_note": "Settled by the agents: S-22 (ui) revised INT-91 at 14:15 UTC and judged it no longer contradicts #auth-jwt-cookie (r2): \"plan now relies on the httpOnly cookie\". A person was asked at 14:12 UTC."`.

---

## 10. Decisions (owner-approved 2026-09-15)

1. **Changing another member's decision needs `person_confirmed: true`.** Any write to an existing decision recorded by another member (revise, update, supersede, revoke, reinstate) is refused with `not_permitted` without it. A member's own agents change their own decisions freely. On success the decision's `decided_by` becomes the member who changed it.
2. **The plan owner alone judges.** Every pair goes to the agent whose plan it is. The decider's agent hears only about conflicts (a notice), never about `no_conflict` verdicts. The mitigation for self-judging bias is visibility: rationale and confidence are on the board.
3. **A person is asked only above the human floor, after a budget.** `team_settings.human_notify_floor`, default `high`. Budgets by project cadence: hackathon 10 minutes, sprint 30 minutes, steady 2 hours. Once per conflict, cleared only when metiche-closed conflicts reopen (§4.7).
4. **PLAN's always_show rule, capped at 5.** An always-show decision is a candidate for every live plan in its project, or on the team when team-wide. At most 5 accepted per team. It ranks below scope and above words, and pairs past the per-minute cap are dropped rather than backlogged.
5. **No `decision_revision` table.** The decision row holds the current wording forever. Earlier wording lives in `decision_recorded` events (`payload.message`) and ages out with event retention.
6. **No `proposed` status in v1.** Every recorded decision is `accepted`. `proposed` stays in the enum, unused.

### 10.1 Refinements made while writing this spec

These tighten the approved design. None changes the model or an owner decision.

- **Escalation condition (b) is dropped.** The approved design also escalated immediately when "no live agent of the decider member exists and the plan owner answered the notice with refused or blocked". The plan owner is told synchronously in `conflicts[]` and never receives a notice, so it has nothing to answer, and the condition could never fire. The time budget in §4.7 is the whole rule. The agent-initiated path through `record_decision`'s `not_permitted` message remains.
- **`confidence` is `*float64`**, so a missing confidence is refused rather than read as 0.
- **`unsure` has its own detector rule**, `decision_contradiction.unsure`, so a future per-rule demotion can tell it apart from judged conflicts.
- **Reopening clears `escalated_at`**, so a reopened conflict can be escalated once more, with an idempotency key that includes `occurrence_count`.
- **Question bodies fit 200 characters**, because `get_instructions` shows at most that (F7).
- **`GET /decisions` returns accepted decisions only**; superseded and revoked ones move to the history endpoint.
