package view

// Copy blocks for the landing page.
//
// These are here rather than inline in landing.templ for one reason: they are
// JSON, and JSON is mostly braces, which is templ's expression syntax. Keeping
// them as pre-highlighted HTML fragments rendered through templ.Raw is less
// noisy than escaping every brace at the call site.
//
// Nothing in this file is dynamic and nothing in it is a credential. No join
// code and no token is ever shown — both are bearer secrets in a short
// costume, and a landing page is the last place either should appear.
//
// The MCP config below shows the token as a placeholder, "mtk_…", and never a
// value. The installer joins once per assistant, over the network, and writes
// each assistant's OWN token into that assistant's config in your home
// directory: `claude mcp add` for Claude Code, ~/.cursor/mcp.json, Windsurf's
// mcp_config.json, and Codex's config.toml (in http_headers). Nothing goes in
// a repository, and ~/.metiche/env keeps only the anchor token, at mode 0600.
// A join code is an INVITE, not a credential: send one as a bearer and the
// server answers 401.

// mcpConfigHTML is a representative entry the installer writes — Cursor's,
// which has the common JSON shape.
const mcpConfigHTML = `<span class="c">// ~/.cursor/mcp.json — written by the installer,</span>
<span class="c">// one entry per assistant, each with its own token</span>
{
  <span class="k">"mcpServers"</span>: {
    <span class="k">"metiche"</span>: {
      <span class="k">"type"</span>: <span class="s">"http"</span>,
      <span class="k">"url"</span>:  <span class="s">"https://mcp.metiche.xyz/v1/mcp"</span>,
      <span class="k">"headers"</span>: {
        <span class="k">"Authorization"</span>: <span class="s">"Bearer </span><span class="hi">mtk_…</span><span class="s">"</span>
      }
    }
  }
}`

// agentLoopHTML is the cadence the skill teaches, in the order it happens.
const agentLoopHTML = `<span class="c">// once per piece of work, read from git</span>
<span class="k">start_session</span>(repo_url, project_key, branch, goal)

<span class="c">// before touching anything; paths from the git root</span>
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

// installHTML is the one command. It is the whole of step two: the installer
// registers the MCP server AND installs the skill, which is why the landing
// page no longer walks through those as separate things to do.
//
// No prompt glyph in front of it: a reader who selects the line by hand
// should get exactly the command, the same bytes the copy button writes.
const installHTML = `<span class="c-str">curl</span> -fsSL https://metiche.xyz/install.sh <span class="c-dim">|</span> sh`

// installCommand is installHTML as plain text — what the copy button puts on
// the clipboard. landing_test.go keeps the two from drifting apart.
const installCommand = `curl -fsSL https://metiche.xyz/install.sh | sh`
