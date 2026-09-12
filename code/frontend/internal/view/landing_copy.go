package view

// Copy blocks for the landing page.
//
// These are here rather than inline in landing.templ for one reason: they are
// JSON, and JSON is mostly braces, which is templ's expression syntax. Keeping
// them as pre-highlighted HTML fragments rendered through templ.Raw is less
// noisy than escaping every brace at the call site.
//
// Nothing in this file is dynamic and nothing in it is a credential. The join
// code is never shown — a real one is a bearer secret in a short costume, and
// a landing page is the last place it should appear. The MCP config below
// deliberately carries no token at all: the agent presents the join code once,
// through the join_team tool, and the server mints a per-agent token from it.

// mcpConfigHTML is the shape of the .mcp.json entry a teammate adds.
const mcpConfigHTML = `<span class="c">// .mcp.json — in the repo, committed, no secret in it</span>
{
  <span class="k">"mcpServers"</span>: {
    <span class="k">"metiche"</span>: {
      <span class="k">"type"</span>: <span class="s">"http"</span>,
      <span class="k">"url"</span>:  <span class="s">"https://mcp.metiche.xyz/mcp"</span>
    }
  }
}`

// agentLoopHTML is the cadence the skill teaches, in the order it happens.
const agentLoopHTML = `<span class="c">// once, at the start of a piece of work</span>
<span class="k">start_session</span>(branch, base_commit, goal)

<span class="c">// before touching anything</span>
<span class="k">declare_intent</span>(<span class="s">"add the booking form"</span>, paths, mode)
<span class="hi">→ conflicts[] comes back in the same response</span>

<span class="c">// as the work moves, and about once a minute</span>
<span class="k">update_intent</span>(status_line, add_paths, drop_paths)
<span class="k">heartbeat</span>()`

// envelopeHTML is the response envelope every tool returns, with the part that
// makes the product work highlighted: the conflict rides back inside the call
// the agent was making anyway.
const envelopeHTML = `<span class="c">// the response to the declare_intent your agent just made</span>
{
  <span class="k">"ok"</span>: <span class="ok">true</span>,
  <span class="k">"key"</span>: <span class="s">"INT-83"</span>,
  <span class="k">"sequence"</span>: 417,
  <span class="k">"revision"</span>: 88,
  <span class="k">"pending"</span>: { <span class="k">"instructions"</span>: 1, <span class="k">"conflicts"</span>: 1, <span class="k">"reviews"</span>: 0 },
  <span class="hi">"conflicts"</span>: [
    {
      <span class="k">"key"</span>: <span class="s">"CF-14"</span>,
      <span class="k">"kind"</span>: <span class="s">"path_overlap"</span>,
      <span class="k">"severity"</span>: <span class="hi">"high"</span>,
      <span class="k">"paths"</span>: [<span class="s">"api/router.go"</span>],
      <span class="k">"suggested_action"</span>: <span class="s">"Mara/api holds api/router.go</span>
        <span class="s">(write, 4m, feat/booking-api). Take api/bookings.go and</span>
        <span class="s">let the router land first."</span>
    }
  ]
}`
