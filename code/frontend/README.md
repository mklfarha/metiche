# metiche board

The live team board: a lane per member → their agents → what each one is doing
right now, the paths it holds, and the conflicts it is in. Go, `templ`, htmx and
SSE. No JS framework and no build step beyond `templ generate`.

## Run it

```sh
cd code/frontend && go run ./cmd/metiche-web
```

No arguments, no setup, no database. It listens on **http://localhost:8787** and
replays the embedded fixture recordings, so the board is populated the moment it
opens and keeps changing for about two minutes as the recorded story plays out.

* `/` — the landing page, with a "see a live demo" link to `/t/demo`
* `/teams` — the demo recordings (this page never lists a real team)
* `/t/demo` — **the board** (team "Orbital Freight", three members, five agents)
* `/t/demo/graph` — the entanglement graph: who is working on the same things
* `/t/demo/contracts` — the produces/consumes matrix
* `/t/demo/conflicts`, `/t/demo/decisions`, `/t/demo/runs`
* `/t/demo-tidewater` — a team with nobody working, i.e. the empty state
* `/healthz` — feed name and both cursors per demo team; live teams are only counted

Every recording is served as a **demo board**: its slug is `demo` or
`demo-<recorded slug>`, and every page of it carries a "DEMO · a recording"
banner so nobody mistakes it for a real team.

Useful flags: `-addr`, `-speed` (replay multiplier), `-warmup` (events delivered
instantly at start), `-loop`, `-fixtures <dir>` to replay your own recordings.

## Running against the real backend (mixed mode)

```sh
go run ./cmd/metiche-web -backend https://api.metiche.xyz
```

That serves three kinds of board at once:

* **Demo boards** (`-demo`, default true): the embedded recordings, exactly as
  above, looping forever so the demo always has motion. They are registered
  first and their slugs (`demo`, `demo-*` as recorded) are reserved: a
  registered slug is never looked up on the backend, and a `-teams` entry that
  collides with one is refused and logged. The backend can mint any
  `[a-z0-9-]` slug, so no route spelling is truly outside its namespace; the
  `demo` prefix keeps the reserved set tiny and legible in the address bar.
* **Discovered live boards** (`-discover`, default true): the first request for
  an unregistered `/t/<slug>` asks the backend `GET /v1/teams/<slug>`. On 200
  the board registers a live feed and serves it; on 404 it serves the normal
  not-found page. The backend answers 404 both for "no such team" and for
  "private and you may not read it", deliberately, and the board does not try
  to tell them apart. Safeguards: the slug must look like a slug before any
  request is made; concurrent first requests share one probe and one feed; a
  404 is remembered for `-discover-notfound-ttl` (30s) and a backend error for
  5s (served as 503, not as "no such team"); at most `-max-discovered-teams`
  (50) are registered this way, after which new slugs are not probed at all.
  Discovered teams stay registered until the process restarts.
* **Pre-warmed live boards** (`-teams`, optional): slugs registered at startup,
  which only saves the first visitor the discovery round trip.

**Only public teams are discoverable in practice.** The board sends a bearer
token only if `METICHE_BOARD_TOKEN` is set, and production has none today, so
every private team is a 404 to the board exactly as it is to a stranger. A
viewer gate that lets a member open their private team's board is a known
backlog item, not something this service does.

**Real teams never appear on a public page.** `/`, `/teams`, `/join` and
`/healthz` are built from demo teams only: the landing link opens a demo,
`/teams` lists demos and says your own board is at `/t/<your-team-slug>`, and
`/join?code=` matches demo codes only — a live team's invite codes are the
backend's business and the board never holds one.

To give the board a credential:

```sh
export METICHE_BOARD_TOKEN=…            # the read API's bearer token
go run ./cmd/metiche-web -backend https://api.metiche.xyz
```

The token is read from the environment and never from a flag: a bearer token
passed as an argument lands in shell history and in every `ps` on the box.
`-backend-token-env` renames the variable it is read from; the value is never a
flag, never a file in this repo, and never logged. The process clears the
variable from its own environment once it has read it.

Other live-mode flags: `-demo`, `-discover`, `-max-discovered-teams`,
`-discover-notfound-ttl`, `-teams` (comma-separated pre-warm slugs) and
`-backfill` (how many events of history to pull into the timeline behind the
snapshot cursor, default 200; `0` starts the rail empty and resumes exactly at
the snapshot).

`internal/feed` is the seam. `Feed` is a two-method interface; `Fixture`
replays JSONL and `Live` reads the real backend. Nothing above that package
knows which one it has.

The two implementations are not symmetrical, and the asymmetry is the design
rather than an omission. A fixture recording carries the whole world in its
event payloads, so folding the log **is** the state. The backend's frames
deliberately do not: they carry a sequence, a kind, whether it was structural,
the subject's short key and a one-line summary — what changed, not what the
thing now is. So `Live` also implements `feed.Snapshotter`, and the live path
runs:

```
GET /v1/teams/{slug}          state, coherent at sequence S   (+ /contracts, /decisions)
GET …/stream?after=S          S+1, S+2, … once each, in order
```

The snapshot is read first because its sequence is the resume cursor.
Everything at or below it is in the state, everything above it arrives on the
stream, and `state.Store.Apply` rejects anything not strictly newer — so the
boundary has no gap and an overlap costs nothing. A structural frame (the
`board_revision` cursor, not `sequence`) triggers a re-read of the snapshot;
a status-line edit does not. Reconnection re-issues `?after=<last delivered>`,
which is exact.

Loading a snapshot switches the store off event folding, because folding a
thin live frame would invent rows rather than merely miss fields. Fixture mode
never loads one and is untouched by any of this — see
`TestFixtureModeStillFolds`.

`internal/feed/wire.go` is the adapter, and its doc comment is the current list
of fields the read API does not expose (a conflict's paths, an agent key, a
session's base commit, contract assertion fields), with what the board does
instead of inventing them.

## The realtime mechanic

Two cursors, two behaviours, one stream. `sequence` advances on every event;
`board_revision` advances only on structural ones. The SSE endpoint emits two
named event types, both carrying **rendered HTML** — the browser does no
templating:

| event | carries | client |
|---|---|---|
| `timeline` | one `<li>` | `sse-swap="timeline"` + `hx-swap="beforeend"` |
| `board` | the whole board container | `sse-swap="board"` + `hx-swap="outerHTML"` |

The graph rides the same two names: the `board` frame carries the graph as an
out-of-band element beside the board, and the page's wrapper class
(`.view-board` / `.view-graph`) decides which one is on screen. One stream, two
event names, two views.

A heartbeat costs one small `<li>` and does not repaint a board somebody is
reading. Reconnection is exact: every frame carries `id: <sequence>`, the
browser replays it as `Last-Event-ID`, the server resumes from there
(`?after=` does the same for a cold connect), and the store drops any sequence
it has already applied — so nothing is missed and nothing is applied twice.

## Layout

```
cmd/metiche-web/   main; flags, team discovery, graceful shutdown
internal/model/    the read shapes: team, member, agent, session, intent,
                   claim, contract, decision, conflict, event
internal/feed/     Feed interface + Fixture (JSONL replay) + Live (backend SSE)
internal/state/    folds events into a snapshot; derives lanes, the matrix
                   and the entanglement graph layout (graph.go)
internal/wording/  every cadence-dependent string, in one map
internal/hub/      renders fragments and fans them out; owns the two cursors
internal/view/     templ components (.templ and generated _templ.go, both committed)
internal/web/      chi router, pages, the SSE endpoint, the board controls,
                   on-demand live team discovery (discovery.go)
static/            vendored htmx + SSE extension, stylesheet, icon, ~30 lines of JS
fixtures/          recorded event streams
```

After editing a `.templ`, run `templ generate` and commit both files.

## Cadence

`project.cadence` is `hackathon | sprint | steady` and it changes two things:
how long a finding may sit before the board calls it overdue (5 minutes / 2
hours / 1 day), and the register the suggested action is written in. The same
finding needs different words — "worth assigning at standup" is sound on a
steady project and useless at a hackathon, where the next planning meeting is
never.

Every cadence-dependent string lives in **one map** in
`internal/wording/wording.go`, keyed by `(cadence, message kind)`, so the
register is tuned in one file rather than rediscovered inside five templates.
Facts are substituted in; if a phrase is missing the board falls back to the
backend's own suggested action rather than inventing advice. The cadence
selector sits in the header on every page — a setting that changes what the
board tells people to do is invisible until its advice is wrong.

## The entanglement graph

`/t/{slug}/graph`. Not a DAG of steps — metiche is not about sequence, it is
about who is working on the same things as whom. Sessions on the left, the
artifacts they touch on the right (path areas collapsed to a common prefix, not
one node per file), an edge for every claim or assertion. Where two sessions
reach the same artifact the node is the crossing: violet, heavier, with a ring
coloured by the conflict's severity. A contract with a consumer and no producer
is the cyan spark, with a dashed cyan edge back to whoever is depending on it.

Layout is computed in Go and emitted as inline SVG — no graph library, no
physics. Positions depend only on the node set and a stable sort, because a
node that jumps on every repaint is unreadable and the board repaints often.

## Design

The palette is the product icon — an eye drawn as two graph strokes crossing —
used as meaning rather than decoration. **Coral** is one side of a collision and
**indigo** the other (challenger/holder, consumer/producer). **Violet** is where
they cross, so every conflict is violet: the contested path, the mismatched
cell, the critical badge. **Cyan** is the spark: something that needs a person
and has no other side yet, which in practice means the unclaimed contract.
Severity reads without text — low is a muted outline, medium cyan, high coral,
critical solid violet — and each carries its own glyph (`○ ● ▲ ◆`) so it
survives greyscale. The board is navy and white until something earns colour.
