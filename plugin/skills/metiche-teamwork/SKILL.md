---
name: metiche-teamwork
description: Work alongside other coding agents and humans on one repo through the metiche MCP server — declare what you are about to do before you do it, hold narrow path claims, heartbeat while you work, and act on the collisions metiche reports inside your own tool responses. Use whenever the metiche MCP tools are connected, and whenever the user says metiche, join code, team board, "who else is touching this", "don't step on the other agent", "we're working in parallel", hackathon team, declare intent, claim paths, heartbeat, pending instructions, or asks you to judge whether two pieces of work conflict.
---

# metiche — working in a team of agents

metiche (Mexican slang for *nosy*) is a shared, live picture of what every agent on a team is
doing to one repo. You report what you are **about to do** and what you are **doing right now**;
the server keeps that as team state, detects collisions deterministically, and tells you about
them **in the response to your own tool call** — while changing course is still cheap.

Nothing here blocks you. Ever. metiche gives you information and a person watching a board; what
you do with it is your judgement. That is also the obligation: information you accept and then
ignore is worse than information you never received, because the person watching learns that the
controls are decorative.

Two things make this work, and both are your job:

1. **Say it before you do it.** A collision found after the edit is a merge conflict. A collision
   found at declare time is a thirty-second conversation.
2. **Stay alive.** Your claims have a TTL. If you go quiet, they lapse and the rest of the team
   starts working on top of you thinking you left.

---

## The loop

```
join_team              once per agent process
  start_session        once per bounded piece of work
    declare_intent     before each chunk of work — the main tool
      heartbeat        ~every 60s while working, always around a batch of edits
      update_intent    status line changed, scope changed, chunk finished
    declare_intent     next chunk
  end_session          when the work is done or you are stopping
```

Everything else — contracts, decisions, judgements, conflict resolution, instructions — hangs off
that spine and is occasional.

### The fifteen tools

The server's own tool schemas are authoritative; read them. This is the map, not the signature
list.

| tool | when | note |
|---|---|---|
| `join_team` | once, at startup | join code → your agent identity. Re-joining with the same client key is idempotent. |
| `start_session` | once per piece of work | branch, base commit, goal. This is also the run record. |
| `end_session` | when you stop | releases claims immediately instead of waiting for TTL |
| **`heartbeat`** | **~every 60s** | alive + extend claims + collect pending counts. Cheapest call in the system. |
| **`declare_intent`** | **before each chunk** | summary + paths + mode. Creates the intent *and* its claims in one transaction. |
| **`update_intent`** | **as things change** | status line, add/drop paths, mark done |
| `check_paths` | before exploring | read-only, no commitment: "who else is in here?" |
| `publish_contract` | when you define or depend on a shape | `role: produces` or `role: consumes` |
| `record_decision` | rare | a team-level rule others should not contradict |
| `get_review_context` | when a response says `review_pending` | read-only |
| `report_judgement` | when handed a `pair_key` | your verdict on one candidate pair |
| `resolve_conflict` | when a conflict is settled or bogus | `resolved` / `converged` / `dismissed` + reason |
| `get_instructions` | when `pending.instructions > 0` | **not read-only** — reading is the delivery receipt |
| `report_back` | after acting on an instruction | close the loop with the human who sent it |
| `get_team_state` | at session start, rarely after | scoped and paginated; not a substitute for the pending counts |

There is **no claim verb**. `declare_intent` makes the claims; `update_intent` adds, drops,
extends and completes them. One concept, not two.

---

## Heartbeat is a time obligation, not a courtesy

This is the rule agents break, and it is the one that quietly breaks the product.

You can spend twenty minutes reading files and writing edits without making a single MCP call.
During those twenty minutes metiche has no reason to believe you exist. Your claims expire, you
vanish from the board, and a teammate's agent declares the same files with a clean result.

So: **heartbeat roughly every sixty seconds, and always around a batch of edits** — before you
start one and after you finish one. It takes no lock, writes no event, and costs almost nothing.
There is no such thing as heartbeating too often within reason, and there is very much such a
thing as too rarely.

**Say why, not just what.** The status line is the sentence a human reads on the board to decide
whether to interrupt you.

- Bad: `working` · `editing files` · `in progress`
- Good: `rewriting the session cookie path in auth.go so login and refresh share one helper`
- Good: `stuck — the token refresh test fails only under -race, bisecting`

A status line that says *why* lets someone answer "should I wait for this or start it myself?"
without asking you.

---

## Declare before you act, not after

`declare_intent` is the main tool because it is the only one that happens at a useful moment.

Declare when you have decided what to do and before you start doing it — after planning, before
the first edit. The response comes back with `conflicts[]` populated if you just walked into
someone, and you have not spent a token on the wrong approach yet.

Declaring after the fact is not coordination, it is logging. If you find yourself calling
`declare_intent` to describe a change you already made, you have used the tool for its least
valuable purpose.

A good declaration:

```
summary:  "Add POST /api/login: password check, mint session cookie, wire the handler"
paths:    ["internal/auth/auth.go", "internal/auth/session.go", "internal/api/routes.go"]
mode:     "write"
kind:     "feature"
external_ref: "ISSUE-412"          # if there is one — an exact issue match is the
                                    # highest-signal duplicate-work key in the system
```

`summary` is capped at 280 characters and it is read by other agents' models, not by a parser.
Write the sentence you would say to a teammate. If you have an issue id, always include it — it
is a free exact match that saves everyone a semantic judgement.

---

## Claim honestly and narrowly

Claim **what you will actually touch**, at the finest granularity you can name.

`app/**` conflicts with everyone. The server knows that: an over-broad claim (depth ≤ 1) is
capped at `low` severity and you get a "narrow this" nudge instead of protection. A claim so wide
it always fires is indistinguishable from no claim at all, and it trains every other agent on the
team to ignore your name.

- Name files when you know them: `internal/auth/auth.go`.
- Name a real subtree when you genuinely own it: `internal/auth/**`, not `internal/**`.
- Use `mode: read` for files you are only reading. Read×read is *never* a conflict, so read
  claims are free and they are how a teammate learns you depend on their file.
- Use `mode: structural` for renames, moves and deletes. These are escalated deliberately: a
  rename breaks readers silently, and the people reading your file need to hear about it.
- **Drop what you finish.** `update_intent(drop_paths: [...])` the moment a file is done. Holding
  a file you are no longer editing is a false positive you are personally generating.

If you do not yet know which files you need, use `check_paths` first. It is read-only and commits
you to nothing — it answers "is anyone in here?" before you decide to be in here.

Paths are normalized on insert: no absolute paths, no `..`, a trailing `/` becomes `**`.
Generated code is dropped from claims entirely by the project's ignore patterns, so do not bother
claiming it and do not be surprised when it does not appear.

---

## Every response carries `pending` — never poll blind

Every single response from every tool ends with the same envelope:

```json
{"ok": true, "key": "INT-83", "sequence": 417, "revision": 88,
 "pending": {"instructions": 0, "conflicts": 0, "reviews": 0}, "note": "..."}
```

That is the push mechanism. MCP cannot push to you, so the counts ride on everything, including a
bare heartbeat.

- **All zero → carry on.** Do not call `get_instructions`. Do not call `get_team_state` to see
  if anything changed. Nothing changed; the response just told you so.
- **`instructions > 0` → call `get_instructions` now.** A human raised a nudge: stop, steer, or a
  question. It has been sitting there since your last call. Act on it, then `report_back`.
- **`conflicts > 0` → you are in a collision** that was found asynchronously (someone declared
  after you). Read it and decide.
- **`reviews > 0`, or `review_pending` is set → call `get_review_context`.** The inline review
  block was dropped to stay inside the token budget; the content is still there.

Polling `get_team_state` on a timer is the anti-pattern. It is scoped, paginated and meant for
session start, not for awareness. Awareness is the counts.

---

## Nobody is blocked. That is why the response matters.

When a conflict comes back, it is not a permission failure. Claims are advisory; you can proceed
through any of them. Ordering assigns **responsibility**, not permission: whoever declared later
is told synchronously, because they have the information in hand and have not started yet.

Every conflict metiche surfaces carries a suggested next action, because *"you and Ana both hold
auth.go"* is noise and *"Ana holds auth.go (write, 4m, feat/auth); consider consuming her
POST /api/login contract instead"* is signal.

Your obligation on receiving one is to **make a visible decision**:

- **Change course** — pick different files, wait for the other agent, take the other half of the
  work. Say so in `update_intent` so the board shows it.
- **Coordinate** — consume their contract instead of inventing a parallel one; `record_decision`
  if you two just settled something the rest of the team needs.
- **Proceed anyway, deliberately** — sometimes that is right. Then `resolve_conflict` with
  `status: acknowledged` (or `dismissed` if it is a false positive) and a reason that says why.
  "Same file, different function, I'll take the merge" is a perfectly good reason.

What you must not do is acknowledge a conflict and change nothing and say nothing. The human
watching the board sees an agent that was told and carried on regardless, and from then on the
whole system reads as theatre.

And tell your human. A conflict that only exists in your tool transcript did not reach anyone.

---

## When you are asked to judge a pair

metiche's server never calls a model — that is a hard design constraint, so the semantic half of
detection is you. When a response hands you a **review block**, it contains a few candidate
decisions and similar intents, each with a `pair_key`, and the server has already assigned that
pair to you and only you, with a short window. N agents never burn N× tokens on one question, so
if you drop it, nobody else picks it up until the window lapses.

Answer with `report_judgement(pair_key, verdict, severity, confidence, rationale)`.

**`no_conflict` is the common answer and it is the correct one roughly four times in five.** It
costs one tiny event and it kills that pair permanently. It is the cheap, expected outcome.

The failure mode to guard against is your own: a model asked *"is this a conflict?"* has a strong
yes-bias. Saying "conflict" to be safe is not safe. It is how the team learns to ignore metiche,
and once an agent's tool output is background noise it never recovers. The server knows about the
bias and only interrupts a human at `confidence ≥ 0.7`, which only works if your confidence is
honest.

So:

- Judge the **actual** question: does this intent contradict this recorded decision? Are these
  two intents the same work? Not "could these conceivably interact."
- Different layers of the same feature are **not** duplicate work. Two agents on one issue id
  probably are.
- Contradicting a recorded decision means doing the thing the decision says not to do. Doing
  something the decision does not mention is not a contradiction.
- **Report confidence you actually hold.** 0.4 means 0.4. Padding it to 0.8 to make sure someone
  looks is exactly the behaviour that poisons the system for every other agent on the team.
- `rationale` is one or two sentences a human will read on the board. Name the specific thing.

---

## Never report secrets

Everything you send becomes team-visible state on a live web board, in an append-only event log,
and in other agents' tool responses. It is not a private channel and there is no redaction pass.

Never put into a `summary`, `status_line`, `rationale`, `note`, contract shape, decision
statement or path list:

- API keys, tokens, join codes, passwords, private keys, session cookies
- connection strings, or a URL with credentials in it
- `.env` **contents** (the path `.env` is fine, the values are not)
- customer data, personal data, or anything from a file you would not paste into a group chat

A contract shape describes **types and field names**, never example values. If a shape's example
would carry a real secret, describe the type and stop.

If you are editing a file that holds credentials, claim the path and describe the change
abstractly: `rotating the storage credential in deploy/prod.yaml` — never what it rotated to.

---

## Worked example: two agents, one collision

Ana and Beto are on the same repo with their own agents. Both have this skill and both are
pointed at the same metiche team. Ana's agent is **A**, Beto's is **B**. Keys are illustrative.

**1. A starts and declares.**

```
A → start_session(project: "metiche", branch: "feat/auth", base_commit: "9f2c1ab",
                  goal: "login endpoint")                      → S-17
A → declare_intent(
      summary: "Add POST /api/login — verify password, mint session, set cookie",
      paths: ["internal/auth/auth.go", "internal/auth/session.go"],
      mode: "write", external_ref: "ISSUE-412")                → INT-83
    ← {"ok":true,"key":"INT-83","pending":{...all zero},"conflicts":[]}
```

Clean. A gets to work, heartbeating each minute with a status line.

**2. A publishes what it produces.** A has settled the response shape, so it says so before
anyone can guess wrong:

```
A → publish_contract(key: "POST /api/login", role: "produces",
      shape: {"session_expires_at": "datetime", "user": {"id": "uuid", "email": "email"}})
```

**3. B declares, and is told inside its own response.**

```
B → declare_intent(
      summary: "Login screen — form, submit, store the session token in localStorage",
      paths: ["web/login.tsx", "internal/auth/auth.go"], mode: "write")
    ← {"ok": true, "key": "INT-91",
       "conflicts": [{
          "key": "CF-14", "kind": "path_overlap", "severity": "critical",
          "note": "Ana's agent holds internal/auth/auth.go (write, 4m, feat/auth) for
                   INT-83 'Add POST /api/login'. It publishes POST /api/login —
                   consider consuming that contract instead of writing the handler.",
          "participants": ["S-17", "S-22"]}],
       "review": {"decisions": [{
          "key": "#auth-jwt-cookie",
          "statement": "The session token lives in an httpOnly cookie, never in
                        localStorage.",
          "pair_key": "pk_9c3f…"}]},
       "pending": {"instructions": 0, "conflicts": 1, "reviews": 1}}
```

Two separate findings in one response, before B has opened an editor.

**4. B acts on the path overlap.** B does not need `auth.go`; it assumed it would have to write
the handler itself. It drops the file and narrows to its own layer:

```
B → update_intent(key: "INT-91",
      drop_paths: ["internal/auth/auth.go"], add_paths: ["web/api/session.ts"],
      status_line: "building the login form against Ana's POST /api/login instead of
                    writing my own handler")
B → resolve_conflict(key: "CF-14", status: "resolved",
      reason: "dropped internal/auth/auth.go — consuming Ana's endpoint")
```

The collision cost B one tool response and no code. That is the whole product.

**5. B judges the decision pair honestly.** B's plan really does put the token in localStorage,
and `#auth-jwt-cookie` really does forbid it. This one is a genuine contradiction:

```
B → report_judgement(pair_key: "pk_9c3f…", verdict: "conflict",
      kind: "decision_contradiction", severity: "high", confidence: 0.9,
      rationale: "My intent stores the session token in localStorage; #auth-jwt-cookie
                  requires an httpOnly cookie. Switching to cookie-based auth — no
                  token handling in the client at all.")
B → update_intent(key: "INT-91",
      summary: "Login screen — form, submit, rely on the httpOnly session cookie")
```

Had B's plan merely *mentioned* auth without touching token storage, the right answer would have
been `no_conflict` at high confidence, and the pair would be dead forever.

**6. B publishes what it consumes, and the shapes disagree.**

```
B → publish_contract(key: "POST /api/login", role: "consumes",
      shape: {"expires_at": "datetime", "user": {"id": "uuid", "email": "email"}})
    ← {"ok": true,
       "conflicts": [{"key": "CF-15", "kind": "contract_mismatch", "severity": "medium",
          "note": "Producer S-17 emits session_expires_at; this consumer requires
                   expires_at. One required out field is missing.",
          "evidence": {"missing_out": ["expires_at"], "extra_out":
                       ["session_expires_at"]}}]}
```

Directional, and it names which side is at fault: a required **out** field the consumer needs
and the producer does not emit.

**7. A finds out on a bare heartbeat, having never polled.**

```
A → heartbeat(status_line: "wiring the cookie into the login handler")
    ← {"ok": true, "pending": {"instructions": 1, "conflicts": 1, "reviews": 0}}
A → get_instructions()
    ← [{"key": "INS-7", "kind": "conflict_notice", "conflict": "CF-15",
        "body": "Beto's login screen consumes POST /api/login and expects
                 expires_at; you emit session_expires_at."}]
```

This is A's **first and only** interruption of the whole exchange, and it is actionable.

**8. A converges.**

```
A → publish_contract(key: "POST /api/login", role: "produces",
      shape: {"expires_at": "datetime", "user": {"id": "uuid", "email": "email"}})
    ← {"ok": true, "conflicts": [], "note": "CF-15 auto-resolved: converged"}
A → report_back(instruction: "INS-7", outcome: "done",
      note: "renamed session_expires_at → expires_at to match Beto's consumer")
A → update_intent(key: "INT-83", status: "done")
A → end_session(key: "S-17", summary: "POST /api/login shipped with cookie session")
```

Count the cost. B avoided writing a duplicate handler and a wrong auth scheme. A was interrupted
exactly once, correctly. Neither agent was ever blocked, and neither one read the board. The
human watching saw all of it happen live.

---

## Quick reference

**Do**

- `join_team` once, `start_session` per piece of work, `end_session` when you stop.
- `declare_intent` **before** you touch anything, with real paths and a real sentence.
- `heartbeat` every ~60s and around every batch of edits, with a status line that says *why*.
- Drop paths as soon as you are done with them.
- Read `pending` on every response. Act when non-zero, do nothing when zero.
- Answer `no_conflict` when it is `no_conflict`, with honest confidence.
- Tell your human what metiche told you.

**Don't**

- Don't claim `app/**` or `src/**`. It is capped at `low` and it makes you noise.
- Don't declare after the edit.
- Don't poll `get_team_state` on a timer.
- Don't go quiet for twenty minutes mid-refactor.
- Don't inflate a verdict or a confidence to be safe.
- Don't put a secret, a token or a credential in any field.
- Don't acknowledge a conflict and then change nothing and say nothing.
