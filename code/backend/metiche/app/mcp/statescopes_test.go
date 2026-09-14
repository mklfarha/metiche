package mcp

import (
	"strings"
	"testing"
	"time"

	"github.com/gofrs/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mklfarha/metiche/backend/enums"
)

// get_team_state's team block, scope=projects (docs/CLI.md §4.2) and
// scope=members (§4.9), through the real transport.

func scopeState(t *testing.T, cs *mcp.ClientSession, args map[string]any) (TeamStateResult, string) {
	t.Helper()
	res, text := callTool(t, cs, "get_team_state", args)
	if res.IsError {
		t.Fatalf("get_team_state %v refused: %s", args, redactSecrets(text))
	}
	var out TeamStateResult
	decodeResult(t, res, &out)
	return out, text
}

func scopeStart(t *testing.T, cs *mcp.ClientSession, slug, project, repo string) string {
	t.Helper()
	args := map[string]any{"team_slug": slug, "project_key": project, "branch": "main", "confirm_new_project": "person"}
	if repo != "" {
		args["repo_url"] = repo
	}
	res, text := callTool(t, cs, "start_session", args)
	if res.IsError {
		t.Fatalf("start_session %s: %s", project, redactSecrets(text))
	}
	var env Envelope
	decodeResult(t, res, &env)
	return env.Key
}

func scopeEnd(t *testing.T, cs *mcp.ClientSession, key string) {
	t.Helper()
	if res, text := callTool(t, cs, "end_session", map[string]any{"session_key": key}); res.IsError {
		t.Fatalf("end_session %s: %s", key, text)
	}
}

func scopeBackdate(t *testing.T, hs *harness, key string, at time.Time) {
	t.Helper()
	if _, err := hs.core.DB().Exec(
		"UPDATE `session` s JOIN `project` p ON p.`id` = s.`project_uuid` SET s.`started_at` = ?, s.`last_heartbeat_at` = ? "+
			"WHERE p.`key` = ? AND p.`team_uuid` = ?", at.UTC(), at.UTC(), key, hs.teamID.String()); err != nil {
		t.Fatal(err)
	}
}

func scopeKeys(ps []StateProject) string {
	keys := make([]string, 0, len(ps))
	for _, p := range ps {
		keys = append(keys, p.Key)
	}
	return strings.Join(keys, ",")
}

// TestIntegrationTeamStateProjects: active projects only, with and without
// live sessions, ordered by last activity then key, paginated.
func TestIntegrationTeamStateProjects(t *testing.T) {
	hs := newHarness(t)
	endpoint, _ := realServer(t, hs)
	ana := hs.join(t, "Ana", "laptop-claude")
	slug := hs.teamSlug(t)
	cs := connectAs(t, endpoint, ana.token)

	scopeEnd(t, cs, scopeStart(t, cs, slug, "gamma", ""))
	scopeBackdate(t, hs, "gamma", time.Now().Add(-72*time.Hour))
	scopeEnd(t, cs, scopeStart(t, cs, slug, "beta", "https://github.com/example/beta.git"))
	scopeBackdate(t, hs, "beta", time.Now().Add(-time.Hour))
	scopeStart(t, cs, slug, "alpha", "git@github.com:Example/Alpha.git")
	scopeStart(t, cs, slug, "alpha", "git@github.com:Example/Alpha.git")
	for _, p := range []struct {
		key    string
		status enums.RecordStatus
	}{{"delta", enums.RECORD_STATUS_ACTIVE}, {"zeta", enums.RECORD_STATUS_INACTIVE}} {
		if _, err := hs.core.DB().Exec("INSERT INTO `project` (`id`,`team_uuid`,`key`,`name`,`status`) VALUES (?,?,?,?,?)",
			uuid.Must(uuid.NewV4()).String(), hs.teamID.String(), p.key, p.key, p.status); err != nil {
			t.Fatal(err)
		}
	}

	all, text := scopeState(t, cs, map[string]any{"scope": "projects"})
	t.Logf("scope=projects -> %s", text)
	if got := scopeKeys(all.Projects); got != "alpha,beta,gamma,delta" {
		t.Fatalf("projects = %s, want alpha,beta,gamma,delta (active only, latest activity first)", got)
	}
	alpha, beta, delta := all.Projects[0], all.Projects[1], all.Projects[3]
	if alpha.LiveSessions != 2 || beta.LiveSessions != 0 {
		t.Errorf("live_sessions alpha=%d beta=%d, want 2 and 0", alpha.LiveSessions, beta.LiveSessions)
	}
	if alpha.RepoURL != "https://github.com/example/alpha" || normalizeRepoURL(alpha.RepoURL) != "github.com/example/alpha" {
		t.Errorf("alpha repo_url = %q, want the canonical https form", alpha.RepoURL)
	}
	if delta.LastActivityAt != "" || delta.RepoURL != "" {
		t.Errorf("delta (no sessions, no repo) = %+v", delta)
	}
	if all.NextCursor != "" || all.Team == nil || all.Team.Slug != slug {
		t.Errorf("next_cursor %q, team %+v", all.NextCursor, all.Team)
	}

	page1, _ := scopeState(t, cs, map[string]any{"scope": "projects", "limit": 2})
	if scopeKeys(page1.Projects) != "alpha,beta" || page1.NextCursor == "" || !page1.Truncated {
		t.Fatalf("page 1 = %s cursor=%q", scopeKeys(page1.Projects), page1.NextCursor)
	}
	page2, _ := scopeState(t, cs, map[string]any{"scope": "projects", "limit": 2, "cursor": page1.NextCursor})
	if scopeKeys(page2.Projects) != "gamma,delta" || page2.NextCursor != "" {
		t.Errorf("page 2 = %s cursor=%q", scopeKeys(page2.Projects), page2.NextCursor)
	}
	if res, text := callTool(t, cs, "get_team_state", map[string]any{"scope": "projects", "cursor": "not-a-cursor"}); !res.IsError {
		t.Errorf("a forged cursor was accepted: %s", text)
	}
}

// TestIntegrationTeamStateTeamBlockOnEveryScope: private by default, public
// once the team is set public, on every scope; the invalid-scope message
// names the new scopes.
func TestIntegrationTeamStateTeamBlockOnEveryScope(t *testing.T) {
	hs := newHarness(t)
	endpoint, _ := realServer(t, hs)
	ana := hs.join(t, "Ana", "laptop-claude")
	cs := connectAs(t, endpoint, ana.token)
	scopes := []string{"sessions", "events", "me", "projects", "members"}

	for _, want := range []string{"private", "public"} {
		if want == "public" {
			if _, err := hs.core.DB().Exec("UPDATE `team` SET `visibility` = ? WHERE `id` = ?", enums.TEAM_VISIBILITY_PUBLIC, hs.teamID.String()); err != nil {
				t.Fatal(err)
			}
		}
		for _, scope := range scopes {
			out, _ := scopeState(t, cs, map[string]any{"scope": scope})
			if out.Team == nil || out.Team.Visibility != want || out.Team.Slug != hs.teamSlug(t) || out.Team.Name != "Test team" {
				t.Errorf("scope=%s team = %+v, want visibility %s", scope, out.Team, want)
			}
		}
	}
	res, text := callTool(t, cs, "get_team_state", map[string]any{"scope": "claims"})
	if !res.IsError || !strings.Contains(text, "scope must be one of sessions, events, me, projects, members") {
		t.Errorf("invalid scope answer = %v %q", res.IsError, text)
	}
}

// TestIntegrationTeamStateMembers: live members only, owners first; a
// teammate's agents only where they worked on this team, your own row all your
// active agents; no client_key or account key; paginated.
func TestIntegrationTeamStateMembers(t *testing.T) {
	hs := newHarness(t)
	endpoint, _ := realServer(t, hs)
	slug := hs.teamSlug(t)

	bob := hs.join(t, "Bob", "bob-cursor")
	ana := hs.join(t, "Ana", "laptop-claude")
	promoteToOwner(t, hs, ana.token)
	gone := hs.join(t, "Test Leaver", "leaver-codex")
	if _, err := hs.core.DB().Exec("UPDATE `member` SET `revoked_at` = UTC_TIMESTAMP(), `status` = ? WHERE `id` = ?",
		enums.RECORD_STATUS_INACTIVE, gone.member.ID.String()); err != nil {
		t.Fatal(err)
	}

	anaCS := connectAs(t, endpoint, ana.token)
	scopeStart(t, anaCS, slug, "alpha", "")

	// Ana's second client works only on another team.
	otherID, code := seedTeam(t, hs, "Other Team")
	_, codexToken := joinWithCode(t, hs, hs.ctxForToken(t, ana.token), ana.token, code, "Ana", "laptop-codex")
	var otherSlug string
	if err := hs.core.DB().QueryRow("SELECT `slug` FROM `team` WHERE `id` = ?", otherID.String()).Scan(&otherSlug); err != nil {
		t.Fatal(err)
	}
	scopeStart(t, connectAs(t, endpoint, codexToken), otherSlug, "elsewhere", "")
	codexID, err := hs.h.resolveToken(t.Context(), codexToken)
	if err != nil || codexID.Agent == nil {
		t.Fatalf("resolving the codex token: %v", err)
	}
	codexKey := codexID.Agent.Key

	bobCS := connectAs(t, endpoint, bob.token)
	bobView, bobText := scopeState(t, bobCS, map[string]any{"scope": "members"})
	t.Logf("scope=members as Bob -> %s", bobText)
	if len(bobView.Members) != 2 || bobView.Members[0].Key != "ana" || bobView.Members[1].Key != "bob" {
		t.Fatalf("members = %+v, want ana (owner) then bob, and no revoked member", bobView.Members)
	}
	anaRow, bobRow := bobView.Members[0], bobView.Members[1]
	if anaRow.Role != "owner" || anaRow.Mine || !bobRow.Mine || bobRow.Role != "member" {
		t.Errorf("roles/mine: ana=%+v bob=%+v", anaRow, bobRow)
	}
	if len(anaRow.Agents) != 1 || anaRow.Agents[0].Key != ana.agent.Key || anaRow.Agents[0].LastSessionAt == "" {
		t.Errorf("Bob sees Ana's agents %+v, want only the one that worked here (%s)", anaRow.Agents, ana.agent.Key)
	}
	if len(bobRow.Agents) != 1 || bobRow.Agents[0].Key != bob.agent.Key {
		t.Errorf("Bob's own row lists %+v, want his active agent %s", bobRow.Agents, bob.agent.Key)
	}
	for _, leak := range []string{"laptop-claude", "laptop-codex", ana.account.Key, bob.account.Key, "Test Leaver"} {
		if strings.Contains(bobText, leak) {
			t.Errorf("scope=members carries %q", leak)
		}
	}

	anaView, _ := scopeState(t, anaCS, map[string]any{"scope": "members", "team_slug": slug})
	keys := map[string]bool{}
	for _, a := range anaView.Members[0].Agents {
		keys[a.Key] = true
	}
	if !anaView.Members[0].Mine || !keys[ana.agent.Key] || !keys[codexKey] {
		t.Errorf("Ana's own row lists %+v, want both %s and %s", anaView.Members[0].Agents, ana.agent.Key, codexKey)
	}

	p1, _ := scopeState(t, bobCS, map[string]any{"scope": "members", "limit": 1})
	if len(p1.Members) != 1 || p1.Members[0].Key != "ana" || p1.NextCursor == "" {
		t.Fatalf("page 1 = %+v cursor %q", p1.Members, p1.NextCursor)
	}
	p2, _ := scopeState(t, bobCS, map[string]any{"scope": "members", "limit": 1, "cursor": p1.NextCursor})
	if len(p2.Members) != 1 || p2.Members[0].Key != "bob" || p2.NextCursor != "" {
		t.Errorf("page 2 = %+v cursor %q", p2.Members, p2.NextCursor)
	}
}
