package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/gofrs/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	teammod "github.com/mklfarha/metiche/backend/core/module/team"
	team_types "github.com/mklfarha/metiche/backend/core/module/team/types"
	team_entity "github.com/mklfarha/metiche/backend/entity/team"
	"github.com/mklfarha/metiche/backend/enums"
)

// Session keys are per team (uq_session_team_key), so one person on two teams
// has an S-5 on each. These tests pin down how a bare session_key resolves
// then, through the REAL transport: the caller's own agent first, live over
// finished next, team_slug when given, and a refusal naming the teams when
// two usable sessions are left.
//
// A session key is the team's event sequence at start (S-<seq>), so two teams
// only share a key by coincidence. The tests start sessions normally and then
// rename them to the colliding keys production had; the parent link and every
// other reference is by uuid, so a rename changes nothing else.

// otherTeam seeds a second team with an uncapped invite and returns its id,
// slug and join code.
func (hs *harness) otherTeam(t *testing.T) (uuid.UUID, string, string) {
	t.Helper()
	id := uuid.Must(uuid.NewV4())
	slug := "other-" + id.String()[:8]
	planID := hs.planID
	if _, err := hs.core.Team().Insert(context.Background(), team_types.UpsertRequest{
		Team: team_entity.Team{
			ID: id, Name: "Other team", Slug: slug,
			Status: enums.RECORD_STATUS_ACTIVE, PlanUUID: &planID,
			PlanSource: enums.PLAN_SOURCE_INSTANCE_DEFAULT, Visibility: enums.TEAM_VISIBILITY_PRIVATE,
		},
	}, teammod.WithSkipCache()); err != nil {
		t.Fatal(err)
	}
	code := "OTHR" + strings.ToUpper(id.String()[:6])
	if _, err := hs.core.DB().Exec(
		"INSERT INTO `invite` (`id`,`team_uuid`,`code`,`uses`,`status`) VALUES (?,?,?,?,?)",
		uuid.Must(uuid.NewV4()).String(), id.String(), code, 0, enums.INVITE_STATUS_ACTIVE); err != nil {
		t.Fatal(err)
	}
	return id, slug, code
}

// joinWith redeems code as the person behind ctx (a new person when ctx
// carries no identity) for clientKey, and returns the token to use.
func (hs *harness) joinWith(t *testing.T, ctx context.Context, carried, code, name, clientKey string) string {
	t.Helper()
	res, _, err := hs.h.JoinTeam(ctx, nil, JoinTeamParams{
		JoinCode: code, MemberName: name, AgentLabel: "test", ClientKey: clientKey,
	})
	if err != nil {
		t.Fatalf("join_team %s: %v", clientKey, err)
	}
	var out JoinTeamResult
	decodeResult(t, res, &out)
	if out.TokenKept || out.Token == "" {
		return carried
	}
	return out.Token
}

// rekey renames sessions on one team, from -> to, in two phases so a target
// key that another session on the team currently holds cannot collide.
func rekey(t *testing.T, hs *harness, team uuid.UUID, renames map[string]string) {
	t.Helper()
	for from := range renames {
		if _, err := hs.core.DB().Exec("UPDATE `session` SET `key` = ? WHERE `team_uuid` = ? AND `key` = ?",
			"tmp-"+from, team.String(), from); err != nil {
			t.Fatalf("rekey %s: %v", from, err)
		}
	}
	for from, to := range renames {
		if _, err := hs.core.DB().Exec("UPDATE `session` SET `key` = ? WHERE `team_uuid` = ? AND `key` = ?",
			to, team.String(), "tmp-"+from); err != nil {
			t.Fatalf("rekey %s -> %s: %v", from, to, err)
		}
	}
}

func mustOK(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	res, text := callTool(t, cs, name, args)
	if res.IsError {
		t.Fatalf("%s %v: %s", name, args, text)
	}
	return text
}

func mustRefuse(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	res, text := callTool(t, cs, name, args)
	if !res.IsError {
		t.Fatalf("%s %v was accepted: %s", name, args, text)
	}
	return text
}

// startSessionOn starts a session on the named team over the transport and
// returns its envelope, checking start_session told the agent the slug.
func startSessionOn(t *testing.T, cs *mcp.ClientSession, slug string, extra map[string]any) Envelope {
	t.Helper()
	args := map[string]any{"project_key": "metiche", "team_slug": slug, "confirm_new_project": "person"}
	for k, v := range extra {
		args[k] = v
	}
	res, text := callTool(t, cs, "start_session", args)
	if res.IsError {
		t.Fatalf("start_session on %s: %s", slug, text)
	}
	var env Envelope
	decodeResult(t, res, &env)
	if env.Key == "" || env.TeamSlug != slug {
		t.Fatalf("start_session answered key %q team_slug %q, want team_slug %q: %s", env.Key, env.TeamSlug, slug, text)
	}
	return env
}

func sessionStatusOn(t *testing.T, hs *harness, team uuid.UUID, key string) enums.SessionStatus {
	t.Helper()
	var s int64
	if err := hs.core.DB().QueryRow("SELECT `status` FROM `session` WHERE `team_uuid` = ? AND `key` = ?",
		team.String(), key).Scan(&s); err != nil {
		t.Fatalf("reading %s: %v", key, err)
	}
	return enums.SessionStatus(s)
}

func intentsOn(t *testing.T, hs *harness, team uuid.UUID, key string) int {
	t.Helper()
	return countRows(t, hs.core.DB(),
		"SELECT COUNT(*) FROM `intent` i JOIN `session` s ON s.`id` = i.`session_uuid` "+
			"WHERE s.`team_uuid` = ? AND s.`key` = ?", team.String(), key)
}

// TestIntegrationSessionKeyPrefersLiveOverEndedAcrossTeams is the production
// failure: S-1..S-6 finished on one team, a supervisor S-3 and its subagent S-5
// live on the other, and every bare-key call from the subagent refused as
// "on more than one of your teams" — invisible to collision detection, unable
// to heartbeat or end.
func TestIntegrationSessionKeyPrefersLiveOverEndedAcrossTeams(t *testing.T) {
	hs := newHarness(t)
	endpoint, _ := realServer(t, hs)
	slugA := hs.teamSlug(t)
	teamB, slugB, codeB := hs.otherTeam(t)

	ana := hs.join(t, "Ana", "client-a")
	if tok := hs.joinWith(t, ana.ctx, ana.token, codeB, "Ana", "client-a"); tok != ana.token {
		t.Fatal("joining a second team with the same client should keep the token")
	}
	cs := connectAs(t, endpoint, ana.token)

	// Team A: six sessions, all ended, renamed S-1..S-6.
	renamesA := map[string]string{}
	for i := 1; i <= 6; i++ {
		env := startSessionOn(t, cs, slugA, nil)
		mustOK(t, cs, "end_session", map[string]any{"session_key": env.Key, "team_slug": slugA})
		renamesA[env.Key] = "S-" + string(rune('0'+i))
	}
	rekey(t, hs, hs.teamID, renamesA)

	// Team B: a live supervisor, and a live subagent linked under it, renamed
	// S-3 and S-5.
	sup := startSessionOn(t, cs, slugB, map[string]any{"goal": "supervisor"})
	sub := startSessionOn(t, cs, slugB, map[string]any{"goal": "subagent", "parent_session_key": sup.Key})
	if sub.ParentSessionKey != sup.Key {
		t.Fatalf("setup: subagent linked under %q, want %q", sub.ParentSessionKey, sup.Key)
	}
	rekey(t, hs, teamB, map[string]string{sup.Key: "S-3", sub.Key: "S-5"})
	if st := sessionStatusOn(t, hs, hs.teamID, "S-5"); st != enums.SESSION_STATUS_ENDED {
		t.Fatalf("setup: team A's S-5 is %s, want ended", st.String())
	}

	// The subagent's calls, bare key, no team_slug.
	intent := map[string]any{
		"session_key": "S-5", "summary": "add the login handler",
		"paths": []string{"app/login.go"}, "idempotency_key": "subagent-intent-1",
	}
	first := mustOK(t, cs, "declare_intent", intent)
	if n := intentsOn(t, hs, teamB, "S-5"); n != 1 {
		t.Fatalf("declare_intent put %d intent(s) on team B's S-5, want 1: %s", n, first)
	}
	if n := intentsOn(t, hs, hs.teamID, "S-5"); n != 0 {
		t.Fatalf("declare_intent put %d intent(s) on team A's ended S-5", n)
	}
	// A replay of the idempotent call is byte-identical and declares nothing.
	if again := mustOK(t, cs, "declare_intent", intent); again != first {
		t.Errorf("declare_intent replay differs:\nfirst:  %s\nreplay: %s", first, again)
	}
	if n := intentsOn(t, hs, teamB, "S-5"); n != 1 {
		t.Errorf("the replay declared a second intent: %d", n)
	}

	mustOK(t, cs, "heartbeat", map[string]any{"session_key": "S-5", "status_line": "writing the handler"})
	var line string
	if err := hs.core.DB().QueryRow("SELECT `status_line` FROM `session` WHERE `team_uuid` = ? AND `key` = ?",
		teamB.String(), "S-5").Scan(&line); err != nil || line != "writing the handler" {
		t.Fatalf("heartbeat did not land on team B's S-5: line=%q err=%v", line, err)
	}

	ended := mustOK(t, cs, "end_session", map[string]any{"session_key": "S-5"})
	if st := sessionStatusOn(t, hs, teamB, "S-5"); st != enums.SESSION_STATUS_ENDED {
		t.Fatalf("end_session left team B's S-5 %s: %s", st.String(), ended)
	}
	// Now BOTH S-5s are finished, and nothing but recency tells them apart,
	// so a bare key is ambiguous again and refused rather than guessed; the
	// replay that names the team is byte-identical.
	if text := mustRefuse(t, cs, "end_session", map[string]any{"session_key": "S-5"}); !strings.Contains(text, slugA) || !strings.Contains(text, slugB) {
		t.Errorf("a bare S-5 with both finished should list both teams: %s", text)
	}
	if again := mustOK(t, cs, "end_session", map[string]any{"session_key": "S-5", "team_slug": slugB}); again != ended {
		t.Errorf("end_session replay differs:\nfirst:  %s\nreplay: %s", ended, again)
	}
	// The supervisor is untouched.
	if st := sessionStatusOn(t, hs, teamB, "S-3"); st != enums.SESSION_STATUS_LIVE {
		t.Errorf("team B's S-3 is %s, want live", st.String())
	}
}

// TestIntegrationSessionKeyLiveOnTwoTeamsNeedsTeamSlug: the one genuinely
// ambiguous case — the same agent holds a live S-77 on each team. A bare key
// is refused with both slugs; team_slug resolves it.
func TestIntegrationSessionKeyLiveOnTwoTeamsNeedsTeamSlug(t *testing.T) {
	hs := newHarness(t)
	endpoint, _ := realServer(t, hs)
	slugA := hs.teamSlug(t)
	teamB, slugB, codeB := hs.otherTeam(t)

	ana := hs.join(t, "Ana", "client-a")
	hs.joinWith(t, ana.ctx, ana.token, codeB, "Ana", "client-a")
	cs := connectAs(t, endpoint, ana.token)

	a, b := startSessionOn(t, cs, slugA, nil), startSessionOn(t, cs, slugB, nil)
	rekey(t, hs, hs.teamID, map[string]string{a.Key: "S-77"})
	rekey(t, hs, teamB, map[string]string{b.Key: "S-77"})

	for _, tool := range []string{"heartbeat", "declare_intent", "end_session"} {
		args := map[string]any{"session_key": "S-77"}
		if tool == "declare_intent" {
			args["summary"] = "fix the parser"
			args["paths"] = []string{"app/parse.go"}
		}
		text := mustRefuse(t, cs, tool, args)
		t.Logf("%s refused: %s", tool, text)
		if !strings.Contains(text, slugA) || !strings.Contains(text, slugB) || !strings.Contains(text, "team_slug") {
			t.Errorf("%s refusal should list both slugs and name team_slug: %s", tool, text)
		}
	}
	if n := intentsOn(t, hs, hs.teamID, "S-77") + intentsOn(t, hs, teamB, "S-77"); n != 0 {
		t.Fatalf("a refused declare_intent wrote %d intent(s)", n)
	}

	mustOK(t, cs, "declare_intent", map[string]any{
		"session_key": "S-77", "team_slug": slugB, "summary": "fix the parser", "paths": []string{"app/parse.go"},
	})
	if a, b := intentsOn(t, hs, hs.teamID, "S-77"), intentsOn(t, hs, teamB, "S-77"); a != 0 || b != 1 {
		t.Fatalf("team_slug %s put intents A=%d B=%d, want 0/1", slugB, a, b)
	}
	// Slug case and whitespace do not matter, as for every other team_slug.
	mustOK(t, cs, "end_session", map[string]any{"session_key": "S-77", "team_slug": " " + strings.ToUpper(slugA) + " "})
	if st := sessionStatusOn(t, hs, hs.teamID, "S-77"); st != enums.SESSION_STATUS_ENDED {
		t.Fatalf("end_session with team_slug %s left it %s", slugA, st.String())
	}
	if st := sessionStatusOn(t, hs, teamB, "S-77"); st != enums.SESSION_STATUS_LIVE {
		t.Fatalf("end_session on team A touched team B's S-77: %s", st.String())
	}
	// With A's S-77 ended, the bare key is no longer ambiguous.
	mustOK(t, cs, "heartbeat", map[string]any{"session_key": "S-77"})

	// A slug naming a team with no such session is not found, and says which team.
	_, slugC, codeC := hs.otherTeam(t)
	hs.joinWith(t, ana.ctx, ana.token, codeC, "Ana", "client-a")
	text := mustRefuse(t, cs, "heartbeat", map[string]any{"session_key": "S-77", "team_slug": slugC})
	if !strings.Contains(text, "no session with key") || !strings.Contains(text, slugC) {
		t.Errorf("S-77 on a team with no S-77: %s", text)
	}
}

// TestIntegrationSessionKeyOtherAgentStillRefused: narrowing to the caller's
// own agent must not turn another agent's session into a match — on one team
// or across two.
func TestIntegrationSessionKeyOtherAgentStillRefused(t *testing.T) {
	hs := newHarness(t)
	endpoint, _ := realServer(t, hs)
	slugA := hs.teamSlug(t)
	teamB, slugB, codeB := hs.otherTeam(t)

	claude := hs.join(t, "Ana", "client-a")
	codex := hs.rejoin(t, claude, "Ana", "client-b")
	hs.joinWith(t, claude.ctx, claude.token, codeB, "Ana", "client-a")
	hs.joinWith(t, codex.ctx, codex.token, codeB, "Ana", "client-b")
	if codex.agent.ID == claude.agent.ID {
		t.Fatal("setup: want two agents")
	}
	csA := connectAs(t, endpoint, claude.token)
	csB := connectAs(t, endpoint, codex.token)

	// codex holds S-77 on team A only.
	onA := startSessionOn(t, csB, slugA, nil)
	rekey(t, hs, hs.teamID, map[string]string{onA.Key: "S-77"})
	const wantOther = "belongs to another of your agents"
	if text := mustRefuse(t, csA, "heartbeat", map[string]any{"session_key": "S-77"}); !strings.Contains(text, wantOther) {
		t.Errorf("one team, other agent's session: %s", text)
	}

	// codex now holds a live S-77 on BOTH teams: still refused, never ambiguous.
	onB := startSessionOn(t, csB, slugB, nil)
	rekey(t, hs, teamB, map[string]string{onB.Key: "S-77"})
	for _, args := range []map[string]any{
		{"session_key": "S-77"},
		{"session_key": "S-77", "team_slug": slugB},
	} {
		text := mustRefuse(t, csA, "end_session", args)
		t.Logf("end_session %v refused: %s", args, text)
		if !strings.Contains(text, wantOther) {
			t.Errorf("end_session %v with the other agent's token: %s", args, text)
		}
	}
	if a, b := sessionStatusOn(t, hs, hs.teamID, "S-77"), sessionStatusOn(t, hs, teamB, "S-77"); a != enums.SESSION_STATUS_LIVE || b != enums.SESSION_STATUS_LIVE {
		t.Fatalf("a refused end_session changed codex's sessions: %s / %s", a.String(), b.String())
	}
	// And codex's own token needs team_slug for them.
	mustRefuse(t, csB, "heartbeat", map[string]any{"session_key": "S-77"})
	mustOK(t, csB, "heartbeat", map[string]any{"session_key": "S-77", "team_slug": slugB})
}

// TestIntegrationSessionKeyOtherAccountNotFound: another person's session is
// not found — with or without a team_slug naming their team.
func TestIntegrationSessionKeyOtherAccountNotFound(t *testing.T) {
	hs := newHarness(t)
	endpoint, _ := realServer(t, hs)
	slugA := hs.teamSlug(t)
	teamB, slugB, codeB := hs.otherTeam(t)

	ana := hs.join(t, "Ana", "client-a")
	bobToken := hs.joinWith(t, context.Background(), "", codeB, "Bob", "client-bob")
	csAna := connectAs(t, endpoint, ana.token)
	csBob := connectAs(t, endpoint, bobToken)

	bob := startSessionOn(t, csBob, slugB, nil)
	rekey(t, hs, teamB, map[string]string{bob.Key: "S-77"})
	for _, args := range []map[string]any{
		{"session_key": "S-77"},
		{"session_key": "S-77", "team_slug": slugB},
	} {
		text := mustRefuse(t, csAna, "heartbeat", args)
		t.Logf("heartbeat %v refused: %s", args, text)
		if !strings.Contains(text, "no session with key") || strings.Contains(text, "belongs to") {
			t.Errorf("heartbeat %v on another account's session: %s", args, text)
		}
	}

	// Ana's own S-77 on her team is unaffected by Bob's S-77.
	own := startSessionOn(t, csAna, slugA, nil)
	rekey(t, hs, hs.teamID, map[string]string{own.Key: "S-77"})
	mustOK(t, csAna, "heartbeat", map[string]any{"session_key": "S-77"})
	if st := sessionStatusOn(t, hs, teamB, "S-77"); st != enums.SESSION_STATUS_LIVE {
		t.Errorf("Bob's S-77 is %s, want live", st.String())
	}
}
