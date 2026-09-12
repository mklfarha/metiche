# The demo

Two terminals, one laptop, one person. A real collision, caught before either
agent opens a file.

The repo is **taqueria_tracker** — a small nuzur-generated Go service for
tracking taquerías, tacos, salsas and visits. It is a good demo repo for one
reason that matters: like most generated projects, nearly all of its Go is
regenerated, and the hand-written surface is small. `app/rest.go` is the one
file every new endpoint has to touch. That is not a contrivance; it is the
shape that makes small teams queue.

## Why one person with two agents is a real demo, not a cheat

metiche's severity rules used to soften any collision between two agents
belonging to the same person, on the reasoning that the person was
coordinating them. That is true when you drive one agent at a time and false
when you fan out several at once — which is exactly when you have the least
idea what each of them is touching.

The rule now keys on concurrency. Two of your sessions both live means full
severity and both agents get told. So a single operator running two terminals
sees precisely what two teammates would see.

## The two asks

Both are obviously reasonable. Neither mentions the other. Neither would guess
they overlap — and that is the entire point.

**Terminal 1**

> Add a rating to a visit: a 1–5 score on `visita`, and expose it through the
> API so I can filter visits by rating.

**Terminal 2**

> Add a heat level to salsa — mild through "no te la acabas" — and expose it
> through the API so I can filter salsas by heat.

Different entities. Different migrations. Different tests. And both of them
have to add a route to `app/rest.go`.

Use two different branches — `feat/visita-rating` and `feat/salsa-heat`.
Same branch works too, but git would eventually show you that collision at
merge time, so metiche scores it one notch lower. Different branches is both
more realistic and the case where nothing else would have told you.

## What the audience should watch

1. **Terminal 1 declares and is clean.** It takes `entity/visita/**` and
   `app/rest.go`. Nothing happens, which is correct — there is nothing to say.

2. **Terminal 2 declares and is told immediately.** `critical`, on
   `app/rest.go`, naming the other agent, its branch, and what it is doing —
   inside the response to a call it was already making, before it has opened
   an editor. This is the moment. Changing course here costs thirty seconds;
   discovering it at merge costs an afternoon.

3. **Terminal 1 finds out without asking.** It never polls. On its next
   heartbeat the `pending` count is non-zero and it fetches the notice. Count
   the interruptions to terminal 1: one, and it was actionable.

4. **The board shows both lanes**, the held paths, and the conflict.

## Setup

One team, two agents, one account — the installer registers the second agent
against the same token with a different `client_key`, which is the documented
multi-agent path.

Point both terminals at the same metiche and the same repo, then give each one
its prompt.

If you want the two agents to be different products — Claude Code in one
terminal, Codex in the other — that works and makes a better story, because
`CODEX_HOME` redirects Codex's entire config directory, so it can hold its own
registration without disturbing your normal setup. metiche does not care which
vendor is on either end; it has never known.

## What to say when someone asks "couldn't a chatbot do this?"

No, and the reason is structural rather than rhetorical. A chatbot has no
shared state across the two terminals, is not present at the instant either
agent decides something, and has nothing to say to the *other* agent. metiche
is the only thing in the room that can see both at once and speak into each
one's own loop at the moment it is deciding.
