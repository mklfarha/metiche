package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	teammod "github.com/mklfarha/metiche/backend/core/module/team"
	team_types "github.com/mklfarha/metiche/backend/core/module/team/types"
	team_entity "github.com/mklfarha/metiche/backend/entity/team"
	"github.com/mklfarha/metiche/backend/enums"
)

// ─────────────────────────────────────────────
// The ordering rule (no database needed)
// ─────────────────────────────────────────────

// TestSortTeamChoicesPutsTheMostRecentFirst covers the rule that decides which
// entry the model reads hardest.
//
// A list is not a set to a language model: the first item gets the attention
// and the last one gets skimmed, so "most recently active for THIS account"
// has to be first or the ordering is actively working against the decision.
func TestSortTeamChoicesPutsTheMostRecentFirst(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	rows := []teamChoiceRow{
		{choice: TeamChoice{Slug: "stale-one"}, lastActive: now.Add(-72 * time.Hour)},
		{choice: TeamChoice{Slug: "yesterday"}, lastActive: now.Add(-24 * time.Hour)},
		{choice: TeamChoice{Slug: "an-hour-ago"}, lastActive: now.Add(-time.Hour)},
		{choice: TeamChoice{Slug: "right-now"}, lastActive: now},
	}
	sortTeamChoices(rows)

	want := []string{"right-now", "an-hour-ago", "yesterday", "stale-one"}
	for i, w := range want {
		if rows[i].choice.Slug != w {
			t.Fatalf("position %d is %q, want %q (full order: %v)", i, rows[i].choice.Slug, w, slugsOf(rows))
		}
	}
}

// TestSortTeamChoicesIsStableOnTies: two teams with identical timestamps is
// exactly what a freshly seeded account looks like, and a list that comes back
// in a different order on every call is one a model cannot reason about.
func TestSortTeamChoicesIsStableOnTies(t *testing.T) {
	same := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	build := func() []teamChoiceRow {
		return []teamChoiceRow{
			{choice: TeamChoice{Slug: "zulu"}, lastActive: same},
			{choice: TeamChoice{Slug: "alpha"}, lastActive: same},
			{choice: TeamChoice{Slug: "mike"}, lastActive: same},
		}
	}
	first := build()
	sortTeamChoices(first)
	if got := slugsOf(first); strings.Join(got, ",") != "alpha,mike,zulu" {
		t.Fatalf("ties broke as %v, want them broken on slug", got)
	}
	// Same input, same answer — twice, because "stable" is a promise about
	// repeated calls and not about one call.
	for i := 0; i < 2; i++ {
		again := build()
		sortTeamChoices(again)
		if strings.Join(slugsOf(again), ",") != strings.Join(slugsOf(first), ",") {
			t.Fatalf("the same input sorted differently on attempt %d: %v then %v",
				i, slugsOf(first), slugsOf(again))
		}
	}
}

// TestLastActiveOnTakesTheLatestSignal covers the key the ordering sorts on,
// including the case the floor exists for: a membership joined minutes ago
// with no sessions behind it is a BETTER guess for the repo being opened now
// than one joined last year, so it must not sort to the bottom.
func TestLastActiveOnTakesTheLatestSignal(t *testing.T) {
	base := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	at := func(d time.Duration) sql.NullTime { return sql.NullTime{Time: base.Add(d), Valid: true} }

	cases := []struct {
		name       string
		joined     time.Time
		seen, work sql.NullTime
		want       time.Time
	}{
		{"a session beat most recently", base.Add(-10 * time.Hour), at(-5 * time.Hour), at(-time.Minute), base.Add(-time.Minute)},
		{"the membership was touched most recently", base.Add(-10 * time.Hour), at(-time.Minute), at(-5 * time.Hour), base.Add(-time.Minute)},
		{"joined and never used anything", base.Add(-2 * time.Minute), sql.NullTime{}, sql.NullTime{}, base.Add(-2 * time.Minute)},
		{"joined long ago, never used", base.Add(-8760 * time.Hour), sql.NullTime{}, sql.NullTime{}, base.Add(-8760 * time.Hour)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := lastActiveOn(tc.joined, tc.seen, tc.work); !got.Equal(tc.want) {
				t.Errorf("lastActiveOn = %v, want %v", got, tc.want)
			}
		})
	}

	// And the floor actually changes the ORDER, which is the thing that
	// matters: a brand-new membership outranks an old unused one.
	rows := []teamChoiceRow{
		{choice: TeamChoice{Slug: "last-year"}, lastActive: lastActiveOn(base.Add(-8760*time.Hour), sql.NullTime{}, sql.NullTime{})},
		{choice: TeamChoice{Slug: "joined-just-now"}, lastActive: lastActiveOn(base.Add(-2*time.Minute), sql.NullTime{}, sql.NullTime{})},
	}
	sortTeamChoices(rows)
	if rows[0].choice.Slug != "joined-just-now" {
		t.Errorf("a membership joined two minutes ago sorted behind one from last year: %v", slugsOf(rows))
	}
}

func slugsOf(rows []teamChoiceRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.choice.Slug)
	}
	return out
}

// ─────────────────────────────────────────────
// The single-vs-many note (no database needed)
// ─────────────────────────────────────────────

// TestListTeamsNoteTellsTheAgentWhichRuleApplies is the test for PLAN.md's
// binding rule.
//
// The note is not decoration and it is not a summary of the array. It is the
// branch itself — "exactly one, use it and announce once; more than one, stop
// and ask" — stated by the server so that the model never has to count the
// array and derive it. What is asserted here is that the words actually
// instruct: the single case must hand over the slug and must NOT tell the
// agent to ask, and the many case must tell it to stop and must name the
// candidates.
func TestListTeamsNoteTellsTheAgentWhichRuleApplies(t *testing.T) {
	t.Run("no teams", func(t *testing.T) {
		note := listTeamsNote(nil)
		for _, must := range []string{"not a member of any team", "create_team", "join_team"} {
			if !strings.Contains(note, must) {
				t.Errorf("the empty note never mentions %q: %s", must, note)
			}
		}
	})

	t.Run("exactly one team", func(t *testing.T) {
		note := listTeamsNote([]TeamChoice{{Slug: "orbital-freight", Name: "Orbital Freight", Role: "owner", Members: 1}})
		for _, must := range []string{"ONE", `team_slug="orbital-freight"`, "Orbital Freight", ".metiche"} {
			if !strings.Contains(note, must) {
				t.Errorf("the single-team note never mentions %q: %s", must, note)
			}
		}
		// PLAN.md: with one team there is nothing to disambiguate, so asking
		// would be friction for no information. The note must not send the
		// agent to the human.
		for _, mustNot := range []string{"ask the person", "STOP", "AMBIGUOUS"} {
			if strings.Contains(note, mustNot) {
				t.Errorf("the single-team note tells the agent to %q, which is friction for no information: %s", mustNot, note)
			}
		}
		// Automatic is fine; silent is not.
		if !strings.Contains(note, "once") {
			t.Errorf("the single-team note does not ask for the one-line announcement: %s", note)
		}
	})

	t.Run("more than one team", func(t *testing.T) {
		note := listTeamsNote([]TeamChoice{
			{Slug: "orbital-freight", Name: "Orbital Freight"},
			{Slug: "hack-night-2026", Name: "Hack Night"},
		})
		for _, must := range []string{"2 teams", "AMBIGUOUS", "STOP", "ask the person",
			"orbital-freight", "hack-night-2026", ".metiche"} {
			if !strings.Contains(note, must) {
				t.Errorf("the many-team note never mentions %q: %s", must, note)
			}
		}
		// Never derive a team from the directory name, the repo name or the
		// remote URL: a plausible guess that is wrong is worse than a question.
		for _, must := range []string{"directory name", "repo name", "remote URL"} {
			if !strings.Contains(note, must) {
				t.Errorf("the many-team note does not rule out guessing from the %s: %s", must, note)
			}
		}
	})

	// The rule is a function of the COUNT and nothing else, at every size.
	for n := 2; n <= 5; n++ {
		teams := make([]TeamChoice, 0, n)
		for i := 0; i < n; i++ {
			teams = append(teams, TeamChoice{Slug: string(rune('a'+i)) + "-team"})
		}
		if note := listTeamsNote(teams); !strings.Contains(note, "AMBIGUOUS") {
			t.Errorf("with %d teams the note does not say it is ambiguous: %s", n, note)
		}
	}
}

// ─────────────────────────────────────────────
// Against a real database
// ─────────────────────────────────────────────

// seedTeam creates a second (or third) team with its own invite, so a test can
// have teams the caller is deliberately NOT a member of.
func seedTeam(t *testing.T, hs *harness, name string) (uuid.UUID, string) {
	t.Helper()
	id := uuid.Must(uuid.NewV4())
	planID := hs.planID
	if _, err := hs.core.Team().Insert(context.Background(), team_types.UpsertRequest{
		Team: team_entity.Team{
			ID: id, Name: name, Slug: slugKey(name, 20) + "-" + id.String()[:8],
			Status: enums.RECORD_STATUS_ACTIVE, PlanUUID: &planID,
			PlanSource: enums.PLAN_SOURCE_INSTANCE_DEFAULT, Visibility: enums.TEAM_VISIBILITY_PRIVATE,
		},
	}, teammod.WithSkipCache()); err != nil {
		t.Fatalf("seeding team %q: %v", name, err)
	}
	code := "SEED" + strings.ToUpper(id.String()[:6])
	if _, err := hs.core.DB().Exec(
		"INSERT INTO `invite` (`id`,`team_uuid`,`code`,`uses`,`status`) VALUES (?,?,?,?,?)",
		uuid.Must(uuid.NewV4()).String(), id.String(), code, 0, enums.INVITE_STATUS_ACTIVE); err != nil {
		t.Fatalf("seeding the invite for %q: %v", name, err)
	}
	return id, code
}

// joinWithCode runs the real join_team tool against an arbitrary invite code.
// The harness's own join is hard-wired to its one team; these tests need more
// than one.
func joinWithCode(t *testing.T, hs *harness, base context.Context, carried, code, memberName, clientKey string) (context.Context, string) {
	t.Helper()
	res, _, err := hs.h.JoinTeam(base, nil, JoinTeamParams{
		JoinCode: code, MemberName: memberName, AgentLabel: "test", ClientKey: clientKey,
	})
	if err != nil {
		t.Fatalf("join_team with a seeded code: %v", err)
	}
	var out JoinTeamResult
	decodeResult(t, res, &out)
	token := out.Token
	if token == "" {
		token = carried
	}
	if token == "" {
		t.Fatal("join_team neither minted a token nor was given one")
	}
	return hs.ctxForToken(t, token), token
}

func listTeams(t *testing.T, hs *harness, ctx context.Context) ListTeamsResult {
	t.Helper()
	res, _, err := hs.h.ListTeams(ctx, nil, ListTeamsParams{})
	if err != nil {
		t.Fatalf("list_teams: %v", err)
	}
	var out ListTeamsResult
	decodeResult(t, res, &out)
	t.Logf("list_teams -> %s", resultText(t, res))
	return out
}

// backdate moves a membership's timestamps, so a test can assert the ORDERING
// rule against real rows. Both columns move together because the recency key
// floors on created_at: leaving it at "now" would drown the signal.
func backdate(t *testing.T, hs *harness, accountID string, teamID uuid.UUID, at time.Time) {
	t.Helper()
	if _, err := hs.core.DB().Exec(
		"UPDATE `member` SET `created_at` = ?, `last_seen_at` = ? WHERE `account_uuid` = ? AND `team_uuid` = ?",
		at.UTC(), at.UTC(), accountID, teamID.String()); err != nil {
		t.Fatalf("backdating the membership: %v", err)
	}
}

// backdateSessions parks every session on a team in the past, so a test can
// take the "I am working here right now" signal out of the recency key and
// leave the membership timestamps to decide. It deliberately does not touch
// `status`, because active_session is a separate signal and the tests assert
// that the two did not get collapsed.
func backdateSessions(t *testing.T, hs *harness, teamID uuid.UUID, at time.Time) {
	t.Helper()
	if _, err := hs.core.DB().Exec(
		"UPDATE `session` SET `last_heartbeat_at` = ? WHERE `team_uuid` = ?", at.UTC(), teamID.String()); err != nil {
		t.Fatalf("backdating the sessions: %v", err)
	}
}

// TestIntegrationListTeamsNeverLeaksAnotherAccountsTeams is the isolation
// proof, and the reason this tool is allowed to answer without a team scope at
// all.
//
// Every other tool here ends in a membership check against a team the caller
// named. This one cannot — naming a team is the question — so the isolation
// lives entirely in the WHERE clause, and a clause is exactly the kind of
// safety that a later edit removes by accident. Ana is on one team, Bob is on
// another, and a third belongs to neither: neither of them may see a single
// row of the others.
func TestIntegrationListTeamsNeverLeaksAnotherAccountsTeams(t *testing.T) {
	hs := newHarness(t)

	// Team A is the harness team; Ana is its only member.
	ana := hs.join(t, "Ana", "client-ana")
	teamA, err := hs.h.teamByID(context.Background(), hs.teamID)
	if err != nil {
		t.Fatal(err)
	}

	// Team B belongs to Bob, a different account entirely.
	teamBID, codeB := seedTeam(t, hs, "Bobs Board")
	bobCtx, _ := joinWithCode(t, hs, context.Background(), "", codeB, "Bob", "client-bob")
	teamB, err := hs.h.teamByID(context.Background(), teamBID)
	if err != nil {
		t.Fatal(err)
	}

	// Team C exists and has nobody in it. Nobody may see it either — "teams
	// that exist" is not a question this tool answers.
	teamCID, _ := seedTeam(t, hs, "Nobody Home")
	teamC, err := hs.h.teamByID(context.Background(), teamCID)
	if err != nil {
		t.Fatal(err)
	}

	anaSees := listTeams(t, hs, ana.ctx)
	if len(anaSees.Teams) != 1 || anaSees.Teams[0].Slug != teamA.Slug {
		t.Fatalf("Ana sees %d team(s) %v, want only her own %q", len(anaSees.Teams), slugList(anaSees.Teams), teamA.Slug)
	}
	bobSees := listTeams(t, hs, bobCtx)
	if len(bobSees.Teams) != 1 || bobSees.Teams[0].Slug != teamB.Slug {
		t.Fatalf("Bob sees %d team(s) %v, want only his own %q", len(bobSees.Teams), slugList(bobSees.Teams), teamB.Slug)
	}

	// Said the other way round, over the whole serialized answer, so a field
	// added later that happened to carry a slug would fail this too.
	anaRaw := mustJSON(t, anaSees)
	for _, forbidden := range []string{teamB.Slug, teamB.Name, teamC.Slug, teamC.Name} {
		if strings.Contains(anaRaw, forbidden) {
			t.Errorf("Ana's answer mentions %q, which belongs to a team she is not a member of: %s", forbidden, anaRaw)
		}
	}
	bobRaw := mustJSON(t, bobSees)
	for _, forbidden := range []string{teamA.Slug, teamA.Name, teamC.Slug, teamC.Name} {
		if strings.Contains(bobRaw, forbidden) {
			t.Errorf("Bob's answer mentions %q, which belongs to a team he is not a member of: %s", forbidden, bobRaw)
		}
	}

	// A revoked membership is not a membership. Revocation is the soft removal
	// in this schema, so the row stays and only the flags say it is over — the
	// case a status-only filter would get wrong.
	if _, err := hs.core.DB().Exec(
		"UPDATE `member` SET `revoked_at` = ? WHERE `account_uuid` = ? AND `team_uuid` = ?",
		time.Now().UTC(), ana.account.ID.String(), hs.teamID.String()); err != nil {
		t.Fatal(err)
	}
	after := listTeams(t, hs, ana.ctx)
	if len(after.Teams) != 0 {
		t.Errorf("a revoked member still sees %v", slugList(after.Teams))
	}
	if !strings.Contains(after.Note, "not a member of any team") {
		t.Errorf("the note for a revoked member reads %q", after.Note)
	}

	// And the endpoint is public, the answer is not.
	if _, _, err := hs.h.ListTeams(context.Background(), nil, ListTeamsParams{}); err == nil {
		t.Error("list_teams answered a request with no token")
	}
}

// TestIntegrationListTeamsSingleThenMany walks the exact path PLAN.md
// describes, against real rows: one team is bound without a question, and the
// moment there are two the answer changes to "stop and ask".
func TestIntegrationListTeamsSingleThenMany(t *testing.T) {
	hs := newHarness(t)
	ana := hs.join(t, "Ana", "client-ana")
	teamA, err := hs.h.teamByID(context.Background(), hs.teamID)
	if err != nil {
		t.Fatal(err)
	}

	// ── One team ────────────────────────────────────────────────────────
	one := listTeams(t, hs, ana.ctx)
	if !one.OK || len(one.Teams) != 1 {
		t.Fatalf("want exactly one team, got %+v", one)
	}
	only := one.Teams[0]
	if only.Slug != teamA.Slug || only.Name != teamA.Name {
		t.Errorf("team = %q/%q, want %q/%q", only.Slug, only.Name, teamA.Slug, teamA.Name)
	}
	if only.Role != enums.MemberRole(enums.MEMBER_ROLE_MEMBER).String() {
		t.Errorf("role = %q, want %q", only.Role, enums.MemberRole(enums.MEMBER_ROLE_MEMBER).String())
	}
	if only.Members != 1 {
		t.Errorf("members = %d, want 1", only.Members)
	}
	if only.ActiveSession {
		t.Error("active_session is true before any session was started")
	}
	if !strings.Contains(one.Note, "ONE") || !strings.Contains(one.Note, teamA.Slug) {
		t.Errorf("the note does not resolve the single-team case: %s", one.Note)
	}
	if strings.Contains(one.Note, "AMBIGUOUS") {
		t.Errorf("the single-team note asks the human anyway: %s", one.Note)
	}

	// A live session on that team flips the disambiguator, and a teammate
	// moves the member count.
	if _, _, err := hs.h.StartSession(ana.ctx, nil, StartSessionParams{
		ProjectKey: "metiche", Goal: "build the login endpoint", ConfirmNewProject: "person"}); err != nil {
		t.Fatal(err)
	}
	hs.join(t, "Cass", "client-cass")

	withSession := listTeams(t, hs, ana.ctx)
	if len(withSession.Teams) != 1 {
		t.Fatalf("Ana is still on one team, got %v", slugList(withSession.Teams))
	}
	if !withSession.Teams[0].ActiveSession {
		t.Error("active_session is false while Ana has a live session on that team")
	}
	if withSession.Teams[0].Members != 2 {
		t.Errorf("members = %d after a second person joined, want 2", withSession.Teams[0].Members)
	}

	// ── A second team, joined with the SAME token ───────────────────────
	teamBID, codeB := seedTeam(t, hs, "Hack Night")
	anaCtx, _ := joinWithCode(t, hs, ana.ctx, ana.token, codeB, "Ana", "client-ana")
	teamB, err := hs.h.teamByID(context.Background(), teamBID)
	if err != nil {
		t.Fatal(err)
	}

	many := listTeams(t, hs, anaCtx)
	if len(many.Teams) != 2 {
		t.Fatalf("want 2 teams after joining a second, got %v", slugList(many.Teams))
	}
	if !strings.Contains(many.Note, "AMBIGUOUS") || !strings.Contains(many.Note, "STOP") {
		t.Errorf("the note does not stop the agent: %s", many.Note)
	}
	for _, slug := range []string{teamA.Slug, teamB.Slug} {
		if !strings.Contains(many.Note, slug) {
			t.Errorf("the note does not name candidate %q: %s", slug, many.Note)
		}
	}

	// ── Ordering, against real rows ─────────────────────────────────────
	// Backdated so the two memberships cannot land in the same DATETIME
	// second, which is what a freshly seeded pair otherwise does.
	now := time.Now().UTC()

	// (i) A session beat OUTRANKS the membership timestamps, and it has to:
	//     Ana's membership of team A is three hours old and her membership of
	//     team B is one hour old, but she has an agent working on team A right
	//     now. "Where was I last actually working" is the question, and the
	//     session is the only column that answers it.
	backdate(t, hs, ana.account.ID.String(), hs.teamID, now.Add(-3*time.Hour))
	backdate(t, hs, ana.account.ID.String(), teamBID, now.Add(-time.Hour))
	working := listTeams(t, hs, anaCtx)
	if working.Teams[0].Slug != teamA.Slug {
		t.Errorf("first entry is %q, want %q — she has a live session there, which outranks an older membership (order: %v)",
			working.Teams[0].Slug, teamA.Slug, slugList(working.Teams))
	}

	// (ii) Retire that signal — park the session's heartbeat back with the
	//      membership — and the membership timestamps decide instead, so the
	//      more recently joined team B comes first.
	backdateSessions(t, hs, hs.teamID, now.Add(-3*time.Hour))
	bFirst := listTeams(t, hs, anaCtx)
	if bFirst.Teams[0].Slug != teamB.Slug {
		t.Errorf("first entry is %q, want the more recently active %q (order: %v)",
			bFirst.Teams[0].Slug, teamB.Slug, slugList(bFirst.Teams))
	}

	// (iii) Flip the recency back and the order flips with it, which is what
	//       proves the order is read from the data rather than from the order
	//       the teams were joined in.
	backdate(t, hs, ana.account.ID.String(), hs.teamID, now.Add(-time.Minute))
	aFirst := listTeams(t, hs, anaCtx)
	if aFirst.Teams[0].Slug != teamA.Slug {
		t.Errorf("first entry is %q, want the more recently active %q (order: %v)",
			aFirst.Teams[0].Slug, teamA.Slug, slugList(aFirst.Teams))
	}

	// Recency and active_session are two different signals and must not have
	// been collapsed: the session is still LIVE throughout the three steps
	// above, including the one where team A sorted second.
	for _, team := range bFirst.Teams {
		if team.Slug == teamA.Slug && !team.ActiveSession {
			t.Error("backdating a heartbeat cleared active_session; the session is still live")
		}
	}

	// Still nothing but the five decision fields: no board, no claims, no
	// paths, and no sessions of anybody's but as a single boolean.
	raw := mustJSON(t, aFirst)
	for _, forbidden := range []string{`"claims"`, `"paths"`, `"sessions"`, `"events"`, `"goal"`, `"branch"`, `"members_list"`} {
		if strings.Contains(raw, forbidden) {
			t.Errorf("list_teams handed back %s: %s", forbidden, raw)
		}
	}
}

// mustJSON re-serializes an answer so a test can assert over the WHOLE of it
// rather than field by field — which is the only version of "it never leaks
// another account's team" that survives somebody adding a field later.
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("re-serializing the answer: %v", err)
	}
	return string(b)
}

func slugList(teams []TeamChoice) []string {
	out := make([]string, 0, len(teams))
	for _, t := range teams {
		out = append(out, t.Slug)
	}
	return out
}
