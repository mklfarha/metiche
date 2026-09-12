package mcp

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mklfarha/metiche/backend/enums"
)

// ─────────────────────────────────────────────
// Tool: list_teams
// ─────────────────────────────────────────────
//
// This tool exists for exactly one decision, described in PLAN.md under
// "Binding a repo to a team — ask, never infer": an agent has opened a repo and
// has to say which team the work belongs to.
//
// Why that decision needs a server call at all. The installer is machine-global
// and creates ONE account, and that account can be on several teams — a
// personal project and a hackathon, say. A `.metiche` file in the repo or in a
// parent directory answers the question when it is there, and when it is not
// the only alternatives are asking the server and guessing. PLAN.md is explicit
// that guessing is the worst failure this system has: a plausible guess that is
// wrong publishes private file paths, branch names and decisions onto a board
// other people can read, and nobody reviews a guess.
//
// So: `.metiche` first, this tool second, a question to the human third, and
// never the directory name, the repo name or the remote URL.

// listTeamsMaxTeams is a runaway guard, not a product limit. One person being
// on more than this many teams is not a case this product has; the cap is here
// so the response can never grow without bound if it ever happens.
const listTeamsMaxTeams = 200

// ListTeamsParams is empty on purpose. Every argument this tool could take —
// a team slug, a session key, a client key — is something the caller does not
// have yet. That is the situation the tool is for.
type ListTeamsParams struct{}

// TeamChoice is one option, carrying exactly what is needed to CHOOSE between
// options and nothing else.
//
// Deliberately absent: the board, anybody's claims, anybody's sessions but the
// caller's own, and every team this account is not a member of. This answer is
// read by a model that is about to pick a board to publish private work onto,
// and the two failure modes are opposite — too little and it guesses, too much
// and it skims. Five fields is what the decision takes.
type TeamChoice struct {
	// Slug is the answer. It is what the agent passes as team_slug, and what it
	// writes into a .metiche file.
	Slug string `json:"slug"`
	// Name is the display name, because a slug is not always recognisable to
	// the human who will be asked to choose.
	Name string `json:"name"`
	// Role is this account's role on this team — owner or member. A personal
	// project is usually the one you own.
	Role string `json:"role"`
	// Members is how many live members the team has. This is the single most
	// useful disambiguator between two plausible teams: a team of one is a
	// personal project, a team of six is other people's board.
	Members int `json:"members"`
	// ActiveSession says whether THIS account has a live session on this team
	// right now. If an agent of yours is already working there, this repo very
	// likely belongs there too.
	//
	// Live only, not stale: a stale session is by definition one nobody has
	// heard from, and offering it as evidence of "I am working here now" would
	// be the wrong tiebreaker to hand a model.
	ActiveSession bool `json:"active_session"`
}

// ListTeamsResult is not an Envelope, and that is a deliberate exception to the
// rule that every tool returns one.
//
// An Envelope carries `sequence` and `revision`, and both are cursors on ONE
// team. This call is the one that happens BEFORE a team has been chosen, so
// there is no team whose cursors these would be, and emitting zeros would read
// to a client as "you have missed every event". `pending` is likewise
// per-session and there is no session yet. What the envelope contract is
// actually for — one small, predictable shape with one line of prose for the
// model — is kept: `ok` and `note` are the same fields with the same meanings.
type ListTeamsResult struct {
	OK    bool         `json:"ok"`
	Teams []TeamChoice `json:"teams"`
	// Note tells the agent which of PLAN.md's two situations it is in. It is
	// not a summary of the array: the rule is decided HERE, on the server, so
	// that the agent never has to count the array and infer it.
	Note string `json:"note"`
}

// ListTeams answers "which teams is this account a member of?".
//
// Authenticated by ACCOUNT and nothing more, which is the only tool here that
// is. It stops at requireAccount instead of going on into RequireTeam or
// RequireSession, because both of those finish with a membership check against
// a team the caller NAMED — and naming a team is the thing this caller is
// trying to work out. See the account-scoped seam documented on requireAccount
// in auth.go; those four team-scoped resolvers are untouched and stay the only
// way to reach a team's data.
//
// The isolation that replaces the membership check is in the WHERE clause: the
// query is rooted at `member` filtered to this account, so a team this account
// is not a live member of has no row to produce and cannot appear whatever else
// is in the database.
func (h *Handler) ListTeams(ctx context.Context, _ *mcp.CallToolRequest, _ ListTeamsParams) (*mcp.CallToolResult, any, error) {
	acct, err := h.requireAccount(ctx)
	if err != nil {
		return nil, nil, err
	}

	rows, err := h.listTeamChoices(ctx, acct.ID.String())
	if err != nil {
		return nil, nil, err
	}
	sortTeamChoices(rows)

	out := ListTeamsResult{OK: true, Teams: make([]TeamChoice, 0, len(rows))}
	for _, r := range rows {
		out.Teams = append(out.Teams, r.choice)
	}
	out.Note = listTeamsNote(out.Teams)
	if len(out.Teams) == listTeamsMaxTeams {
		out.Note += fmt.Sprintf(" (only the first %d memberships are listed.)", listTeamsMaxTeams)
	}
	return jsonValue(out)
}

// teamChoiceRow is one answer row plus the sort key, which never reaches the
// response. The key is derived data and a client that could see it would start
// depending on it; what the caller is entitled to is the ORDER, which is the
// thing the rule is actually about.
type teamChoiceRow struct {
	choice TeamChoice
	// lastActive is when THIS account was last doing anything on this team.
	// Not when the team was last active: a hackathon team that six other people
	// are hammering is not thereby the team this repo belongs to.
	lastActive time.Time
}

// sortTeamChoices puts the team this account touched most recently first.
//
// The order is load-bearing rather than cosmetic. A model reads the first entry
// of a list most carefully and the last one barely at all, so the entry it
// reads hardest should be the one most likely to be right — and "the team I was
// working on an hour ago" beats every other cheap signal available here.
//
// Ties break on slug so that two teams with identical timestamps — which is
// what a freshly seeded account looks like — come back in the same order on
// every call. A list that reshuffles between two calls is a list a model
// cannot reason about.
func sortTeamChoices(rows []teamChoiceRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		if !rows[i].lastActive.Equal(rows[j].lastActive) {
			return rows[i].lastActive.After(rows[j].lastActive)
		}
		return rows[i].choice.Slug < rows[j].choice.Slug
	})
}

// listTeamsNote is PLAN.md's binding rule, written out for the model.
//
// The rule is decided here rather than left to the agent on purpose. "If there
// is exactly one, use it; otherwise ask" is a branch, and a branch the model
// has to execute for itself is a branch it will sometimes execute wrong — in a
// place where being wrong means private work on somebody else's board. The
// server knows the count, so the server states the conclusion.
func listTeamsNote(teams []TeamChoice) string {
	switch len(teams) {
	case 0:
		return "You are not a member of any team. Call create_team to start one, or join_team with a join code " +
			"somebody gave you. Do not start a session until you are on a team."
	case 1:
		t := teams[0]
		return fmt.Sprintf(
			"Exactly ONE team, so there is nothing to ask: bind this repo to team_slug=%q and pass that on every call. "+
				"Tell the person, once and in one line, that you bound this repo to %s — automatic is fine, silent is not. "+
				"Then write \"team = %s\" into a .metiche file in the repo so nobody is asked again.",
			t.Slug, t.Name, t.Slug)
	default:
		slugs := make([]string, 0, len(teams))
		for _, t := range teams {
			slugs = append(slugs, t.Slug)
		}
		return fmt.Sprintf(
			"You are on %d teams, so this is AMBIGUOUS: STOP and ask the person which of these this repo belongs to (%s). "+
				"Do not guess, and do not infer it from the directory name, the repo name or the remote URL — "+
				"declaring private work onto the wrong team's board is the worst failure this system has. "+
				"When they answer, write \"team = <slug>\" into a .metiche file in the repo so they are asked once, not once per session.",
			len(teams), strings.Join(slugs, ", "))
	}
}

// listTeamChoices reads this account's live memberships in one query.
//
// One query rather than a membership list followed by a lookup per team: this
// is the first call an agent makes in a repo, before it has done anything
// useful, and a round trip per team is latency the person is watching.
//
// Raw SQL because two of the five fields are aggregates over other tables and
// the generated fetch-by-index cannot express them. Every subquery is bounded
// by an id that is already restricted to a team this account belongs to.
func (h *Handler) listTeamChoices(ctx context.Context, accountID string) ([]teamChoiceRow, error) {
	rows, err := h.core.DB().QueryContext(ctx,
		"SELECT t.`slug`, t.`name`, m.`role`, m.`last_seen_at`, m.`created_at`, "+
			"(SELECT COUNT(*) FROM `member` peers WHERE peers.`team_uuid` = t.`id` "+
			"AND peers.`revoked_at` IS NULL AND peers.`status` = ?) AS member_count, "+
			"(SELECT COUNT(*) FROM `session` livesess WHERE livesess.`member_uuid` = m.`id` "+
			"AND livesess.`status` = ?) AS live_sessions, "+
			"(SELECT MAX(hist.`last_heartbeat_at`) FROM `session` hist WHERE hist.`member_uuid` = m.`id`) AS last_worked "+
			"FROM `member` m JOIN `team` t ON t.`id` = m.`team_uuid` "+
			// THE ISOLATION. Rooted at this account's own membership rows and
			// filtered to live ones, so a team the account is not a member of —
			// or has been revoked from — produces no row at all. There is no
			// path through this statement that reaches another account's teams.
			"WHERE m.`account_uuid` = ? AND m.`revoked_at` IS NULL AND m.`status` = ? AND t.`status` = ? "+
			"LIMIT ?",
		enums.RECORD_STATUS_ACTIVE, enums.SESSION_STATUS_LIVE,
		accountID, enums.RECORD_STATUS_ACTIVE, enums.RECORD_STATUS_ACTIVE, listTeamsMaxTeams)
	if err != nil {
		return nil, retryable(err, "listing your teams")
	}
	defer func() { _ = rows.Close() }()

	var out []teamChoiceRow
	for rows.Next() {
		var (
			slug, name             string
			role                   int64
			memberSeen, lastWorked sql.NullTime
			joinedAt               time.Time
			memberCount, liveCount int
		)
		if err := rows.Scan(&slug, &name, &role, &memberSeen, &joinedAt,
			&memberCount, &liveCount, &lastWorked); err != nil {
			return nil, err
		}
		out = append(out, teamChoiceRow{
			choice: TeamChoice{
				Slug:          slug,
				Name:          name,
				Role:          enums.MemberRole(role).String(),
				Members:       memberCount,
				ActiveSession: liveCount > 0,
			},
			lastActive: lastActiveOn(joinedAt, memberSeen, lastWorked),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, retryable(err, "listing your teams")
	}
	return out, nil
}

// lastActiveOn is the recency key: the latest of when this account joined the
// team, when its membership was last touched, and when any of its sessions on
// that team last beat.
//
// joinedAt is the floor rather than the zero time because a membership with no
// activity behind it is not evidence of nothing — a team joined ten minutes ago
// is a far better guess for the repo now being opened than a team joined last
// year and never used. Sorting the brand-new membership last would get exactly
// the common case backwards.
func lastActiveOn(joinedAt time.Time, memberSeen, lastWorked sql.NullTime) time.Time {
	latest := joinedAt.UTC()
	for _, t := range []sql.NullTime{memberSeen, lastWorked} {
		if t.Valid && t.Time.UTC().After(latest) {
			latest = t.Time.UTC()
		}
	}
	return latest
}
