package view

import "github.com/a-h/templ"

// The public docs at /docs: a manual for people, served by the same process as
// the landing page and styled like it.
//
// Every behavioural claim on these pages is checked against the deployed code
// (the backend's app/mcp, app/browser, app/authz, install.sh and this board),
// and nothing that is not built is described as working. Unbuilt features are
// named only under a "Coming next" badge.
//
// Adding a page is two steps: write a templ component in docs_pages.templ and
// add one DocPage below. The order of docPages is the sidebar order and the
// order of the previous / next links. A command-line page belongs here once
// the CLI ships; until then Getting started carries a one-paragraph note and
// no commands.

// DocPage is one page of the docs.
type DocPage struct {
	// Slug is the URL segment: /docs/<slug>.
	Slug string
	// Title is the page's <h1> and the first half of its <title>.
	Title string
	// Nav is the short label in the sidebar.
	Nav string
	// Summary is the one line shown for the page on the docs index.
	Summary string

	body func() templ.Component
}

// URL is the page's path.
func (p DocPage) URL() string { return "/docs/" + p.Slug }

var docPages = []DocPage{
	{Slug: "getting-started", Title: "Getting started", Nav: "Getting started",
		Summary: "Install, create or join a team, which assistants get configured, and opening your board signed in.",
		body:    docGettingStarted},
	{Slug: "inviting-teammates", Title: "Inviting teammates", Nav: "Inviting teammates",
		Summary: "Make a join code from the board or through your assistant, what the limits are, revoking, and how a teammate joins.",
		body:    docInviting},
	{Slug: "the-board", Title: "The board", Nav: "The board",
		Summary: "Lanes and subagents, conflicts and how they get settled, run history, signing in and out, and your account page.",
		body:    docBoard},
	{Slug: "repos-and-teams", Title: "Repos and teams", Nav: "Repos and teams",
		Summary: "The one-time question about putting a repository on a team, the .metiche file, and working on several teams.",
		body:    docReposTeams},
	{Slug: "working-with-agents", Title: "Working with agents", Nav: "Working with agents",
		Summary: "What your agent does on its own, claiming files narrowly, delegating to subagents, and when it asks you.",
		body:    docAgents},
	{Slug: "privacy-and-security", Title: "Privacy and security", Nav: "Privacy and security",
		Summary: "Who can see a board, sign-in links and cookies, what is stored and what never is, and why invites are door codes.",
		body:    docPrivacy},
	{Slug: "troubleshooting", Title: "Troubleshooting", Nav: "Troubleshooting",
		Summary: "An assistant that is not connected, a board that is a 404, installer refusals, the session cap and confirm_repo_binding.",
		body:    docTroubleshooting},
	{Slug: "tool-reference", Title: "Tool reference", Nav: "Tool reference",
		Summary: "Every tool the metiche MCP server offers, one line each, grouped, and what is coming next.",
		body:    docTools},
}

// DocPages returns every docs page in sidebar order.
func DocPages() []DocPage { return append([]DocPage(nil), docPages...) }

// LookupDoc finds a docs page by slug.
func LookupDoc(slug string) (DocPage, bool) {
	for _, p := range docPages {
		if p.Slug == slug {
			return p, true
		}
	}
	return DocPage{}, false
}

// docPrev and docNext are the pages before and after slug in sidebar order.
func docPrev(slug string) (DocPage, bool) { return docOffset(slug, -1) }
func docNext(slug string) (DocPage, bool) { return docOffset(slug, +1) }

func docOffset(slug string, by int) (DocPage, bool) {
	for i := range docPages {
		if docPages[i].Slug == slug {
			if j := i + by; j >= 0 && j < len(docPages) {
				return docPages[j], true
			}
			break
		}
	}
	return DocPage{}, false
}

// Code blocks. Kept as strings so templ renders them escaped and with their
// line breaks intact. Every secret is a placeholder in angle brackets.
const (
	docInstall     = "curl -fsSL https://metiche.xyz/install.sh | sh"
	docInstallJoin = "curl -fsSL https://metiche.xyz/install.sh | METICHE_JOIN_CODE=<join code> sh"
	docInstallMake = "curl -fsSL https://metiche.xyz/install.sh | METICHE_TEAM_NAME=\"<team name>\" sh"
	docMeticheFile = "team = test-team\nproject = shop"
	docLoop        = `start_session      once per piece of work: repo_url, project_key, branch, goal
  declare_intent   before each chunk: one sentence, the files, the mode
    heartbeat      about every 60 seconds, with a status line that says why
    update_intent  as the work moves: add_paths, drop_paths, done
end_session        when the work is finished or stopped`
	docSessionCap = "this agent already has 8 open sessions, the most one agent may hold: <the sessions> — end the ones that are not this terminal with end_session, then call start_session again"
	docBindingQ   = "Should work in <repo> go on team <team name>'s board, where its members can see it?"
	docEnvReload  = ". ~/.metiche/env"
)
