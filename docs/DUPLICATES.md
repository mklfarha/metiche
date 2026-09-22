# Duplicate work: two agents about to build the same thing, told before either finishes

Status: **design approved 2026-09-21, not yet built.** The data model change is one column,
`intent.wording_revision`, in nuzur draft `v8-wording-revision` (under review); the migration is
`deploy/sql/2026-09-wording-revision.sql`, written by the codegen agent (§2). The owner's answers are
folded in as decisions (§10). Build agents implement straight from this file. §9 "Frozen interfaces" is
the contract between them; change it only through the coordinator.

This covers the fourth conflict kind in PLAN, `duplicate_work`. It adds no tool. It extends three that
exist: the inline review block on `declare_intent` / `update_intent`, `get_review_context` and
`report_judgement`, exactly as docs/DECISIONS.md §4.6 promised ("Duplicate work reuses the ledger,
get_review_context items (kind) and report_judgement's dispatch"). `resolve_conflict` and dismissals
come next; nothing here boxes them in (§4.9).

## Why

Path overlap catches two agents in one file. Decisions catch a plan that breaks an agreement. Neither
catches the most expensive collision in a small team: two agents, in different files, both building the
login screen. Each is right from everything it can see; the waste shows up at merge, an hour later.

**The hackathon case is the whole feature.** In a hackathon there are no issue numbers. People say
"add login page" and "build the login screen" and start typing. So the signal that matters most is the
*wording* of two short, informal plans in the same repository, with no `external_ref` at all. The issue id
is the strongest signal when it exists (a sprint team with a tracker), and it is the only one that crosses
repositories; but the thresholds, the stoplist, the synonym folding and the frequency filter in §4.2 are
designed for three-to-six-word summaries, and the test vectors in §7.1 are hackathon summaries.

**The direction.** When a plan is declared, or its wording changes, the server finds live plans that may
be the same work: same issue id anywhere on the team, or summaries that say the same thing in the same
repository. It hands each pair to **the agent that just declared or reworded**, whose model judges
whether the two plans would produce the same change. That agent declared later, so on a conflict it is
the one asked to yield. The server never calls a model and never decides "same thing" itself. The agent
that was there first is told only if the conflict is still standing after a short grace, and a person is
asked only if the agents have not settled it within the project's budget.

---

## 0. What the code does today (verified at 9384f2a)

| # | finding | where |
|---|---|---|
| F1 | `conflict_kind.duplicate_work` exists and nothing raises it. The conflict history offers only live kinds and excludes it. | `enums/conflict_kind.go:21`; `app/webapi/conflicthistory.go` `liveConflictKinds` |
| F2 | `judgement.kind` is a `conflict_kind` and validation already accepts `duplicate_work`. Subjects are `subject_kind`, which has `intent`. | `entity/judgement/judgement.go:21-27`, `judgement_validate.go:31`; `enums/subject_kind.go:17` |
| F3 | `intent.external_ref` is stored (trimmed, ≤120) and never read. `idx_intent_external_ref (team_uuid, external_ref)` exists. | `app/mcp/intents.go` `DeclareIntent`; `create.sql:315,325` |
| F4 | `intent_token` exists, and no hand-written code writes or reads it. | `create.sql:392-408`; grep over `app/` |
| F5 | `update_intent` bumps `intent.revision` on a changed summary **or** added/dropped paths (`material`). A revision-scoped duplicate pair would therefore be re-asked on every `add_paths`. | `app/mcp/intents.go` `UpdateIntent` |
| F6 | The decision reviewer writes `m.Envelope.Review` and the note lead itself. A second reviewer writing the same field would overwrite it. | `app/mcp/decisionreview.go` `reviewIntent` |
| F7 | `judgementRow` names subject a `DecisionID` and subject b `IntentID`. `applyReportJudgement` always loads a decision for subject a; `detectReportJudgement` dispatches on kind and returns "pairs of kind %s cannot be judged yet" for anything else. | `app/mcp/reviewcontext.go`, `reportjudgement.go` |
| F8 | `expireIntentJudgements` expires pending pairs assigned to a session with `subject_b_uuid` = the intent. `judgeName` falls back to subject b's session. Both work unchanged if subject b is always the judge's plan. | `app/mcp/decisionresolve.go`, `reviewcontext.go` |
| F9 | The sweeper's judgement pass filters `kind = decision_contradiction`, so a new kind needs its own pass, and cannot be silently re-armed by the decision rule. | `app/sweeper/decisions.go` `loadOpenPairs` |
| F10 | `pendingCounts.Conflicts` counts every participant row on an open conflict. Attaching a participant is an interruption: the skill tells an agent with `conflicts > 0` to call `get_instructions`. | `app/mcp/envelope.go` `pendingCounts`; `SKILL.md` "Every response carries pending" |
| F11 | Path overlap does not soften for one person's two agents when both are live (`same_member_concurrent`): solo fan-out is exactly who needs telling. | `app/coordination/paths.go:530-549` |
| F12 | `conflict_resolution.yielded` exists and nothing uses it. | `enums/conflict_resolution.go:16` |
| F13 | `session.parent_session_uuid` links a subagent to its supervisor. A supervisor's plan and its subagent's plan routinely share wording. | `create.sql:283,288`; `app/mcp/sessions.go` |
| F14 | Two sessions producing one contract are flagged on the board and nowhere else. Out of scope here (§10.6). | `code/frontend/internal/state/derive.go:637-641` |
| F15 | Public text: the landing page and docs list duplicate work as not built (`landing.templ:18,349-351,469-477`; `docs_pages.templ:686-694`). README §4 says an issue match needs "no model". The skill does **not** mention duplicate work under "Coming next" (`SKILL.md:88-95` names only `resolve_conflict`); it mentions it only in the `external_ref` comment (`SKILL.md:175-181`). | as listed |

---

## 1. What PLAN and MODEL specify

### 1.1 The server never calls a model

Unchanged from DECISIONS §1.1: PLAN:18-19, PLAN:32, PLAN:483-484. Whether two plans are the same work is
a semantic judgement and is made by an agent's own model, always.

### 1.2 What they specify

- **Duplicate work is a v1 conflict kind.** PLAN:33.
- **The issue id.** `intent.external_ref` is "the highest-signal duplicate-work key because it is an exact match", with an `intent_token` side table for retrieval. PLAN:146-149; MODEL:35.
- **The review block.** "top ~3 similar intents (exact external_ref match first, then shared contracts, then sub-threshold path overlap, then token overlap)", each with a server `pair_key`, under the 700-token budget with `get_review_context` as the escape hatch. PLAN:214-225.
- **The pair key and the ledger.** Revision-scoped, symmetric, judged once, assigned to the plan's own agent with a 15-minute window re-armed at most three times; `no_conflict` must be cheap. PLAN:227-236; MODEL:54.
- **Races.** The later declarer is told synchronously; the earlier asynchronously. PLAN:239-244.
- **Noise.** Record floor `low`, notify floor `medium`; model verdicts interrupt only at confidence ≥ 0.7 "because models asked 'is this a conflict?' have a strong yes-bias"; ≤ 3 review items per session per minute; one conflict per pair; never without a suggested action. PLAN:247-255.
- **Scope.** Claims are project-scoped "or a poly-repo team gets cross-repo false positives". PLAN:139-140.
- **Next.** `resolve_conflict(status='dismissed', reason)`, per-rule dismissal rates and auto-demotion. PLAN:257-260.

### 1.3 Where they disagree, and what this spec does

1. **The issue id: judged or not.** README §4 says an exact issue match needs "no model"; PLAN uses it only as the top-ranked candidate in a judged block. **Decided (§10.1):** judged, top signal, severity floor `medium` on a conflict verdict. Agents on one ticket often split it (backend and UI). README §4 is reworded (§6.3).
2. **How many pairs.** "~3 similar intents" plus ~3 decisions cannot fit "≤ 3 review items per session per minute". **Decided:** at most 2 duplicate pairs per call, inside the one shared per-minute cap.
3. **`intent_token`.** PLAN and DECISIONS §3.1 expected duplicate work to write it. **Decided (§10.5):** it stays in the model and unused. Reading ≤ 100 live summaries of the project and scoring them in Go is inside the budget, needs no writes inside the lock on every declaration, and lets the scorer use synonym folding and a per-scan frequency filter a stored token table could not.
4. **What "material" means.** A revision-scoped key re-asks on every path change (F5). **Decided (§10.3):** a new column, `intent.wording_revision`, bumped only when the wording materially changes (§2.2). Duplicate pairs key on it.

### 1.4 Where PLAN is silent, decided here

| Silence | Decision | § |
|---|---|---|
| Who judges when both sides are plans | the session whose declaration or rewording surfaced the pair | 4.3 |
| Fault | the judge's plan yields (`at_fault: "later"`) | 4.3 |
| Cross-project pairs | issue id only | 4.2 |
| Wording in a hackathon | the whole feature there; thresholds designed for short summaries | 4.2 |
| Relation to path overlap | two conflicts, one interruption | 4.6 |
| When the incumbent hears | after a grace, and only if still unsettled | 4.5 |
| One side finishes | the duplicate stands while the yield side is still building | 4.7 |
| Same person's two agents | no softening | 4.4 |
| Supervisor and subagent | never paired | 4.2 |

---

## 2. Data model — nuzur `v8-wording-revision` (under review)

### 2.1 What already exists (unchanged)

All in `code/backend/metiche/core/repository/sql/schema/create.sql`.

| Table / index / enum | Used for |
|---|---|
| `intent` summary, `external_ref`, `revision`, `kind`, `status`, `expires_at` (`:311-320`) | the plans |
| `idx_intent_live (project_uuid, status, expires_at)` (`:324`) | the wording scan |
| `idx_intent_external_ref (team_uuid, external_ref)` (`:325`) | the team-wide issue lookup |
| `session.parent_session_uuid` (`:283`) | the supervisor/subagent skip |
| `judgement` (`:588-622`) with `uq_judgement_pair`, `idx_judgement_assignment`, `idx_judgement_subject (team_uuid, subject_a_uuid, subject_b_uuid)`, `idx_judgement_team_open` | the ledger. `idx_judgement_subject` serves both orders of an unordered pair as `a = ? AND b IN (…)` and `a IN (…) AND b = ?` |
| `conflict` with `escalated_at`, `idx_conflict_open`, `uq_conflict_dedupe` (`:526-563`) | the conflict |
| `conflict_participant`, `idx_participant_session` | who is involved |
| `instruction`, `idx_instruction_pending` | the notice and the question |
| `team_settings` `judge_window_seconds`, `max_reviews_per_minute`, `review_block_enabled`, `human_notify_floor`, `demoted_rules` | tuning, no new field |
| `conflict_evidence` `overlap_path`, `a_/b_label`, `a_/b_summary`, `a_/b_pattern`, `adjusters`, `field_issues`, `detail` | evidence, no new field |
| enums: `conflict_kind.duplicate_work`, `subject_kind.intent`, `judgement_status`, `judgement_verdict`, `conflict_resolution.yielded/converged/superseded/coordinated`, `event_kind` `judgement_reported`/`conflict_escalated`/`conflict_resolved`/`instruction_raised`, `instruction_kind` `conflict_notice`/`question`, `instruction_status.dismissed`, `participant_role` `initiator`/`incumbent`, `detected_by.agent` | **no enum changes** |
| `intent_token` | **not used** (§10.5) |

### 2.2 The v8 change: `intent.wording_revision`

One field, drafted by the coordinator in nuzur `v8-wording-revision`:

| | |
|---|---|
| entity | `intent` |
| field | `wording_revision` |
| type | integer, the same type code and size as `intent.revision` (INT) |
| nullable | no |
| default | 1 |
| index | none |
| relationship | none |
| description | "How many times the plan's wording has materially changed. Bumped by update_intent only when the summary's normalized tokens (or the external_ref) change; path, status and status-line edits never bump it. Baked into duplicate_work pair keys, so a reworded plan is asked once more whether it duplicates another, and a plan whose files changed is not." |

**The rule for a material wording change** (`coordination.WordingChanged`, pure):

- `WordingKey(summary, externalRef) = strings.Join(coordination.Tokenize(summary, 64), " ") + "\x00" + strings.ToLower(strings.TrimSpace(externalRef))`.
- Material when `WordingKey(old) != WordingKey(new)`. So case, punctuation, word forms folded by `Tokenize` (plurals) and stopword edits are cosmetic and bump nothing.
- `external_ref` cannot change after `declare_intent` today (`update_intent` has no such parameter); it is in the key so the rule stays right if that ever changes.
- **Path-only edits never bump it.** `intent.revision` keeps its current meaning for decisions.
- **Reverting to earlier wording is a new revision**, so the pair is asked once more. That is the accepted cost of an honest counter (§10.3); it costs one judgement and interrupts nobody unless the verdict is a conflict.

For `duplicate_work` pairs, `judgement.subject_a_revision` and `subject_b_revision` store the intents'
`wording_revision`. That is honest to the column name, and it is the only kind that uses this counter;
decision pairs keep `intent.revision`.

### 2.3 Migration: `deploy/sql/2026-09-wording-revision.sql` (applied BEFORE the new backend binary)

Written by the codegen agent; authoritative. Its single statement:

```sql
ALTER TABLE `intent` ADD COLUMN `wording_revision` INT NOT NULL DEFAULT 1 AFTER `revision`;
```

- **Order is load-bearing.** An old binary on the new schema is fine: every intent INSERT and UPDATE it runs names its columns, so the new one takes its default, and nothing it reads selects it. A new binary on the old schema fails on every intent insert and every duplicate query.
- **No data migration.** Existing intents get `wording_revision = 1`, which is correct: no duplicate pair exists yet.
- MySQL 8 adds a defaulted NOT NULL column in place; the statement is instant on this table size.
- **Verify** with `SHOW CREATE TABLE intent` against the regenerated `create.sql` (column order, default).
- `deploy/scripts/apply-schema.sh` only runs `CREATE TABLE IF NOT EXISTS`; this file is how production gets the column.

---

## 3. Tools

### 3.0 The shared pattern, and what is reused

Every rule of DECISIONS §3.0 holds: resolve the caller with `RequireSessionOnTeam`; validate outside the
lock; write through `commit(Mutation{Apply, Detect})`; JSON as `string(...)`; every query inside the lock a
point lookup or a LIMITed index range scan; agent text through `sanitizeNoteText`.

**Reused unchanged:** `commit`; `coordination.PairKey`, `Tokenize`, `JudgeMinConfidence`,
`EscalationBudget`, `ShouldEscalate`; the `insertDecisionJudgement` SQL shape
(`ON DUPLICATE KEY UPDATE id = id`); `existingPairKeys`; `reviewsAssignedRecently` (**one cap shared by both
kinds**); `loadDecisionSettings`; `loadLiveIntents`; `loadIntentHeldPaths`; `renderReviewBlock`'s cut order;
`pendingCounts` and `NoteForPending` (its reviews text, "pair(s) to judge against your plan", fits both
kinds); `appendConflictResolvedEvent`; `loadSettleNotes`; `describeHolder`; `clip`; `sanitizeNoteText`;
`upsertDecisionParticipant`; the reopen and silence logic of `raiseDecisionConflict`; the sweeper's
`appendEvent`, `errNothingToSay`, `insertQuestion` and `fitQuestion`.

**New:** `app/coordination/duplicates.go` (pure scoring, severity, grace, wording rule);
`app/mcp/duplicatereview.go` (the Detect hook); `app/mcp/duplicateresolve.go` (dedupe key, raising,
settlement, expiry, wording); `app/mcp/reviewrender.go` (one renderer for the shared block);
`app/sweeper/duplicates.go`.

| Constant | Value | Meaning |
|---|---|---|
| `duplicatePairKind` | `"duplicate_work"` | judgement and pair kind |
| `duplicateMaxLiveIntentsScanned` | 100 | project scan, `idx_intent_live` |
| `duplicateSameIssueLimit` | 8 | team-wide issue lookup |
| `duplicateMaxTokens` | 32 | per summary, from `Tokenize` |
| `duplicateMaxCandidates` | 10 | scored candidates kept before the pinned and existing checks |
| `duplicateMaxPairsPerCall` | 2 | pairs one call mints |
| `coordination.DuplicateKeyRunes` | 5 | comparison key length (§4.2) |
| `coordination.DuplicateFrequencyMinScanned` | 8 | the frequency filter runs only at ≥ 8 summaries |
| `coordination.DuplicateFrequencyMinCount` | 4 | a key in ≥ max(4, ⌈n/3⌉) summaries is dropped |
| judge window | 15m | `decisionJudgeWindow`, `team_settings.judge_window_seconds` overrides |
| rate cap | 3 per 60s | `max_reviews_per_minute`, shared with decisions |
| `judgeMaxAssignments` | 3 | re-arms |
| `coordination.JudgeMinConfidence` | 0.70 | inclusive |
| notice grace | `EscalationBudget(cadence)/5` | hackathon 2m, sprint 6m, steady 24m |
| escalation budgets | hackathon 10m, sprint 30m, steady 2h | reused |
| `duplicateNoticeActionChars` | 200 | `instructionTextChars` |
| `duplicateQuoteChars` | 60 | rationale quoted in the incumbent notice |
| `duplicateSummaryActionChars` | 120 | other plan's summary in the suggested action |
| `duplicateSummaryQuestionChars` | 40 | summary in a question body |

Detector rules on `conflict.detector_rule`, named by the strongest signal so dismissals can demote the
wording rule without silencing the issue rule: `duplicate_work.same_issue`, `duplicate_work.words`
(every wording rule, §4.2), and `duplicate_work.unsure` for an unsure verdict.

**Storage convention.** Subject **b** is always the judge's plan: the intent whose declaration or rewording
surfaced the pair. Subject **a** is the other plan. The key stays symmetric; storage order is fixed. This
keeps F8's helpers correct unchanged.

```go
pairKey := coordination.PairKey("duplicate_work",
    coordination.PairSubject{Kind: "intent", UUID: otherIntent, Revision: otherWordingRevision},
    coordination.PairSubject{Kind: "intent", UUID: judgeIntent, Revision: judgeWordingRevision})

// Symmetric and not revision-scoped: whichever side surfaces the pair, one row.
func DuplicateWorkDedupeKey(intentA, intentB string) string // sha256("duplicate_work|" + min + "|" + max), lowercased uuids, hex
```

### 3.1 `declare_intent` and `update_intent`: the duplicate reviewer

No parameter changes. Two jsonschema texts change:

```go
ExternalRef string `json:"external_ref,omitempty" jsonschema:"The issue or ticket id this is for, if there is one. It is the highest-signal duplicate-work key there is: another live plan on this team with the same id, in any repository, is put to you to judge in this response."`
```

`UpdateIntentParams.Summary`:

```go
Summary string `json:"summary,omitempty" jsonschema:"A replacement summary, when the plan changed. Max 280 characters. Changing it bumps the intent's revision, which is what lets other agents' models take one fresh look at a plan they already judged, and a reworded summary asks you once more whether your plan duplicates another agent's."`
```

#### Writing `wording_revision`

- `declare_intent` inserts `WordingRevision: 1` (the generated field).
- `update_intent` Apply, when `summary != ""`: re-read `summary` and `external_ref` by primary key under the lock, compute `coordination.WordingChanged`, and add `wording_revision = wording_revision + ?` to the existing UPDATE (1 when changed, else 0). It sets `req.WordingChanged = true` on the shared `pathDetectionRequest` when it bumped. Read under the lock, not from `resolveIntent`, so two concurrent updates of one intent cannot both skip the bump.

#### The chain (`app/rest.go`)

```go
handler.SetDetector(metichemcp.ChainDetectors(
    metichemcp.NewPathDetector(coreImpl, logger),
    metichemcp.NewDuplicateReviewer(coreImpl, logger),
    metichemcp.NewDecisionReviewer(coreImpl, logger),
    metichemcp.NewReviewRenderer(),
))
```

- **Duplicates before decisions.** A duplicate is time-critical, so it takes the shared cap first, but at most 2 pairs per call, so one slot is always left for a decision.
- **One renderer.** `pathDetectionRequest` gains `ReviewPairs []reviewBlockPair`. Both reviewers append to it; neither writes the envelope. `NewReviewRenderer` renders `ReviewPairs` into `m.Envelope.Review` (cut order below) and appends the note lead once. For a response with only decision pairs, the bytes are identical to 9384f2a (golden test, §7.2).
- `pathDetectionRequest` also gains `OverlapSessions map[string]string` (other session uuid → overlap path), filled by the path detector's loop for each recorded overlap; `ExternalRef string`; `ParentSessionUUID string`; `WordingChanged bool`.

#### When the duplicate reviewer runs

- `intent_declared`: always.
- `intent_updated`, not terminal, with `req.WordingChanged`: yes.
- Anything else: return immediately. A path-only or status-line update mints nothing and leaves pending duplicate pairs answerable.

#### Steps (inside Detect, under the lock, every query bounded)

1. **The caller's plan**, by primary key: `summary`, `external_ref`, `status`, `kind`, `wording_revision`, the project's `key` and `case_insensitive_paths`. Not declared or active → return.
2. **On a rewording, expire the other side's stale pairs** (pairs other sessions were asked about this plan's old wording):
   ```sql
   UPDATE `judgement` SET `status` = ?, `updated_at` = ?
   WHERE `team_uuid` = ? AND `subject_a_uuid` = ? AND `kind` = ? AND `status` = ?
   ```
   (`idx_judgement_subject`.) The caller's own stale pairs (subject b) stay pending: `report_judgement` refuses them as stale and the sweeper expires them, exactly as decisions do.
3. **same_issue**, when `external_ref` is set. Team-wide, `idx_intent_external_ref`:
   ```sql
   SELECT i.`id`, i.`key`, i.`summary`, i.`wording_revision`, i.`session_uuid`, i.`kind`, COALESCE(s.`parent_session_uuid`, '')
   FROM `intent` i JOIN `session` s ON s.`id` = i.`session_uuid`
   WHERE i.`team_uuid` = ? AND i.`external_ref` = ? AND i.`status` IN (?, ?) AND i.`expires_at` > ?
     AND s.`status` IN (?, ?) AND i.`session_uuid` <> ?
   LIMIT 8
   ```
4. **words**, this project only. `loadLiveIntents` (extended to return `wording_revision`, `kind` and the session's `parent_session_uuid`), `idx_intent_live`, LIMIT 100, excluding the caller's session. Then in Go: `coordination.DuplicateTermsOf` for the caller and each candidate; `coordination.FrequentTerms` over all of them; `coordination.ScoreDuplicate` for each, with `pathsOverlap = req.OverlapSessions[candidate.session] != ""`.
4b. **Re-judge an open conflict, whatever the score** (rewordings only). Every open or acknowledged `duplicate_work` conflict this plan is in (`OpenDuplicateConflictsOfSession`'s two paths plus this intent, capped at `settleMaxConflicts`) names its other plan in the `plans:` fact; each such plan that is still declared or active, with its session live or stale, is a **re-judge candidate** and gets a pair at the new wording even when the new summary no longer scores against it. A genuine re-scope ("build the login screen" → "add password strength meter") stops resembling the other plan, and without this pair nothing but a plan ending could close the conflict (§4.7 rule 5). Plans with no open duplicate conflict keep the score gate exactly as before. Re-judge candidates skip steps 5 and 6 (they were paired once already) but not the pinned and existing-key checks.
5. **Drop:** the caller's own session (already excluded); a supervisor/subagent pair (`candidate.session == caller.parent` or `candidate.parent == caller.session`; siblings are kept, because two subagents on one thing is a real duplicate); candidate intents of kind `hold`; pairs pinned at any revision, either order:
   ```sql
   SELECT `subject_a_uuid`, `subject_b_uuid` FROM `judgement`
   WHERE `team_uuid` = ? AND `kind` = ? AND `pinned` = 1
     AND ((`subject_a_uuid` = ? AND `subject_b_uuid` IN (…)) OR (`subject_a_uuid` IN (…) AND `subject_b_uuid` = ?))
   LIMIT ?
   ```
   and pairs whose exact key already exists in any status (`existingPairKeys`).
6. **Rank** with `coordination.RankDuplicateCandidates`: same_issue 3 > words_and_paths 2 > words 1, then core shared keys descending, then shared keys, then intent key ascending. Re-judge candidates go **first**, in conflict order, and are all kept; scored candidates fill what is left of `duplicateMaxPairsPerCall`. So under the per-minute cap a re-judge pair is assigned before any new candidate and is never starved into the backlog behind one.
7. **Insert**, a = candidate, b = caller, both at `wording_revision`, `kind = duplicate_work`, `status = pending`. **Assigned** to the caller (`judging_expires_at = now + window`, `assignment_count = 1`) while `reviewsAssignedRecently` is under the cap; **otherwise unassigned** (backlog; the sweeper assigns it to subject b's session). Staleness is by `wording_revision`, so a backlogged pair can never be judged against old wording.
8. **Append** a `reviewBlockPair` for each inserted and assigned pair. A re-judge pair's `why` is `open_conflict`.

#### The review block (shared with decisions)

`reviewBlockPair` gains four fields, all `omitempty`, so decision pairs keep their bytes:

```go
type reviewBlockPair struct {
	PairKey   string `json:"pair_key"`
	Kind      string `json:"kind,omitempty"`      // "duplicate_work"; omitted on decision pairs
	Decision  string `json:"decision,omitempty"`  // decision pairs
	Statement string `json:"statement,omitempty"` // decision pairs
	Plan      string `json:"plan,omitempty"`      // duplicate pairs: the other plan's key
	With      string `json:"with,omitempty"`      // duplicate pairs: describeHolder of its owner
	Summary   string `json:"summary,omitempty"`   // duplicate pairs: the other plan's summary
	Why       string `json:"why"`                 // decisions: scope|always_show|words; duplicates: same_issue|words_and_paths|words|open_conflict
}
```

`Decision` becomes `omitempty`; it is always set on decision pairs, so their bytes do not change.

```json
{"pairs":[
  {"pair_key":"5b1e…","kind":"duplicate_work","plan":"INT-83","with":"Ana (ui)","summary":"add login page","why":"words"},
  {"pair_key":"9c1e…","decision":"#auth-jwt-cookie","statement":"Sessions are a signed JWT in an httpOnly, Secure cookie. Never store tokens in localStorage or sessionStorage.","why":"scope"}],
 "more":0,"answer_with":"report_judgement"}
```

- Order: duplicate pairs first, then decision pairs.
- **Cut order** at `reviewInlineChars` (1200): drop every `statement` and `summary`; then drop pairs from the end, counted in `more`; then drop the block.
- `review_block_enabled = false`: pairs are still assigned; only the block is omitted.
- **Note**, appended once by the renderer when pairs were assigned, unchanged text: `; judge %d pair(s) against your plan before you edit: see review, then report_judgement`, or `…: call get_review_context, then report_judgement` when there is no block.
- **Replay.** Everything happens in Detect, so the block and the pair rows are in the stored snapshot, and a replayed declaration returns the same pair keys.

### 3.2 `get_review_context`

**Annotation: readOnly.** Params, limits, budget (2200) and errors are unchanged (DECISIONS §3.2).

Description:

> Read the pairs metiche has asked you to judge: your own plan next to a recorded team decision, or next to another agent's live plan that may be the same work, with why they were paired. Call it when pending.reviews is above zero, or when a response's review block did not carry everything you need, then answer each pair with report_judgement. Read-only: it changes nothing and can be called again. metiche never judges anything itself — your model does.

Struct changes (all additions `omitempty`, so decision items keep their bytes):

```go
type ReviewItem struct {
	PairKey    string          `json:"pair_key"`
	Kind       string          `json:"kind"`     // "decision_contradiction" | "duplicate_work"
	Question   string          `json:"question"`
	Why        []string        `json:"why"`
	Decision   *ReviewDecision `json:"decision,omitempty"`
	Plan       ReviewPlan      `json:"plan"`            // always the caller's own plan
	Other      *ReviewPlan     `json:"other,omitempty"` // duplicate_work: the other agent's plan
	AnswerWith string          `json:"answer_with"`
	ExpiresAt  *string         `json:"expires_at,omitempty"`
}

type ReviewPlan struct {
	Key         string   `json:"key"`
	Summary     string   `json:"summary"`
	Paths       []string `json:"paths"` // at most 8 held paths
	Revision    int64    `json:"revision"`
	Who         string   `json:"who,omitempty"`          // other plan only: describeHolder
	SessionKey  string   `json:"session_key,omitempty"`  // other plan only
	Status      string   `json:"status,omitempty"`       // other plan only: declared | active
	ExternalRef string   `json:"external_ref,omitempty"` // when set
}
```

`judgementRow` fields `DecisionID`/`DecisionRev`/`IntentID`/`IntentRev` are renamed
`SubjectAID`/`SubjectARev`/`SubjectBID`/`SubjectBRev`; `buildReviewItem` dispatches on `j.Kind`.

For `duplicate_work`:
- `question`: `Would carrying out %s build the same thing %s (%s) is already building — the same change, not just the same area?` (caller's key, other key, who). The criterion is in the question on purpose, against the yes-bias.
- `why`, exactly, in this order when they apply:
  - `same issue: %s`
  - `your summaries share words: %s` (the caller's own words for up to 4 shared keys, comma-joined; §4.2)
  - `you also claim overlapping files: %s` (first overlap of the two plans' held write paths)
  - `the duplicate conflict %s between these plans is still open: judge your plan as it is worded now` (the conflict key, when the pair's conflict is open or acknowledged; this is the why of a re-judge pair, §3.1 step 4b)
- `revision` is the intents' `revision` for display; `wording_revision` is never shown.

```json
{"ok":true,"key":"S-22","sequence":515,"revision":97,"pending":{"instructions":0,"conflicts":0,"reviews":1},
 "note":"1 pair(s) to judge: read each, then report_judgement",
 "reviews":[{"pair_key":"5b1e…","kind":"duplicate_work",
   "question":"Would carrying out INT-92 build the same thing INT-83 (Ana (ui)) is already building — the same change, not just the same area?",
   "why":["your summaries share words: login, screen"],
   "plan":{"key":"INT-92","summary":"build the login screen","paths":["web/src/pages/Login.tsx"],"revision":1},
   "other":{"key":"INT-83","summary":"add login page","paths":["web/src/routes/login.tsx"],"revision":1,
     "who":"Ana (ui)","session_key":"S-17","status":"active"},
   "answer_with":"report_judgement(pair_key, verdict, confidence, severity, rationale)",
   "expires_at":"2026-09-21T14:20:00Z"}]}
```

### 3.3 `report_judgement`

**Annotation: idempotent.** Params unchanged; idempotency key unchanged:
`judgement_reported:<judgement uuid>:<verdict>`.

Description:

> Give your verdict on a pair from get_review_context or a review block: would your plan, as written, break the recorded decision — or build the same thing the other agent's plan is already building? 'no_conflict' is the usual answer; 'conflict' only when doing the plan would break the statement, or would produce the same change as the other plan (the same area is not enough); 'unsure' when what you were shown does not settle it. Include an honest confidence and a one-line rationale; both are shown on the board. A conflict comes back in conflicts[] with what to do. Change your plan and update_intent, and you will be asked once more; a no_conflict then settles it. Safe to retry: the same verdict on the same pair returns the first answer.

jsonschema texts (the other fields are unchanged):

```go
Verdict   string `json:"verdict" jsonschema:"'conflict' if carrying out your plan as written would break the decision, or would build the same thing the other plan is already building; 'no_conflict' if not (the usual answer: unrelated, compatible, or only the same area); 'unsure' if what you were shown does not settle it."`
Severity  string `json:"severity,omitempty" jsonschema:"For a conflict: 'low', 'medium' (default) or 'high': how much breaks, or how much work is wasted, if you go ahead. Ignored otherwise."`
Rationale string `json:"rationale,omitempty" jsonschema:"One line naming what meets what: 'plan stores the token in localStorage; #auth-jwt-cookie forbids it', or 'both build the login page; INT-83 already has the route'. Max 400 characters. Required for conflict and unsure. Shown on the board; no secrets."`
```

Validation outside the lock is unchanged. The judgement is read before the lock (as today), and the
Mutation is built per kind:

| | decision_contradiction | duplicate_work |
|---|---|---|
| `SubjectKind` | decision | intent |
| `SubjectUUID` / `SubjectKey` | the decision | subject a (the other plan) and its key |
| `Payload.IntentUUID` | the plan | subject b (the judge's plan) |
| `Payload.Paths` | [decision key] | [subject a's key] |
| summary | `%s judged %s against %s: %s` (label, plan, decision, verdict words) | `%s judged %s against %s: %s` (label, b key, a key, verdict words) |

#### Apply for `duplicate_work` (under the lock)

1. The caller's session is workable (re-read).
2. The judgement is `pending`, or `judged` with the same verdict (falls through to commit's replay).
3. Both intents exist and are declared or active; each one's `wording_revision` equals its stored subject revision.
4. Subject a's session is live or stale.
5. `UPDATE judgement SET status = judged, verdict, severity (conflict only), confidence, rationale, judged_at, updated_at WHERE id = ? AND status = pending`, exactly as decisions.

| When | Message |
|---|---|
| a plan was reworded | `%s was reworded after this pair was made: call get_review_context for the current pair` (the reworded intent's key) |
| a plan ended | `%s is %s; nothing to judge` (intent key, status) |
| the other session ended | `%s's session has ended; nothing to judge` (subject a's key) |
| a plan row is gone | `this pair's plan no longer exists; nothing to judge` |

Every other error of DECISIONS §3.3 applies unchanged.

#### Detect for `duplicate_work` (`detectDuplicateVerdict`)

`detectReportJudgement` gains the case; the event fields are set per the table above.

- **`no_conflict`.** If the conflict with `DuplicateWorkDedupeKey(a, b)` is open or acknowledged, settle it with `DuplicateReleaseJudged` (§4.7). Otherwise nothing more.
- **`unsure`.** Raise or reopen at `low`, rule `duplicate_work.unsure`. No notice; no `conflicts[]` entry.
- **`conflict`.** Raise or reopen, rule `duplicate_work.same_issue` when the two share an `external_ref`, else `duplicate_work.words`; severity from `coordination.DuplicateSeverity` (§4.4).

Raising or reopening, following `raiseDecisionConflict`:

- **conflict row on insert:** `key = conflictKey(tc, 0)`, `kind = duplicate_work`, `dedupe_key`, `severity`, `status = open`, `detected_by = agent`, `detector_rule`, `confidence`, `evidence`, `suggested_action` (the yield side's action, §4.10), `suggested_yield_session_uuid` = subject b's session, `suggested_yield_reason` = `same work as %s` (a's key, ≤ 80), `project_uuid` = b's project, `occurrence_count = 1`, first and last detected now.
- **evidence:**
  - `overlap_path` = the shared `external_ref` when there is one, else the first overlapping held write path of the two plans, else null;
  - `a_label` = `describeHolder(a's owner)`, `a_summary` = a's summary, `a_pattern` = a's first held write path or null;
  - `b_label`, `b_summary`, `b_pattern` likewise for b;
  - `adjusters`, typed facts in this order: `plans:<a key>,<b key>` (always first), `same_issue:<ref>` when shared, `words:<caller's words for the shared keys, comma-joined>` when the wording matched, `paths:<overlap>` when the claims overlap, `same_member_concurrent` when both plans belong to one person;
  - `field_issues` = [`%s's model %s: %s`] (b's session key, `(%.2f)` or `(unsure)`, rationale);
  - `detail` = rule.
- **existing open or acknowledged row:** bump `occurrence_count`, set `last_detected_at`, `severity`, `evidence`, `suggested_action`, `detector_rule`, `confidence`, and `suggested_yield_session_uuid` = this judge's session (the side that changed last is responsible).
- **existing row resolved by metiche** (`resolved_by_member_uuid` NULL): reopen; clear resolution fields, `notified_at`, `max_severity_notified`, `escalated_at`; bump the count. It is news again.
- **existing row resolved or dismissed by a person:** silenced; count only.
- **`judgement.conflict_uuid`** = the conflict id.
- **participants:** subject b's session only (`subject_kind = intent`, `subject_uuid` = b, role `initiator`). Subject a's session is attached when it is told (§4.5), because attaching it is itself an interruption (F10).
- **no instruction** is written here. The incumbent's notice is the sweeper's (§4.5).
- **caller:** when severity ≥ `medium` and not silenced, append a `ConflictNotice` and stamp the caller's participant `notified_at`.

#### Envelope

`ConflictNotice` gains one field:

```go
// DuplicateOf is set only on a duplicate_work conflict: the other plan's key.
// AtFault is then always "later".
DuplicateOf string `json:"duplicate_of,omitempty"`
```

```json
{"ok":true,"key":"CF-44","sequence":516,"revision":98,"pending":{"instructions":0,"conflicts":1,"reviews":0},
 "note":"judged INT-92 against INT-83: same work (0.85) — read conflicts[] before you edit",
 "conflicts":[{"key":"CF-44","kind":"duplicate_work","severity":"medium","with":"Ana (ui)","duplicate_of":"INT-83",
   "at_fault":"later",
   "suggested_action":"INT-83 (Ana (ui), active 4m) is already building this: \"add login page\". Stop before you edit: mark INT-92 superseded with update_intent, or re-scope it to a different part and update_intent the summary, which asks you to judge once more. If your plan should be the one that continues, settle that with Ana's agent (S-17) first."}]}
```

- `Envelope.Key` is the conflict key when a conflict was raised, reopened or settled, else the caller's intent key.
- `With` is `describeHolder` of subject a's owner.

| Verdict | Note |
|---|---|
| no_conflict | `judged %s against %s: not the same work%s` (b key, a key, `confidencePart`), plus `; %s settled` when a conflict closed |
| conflict ≥ 0.7 | `judged %s against %s: same work%s — read conflicts[] before you edit` |
| conflict < 0.7 | `judged %s against %s: same work%s, recorded low on the board; below 0.7 it interrupts nobody` |
| unsure | `judged %s against %s: unsure, recorded low on the board` |
| silenced | `judged %s against %s: same work%s; a person already settled this one, so it stays closed` |

#### Events

- One `judgement_reported`, fields per the table above. `Payload.ConflictUUID` when there is one, `Severity`, `Detail` = verdict, `Message` = sanitized rationale.
- `Structural: true` exactly when a conflict was raised or reopened; otherwise false (PLAN's "one tiny event").
- A settlement adds its own structural `conflict_resolved`.

### 3.4 Changes to existing tools and files

| File | Change |
|---|---|
| `app/mcp/intents.go` | declare inserts `WordingRevision: 1`; update Apply re-reads and bumps `wording_revision` by `coordination.WordingChanged` and sets `req.WordingChanged`; `pathDetectionRequest` gets `ExternalRef` and `ParentSessionUUID` from the tool. On a terminal update, after the decision settle: `ExpireDuplicatePairsOnIntents(team, [intent])` (subject a side; the existing `expireIntentJudgements` already covers subject b), then settle `OpenDuplicateConflictsOfSession` with `DuplicateReleaseIntentEnded` |
| `app/mcp/sessions.go` | `EndSession` Apply, after the decision settle: read the session's declared/active intent ids (LIMIT 64), `ExpireDuplicatePairsOnIntents`, then settle with `DuplicateReleaseSessionEnded`. `ExpireJudgementsOfSession` already covers pairs this session was judging |
| `app/mcp/detector.go` | `pathDetectionRequest` fields (§3.1); the detect loop records `req.OverlapSessions[o.Theirs.SessionUUID] = o.Verdict.OverlapPath` for each recorded overlap |
| `app/mcp/decisionreview.go` | append to `req.ReviewPairs` instead of writing the envelope and the note; `reviewBlockPair` fields; `renderReviewBlock` also drops `Summary` in its first cut |
| `app/mcp/reviewrender.go` (new) | `NewReviewRenderer` |
| `app/mcp/reviewcontext.go` | `ReviewItem.Other`, `ReviewPlan` fields, `judgementRow` rename, dispatch |
| `app/mcp/reportjudgement.go` | per-kind Mutation, Apply dispatch, `detectDuplicateVerdict` |
| `app/mcp/recorddecision.go` | the two tool descriptions (§3.2, §3.3); `loadLiveIntents` returns `wording_revision`, `kind`, parent session |
| `app/mcp/envelope.go` | `ConflictNotice.DuplicateOf` |
| `app/mcp/conflictresolve.go` | `SettledConflict.PlanKeys [2]string`; `ConflictResolvedSummary` duplicate case (§4.10) |
| `app/mcp/server.go` | `serverInstructions` paragraph (§6.2) |
| `app/rest.go` | the chain (§3.1) |
| `app/webapi/conflicthistory.go` | `liveConflictKinds` gains `duplicate_work` |

---

## 4. Detection, severity, settlement

### 4.1 What the server decides, and what it never decides

**Deterministic, on the server:** candidacy (issue id team-wide; wording within the project; a path
overlap only lowers the wording bar, never makes a candidate by itself); the exclusions; the rate cap;
staleness by `wording_revision`; severity adjusters; fault; the notify floor, the notice grace and the
human floor; settlement and escalation.

**Never on the server:** whether two plans are the same work or merely related. That includes a shared
issue id: agents on one ticket often split it. Every candidate goes to the judge's own model, with the
criterion ("the same change, not just the same area") in the question. PLAN:251's yes-bias is guarded by
the question, the 0.7 floor and the required rationale. The judge is also the side asked to yield, which
biases it the other way, towards `no_conflict`; as for decisions (DECISIONS §10.2), the mitigation is
visibility: verdict, confidence and rationale are on the board.

**Path overlap is its own kind.** It settles when the paths stop overlapping; a duplicate settles when a
plan yields or is re-scoped (§4.6).

### 4.2 Wording: the signal that is the whole feature in a hackathon

A hackathon summary is three to six informal words with no issue id: "add login page", "build the login
screen", "stripe checkout", "docker setup". Raw token overlap fails on exactly these: after stopwords,
"add login page" and "build the login screen" share one word. So the comparison runs on **concept keys**,
built by `coordination.DuplicateTermsOf(summary, projectKey)`, pure:

1. **Join phrases** that split into stopwords, matched case-insensitively on whole words across a space, hyphen, underscore or nothing (`SignIn`), before `Tokenize` lowercases (so camelCase still splits): `sign in`→`signin`, `sign up`→`signup`, `sign out`→`signout`, `log in`→`login`, `log out`→`logout`, `set up`→`setup`, `check out`→`checkout`; and `github actions` / `github workflow(s)` / `gh actions`→`githubci`, `socket.io`→`socketio`, `real time`→`realtime`. Bare `actions` and `workflow` are not joined or folded: too common outside CI.
2. **`coordination.Tokenize(text, 32)`**: splits identifiers, drops single characters, numbers and the shared stopwords (which already include `add`, `new`, `make`, `use`), folds plurals.
3. **Drop the duplicate stoplist**: task verbs that name no deliverable — `build`, `create`, `implement`, `write`, `wire`, `hook`, `fix`, `update`, `handle`, `support`, `improve`, `refactor`, `clean`, `cleanup`, `setup`, `set`, `get`, `start`, `finish`, `do`, `try`, `quick`, `initial`, `first`, `basic`, `simple`, and the forms of fix that `Tokenize` does not fold to it: `fixe` (from `fixes`), `fixes`, `fixed`, `fixing`. Also drop the tokens of the project key (a repo called `recipe-share` says `recipe` in every summary); the project key gets the same phrase joins.
4. **Fold synonyms** to one canonical word:

   | canonical | folded from | layer |
   |---|---|---|
   | `page` | page, screen, view, ui, frontend, form, modal, dialog | ui |
   | `endpoint` | endpoint, route, api, apis, handler, backend, server, controller | api |
   | `schema` | schema, table, migration, database, db, sql | data |
   | `login` | login, signin, logon | |
   | `signup` | signup, register, registration | |
   | `logout` | logout, signout | |
   | `auth` | auth, authentication, authn | |
   | `image` | image, img, picture, photo | |
   | `button` | button, btn | |
   | `reset` | reset, forgot, recover, recovery | |
   | `ci` | ci, cd, cicd, pipeline, githubci (step 1) | |
   | `theme` | theme, mode, darkmode | |
   | `toggle` | toggle, switcher | |
   | `realtime` | realtime, websocket, socket, socketio, live, pubsub | |

   (`apis` is listed because `Tokenize` does not fold a plural after `i`. The last five rows were added on 2026-09-21 to close misses found by the §7.1 vectors; each has should-not-match vectors guarding it.)

5. **Comparison key** = the first `DuplicateKeyRunes` (5) runes of the canonical word. This matches word forms the plural fold misses (`summary`/`summarize` → `summa`, `deploy`/`deployment` → `deplo`, `notify`/`notification` → `notif`, `websocket` → `webso`) at the cost of rare false matches (`product`/`production`) that the judge then answers.
6. **Vague keys** stay in the set but never count as shared and block the single-concept rule: `test`, `bug`, `issue`, `error`, `feature`, `stuff`, `thing`, `code`, `work`, `task`, `part`, `flow`, `demo`, `mvp`, `app`, `user`, `style`, `lint`, `typo`, `doc` (plurals are folded first, so `docs` is `doc`). Vague words are matched on their 5-rune key, after folding: `styles` is vague, `styling` (`styli`) is not.
7. De-duplicate, first-seen order. Keep, per key, the first original word the summary used, for the `why` line.

**Frequency filter** (`coordination.FrequentTerms`). Only when the scan holds at least
`DuplicateFrequencyMinScanned` (8) summaries, caller included: a key present in at least
max(`DuplicateFrequencyMinCount` (4), ⌈n/3⌉) of them is dropped from every comparison. A hackathon project
with five live plans never filters, so two plans both saying "login" are never hidden by it; a sprint
project with thirty plans stops matching on its domain word.

**The rules** (`coordination.ScoreDuplicate(mine, theirs, drop, pathsOverlap)`), with M and T the two key
sets after the filter:

- **Layer guard.** If both sides name a layer and their layer sets are disjoint (a `page` against an `endpoint`, a `page` against a `schema`), not a candidate. That is a producer and a consumer, which is contracts' job, not duplication. The guard reads each plan's layers **before** the frequency filter: a layer is what a plan is, not a similarity signal, so a project where `page` is frequent still does not pair "login page" with "login endpoint".
- S = shared keys that are not vague. C = those of S that are not layer words (the *core*). m = the smaller count of non-vague keys. m = 0 is never a candidate.
- **words:** C ≥ 1, S ≥ 2 and S/m ≥ 0.6.
- **words (long):** C ≥ 3 and S/m ≥ 0.4.
- **words (single concept):** C = 1 and one side's whole key set, vague keys included, is exactly that key, and the other side has at most 2 non-vague keys. ("docker setup" and "set up docker compose".)
- **words (same layer set):** C = 0, S ≥ 1, and the two sides' non-vague key sets are identical. ("set up the database" and "database schema and migrations" are both `{schem}`.) Sets that merely share a layer word ("login page" `{login, page}` against "signup page" `{signu, page}`; "seed the database" `{seed, schem}` against "database schema" `{schem}`) never fire.
- **words_and_paths:** the two sessions' claims overlap (`req.OverlapSessions`), C ≥ 1 and S/m ≥ 0.5.
- Otherwise not a candidate.

The first rule that fits names the `why` (`words` or `words_and_paths`; `ScoreDuplicate` reports `words`, `words_long`, `words_single` or `words_layer`, all of which are the `words` why); an issue match is `same_issue`
whatever the wording. The detector rule on a conflict is `duplicate_work.words` for every wording rule.

**Worked examples** (all are §7.1 test vectors):

| Summary A | Summary B | Keys A | Keys B | Result |
|---|---|---|---|---|
| add login page | build the login screen | login, page | login, page | **candidate** (words: C 1, S 2, 2/2) |
| login form | sign in page | login, page | login, page | **candidate** |
| dark mode toggle | add dark mode | dark, theme, toggl | dark, theme | **candidate** |
| stripe checkout | checkout flow with stripe | strip, check | check, flow*, strip | **candidate** |
| docker setup | set up docker compose | docke | docke, compo | **candidate** (single concept) |
| login page | login endpoint | login, page | login, endpo | not: layers ui vs api |
| signup page | login page | signu, page | login, page | not: C 0 |
| add tests for auth | auth middleware | test*, auth | auth, middl | not: S 1; single blocked by the vague key |
| set up the database | database schema and migrations | schem | schem | **candidate** (same layer set) |
| password reset flow | forgot password page | passw, reset, flow* | reset, passw, page | **candidate** (words: C 2, S 2, 2/2) |

(* = vague)

### 4.3 Who judges, and who is at fault

- **Judge:** the session whose call surfaced the pair: the later declarer, or the side that just reworded. It is the plan's own agent, it has both plans in hand in this very response, and it has not started. Pairs are only ever assigned or re-armed to subject b's session.
- **Fault:** `at_fault: "later"`; the yield session is subject b's. PLAN:241-243: "ordering assigns responsibility, not permission". When the incumbent rewords and judges a conflict, it becomes subject b of the new pair and the yield side (the side that changed last).
- The suggested action always offers the other way out: settle with the other agent first if this plan should be the one that continues.

### 4.4 Severity (pure: `coordination.DuplicateSeverity`)

| Step | Rule |
|---|---|
| 1 | `unsure` → `low`. Stop. |
| 2 | `conflict` with confidence < 0.70 → `low`. Stop. |
| 3 | The requested severity, default `medium`, clamped to [`low`, `high`]. An agent verdict never reaches `critical`. |
| 4 | The two plans share an `external_ref` → floor `medium`. |
| 5 | **No same-member softening.** Both plans are live by construction, and one person's two live agents duplicating each other is solo fan-out, exactly who needs telling (F11). `same_member_concurrent` is recorded in the evidence. |

- **Notify floor:** `medium`. Below it the conflict is on the board and nobody is interrupted.
- **Human floor:** `team_settings.human_notify_floor`, default `high`.
- **Demoted rule:** when `team_settings.demoted_rules` contains the conflict's detector rule, the severity is `low` (record-only). This is the hook `resolve_conflict`'s auto-demotion will use, and a working kill switch today: add `duplicate_work.words` to silence the wording signal on a team.

### 4.5 Notices: the incumbent hears only if it is still standing

| Who | How |
|---|---|
| The judge | the inline `review` block; `pending.reviews` on every response, a bare heartbeat included |
| The judge, on a conflict | `conflicts[]` in its own `report_judgement` response |
| The incumbent (subject a) | after the grace, and only if the conflict is still open: a `conflict_notice`, and from then on `pending.conflicts` as a participant |
| Escalation | a `question` to both sides (§4.8) |

**Why the grace (§10.2).** A judge told "stop before you edit" usually yields on its next call. If it
does, the incumbent has nothing to do, and telling it would be the one interruption with no action behind
it. So the incumbent is told only when the duplicate outlives
`coordination.DuplicateNoticeGrace(cadence) = EscalationBudget(cadence)/5`: hackathon 2m, sprint 6m,
steady 24m.

**The sweeper pass** (`noticeDuplicateIncumbents`, per team, after the pair pass):

```sql
SELECT c.`id` FROM `conflict` c
WHERE c.`team_uuid` = ? AND c.`status` IN (?, ?) AND c.`severity` >= ? AND c.`kind` = ?
  AND (c.`notified_at` IS NULL OR c.`max_severity_notified` < c.`severity`)
ORDER BY c.`first_detected_at` LIMIT 16
```

(`idx_conflict_open`; the floor is `medium`.) For each, through `appendEvent` with the locked `extra` hook:

1. Re-read the conflict: still open or acknowledged, still unnotified at this severity, severity ≥ `medium`.
2. `now − COALESCE(last_detected_at, first_detected_at) ≥ DuplicateNoticeGrace(cadence of the conflict's project)`.
3. From the evidence's `plans:` fact, read both intents by `uq_intent_team_key`. Subject a must be declared or active with its session live or stale; subject b must be declared or active. Otherwise `errNothingToSay` (the settle paths close it).
4. Attach subject a's session: `subject_kind = intent`, `subject_uuid` = a, role `incumbent` (`upsertDecisionParticipant`).
5. **Fold a pending path notice.** If an instruction to a's session with `kind = conflict_notice`, `status = pending`, `ref_kind = conflict` refers to an open `path_overlap` conflict that has b's session as a participant (`idx_instruction_pending`, LIMIT 8, then a participant point read), set it `status = dismissed`, `action_note = "superseded by %s"` (this conflict's key), and name it in the new body.
6. Insert the notice: `source = server`, `kind = conflict_notice`, `ref_kind = conflict`, `ref_uuid` = conflict, `requires_report = 0`, `expires_at = now + 4h`, body `%s (%s) on %s: %s` (conflict key, severity, a's key, the incumbent action, §4.10), so `splitNoticeBody` separates the action.
7. `UPDATE conflict SET notified_at = now, max_severity_notified = severity, updated_at = now`.

Event `instruction_raised`, `Structural: true` (a lane gains a conflict badge), subject conflict, summary
`%s: %s told about %s` (conflict key, a's agent label, b's key), payload `conflict_uuid`, `severity`,
`instruction_uuid`, `message` = the body. Idempotency key
`sweep:duplicate_notice:<conflict id>:<occurrence_count>:<severity int>`.

A reopened conflict has `notified_at` cleared, so it waits a fresh grace; a severity rise re-notifies
once.

### 4.6 Path overlap and duplicate work together

They are two rows, and that is right: different dedupe keys (the claim pair against the intent pair),
different settlement. Interruptions:

- **The judge** sees the path conflict in `conflicts[]` and the duplicate pair in `review` in the same response, then its own verdict's `conflicts[]`. Its own calls, not interruptions.
- **The incumbent** gets the path notice immediately (unchanged). If the duplicate is still standing after the grace and that path notice is still unread, the duplicate notice replaces it (§4.5 step 5): one interruption. If the path notice was already read, the duplicate notice is a second one, carrying news the first did not ("they were building the same thing, and were told to stop").
- A path overlap between the two sessions lowers the wording bar (words_and_paths) and never makes a candidate alone: a path overlap is common and already handled; asking "is it the same thing?" for every one would double the cost of every collision.

### 4.7 Settlement

On the transaction that holds the team lock (or inside `appendEvent` for the sweeper). Each is a
conditional `UPDATE conflict SET status = resolved, resolution, resolution_note, resolved_at WHERE id = ? AND status IN (open, acknowledged)`
plus one structural `conflict_resolved` (`appendConflictResolvedEvent`). `resolved_by_member_uuid` stays
NULL.

`decideDuplicateSettlement` is pure. b is the conflict's yield side (`suggested_yield_session_uuid`'s
intent per the `plans:` fact), a the other. First rule that fits wins:

| # | State | Resolution |
|---|---|---|
| 1 | either intent is abandoned or superseded, or its row is gone | YIELDED |
| 2 | b is done | SUPERSEDED (it finished anyway; the work was duplicated) |
| 3 | b's session is ended or abandoned, or gone | SUPERSEDED |
| 4 | a's session is ended or abandoned, or gone, and a is not done | SUPERSEDED (nobody is left to duplicate) |
| 5 | the latest judged verdict on the unordered pair is `no_conflict` | CONVERGED |
| – | anything else, **including a done while b is still live** | stays open |

- Rule 5 reads `idx_judgement_subject` in both orders, `kind = duplicate_work AND status = judged ORDER BY judged_at DESC LIMIT 1` each, and takes the later.
- "a done while b live" stays open on purpose: b is rebuilding finished work, and escalation may rightly fire.
- A chosen resolution becomes **COORDINATED** when a participant answered this conflict's notice with `report_back` done or acknowledged and a note (`loadSettleNotes`), unless the release is `DuplicateReleaseSessionAbandoned`.

| Release | Called from |
|---|---|
| `DuplicateReleaseJudged` | `report_judgement` no_conflict |
| `DuplicateReleaseIntentEnded` | `update_intent` terminal, either side |
| `DuplicateReleaseSessionEnded` | `end_session`, either side |
| `DuplicateReleaseSessionAbandoned` | sweeper, beside `settleDecisionConflicts` in `settleAfterAbandon` |

**Finding the conflicts of a session** (`OpenDuplicateConflictsOfSession`): the incumbent may not be a
participant yet, so it is the union, de-duplicated, of (a) the session's participant rows on open or
acknowledged `duplicate_work` conflicts (`idx_participant_session`), and (b) conflicts reached from
`judgement` rows with `subject_a_uuid` in the session's declared or active intents (≤ 64),
`kind = duplicate_work`, `conflict_uuid IS NOT NULL` (`idx_judgement_subject`), then filtered to open or
acknowledged by primary key. Capped at `settleMaxConflicts`.

A rewording never settles anything by itself. While the pair's duplicate conflict is open, a rewording of either plan **always** earns a re-judge pair against the other plan, whatever the new wording scores (§3.1 step 4b), assigned to the reworded plan's own session ahead of new candidates under the cap. That verdict converges the conflict (rule 5) or keeps it; a conflict verdict makes the reworded plan the yield side (§4.3).

### 4.8 Asking a person — only when the agents did not settle it

The decisions escalation step (DECISIONS §4.7) is extended to `kind IN (decision_contradiction, duplicate_work)`,
same query, same `ShouldEscalate`, same budgets, same "once" guard and idempotency key. For
`duplicate_work`:

- **intent live** means both intents declared or active and both sessions live or stale.
- `openSince` = GREATEST(COALESCE(`last_detected_at`, `first_detected_at`), MAX(participant `notified_at`)).
- Questions (§4.10) to subject b's session and, when attached and live, subject a's.
- Event `conflict_escalated`, summary `%s: a person was asked about %s` (conflict key, `<b key> / <a key>`).

### 4.9 Reopen, silence, and room for `resolve_conflict`

- **Reopen:** a new `conflict` or `unsure` verdict on the same dedupe key after metiche closed it (§3.3).
- **Silence:** a conflict a person resolved or dismissed is only counted.
- **Pinned:** the reviewer never mints a pair whose two intents have a `pinned` duplicate judgement at any revision, in either order.
- **`resolve_conflict`** will set `resolved_by_member_uuid`, `dismiss_reason` and pin the pair's judgements (both orders, `idx_judgement_subject`), and count dismissals per rule (`duplicate_work.words` apart from `duplicate_work.same_issue`). It needs nothing else from this design.

### 4.10 Exact wording

`{a}`, `{b}` are intent keys. `{who}` is `describeHolder` of a's owner; `{member}` a display name.
`{status}` is `declared` or `active`; `{age}` is `humanAge` since a was declared. `{summary…}` is
`clip(a summary, 120)`. `{aSession}`, `{bSession}` are session keys. A session name is `settleSide.name()`,
like `S-22 (ui)`. `{time}` is `clock()`. Everything goes through `clip` to its column or field limit.

**Suggested action for the yield side** (`conflict.suggested_action`, `ConflictNotice.SuggestedAction`, ≤ 400):

| Case | Text |
|---|---|
| another member | `{a} ({who}, {status} {age}) is already building this: "{summary…}". Stop before you edit: mark {b} superseded with update_intent, or re-scope it to a different part and update_intent the summary, which asks you to judge once more. If your plan should be the one that continues, settle that with {member}'s agent ({aSession}) first.` |
| the same member | `{a} ({aSession}, your own person's other agent, {status} {age}) is already building this: "{summary…}". Stop before you edit: mark {b} superseded, or re-scope it and update_intent the summary. If yours should continue, settle it with {aSession} first.` |
| unsure | `{b} may overlap {a} ({who}): "{summary…}". Nothing to do now; if it matters, ask {member}'s agent ({aSession}) which part each of you takes, then update_intent.` |

**Incumbent notice action** (after `%s (%s) on %s: `; ≤ 200; the quoted rationale gives way first, at most
60, like `deciderAction`):
`{judge name} judged its plan {b} is the same work as yours: "{rationale…}", and was told to stop. Carry on; if its part should stay, settle the split with {bSession}.`
When a path notice was folded in (§4.5), append ` (also {path conflict key})` if it fits; otherwise omit it.

**Escalation question bodies** (≤ 200, `{summary…}` = `clip(summary, 40)`):
- yield side: `{conflict}: {b} duplicates {a} ({other member}), unsettled {budget}. Ask your person: stop {b}, or agree with {other member} who keeps it? Then report_back their answer.`
- incumbent: `{conflict}: {member}'s agent is building the same thing as your {a}, unsettled {budget}. Ask your person who keeps it: "{summary…}". Then report_back their answer.`

`{budget}` renders `10m`, `30m` or `2h`.

**Resolution notes** (`conflict.resolution_note`, ≤ 400, free text sanitized):

| Case | Note |
|---|---|
| YIELDED | `Settled by the agents: {owner} marked {intent} {status} at {time}, leaving {other} ({other owner}) to build it.` |
| SUPERSEDED, b done | `Settled by the agents: {owner} finished {b} at {time} anyway, so the work was duplicated; reconcile the two at merge.` |
| SUPERSEDED, session ended | `Settled by the agents: {owner} ended its session at {time}{outcomeSuffix}, ending {intent}.` |
| SUPERSEDED, abandoned | `Cleared by metiche: {owner} was abandoned at {time} after no heartbeat, ending {intent}.` |
| CONVERGED | `Settled by the agents: {judge} re-scoped {intent} at {time} and judged it no longer duplicates {other}: "{rationale}".` |
| COORDINATED | the sentence of what closed it, then ` {S-x} said: "{note}"` exactly as `buildResolutionNote` quotes |
| any, after escalation | append ` A person was asked at {escalated time}.` |

When the rationale is empty, the `: "{rationale}"` part is dropped.

**Timeline summary** (`ConflictResolvedSummary`, duplicate case):
`%s settled (%s): %s no longer duplicates %s` (conflict key, resolution, b key, a key).

### 4.11 Noise control, in one place

- One look per wording pair: the key is `wording_revision`-scoped, so a `no_conflict` stands until a side really rewords. Paths, status, status lines and cosmetic edits never re-ask.
- Candidacy needs concept overlap under §4.2's rules; producer/consumer pairs are cut by the layer guard; supervisor/subagent pairs and `hold` plans are never paired; the domain word is dropped by the frequency filter where there are enough plans for it to mean something.
- At most 2 duplicate pairs per call, inside the shared ≤ 3 per minute.
- Confidence < 0.7 records `low` and interrupts nobody. `no_conflict` costs one non-structural event.
- The incumbent is never interrupted by a pair, only by a conflict that outlived the grace.
- A person is asked only above the human floor (default `high`) after the budget, once per conflict.
- `demoted_rules` makes a rule record-only.

---

## 5. Board

### 5.1 Backend reads (`app/webapi`, owner B2)

- `GET /v1/teams/{slug}/conflicts`, the conflict history, the session page and run history: `conflictColumns` adds `JSON_VALUE(c.evidence, '$.a_label')`, `'$.a_summary'`, `'$.b_label'`, `'$.b_summary'` and `JSON_EXTRACT(c.evidence, '$.adjusters')`. For `duplicate_work` the wire carries `plans`, `signals`, `issue_ref` and `judge_note` (§9.3); `paths` is `[b_pattern, a_pattern]` without nulls, and `overlap_path` goes to `issue_ref` only when it is the shared issue (the `same_issue:` fact says so).
- `liveConflictKinds` gains `duplicate_work`, so the history filter offers it.
- No new route.

### 5.2 Frontend (`code/frontend`, owner A5)

- `model.Conflict` gains `Plans []ConflictPlan`, `Signals []string`, `IssueRef string`; `feed/wire.go` maps §9.3 exactly.
- `view/conflicts.templ`, duplicate cards:
  - the two plans side by side, key, who and summary, the yielding one badged `asked to stop`;
  - `why paired` chips from `signals`;
  - `what the judge said` from `judge_note` (reused);
  - the `a person was asked` badge from `escalated_at` (reused);
  - settled: the existing "how it was settled" block shows `resolution_note`.
- `view.go` `conflictLabel` already says "duplicate work"; `wording.KindDuplicate` already exists (`wording.go:87-89`).
- `state/store.go` needs nothing: `judgement_reported` is already generic, and the board refetches on structural frames.

---

## 6. Skill and server text

### 6.1 Skill 0.7.0

Both copies stay byte-identical; `plugin.json` and `marketplace.json` go to `0.7.0`.

**Tools table**, the `get_review_context` row:

```
| `get_review_context` | when `pending.reviews > 0` | read-only: your plan next to a decision, or next to another agent's plan |
```

**"Judge the pairs you are handed"**, the first paragraph becomes:

> When `pending.reviews > 0`, or a response carries `review`, metiche is asking your own model one of two questions: does your plan break a recorded decision, or is another agent's live plan the same work as yours? metiche never judges anything itself. Do it before you edit.

**New section after it:**

> ## When another agent is building the same thing
>
> A pair with `"kind": "duplicate_work"` puts your plan next to another agent's: the same issue id, or summaries that say the same thing — "add login page" and "build the login screen". Answer the narrow question: would doing your plan produce the same change theirs will? Same page, same endpoint, same fix: `conflict`. Same area, different part (the login form and the login handler): `no_conflict`, the usual answer. Give a real confidence and a rationale naming the two things that match or differ.
>
> On a conflict you declared later, so you yield. Before you edit, do one of these and say it with `update_intent`:
> - **Stop:** `status: "superseded"`, and tell your person what you found instead of redoing it.
> - **Re-scope:** take a different part and reword the `summary`. You'll be asked once more, and `no_conflict` closes it.
> - **Keep going** only after you settled it with the other agent (its session key is in `suggested_action`), and put what you agreed in your status line.
>
> Write summaries that name the thing, even in a hurry: "login page", not "frontend stuff". That one line is how two agents find out they are building the same thing.
>
> If a `conflict_notice` says another agent was about to build what you are building: carry on. If its part should stay, agree the split with its agent, then `report_back` what you agreed. Nobody is interrupted when a pair turns out to be different work.

**Worked example**, a step 3c after 3b:

> **3c. B's plan is A's plan.** A declared `INT-83` "add login page" a few minutes ago. B's declaration comes back with a pair:
>
> ```
> B → declare_intent(session_key: "S-22", summary: "build the login screen", paths: ["web/src/pages/Login.tsx"])
>     ← {"key": "INT-92", "review": {"pairs": [{"pair_key": "5b1e…", "kind": "duplicate_work", "plan": "INT-83",
>          "with": "Ana (ui)", "summary": "add login page", "why": "words"}]}, "pending": {"reviews": 1}}
> B → report_judgement(session_key: "S-22", pair_key: "5b1e…", verdict: "conflict", confidence: 0.85,
>       rationale: "both build the login page; INT-83 already has the route")
>     ← {"conflicts": [{"key": "CF-44", "kind": "duplicate_work", "duplicate_of": "INT-83", "at_fault": "later", …}]}
> B → update_intent(session_key: "S-22", intent_key: "INT-92", summary: "wire the login page to POST /api/login")
>     ← {"pending": {"reviews": 1}}
> B → report_judgement(session_key: "S-22", pair_key: "7ad0…", verdict: "no_conflict", confidence: 0.9,
>       rationale: "now the API call behind A's page, not the page")
>     ← {"note": "judged INT-92 against INT-83: not the same work (0.90); CF-44 settled"}
> ```
>
> A was interrupted zero times: B changed course inside the grace.

**Quick reference, Do**: add `Answer a duplicate-work pair on the narrow question — same change, not same area — and yield before you edit when you declared later.`

### 6.2 Server instructions (`serverInstructions`, after the decisions paragraph)

```
A review pair of kind duplicate_work puts your plan next to another agent's live plan: answer whether doing yours
would build the same thing theirs is building (the same area is not enough). On a conflict you declared later, so
stop (update_intent status superseded) or re-scope and reword the summary before you edit. Name the thing in every
summary, even a hurried one: that line is how two agents find out they are building the same thing.
```

### 6.3 Landing, docs and README (owner: coordinator, Wave C)

- `landing.templ`: `collideDuplicate` becomes a live card in the present tense ("two agents write 'add login page' and 'build the login screen'; the second agent's own model is asked, and it changes course before it edits"); the file comment and the "Not built yet" heading keep only dismissals.
- `docs_pages.templ`: "Duplicate work" leaves the not-built list (Dismissals stays); the board page describes the duplicate card; the review tools' rows mention both kinds.
- README §4 becomes:
  > ### 4. Duplicate work — semantic, found from the wording
  >
  > Two agents writing "add login page" and "build the login screen" are about to build the same thing, and in a hackathon there is no ticket to tell you. metiche compares live plans in the same repository by what their summaries name — folding "screen" into "page" and ignoring verbs like "build" — and hands the likely pairs to the agent that declared second, whose own model judges whether it is the same change. Two plans on the same issue id are the strongest pair of all, across every repository on the team, and they are judged too, because people split tickets. The agent that was there first hears about it only if the duplicate is still standing a couple of minutes later.
- `landing_test.go` and `docs_test.go` change with it.

---

## 7. Test plan

Integration tests need `METICHE_TEST_MYSQL_DSN` with `parseTime=true&interpolateParams=true` and run with
`go test -p 1`, against a schema with `wording_revision`.

### 7.1 Unit (pure, no database)

**`app/coordination/duplicates_test.go`**, table-driven. Each vector names the expected result and rule.

Should be candidates (hackathon, no issue id):

| # | A | B | rule |
|---|---|---|---|
| 1 | add login page | build the login screen | words |
| 2 | login form | sign in page | words |
| 3 | dark mode toggle | add dark mode | words |
| 4 | stripe checkout | checkout flow with stripe | words |
| 5 | docker setup | set up docker compose | single concept |
| 6 | leaderboard endpoint | api route for the leaderboard | words |
| 7 | realtime chat with websockets | websocket chat | words |
| 8 | openai summaries | summarize notes with openai | words |
| 9 | deploy to vercel | vercel deployment | words |
| 10 | navbar | fix navbar on mobile | single concept |
| 11 | upload profile pictures | profile photo upload | words |
| 12 | leaderboard | leaderboard page | single concept |
| 13 | Add POST /api/login: password check, mint session cookie, wire the handler | implement login endpoint with refresh tokens and session cookie | words |
| 14 | navbar icons | navbar spacing tweaks (the two sessions' claims overlap) | words_and_paths |
| 15 | password reset flow | forgot password page | words |
| 16 | set up the database | database schema and migrations | same layer set (`words_layer`) |
| 17 | CI pipeline | github actions ci | single concept |
| 18 | add dark mode | light and dark theme switcher | words |
| 19 | realtime updates with websockets | live updates via socket.io | single concept |
| 20 | socket.io chat | live chat | words |
| 21 | ci/cd pipeline | github workflow for tests | single concept |
| 22 | dark mode switcher | theme toggle | words |

Rows 15–22 were added on 2026-09-21: 15–19 are misses the implementer found with its own pairs, closed by the coordinator's decision (bias towards catching: a missed duplicate is the failure this feature exists to stop; a false candidate costs one quiet judgement).

Should not be candidates:

| # | A | B | why |
|---|---|---|---|
| 1 | login page | login endpoint | layer guard |
| 2 | signup page | login page | no core key shared |
| 3 | user profile page | user profile api | layer guard |
| 4 | fix typo in footer | footer links | S 1 |
| 5 | add tests for auth | auth middleware | S 1; vague key blocks single concept |
| 6 | add loading spinner | loading states on the dashboard | S 1 |
| 7 | seed the database | database schema | shared key is a layer word only |
| 8 | deploy to vercel | vercel env vars | S 1 |
| 9 | chat ui | chat backend | layer guard |
| 10 | logout button | login page | nothing shared |
| 11 | fix login bug | login page styling | S 1; vague key blocks single concept |
| 12 | map view of events | events api | layer guard |
| 13 | refactor session store to redis | session cookie expiry bug | S 1 |
| 14 | shop checkout | shop search (project key `shop`) | project key dropped; nothing shared |
| 15 | navbar icons | navbar spacing tweaks (no claim overlap) | S 1: only the path boost makes it a candidate (should-match #14) |
| 16 | live demo deploy | realtime chat | S 1 (the realtime fold) |
| 17 | theme colors | dark mode | S 1 (the theme fold): a palette and a dark mode are related, not the same work |
| 18 | reset button styling | password reset | S 1 (the reset fold) |
| 19 | database backup script | schema migrations | shared key is a layer word only; the sets differ |
| 20 | github readme | ci pipeline | nothing shared: `github` alone is not CI |
| 21 | offline mode | dark theme | S 1 (the theme fold) |
| 22 | edit mode | dark mode | S 1 (the theme fold) |
| 23 | websocket server | websocket client ui | layer guard |
| 24 | api docs | backend error handling | shared key is a layer word only; the sets differ |
| 25 | signup page | login screen | no core key shared; the sets differ |
| 26 | account recovery email | password reset page | S 1 (the reset fold) |
| 27 | github actions for lint | user actions menu | nothing shared: bare `actions` is not CI |

Rows 16–27 guard the 2026-09-21 folds and the same-layer-set rule.

**Accepted false candidates**, pinned in the implementer's own pairs so a change is visible: "login page tests" / "login page" (words 2/2: the vague `test` blocks only the single-concept rule) and "dark mode toggle" / "dark mode colors for charts" (words 2/3). Found with the new folds and **not yet decided**: "data pipeline" / "ci setup" (single concept on `ci`), "ui tests" / "frontend bugs" (same layer set `{page}`) and "fixed navbar" / "navbar dropdown" (`fixed` is stoplisted, leaving `{navba}`).

Every row above was worked through §4.2's pipeline by hand; the implementer pins them as written, and a
row that disagrees with the code is a bug in one of the two, settled through the coordinator, never by
editing the expectation to match. The file must also include:

- `DuplicateTermsOf`: phrase joining (`sign in`, `log in`, `set up`, `check out`); the stoplist; project-key dropping; every synonym row; the 5-rune key; the vague set; `Words` keeps the caller's original words; same input, same output.
- `FrequentTerms`: nil below 8 summaries; at n = 9 a key in 4 is dropped and a key in 3 kept; at n = 30 the bar is 10.
- `ScoreDuplicate`: each rule at its boundary (S/m 0.59 vs 0.60; long rule 0.39 vs 0.40 with C 3; single concept with the other side at 2 vs 3 keys; words_and_paths 0.49 vs 0.50); m = 0.
- `WordingChanged`: case, punctuation, plural and stopword edits are not changes; a new noun is; an `external_ref` change is; reverting is a change relative to the intermediate wording.
- `RankDuplicateCandidates`: order and cap.
- `DuplicateSeverity`: the full §4.4 table, 0.69 vs 0.70, the issue floor, the high clamp, unsure.
- `DuplicateNoticeGrace`: per cadence (2m, 6m, 24m, default 6m).

**`app/coordination/pairkey_test.go`:** a duplicate pair is symmetric; a `wording_revision` bump mints a new key; the same two uuids under `duplicate_work` and `decision_contradiction` give different keys.

**`app/coordination/importguard_test.go`:** add `app/mcp/{duplicatereview,duplicateresolve,reviewrender}.go`.

**`app/mcp/duplicates_test.go`:**
- `decideDuplicateSettlement`: every §4.7 row plus COORDINATED, and "a done, b live" stays open;
- every §4.10 string fits its limit (400, 200, 200) with maximal names and keys, and masks `mtk_…`, `bearer …`, `password=…`;
- the mixed review block cut order at 1200;
- **golden:** a decision-only review block and note render byte-identical to 9384f2a's;
- `DuplicateWorkDedupeKey` ignores argument order and case;
- `DuplicatePlanKeys` parses `plans:` and refuses a malformed fact.

### 7.2 Integration, `app/mcp` (`duplicates_integration_test.go`)

- **Declaring:**
  - Bob declares "build the login screen" after Ana's "add login page": one pair, a = Ana's intent, b = Bob's, assigned to Bob, in the block with `why: words`;
  - the same `external_ref` on two projects of one team pairs with `why: same_issue`; the same wording on two projects does not pair;
  - the caller's own other intent, a supervisor/subagent pair and a `hold` plan are skipped; two sibling subagents are paired;
  - a pinned pair is skipped in both storage orders;
  - a replayed declaration returns the same pair keys and writes one event.
- **The cap:** 2 duplicate pairs and 2 decision candidates on one declaration → 2 duplicate pairs and 1 decision pair assigned, the rest backlogged; the per-minute count spans both kinds.
- **Updating:**
  - `add_paths`, `drop_paths` and `status_line` leave `wording_revision` unchanged, mint nothing, and the pending pair is still answerable;
  - a cosmetic summary edit (case, punctuation) leaves it unchanged;
  - a real rewording bumps it, mints a new pair and expires pending pairs where this intent is subject a;
  - reverting to the old wording bumps again and asks once more (the accepted cost);
  - two concurrent rewordings of one intent bump it twice.
- **`get_review_context`:** `other` populated; decision items byte-identical; `judgement`, `instruction` and `team_event` counts and `team.sequence` unchanged.
- **`report_judgement`:**
  - `no_conflict`: non-structural event, no conflict row;
  - `conflict` 0.85: the conflict row with §3.3's evidence (adjusters start with `plans:`), only the initiator participant, **no instruction**, the caller's `conflicts[]` with `duplicate_of` and `at_fault: "later"`, structural event;
  - `conflict` 0.6: low, no `conflicts[]`; `unsure`: low, rule `.unsure`;
  - a shared issue id floors a `low`-requested conflict at `medium`;
  - one person's two live agents: no softening, `same_member_concurrent` in the adjusters;
  - `duplicate_work.words` in `demoted_rules`: recorded `low`;
  - replay byte for byte; a different verdict refused; a reworded plan, an ended plan and the other session ended are each refused with §3.3's message.
- **Settlement:** every §4.7 row, reached through `update_intent` terminal on **each** side, `end_session` on **each** side (the incumbent found through the judgement path before it is a participant), and rewording followed by `no_conflict`; reopen after metiche closed it (`notified_at`, `escalated_at` cleared); silenced after `resolved_by_member_uuid` is set by hand.
- **With path overlap:** both kinds recorded as two rows; the pair is a words_and_paths candidate at S 1.
- **Concurrency:** 8 goroutines declare near-identical summaries on one project seeded with 100 live intents → exactly one judgement per pair key, a gapless `team.sequence`, and `metiche_team_lock_hold_ms` p99 under 25ms.
- **Session keys across teams:** a key present on two teams, for all three tools.

### 7.3 Integration, sweeper and webapi

- **`app/sweeper` (`duplicates` MySQL tests):**
  - a backlogged pair is assigned only to subject b's session, under the cap;
  - a lapsed window is re-armed to `assignment_count` 3, then expired;
  - a pair is expired when either `wording_revision` moved or subject a's session is gone;
  - the incumbent notice: nothing at grace − 1s, fires at grace, once (a second pass writes nothing), attaches the incumbent participant, `instruction_raised` structural, the body ≤ 200 and split by `splitNoticeBody`; a severity rise re-notifies once; a reopened conflict waits a fresh grace;
  - no notice when the judge yielded inside the grace (the conflict is YIELDED first);
  - a pending path notice between the two sessions is dismissed with `superseded by CF-…`, a delivered one is left alone;
  - escalation: never below the human floor, never when a plan ended, fires once at the budget, questions ≤ 200 to both sides;
  - abandonment of either side settles per §4.7.
- **`app/webapi` (MySQL):** `/conflicts` and the history carry §9.3's fields for a duplicate conflict and nothing new for other kinds; `issue_ref` only with a shared issue; the history filter accepts `duplicate_work`.
- **Stored rows, not code paths** (docs/LEARNINGS.md §3): the `team_event` rows of the notice, the settle and the escalation are read back column by column.

### 7.4 End to end, through the real transport (`app/duplicates_e2e_mysql_test.go`)

Two agents with their own tokens over `/v1/mcp` (`inviteWorld.connect`, `tool`), project cadence
`hackathon`, **no `external_ref` anywhere**. Every name is a test name.

1. Ana's agent starts S-17 on `shop` and declares INT-83 "add login page" on `web/src/routes/login.tsx`.
2. Bob's agent starts S-22 and declares "build the login screen" on `web/src/pages/Login.tsx`. The response has `review` with one `duplicate_work` pair, `plan` INT-83, `why: words`; `pending.reviews` is 1. `get_review_context` shows `other.key` INT-83.
3. Bob reports `conflict` 0.85. `conflicts[]` has `duplicate_of: "INT-83"`, `at_fault: "later"`, severity medium.
4. Ana heartbeats inside the grace: `pending` is all zero.
5. **Variant A (yields in time):** Bob rewords to "wire the login page to POST /api/login" (`wording_revision` 2, a new pair), answers `no_conflict`: CF CONVERGED with §4.10's note. Over the whole run Ana received **zero** instructions. REST: the history carries the note; `/events` has two `judgement_reported`.
6. **Variant B (does not yield):** the sweeper at clock + 2m: Ana's `pending.instructions` 1, `pending.conflicts` 1; `get_instructions` shows §4.10's body with `ref` CF. Bob marks INT-92 `superseded`: CF YIELDED with §4.10's note.
7. **Variant C (serious):** Bob asks `severity: high` and does nothing; at + 10m both agents receive the question.
8. **Variant D (noise):** Ana "login page", Bob "login endpoint": no pair. Then Ana "fix navbar spacing", Bob "navbar dropdown menu": no pair. Then a real pair judged `no_conflict`, followed by `add_paths` and status lines on both sides: no new pair, zero instructions.
9. **Variant E (issue id):** a sprint team; Ana on project `api`, Bob on project `web`, both `external_ref: "ISSUE-412"`, different wording: one pair, `why: same_issue`; a conflict verdict requested `low` is recorded `medium`.

### 7.5 Frontend

- `feed/conflicts_wire_test.go` decodes §9.3's example verbatim.
- `view/conflicts_test.go`: the duplicate card with both plans, the `asked to stop` badge, the chips, the judge note, the escalated badge, the settled note.
- `web/docs_test.go`: the tool list still equals the registered tools; `view/landing_test.go`: the duplicate card is live.

### 7.6 Mutation targets

Each, flipped, must turn at least one test red. Paste the one-line diff with the failing `-v` output; a
skipped test is not run.

- `wording_revision` bump keyed on `material` instead of `WordingChanged`; `WordingChanged` on the raw summary instead of tokens;
- the duplicate pair key using `revision` instead of `wording_revision`;
- the rewording-only trigger removed (the reviewer runs on every material update);
- the subject-a expiry on rewording removed;
- the staleness compare removed in `report_judgement` Apply, and in the sweeper;
- each `ScoreDuplicate` comparison (`>= 0.6` → `>`, `>= 2`, `>= 3`, `>= 0.4`, the single-concept "≤ 2", `>= 0.5`); the layer guard removed; vague keys counted as shared; the same-layer-set rule removed (vector 16 must fail); the realtime row removed (vectors 19 and 20 must fail);
- one synonym row removed (vector 1 must fail); the phrase join removed (vector 2 must fail); the project-key drop removed;
- the frequency filter's `>= 8` or `max(4, ⌈n/3⌉)`;
- the project filter removed from the words scan; the issue lookup losing `team_uuid`;
- the supervisor/subagent skip, the `hold` skip, the pinned check in either order removed;
- `ON DUPLICATE KEY UPDATE id = id` → plain INSERT; `DuplicateWorkDedupeKey` order-sensitive;
- `judge_session_uuid = caller` removed;
- `confidence >= 0.70` → `>`; the issue floor; the notify floor; the human floor; the `demoted_rules` check;
- an instruction written in `report_judgement` (the incumbent must not be told there); the incumbent participant attached there;
- the grace compare; `notified_at IS NULL` in the notice pass; the path-notice dismissal;
- any settlement rule reordered (rule 5 before 1); "a done, b live" settling;
- the reopen guard `resolved_by_member_uuid IS NULL` removed; `notified_at = NULL` or `escalated_at = NULL` removed from reopen;
- any `Structural` flag inverted; any new `LIMIT` removed; `sanitizeNoteText` on the rationale removed;
- any write inside `get_review_context`;
- the decision-only golden bytes (any change to decision pair rendering).

---

## 8. Build plan

**The rules for every agent** (docs/LEARNINGS.md §8): every brief names the files the agent owns and two
agents never share one; generated code, `create.sql` and `deploy/` are off limits; proof is the command
that was run and its output; each agent runs its own database; commits are in the owner's name with no
trailers.

### Wave 0 — schema

- The coordinator's nuzur draft `v8-wording-revision` (§2.2) is reviewed and published by the owner.
- The codegen agent regenerates and writes `deploy/sql/2026-09-wording-revision.sql` (§2.3).
- §9 is frozen by this document.

### Wave A — parallel, no codegen needed

| Agent | Owns | Proof |
|---|---|---|
| A1 coordination | `app/coordination/duplicates.go`, `duplicates_test.go`, additions to `pairkey_test.go` and `importguard_test.go` | `go test ./app/coordination/ -v` output, every §7.1 vector listed |
| A5 frontend | `internal/model/*`, `internal/feed/wire.go` + tests, `internal/view/conflicts.templ`, generated `_templ.go`, view tests; builds against §9.3's example | `templ generate`, `go test ./...` in `code/frontend`, a screenshot of a duplicate card from the fixture |
| A6 skill | both `SKILL.md` copies, `plugin.json`, `marketplace.json` (0.7.0) | the two files `diff`ed identical; merged only after Wave B is deployed |

### Wave B — after codegen

| Agent | Owns | Needs |
|---|---|---|
| B1 `app/mcp` (the seams: one owner) | new `duplicatereview.go`, `duplicateresolve.go`, `reviewrender.go`, `duplicates_test.go`, `duplicates_integration_test.go`; edits to `intents.go`, `sessions.go`, `detector.go`, `decisionreview.go`, `reviewcontext.go`, `reportjudgement.go`, `recorddecision.go`, `envelope.go`, `conflictresolve.go`, `server.go`, `server_test.go`, the decision golden tests | A1's functions (§9.1); the generated `WordingRevision` field |
| B2 webapi + wiring | `app/webapi/wire.go`, `conflicts.go`, `conflicthistory.go`, webapi MySQL tests, `app/rest.go` (the chain) | B1's `NewDuplicateReviewer`, `NewReviewRenderer` compiling |
| B3 sweeper | new `app/sweeper/duplicates.go` (pair pass, incumbent notice) + tests; edits to `sweeper.go` (`RunOnce` order: decision pairs → duplicate pairs → duplicate notices → escalation), `decisions.go` (escalation for both kinds, duplicate questions), `conflicts.go` (`settleAfterAbandon`) | B1's §9.2 functions; A1's `DuplicateNoticeGrace`, `ShouldEscalate`, `EscalationBudget` |

B2 and B3 start on §9's signatures with stub bodies.

### Wave C — the coordinator

- `app/duplicates_e2e_mysql_test.go` (§7.4);
- `landing.templ`, `docs_pages.templ`, README §4, `landing_test.go`, `docs_test.go` (§6.3);
- cross-references in PLAN (§1.3 decisions), MODEL (`intent.wording_revision`; `intent_token` unused) and DECISIONS (§3.1's "Duplicate work will need it" corrected to point here);
- the full backend and frontend suites; the lock-hold p99 with 100 live intents seeded;
- the migration applied to a copy of the current schema, `SHOW CREATE TABLE intent` compared with `create.sql`;
- the deploy runbook: migration first, then the backend, then the board, then the skill.

### Risks

1. **Lock hold on every `declare_intent`.** Unlike decisions there is no "nothing recorded" fast path. Added inside the lock: one `idx_intent_live` range scan (LIMIT 100) joined to session, Go scoring of ≤ 100 short summaries, an optional issue lookup (LIMIT 8), ≤ 3 point or IN queries, ≤ 2 inserts; estimated +1 to 3ms, unmeasured. Mitigations: the scan is skipped when the caller's summary has no non-vague key; the per-minute count is shared; `demoted_rules` is a kill switch. The histogram decides, under §7.2's seeded concurrency test.
2. **Regressing shipped decisions.** The shared renderer, the `judgementRow` rename and the `loadLiveIntents` change touch built code; the decision golden bytes and the full decision e2e suite must stay green.
3. **Wording misses and false candidates.** §4.2 is a small hand-built vocabulary. Misses are silent; false candidates cost one judgement each and interrupt nobody on `no_conflict`. The synonym and stoplist tables are the tuning surface, extended by adding §7.1 vectors first.
4. **Self-judging bias** towards `no_conflict` (the judge is the yield side). Mitigation: visibility on the board.
5. **Cap competition.** Duplicate pairs take cap slots before decision pairs; decision scope and words candidates backlog more often.
6. **`judgement` grows unbounded** (DECISIONS risk 7); duplicate pairs add rows. A batched retention cleanup is follow-up work.
7. **Issue-id matching** relies on `truncate`'s trim and the column's case-insensitive collation; verify with `SHOW CREATE TABLE intent`.
8. **Migration order** (§2.3), and **must ship together**: `docs_test.go` fails unless docs change with the feature; the skill must not advertise it before deploy.

---

## 9. Frozen interfaces

### 9.1 `app/coordination` — owner A1, used by B1 and B3. Pure: no database, no clock, no network.

```go
// The material-change rule for intent.wording_revision (§2.2).
func WordingKey(summary, externalRef string) string
func WordingChanged(oldSummary, oldRef, newSummary, newRef string) bool

// Concept keys (§4.2).
const DuplicateKeyRunes = 5
const DuplicateFrequencyMinScanned = 8
const DuplicateFrequencyMinCount = 4

type Layer string

const (
	LayerUI   Layer = "ui"
	LayerAPI  Layer = "api"
	LayerData Layer = "data"
)

type DuplicateTerms struct {
	Keys   []string          // comparison keys, first-seen order, de-duplicated
	Words  map[string]string // key -> the first original word the summary used
	Vague  map[string]bool   // keys that never count as shared
	Layers map[string]Layer  // key -> layer, for layer words only
}

func DuplicateTermsOf(summary, projectKey string) DuplicateTerms

// FrequentTerms returns the keys to drop from every comparison in one scan
// (caller included in all); nil below DuplicateFrequencyMinScanned.
func FrequentTerms(all []DuplicateTerms) map[string]bool

type DuplicateScore struct {
	Candidate bool
	Rule      string   // "words" | "words_long" | "words_single" | "words_layer" | "words_and_paths" | ""
	Shared    []string // non-vague shared keys, in mine's order
	Core      int      // shared keys that are not layer words
	Ratio     float64  // len(Shared) / smaller non-vague key count
}

func ScoreDuplicate(mine, theirs DuplicateTerms, drop map[string]bool, pathsOverlap bool) DuplicateScore

// Ranking.
type DuplicateSignal int

const (
	DupSignalWords         DuplicateSignal = 1
	DupSignalWordsAndPaths DuplicateSignal = 2
	DupSignalSameIssue     DuplicateSignal = 3
)

func (s DuplicateSignal) String() string // "words" | "words_and_paths" | "same_issue"

type DuplicateCandidate struct {
	OtherIntentUUID string
	OtherIntentKey  string
	Signal          DuplicateSignal
	Core            int
	Shared          int
}

func RankDuplicateCandidates(in []DuplicateCandidate, max int) []DuplicateCandidate

// Severity (§4.4). Returns SeverityNone for a no_conflict verdict.
type DuplicateSeverityInput struct {
	Verdict    string   // "conflict" | "unsure"
	Confidence float64
	Requested  Severity // SeverityLow..SeverityHigh; zero means medium
	SameIssue  bool
	Demoted    bool     // the rule is in team_settings.demoted_rules
}

func DuplicateSeverity(in DuplicateSeverityInput) Severity

// EscalationBudget(cadence) / 5: hackathon 2m, sprint 6m, steady 24m, else 6m.
func DuplicateNoticeGrace(cadence string) time.Duration
```

### 9.2 `app/mcp` — owner B1, used by B2 (rest.go) and B3 (sweeper)

`queryer` is the package's existing unexported interface; callers outside pass a `*sql.Tx` or `*sql.DB`.

```go
// duplicatereview.go

// NewDuplicateReviewer returns the Detect hook that pairs a declared or reworded intent with
// live plans that may be the same work, assigns the judgements and appends review pairs to the
// request for the renderer (§3.1). It returns no ConflictNotices.
func NewDuplicateReviewer(coreImpl *core.Implementation, logger *zap.Logger) DetectHook

// reviewrender.go

// NewReviewRenderer returns the last hook in the chain: it renders the request's review pairs
// into Envelope.Review with the cut order and appends the note lead once. For decision pairs
// alone its output is byte-identical to the decision reviewer's before this change.
func NewReviewRenderer() DetectHook

// duplicateresolve.go

const (
	RuleDuplicateSameIssue = "duplicate_work.same_issue"
	RuleDuplicateWords     = "duplicate_work.words"
	RuleDuplicateUnsure    = "duplicate_work.unsure"
)

// One conflict per unordered pair of intents, not per revision.
func DuplicateWorkDedupeKey(intentA, intentB string) string

type DuplicateReleaseKind int

const (
	DuplicateReleaseJudged           DuplicateReleaseKind = iota + 1 // report_judgement no_conflict
	DuplicateReleaseIntentEnded                                      // update_intent done/abandoned/superseded
	DuplicateReleaseSessionEnded                                     // end_session
	DuplicateReleaseSessionAbandoned                                 // the sweeper writing a session off
)

type DuplicateRelease struct {
	TeamUUID    uuid.UUID
	SessionUUID uuid.UUID // whose call or lapse caused it
	Kind        DuplicateReleaseKind
	At          time.Time // the transaction's clock
}

// OpenDuplicateConflictsOfSession lists the open or acknowledged duplicate_work conflicts a
// session is in, as a participant or as the owner of subject a (§4.7), oldest first, at most
// settleMaxConflicts.
func OpenDuplicateConflictsOfSession(ctx context.Context, q queryer, teamUUID, sessionUUID uuid.UUID) ([]uuid.UUID, error)

// SettleDuplicateConflict re-evaluates one duplicate_work conflict (§4.7) and closes it when a
// rule fits. It returns false and writes nothing when the conflict is closed already or still
// stands. Must run on the transaction holding the team lock. The caller appends the
// conflict_resolved event (ConflictResolvedSummary / ConflictResolvedPayload handle the kind).
func SettleDuplicateConflict(ctx context.Context, q queryer, conflictID uuid.UUID, rel DuplicateRelease) (SettledConflict, bool, error)

// ExpireDuplicatePairsOnIntents marks pending duplicate_work judgements whose subject a is one of
// intentIDs expired. idx_judgement_subject. No event.
func ExpireDuplicatePairsOnIntents(ctx context.Context, q queryer, teamUUID uuid.UUID, intentIDs []uuid.UUID, now time.Time) (int64, error)

// DuplicatePlanKeys reads the "plans:<a>,<b>" fact from a conflict's evidence adjusters.
func DuplicatePlanKeys(adjusters []string) (a, b string, ok bool)

// Wording (§4.10), single-sourced for the sweeper.
func DuplicateIncumbentAction(judgeName, yieldKey, rationale, yieldSession, pathConflictKey string) string // ≤ 200
func DuplicateEscalationQuestions(conflictKey, yieldKey, incumbentKey, yieldMember, incumbentMember, incumbentSummary, budget string) (yieldSide, incumbent string) // each ≤ 200
```

`SettledConflict` gains `PlanKeys [2]string` (b, a); `ConflictNotice` gains `DuplicateOf`.

### 9.3 Conflict wire: the new fields

`conflictWire` gains, on `/conflicts`, `/conflicts/history`, the session page and run history:

```go
// Plans, Signals and IssueRef are set only on duplicate_work (docs/DUPLICATES.md §5.1).
Plans    []conflictPlanWire `json:"plans,omitempty"`     // [incumbent, yield side], from evidence a_/b_ and the plans: fact
Signals  []string           `json:"signals,omitempty"`   // human text of the evidence facts, in order
IssueRef string             `json:"issue_ref,omitempty"` // the shared external_ref, when there is one

type conflictPlanWire struct {
	Key     string `json:"key"`
	Who     string `json:"who"`
	Summary string `json:"summary"`
	Path    string `json:"path,omitempty"`
	Yields  bool   `json:"yields"`
}
```

`JudgeNote` (evidence `field_issues[0]`) and `EscalatedAt` now also apply to `duplicate_work`.
`signals` renders the facts as: `same issue ISSUE-412`, `shared words: login, screen`,
`also share web/src/auth/**`, `one person's two agents`; the `plans:` fact is never a signal.

A full open duplicate conflict, after the incumbent was told:

```json
{
  "key": "CF-44",
  "kind": "duplicate_work",
  "severity": "medium",
  "status": "open",
  "detected_by": "agent",
  "detector_rule": "duplicate_work.words",
  "suggested_action": "INT-83 (Ana (ui), active 4m) is already building this: \"add login page\". Stop before you edit: mark INT-92 superseded with update_intent, or re-scope it to a different part and update_intent the summary, which asks you to judge once more. If your plan should be the one that continues, settle that with Ana's agent (S-17) first.",
  "occurrence_count": 1,
  "first_detected_at": "2026-09-21T14:02:40Z",
  "last_detected_at": "2026-09-21T14:02:40Z",
  "paths": ["web/src/pages/Login.tsx", "web/src/routes/login.tsx"],
  "plans": [
    {"key": "INT-83", "who": "Ana (ui)", "summary": "add login page", "path": "web/src/routes/login.tsx", "yields": false},
    {"key": "INT-92", "who": "Bob (frontend)", "summary": "build the login screen", "path": "web/src/pages/Login.tsx", "yields": true}
  ],
  "signals": ["shared words: login, screen"],
  "judge_note": "S-22's model (0.85): both build the login page; INT-83 already has the route",
  "escalated_at": "2026-09-21T14:12:40Z",
  "participants": [
    {"session_key": "S-22", "member_name": "Bob", "agent_label": "frontend", "role": "initiator", "subject_kind": "intent"},
    {"session_key": "S-17", "member_name": "Ana", "agent_label": "ui", "role": "incumbent", "subject_kind": "intent"}
  ]
}
```

Settled, the same object carries `"status": "resolved"`, `"resolution": "yielded"`,
`"resolved_at": "2026-09-21T14:15:02Z"` and
`"resolution_note": "Settled by the agents: S-22 (frontend) marked INT-92 superseded at 14:15 UTC, leaving INT-83 (S-17 (ui)) to build it. A person was asked at 14:12 UTC."`.
With a shared issue id it also carries `"issue_ref": "ISSUE-412"` and the signal `same issue ISSUE-412`,
and `detector_rule` is `duplicate_work.same_issue`. Before the incumbent is told, `participants` holds only
the initiator; `plans` always holds both.

### 9.4 The evidence, as stored

```json
{"overlap_path": null,
 "a_label": "Ana (ui)", "a_summary": "add login page", "a_pattern": "web/src/routes/login.tsx",
 "b_label": "Bob (frontend)", "b_summary": "build the login screen", "b_pattern": "web/src/pages/Login.tsx",
 "adjusters": ["plans:INT-83,INT-92", "words:login,screen"],
 "field_issues": ["S-22's model (0.85): both build the login page; INT-83 already has the route"],
 "detail": "duplicate_work.words"}
```

---

## 10. Decisions (owner-approved 2026-09-21)

1. **An exact `external_ref` match is judged, not decided.** It is the top-ranked signal and the only one that crosses repositories, and a conflict verdict on it is floored at `medium`. Agents split tickets. README §4 is reworded to match.
2. **The incumbent is told after a grace, and only if the conflict is still unsettled.** The grace is `EscalationBudget(cadence)/5` (hackathon 2m, sprint 6m, steady 24m). The incumbent becomes a participant at that moment, not before.
3. **Wording changes are counted by a new column, `intent.wording_revision`** (INT NOT NULL DEFAULT 1, nuzur `v8-wording-revision`, migration `deploy/sql/2026-09-wording-revision.sql`). It is bumped only when the summary's normalized tokens, or the `external_ref`, change; path edits never bump it. Duplicate pairs store it in `judgement.subject_*_revision`. Reverting to earlier wording is a new revision and is asked once more: the accepted cost.
4. **Across projects, the issue id only.** In a hackathon there are no issue ids, so the within-project wording signal is the whole feature there, and §4.2 is designed for short, informal summaries: phrase joining, a verb stoplist, synonym folding, 5-rune keys, a layer guard, and a frequency filter that stays off in a small project.
5. **`intent_token` stays in the model, unused.**
6. **Two producers of one contract** as a duplicate signal is a follow-up, out of scope.

### 10.1 Refinements made while writing this spec

These tighten the approved design. None changes the model or an owner decision.

- **Short-summary thresholds.** The design's first rule (≥ 3 shared tokens, ratio 0.5) missed the owner's own example: "add login page" and "build the login screen" share one raw token. §4.2 replaces it with concept keys and four rules, validated against §7.1's vectors.
- **The layer guard.** "login page" and "login endpoint" share their subject and are a producer and a consumer, not a duplicate; without the guard every such pair would be judged.
- **Vague keys block the single-concept rule**, so "add tests for auth" does not pair with everything about auth.
- **`wording_revision` is computed under the lock**, from the row, so concurrent rewordings cannot both skip the bump.
- **Only the subject-a side is expired on a rewording.** The caller's own stale pairs are refused as stale and expired by the sweeper, exactly as decisions do, so there is one staleness path.
- **No instruction is written by `report_judgement`** for a duplicate; the sweeper owns the incumbent's notice, so a judge that yields in time interrupts nobody.

**Made while implementing Wave A1 (2026-09-21), accepted by the coordinator:**

- **The layer guard reads layers before the frequency filter** (`app/coordination/duplicates.go`, `duplicateLayerGuard`). Applying it after the filter would let a project where `page` is frequent pair "login page" with "login endpoint". Pinned by `TestScoreDuplicateLayerGuardSurvivesTheFilter`.
- **Phrase joins match case-insensitively on the original text** instead of lowercasing first (`duplicatePhraseJoins`), so `Tokenize` still splits camelCase; `sign-in`, `sign_in` and `SignIn` join too. Identical for lowercase input. The project key gets the same joins.
- **The import guard checks the three future `app/mcp` files once they exist** (`importguard_test.go`): `duplicatereview.go`, `duplicateresolve.go`, `reviewrender.go`. Listing them before Wave B writes them would fail the test; any stat error other than not-found fails it.
- **`Demoted` wins over the issue floor** in `DuplicateSeverity`: a demoted rule always records `low`. It cannot clash in practice, because a shared issue carries the `same_issue` rule, not the demoted `words`.
- **Re-judge after a re-scope (2026-09-21, follow-up to B1).** A rewording under an open duplicate conflict always mints a re-judge pair against the other plan, whatever the score (§3.1 step 4b, `why: open_conflict`), ordered before new candidates so the per-minute cap cannot starve it. Found by reading `reviewIntent`: a genuine re-scope no longer scored as a candidate, so no pair was minted and the conflict could only close when a plan ended, while the incumbent could still be told after the grace and a person asked. Settling on the rewording alone, with no judgement, was rejected: the owner's rule is that the agents settle it and the agent's own model decides, and a rewording that keeps the same work under new words must stay open. Plans with no open conflict keep the score gate, so ordinary rewordings add no pairs.
- **Bias towards catching.** Wording misses found with hackathon pairs were closed with five synonym rows (`reset`, `ci`, `theme`, `toggle`, `realtime`), three phrase joins, the fix forms in the stoplist, `apis`, and the same-layer-set rule (§4.2). `ScoreDuplicate` gained the rule value `words_layer` (§9.1); like every wording rule it is the `words` why and `duplicate_work.words` on a conflict.
