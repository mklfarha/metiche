---
name: metiche-teamwork
description: Work alongside other coding agents and humans on one repo through the metiche MCP server — declare what you are about to do before you do it, hold narrow path claims, heartbeat while you work, and act on the collisions metiche reports inside your own tool responses. Use whenever the metiche MCP tools are connected, and whenever the user says metiche, join code, team board, "who else is touching this", "don't step on the other agent", "we're working in parallel", hackathon team, invite a teammate, declare intent, claim paths, heartbeat, or pending instructions.
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
join_team              once per machine (the installer usually does it for you)
  start_session        once per bounded piece of work
    declare_intent     before each chunk of work — the main tool
      heartbeat        ~every 60s while working, always around a batch of edits
      update_intent    status line changed, scope changed, chunk finished
    declare_intent     next chunk
  end_session          when the work is done or you are stopping
```

Everything else — instructions from a human, inviting a teammate, opening the board — hangs off
that spine and is occasional.

### The tools

The server's own tool schemas are authoritative; read them. This is the map, not the signature
list, in the order you meet them.

| tool | when | note |
|---|---|---|
| `join_team` | once, at setup | a join code (or `team_slug` plus one of your tokens) → this agent's bearer token. The installer normally does this. |
| `create_team` | only when nobody has set the team up | makes the team, joins you, returns its first join code |
| `list_teams` | before `start_session`, when no `.metiche` file names the team | one team → use it; more than one → ask the person which |
| `start_session` | once per piece of work | `repo_url`, `project_key`, `branch` straight from git (see below), `base_commit`, `goal`. Returns your `session_key`. |
| **`declare_intent`** | **before each chunk** | summary + paths + mode. Creates the intent *and* its claims in one transaction. |
| `check_paths` | before exploring | read-only, no commitment: "who else is in here?" |
| **`heartbeat`** | **~every 60s** | alive + extend claims + status line + pending counts. Cheapest call in the system. |
| **`update_intent`** | **as things change** | status line, `add_paths` / `drop_paths`, mark done |
| `get_instructions` | when `pending.instructions > 0` | **not read-only** — reading is the delivery receipt |
| `report_back` | after acting on an instruction | `done` / `acknowledged` / `refused` / `blocked` / `not_applicable` + a note |
| `end_session` | when you stop | releases claims immediately instead of waiting for TTL |
| `get_team_state` | at session start, rarely after | scoped and paginated; not a substitute for the pending counts |
| `open_board` | when the person asks to see the board | one-time sign-in link; see below |
| `sign_out_browsers` | only when the person asks | lists signed-in browsers; signs out only on request |
| `health` | when a call fails with a transport error | tells "metiche is down" apart from "my session is broken" |
| `create_invite` | when the person asks to add someone | returns a join code **once**; see "Inviting a teammate" |
| `list_invites` | to see what is outstanding | never shows codes |
| `revoke_invite` | when a code is no longer wanted or leaked | takes an `invite_id` |

There is **no claim verb**. `declare_intent` makes the claims; `update_intent` adds, drops,
extends and completes them. One concept, not two.

### Coming next, not available yet

`publish_contract`, `record_decision`, `get_review_context`, `report_judgement` and
`resolve_conflict` are planned but **not on the server**. Never call them. If a note, a suggested
action or an instruction names one, skip that step and do what works today: read the conflict,
change course or coordinate, narrow or drop paths with `update_intent`, and answer instructions
with `report_back` (a request to judge a pair gets `blocked`, noting that judging is not built).

---

## Name the repository from git, never from a guess

Claims are scoped to a **project**, and a project is a repository. Two agents in one repository
that land on two projects cannot see each other at all: they edit the same file and no conflict is
ever raised. That happens when an agent invents a project name, or names the project after the
folder it happened to be started in — a parent of the repo, or a subfolder of it.

So read all three from git, run inside the repository:

| `start_session` field | send exactly |
|---|---|
| `repo_url` | the output of `git remote get-url origin` — omit it only if there is no remote |
| `project_key` | the basename of `git rev-parse --show-toplevel` (the git root folder's name) |
| `branch` | the output of `git branch --show-current` — omit it on a detached HEAD |

metiche matches projects by `repo_url` first (https or ssh, `.git`, letter case and any embedded
credentials are all normalized away), so with it every agent in the repository lands on one project
even if their keys differ. If `start_session` refuses because the key belongs to a different
repository, use the git root's basename, or omit `project_key` and send only `repo_url`. If its
note says it created a project while the team already has others, and one of those is this
repository, `end_session` and start again with the right `repo_url`.

**Which team, which project.** Before `start_session`, look for a `.metiche` file, walking up from your working directory to the git root. If you find one, pass its `team` as `team_slug` and its `project` as `project_key` (take `project` only from a file at or below the git root), always send `repo_url`, and add `confirm_new_project: "metiche_file"`. If `start_session` answers `code: "confirm_repo_binding"`, nothing went wrong and nothing was created: this repository isn't a project on that team yet, and putting its work on a board every member can see is your person's decision. Ask them, verbatim: "Should work in <repo> go on team <name>'s board, where its members can see it?" On yes, call `start_session` again with the same arguments plus `confirm_new_project: "person"`, then write `.metiche` at the git root with `team = <slug>` and `project = <key>` so nobody is asked again. On no, don't use metiche in this repository, or `create_team` for this work and start the session with that `team_slug`. Never pass `"person"` unless your person actually said yes.

**Every path is relative to the git root, never to your working directory.** Started in `~/src`
with the repository at `~/src/shop`? The file is `app/rest.go`, not `shop/app/rest.go`. Started in
`~/src/shop/app`? Still `app/rest.go`, not `rest.go`. That holds for `declare_intent`,
`update_intent` and `check_paths` alike — two agents spelling one file two ways never collide.

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
kind:     "implement"
external_ref: "ISSUE-412"          # if there is one — an exact issue match is the
                                    # highest-signal duplicate-work key in the system
```

`summary` is capped at 280 characters and it is read by other agents' models, not by a parser.
Write the sentence you would say to a teammate. If you have an issue id, always include it — two
agents on the same ticket is worth knowing immediately.

---

## Claim narrowly

Claim **the specific files you are about to edit**, or the narrowest folder that holds them.

- **A repo-wide pattern protects nothing.** `**`, `**/*.go`, `.` and `app/**` are accepted, but a
  pattern whose fixed part is at most one folder deep is over-broad: every conflict on it is
  capped at `low` severity, below the notify floor. It shows on the board and warns nobody, you
  included.
- **`*` does not cross `/`.** `*.go` is Go files at the repo root only. It is not every Go file.
- **Relative to the git root** (`git rev-parse --show-toplevel`), whatever directory you are in.
  An absolute path is refused (`path is absolute; claims are repo-relative`).
- **Widen as the work moves.** Start with what you know, then `update_intent(add_paths: [...])`
  when you find the next file. Added paths are collision-checked like a declaration.

```
good:  paths: ["app/rest.go", "app/mcp/intents.go"]   # the files you will edit
good:  paths: ["app/mcp/*.go"]                        # one package you really are all over
bad:   paths: ["**/*.go"]                             # repo-wide: recorded low, warns nobody
bad:   paths: ["*.go"]                                # root-level files only, not what you meant
bad:   the file's full path on your disk             # absolute: refused
```

- Use `mode: read` for files you are only reading. Read×read is *never* a conflict, so read
  claims are free and they are how a teammate learns you depend on their file.
- Use `mode: structural` for renames, moves and deletes. These are escalated deliberately: a
  rename breaks readers silently, and the people reading your file need to hear about it.
- **Drop what you finish.** `update_intent(drop_paths: [...])` the moment a file is done. Holding
  a file you are no longer editing is a false positive you are personally generating.

If you do not yet know which files you need, use `check_paths` first. It is read-only and commits
you to nothing — it answers "is anyone in here?" before you decide to be in here.

Paths are normalized on insert: no absolute paths, no `..`, a trailing `/` becomes `**`, and a
name with no wildcard and no dot (`app/mcp`) is read as a folder (`app/mcp/**`).
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
- **`instructions > 0` → call `get_instructions` now.** A human raised a nudge (stop, steer, a
  question), or somebody walked into files you hold. It has been sitting there since your last
  call. Act on it, then `report_back`.
- **`conflicts > 0` → a collision involves you.** If it was not in this response's `conflicts[]`,
  call `get_instructions` (a notice raised for you is delivered there) and `check_paths` on the
  files you hold to see who is in them. Then decide.
- **`reviews`** counts pairs waiting to be judged. Judging is not built yet (see "Coming next");
  there is nothing to call for it.

Polling `get_team_state` on a timer is the anti-pattern. It is scoped, paginated and meant for
session start, not for awareness. Awareness is the counts.

---

## Nobody is blocked. That is why the response matters.

When a conflict comes back, it is not a permission failure. Claims are advisory; you can proceed
through any of them. Ordering assigns **responsibility**, not permission: whoever declared later
is told synchronously, in `conflicts[]`, because they have the information in hand and have not
started yet. The agent already holding the files gets a `conflict_notice` through
`get_instructions`, when the collision is serious enough to interrupt and it belongs to a
different person.

Every conflict carries `with` (who), `paths` (where), `severity` and a `suggested_action`, because
*"you and Ana both hold auth.go"* is noise and *"Ana (backend) holds internal/auth/auth.go (write,
4m, feat/auth). You are both editing it: settle it between you — split the file or sequence the
work; ask your human only if you can't."* is signal.

Your obligation on receiving one is to **make a visible decision**:

- **Settle it between you** — the default, whether the other agent is a teammate's or your own
  human's second agent. Two agents can almost always do this in one exchange. **Split the file**:
  one of you moves its part into a new file, so the shared file keeps only a line or two. Or
  **sequence the work**: one goes first, the other waits for the release. Then
  `update_intent(drop_paths: [...], add_paths: [...])` so your claims match what you agreed, and
  say so in the status line so the board shows it.
- **Ask your human** — only when you genuinely can't settle it: you both have to rewrite the same
  code, or choosing between the two changes is a product decision. Tell them who you collided with
  and on what.
- **Proceed anyway, deliberately** — sometimes that is right. Keep the claim, and say why in
  `update_intent(status_line: ...)`: "same file, different function, I'll take the merge" is a
  perfectly good reason.

When the conflict reached you as an instruction, close it with
`report_back(instruction_key, outcome, note)`: `done` when you acted, `acknowledged` when you have
taken it on but are not finished, `refused` or `blocked` with a reason, `not_applicable` when it
does not concern you. That report is how the person who raised it sees your answer.

**When you settle it, `report_back` on the notice with a one-line note of what you agreed**, for
example *"Different feature, same file: I am moving my handler into internal/auth/refresh.go and
releasing auth.go in a few minutes; add your route after that."* That note becomes the explanation
on the board. Once the overlap is really gone — one of you drops the path, finishes the intent or
ends the session — metiche closes the conflict by itself and shows it as settled: who released
what, when, and what you said. While you both still hold overlapping paths it stays open, whatever
the note says.

What you must not do is read a conflict and change nothing and say nothing. The human watching
the board sees an agent that was told and carried on regardless, and from then on the whole system
reads as theatre.

And tell your human. A conflict that only exists in your tool transcript did not reach anyone.

---

## Never report secrets

Everything you send becomes team-visible state on a live web board, in an append-only event log,
and in other agents' tool responses. It is not a private channel and there is no redaction pass.

Never put into a `summary`, `status_line`, `goal`, `note` or path list:

- API keys, tokens, join codes, passwords, private keys, session cookies
- connection strings, or a URL with credentials in it
- `.env` **contents** (the path `.env` is fine, the values are not)
- customer data, personal data, or anything from a file you would not paste into a group chat

If you are editing a file that holds credentials, claim the path and describe the change
abstractly: `rotating the storage credential in deploy/prod.yaml` — never what it rotated to.

---

## When the person asks to see the board

Call `open_board` (with `team_slug` if you know it from `.metiche` or `list_teams`) and give the
person its `login_url`. That link signs **one** browser in as them. It works **once**, for **10
minutes**.

- If you can run commands, write the link into a private temporary file and open that file with
  the OS opener. Never put the link itself on a command line.
- Otherwise show it to the person once.
- Never paste it anywhere shared: not the repository, a commit, an issue, a PR, a chat channel,
  or any metiche field. `board_url` is the plain address, and that one is safe to share.

`sign_out_browsers` with no arguments only **lists** the browsers signed in to the account and
changes nothing. `session_key` signs one out; `all=true` signs out every one. Do either only when
the person asks.

Never pass a token in chat, yours or theirs. The sign-in link is the only thing you hand over, and
only to the person.

---

## Inviting a teammate

When the person asks to add someone to the team:

1. Call `create_invite`, optionally with a label, a use limit and an expiry (the tool's schema
   names the fields). Defaults are **1 use** and **7 days**. An owner can go up to 100 uses and
   30 days; a member up to 25 uses and 7 days.
2. The join code is in that response and nowhere else: `list_invites` never shows it again. Give
   it to the person you are working for, and they pass it on privately, the way they would send
   a password.
3. The teammate runs the installer, chooses **join** and pastes the code at the prompt:

   ```sh
   curl -fsSL https://metiche.xyz/install.sh | sh
   ```

   or, without a prompt, `curl -fsSL https://metiche.xyz/install.sh | METICHE_JOIN_CODE=<code> sh`.

`list_invites` shows what is outstanding. `revoke_invite` with an `invite_id` kills one at once:
do that when a code is no longer needed or went somewhere it should not have.

A join code is a key to the team. Never paste one into the repository, a commit, an issue, a PR,
a public chat, or any metiche field.

---

## Worked example: two agents, one collision

Ana and Beto are on the same repo with their own agents. Both have this skill and both are
pointed at the same metiche team. Ana's agent is **A** (label `backend`), Beto's is **B** (label
`ui`). Keys and wording are illustrative.

**1. A starts and declares.**

```
A → start_session(repo_url: "git@github.com:acme/shop.git", project_key: "shop",
                  branch: "feat/auth", base_commit: "9f2c1ab",
                  goal: "login endpoint")                      → session_key S-17
A → declare_intent(session_key: "S-17",
      summary: "Add POST /api/login — verify password, mint session, set cookie",
      paths: ["internal/auth/auth.go", "internal/auth/session.go"],
      mode: "write", kind: "implement", external_ref: "ISSUE-412")
    ← {"ok": true, "key": "INT-83", "pending": {...all zero}}
```

Clean. A gets to work, heartbeating each minute with a status line.

**2. B declares, and is told inside its own response.**

```
B → declare_intent(session_key: "S-22",
      summary: "Login screen — form, submit, call the login endpoint",
      paths: ["web/login.tsx", "internal/auth/auth.go"], mode: "write")
    ← {"ok": true, "key": "INT-91",
       "conflicts": [{
          "key": "CF-14", "kind": "path_overlap", "severity": "critical",
          "with": "Ana (backend)", "paths": ["internal/auth/auth.go"],
          "suggested_action": "Ana (backend) holds internal/auth/auth.go (write, 4m, feat/auth).
                               You are both editing it: settle it between you — split the file
                               or sequence the work; ask your human only if you can't."}],
       "pending": {"instructions": 0, "conflicts": 1, "reviews": 0}}
```

B has not opened an editor yet.

**3. B changes course.** B does not need `auth.go`; it assumed it would have to write the
handler itself. It drops the file, narrows to its own layer, and tells Beto:

```
B → update_intent(session_key: "S-22", intent_key: "INT-91",
      drop_paths: ["internal/auth/auth.go"], add_paths: ["web/api/session.ts"],
      status_line: "building the login form against Ana's POST /api/login, not my own handler")
```

The collision cost B one tool response and no code. That is the whole product. And because B no
longer holds `auth.go`, the overlap is gone: metiche closes CF-14 by itself as settled, and the
board shows who released what and when.

**4. A finds out on a bare heartbeat, having never polled.**

```
A → heartbeat(session_key: "S-17", status_line: "wiring the cookie into the login handler")
    ← {"ok": true, "pending": {"instructions": 1, "conflicts": 1, "reviews": 0},
       "note": "1 instruction(s) and 1 open conflict(s) involve you — call get_instructions"}
A → get_instructions(session_key: "S-17")
    ← {"instructions": [{"key": "IN-7", "kind": "conflict_notice", "with": "Beto (ui)",
        "paths": ["internal/auth/auth.go"], "severity": "critical",
        "suggested_action": "Beto (ui) holds internal/auth/auth.go ... Both editing it, they
                             arrived second: settle it between you — split the file or sequence
                             the work; ask your human only if you can't.",
        "report_back": true}]}
A → check_paths(session_key: "S-17", paths: ["internal/auth/auth.go"])
    ← {"holders": [], "note": "... nobody else is holding them right now ..."}
A → report_back(session_key: "S-17", instruction_key: "IN-7", outcome: "done",
      note: "Beto dropped auth.go and builds against my endpoint; I keep the handler")
```

This is A's **first and only** interruption of the whole exchange, and it took one answer.

**5. A finishes.**

```
A → update_intent(session_key: "S-17", intent_key: "INT-83", status: "done")
A → end_session(session_key: "S-17", outcome: "succeeded",
      note: "POST /api/login shipped with cookie session")
```

Count the cost. B avoided writing a duplicate handler. A was interrupted exactly once, correctly.
Neither agent was ever blocked, and neither one read the board. The humans watching saw all of it
happen live.

---

## Quick reference

**Do**

- `start_session` per piece of work, `declare_intent` per chunk, `heartbeat` and `update_intent`
  while you work, `end_session` when you stop.
- Send `repo_url` and `project_key` straight from git, and every path relative to the git root.
- `declare_intent` **before** you touch anything, with the specific files and a real sentence.
- `heartbeat` every ~60s and around every batch of edits, with a status line that says *why*.
- Drop paths as soon as you are done with them.
- Read `pending` on every response. Act when non-zero, do nothing when zero.
- Answer every instruction with `report_back`, including a refusal.
- Settle a collision with the other agent yourselves — split the file or sequence the work — and
  `report_back` a one-line note of what you agreed. Ask your human only if you can't.
- Tell your human what metiche told you.

**Don't**

- Don't call a tool that is not in the table above, whatever a note says.
- Don't make up a project_key, or name it after the folder you were started in.
- Don't claim `**`, `**/*.go` or `app/**`. They are capped at `low` and warn nobody.
- Don't send an absolute path, and don't expect `*.go` to reach past the repo root.
- Don't paste a join code anywhere but privately to the person you work for.
- Don't declare after the edit.
- Don't poll `get_team_state` on a timer.
- Don't go quiet for twenty minutes mid-refactor.
- Don't put a secret, a token or a credential in any field.
- Don't read a conflict and then change nothing and say nothing.
