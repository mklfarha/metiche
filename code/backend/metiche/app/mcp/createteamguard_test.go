package mcp

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/gofrs/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mklfarha/metiche/backend/enums"
)

// The duplicate-name guard on create_team (docs/CLI.md §4.8, decision §10 Q8),
// through the real transport: the SDK's streamable client against
// mcp.Register behind httptest, carrying only Authorization.

// guardServer is realServer with a create_team budget large enough for a test
// that creates many teams from 127.0.0.1.
func guardServer(t *testing.T, hs *harness) string {
	t.Helper()
	t.Setenv("METICHE_CREATE_TEAM_PER_HOUR", "1000")
	endpoint, _ := realServer(t, hs)
	return endpoint
}

func createTeamArgs(name, key string) map[string]any {
	return map[string]any{
		"team_name": name, "member_name": "Ana", "agent_label": "claude on laptop",
		"client_key": "laptop-claude", "client_kind": "claude", "idempotency_key": key,
	}
}

// createFirstTeam is a first contact: no token, so the guard cannot apply. It
// returns the creator's token and the result.
func createFirstTeam(t *testing.T, endpoint, name string) (string, CreateTeamResult, map[string]any) {
	t.Helper()
	args := createTeamArgs(name, uuid.Must(uuid.NewV4()).String())
	anon := connectAs(t, endpoint, "")
	res, text := callTool(t, anon, "create_team", args)
	if res.IsError {
		t.Fatalf("first create_team %q: %s", name, redactSecrets(text))
	}
	var out CreateTeamResult
	decodeResult(t, res, &out)
	if out.Token == "" || !out.Created {
		t.Fatalf("first create_team: token present=%v created=%v", out.Token != "", out.Created)
	}
	return out.Token, out, args
}

func teamCount(t *testing.T, hs *harness) int {
	t.Helper()
	return countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `team`")
}

// TestIntegrationCreateTeamGuardRefusesASecondTeamWithTheSameName: same name,
// another idempotency key, the same token → already_exists naming the slug,
// and no team row.
func TestIntegrationCreateTeamGuardRefusesASecondTeamWithTheSameName(t *testing.T) {
	hs := newHarness(t)
	endpoint := guardServer(t, hs)
	token, first, _ := createFirstTeam(t, endpoint, "Hack Night")
	cs := connectAs(t, endpoint, token)

	before := teamCount(t, hs)
	res, text := callTool(t, cs, "create_team", createTeamArgs("Hack Night", uuid.Must(uuid.NewV4()).String()))
	t.Logf("second create_team, same name -> isError=%v %s", res.IsError, redactSecrets(text))
	if !res.IsError {
		t.Fatalf("a second team with the same name was created: %s", redactSecrets(text))
	}
	if !strings.HasPrefix(text, "already_exists: ") {
		t.Errorf("the refusal does not start with already_exists: %q", text)
	}
	if !strings.Contains(text, "slug: "+first.TeamSlug+")") || !strings.Contains(text, "allow_duplicate_name: true") {
		t.Errorf("the refusal must name the existing slug %q and the override: %q", first.TeamSlug, text)
	}
	if after := teamCount(t, hs); after != before {
		t.Errorf("the refused call created a team: %d -> %d", before, after)
	}
}

// TestIntegrationCreateTeamGuardReplayIsAnswered: the call that created the
// team, retried with its token, must come back created=false, not refused by
// its own result.
func TestIntegrationCreateTeamGuardReplayIsAnswered(t *testing.T) {
	hs := newHarness(t)
	endpoint := guardServer(t, hs)
	token, first, args := createFirstTeam(t, endpoint, "Replay Night")
	cs := connectAs(t, endpoint, token)

	res, text := callTool(t, cs, "create_team", args)
	t.Logf("replay -> isError=%v %s", res.IsError, redactSecrets(text))
	if res.IsError {
		t.Fatalf("the replay of the creating call was refused: %s", redactSecrets(text))
	}
	var replay CreateTeamResult
	decodeResult(t, res, &replay)
	if replay.Created || replay.TeamSlug != first.TeamSlug || replay.JoinCode != first.JoinCode {
		t.Errorf("replay: created=%v slug=%q (want %q) same code=%v", replay.Created, replay.TeamSlug, first.TeamSlug, replay.JoinCode == first.JoinCode)
	}
	if replay.Token != "" || !replay.TokenKept {
		t.Errorf("the replay minted a token (present=%v kept=%v)", replay.Token != "", replay.TokenKept)
	}
}

// TestIntegrationCreateTeamGuardAllowDuplicateName: the explicit override
// creates the second team, with the -xxxxxx slug suffix.
func TestIntegrationCreateTeamGuardAllowDuplicateName(t *testing.T) {
	hs := newHarness(t)
	endpoint := guardServer(t, hs)
	token, first, _ := createFirstTeam(t, endpoint, "Hack Night")
	cs := connectAs(t, endpoint, token)

	args := createTeamArgs("Hack Night", uuid.Must(uuid.NewV4()).String())
	args["allow_duplicate_name"] = true
	res, text := callTool(t, cs, "create_team", args)
	t.Logf("allow_duplicate_name -> isError=%v %s", res.IsError, redactSecrets(text))
	if res.IsError {
		t.Fatalf("allow_duplicate_name was refused: %s", redactSecrets(text))
	}
	var second CreateTeamResult
	decodeResult(t, res, &second)
	if !second.Created || second.TeamSlug == first.TeamSlug || !strings.HasPrefix(second.TeamSlug, first.TeamSlug+"-") {
		t.Errorf("second team: created=%v slug=%q, want a new %q-xxxxxx slug", second.Created, second.TeamSlug, first.TeamSlug)
	}
}

// TestIntegrationCreateTeamGuardNormalization: slugKey(name, 40) decides.
func TestIntegrationCreateTeamGuardNormalization(t *testing.T) {
	hs := newHarness(t)
	endpoint := guardServer(t, hs)
	token, _, _ := createFirstTeam(t, endpoint, "Hack Night")
	cs := connectAs(t, endpoint, token)

	for _, name := range []string{"Hack Night", "hack-night", " HACK  night ", "hack_night!"} {
		res, text := callTool(t, cs, "create_team", createTeamArgs(name, uuid.Must(uuid.NewV4()).String()))
		if !res.IsError || !strings.HasPrefix(text, "already_exists: ") {
			t.Errorf("%q was not refused as the same name: isError=%v %s", name, res.IsError, redactSecrets(text))
		}
	}
	res, text := callTool(t, cs, "create_team", createTeamArgs("Hack Nights", uuid.Must(uuid.NewV4()).String()))
	if res.IsError {
		t.Errorf("\"Hack Nights\" is a different name and was refused: %s", redactSecrets(text))
	}
}

// TestIntegrationCreateTeamGuardIgnoresTeamsYouAreNotOn: a same-named team the
// account is not on, has left, or that is inactive does not block, and its slug
// never appears in any answer.
func TestIntegrationCreateTeamGuardIgnoresTeamsYouAreNotOn(t *testing.T) {
	hs := newHarness(t)
	endpoint := guardServer(t, hs)

	// Bob's team: Ana is not on it.
	_, bobs, _ := createFirstTeam(t, endpoint, "Canary Crew")
	// Ana's own two teams, one she leaves and one that is retired.
	token, left, _ := createFirstTeam(t, endpoint, "Left Crew")
	cs := connectAs(t, endpoint, token)
	res, text := callTool(t, cs, "create_team", createTeamArgs("Retired Crew", uuid.Must(uuid.NewV4()).String()))
	if res.IsError {
		t.Fatalf("create Retired Crew: %s", redactSecrets(text))
	}
	var retired CreateTeamResult
	decodeResult(t, res, &retired)
	if _, err := hs.core.DB().Exec("UPDATE `member` m JOIN `team` t ON t.`id` = m.`team_uuid` SET m.`revoked_at` = UTC_TIMESTAMP(), m.`status` = ? WHERE t.`slug` = ?",
		enums.RECORD_STATUS_INACTIVE, left.TeamSlug); err != nil {
		t.Fatal(err)
	}
	if _, err := hs.core.DB().Exec("UPDATE `team` SET `status` = ? WHERE `slug` = ?", enums.RECORD_STATUS_INACTIVE, retired.TeamSlug); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"Canary Crew", "Left Crew", "Retired Crew"} {
		res, text := callTool(t, cs, "create_team", createTeamArgs(name, uuid.Must(uuid.NewV4()).String()))
		if res.IsError {
			t.Errorf("%q was refused although Ana is not a live member of an active team with that name: %s", name, redactSecrets(text))
		}
		if strings.Contains(text, bobs.TeamSlug+`"`) || strings.Contains(text, "slug: "+bobs.TeamSlug) {
			t.Errorf("the answer names Bob's team slug %q", bobs.TeamSlug)
		}
	}
}

// TestIntegrationCreateTeamGuardUnauthenticatedFirstContact: no token, no
// account, nothing to compare: allowed even for a name that exists elsewhere.
func TestIntegrationCreateTeamGuardUnauthenticatedFirstContact(t *testing.T) {
	hs := newHarness(t)
	endpoint := guardServer(t, hs)
	createFirstTeam(t, endpoint, "Open Night")
	createFirstTeam(t, endpoint, "Open Night")
	if got := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `team` WHERE `name` = 'Open Night'"); got != 2 {
		t.Errorf("%d teams named Open Night, want 2 (two different anonymous people)", got)
	}
}

// TestIntegrationCreateTeamGuardConcurrentCallsMakeOneTeam: two concurrent
// calls from one account with different keys serialize on the account's named
// lock (lockAccountCreates), so exactly one team is created and the other call
// is refused with already_exists. The race is NOT accepted.
func TestIntegrationCreateTeamGuardConcurrentCallsMakeOneTeam(t *testing.T) {
	hs := newHarness(t)
	endpoint := guardServer(t, hs)
	token, _, _ := createFirstTeam(t, endpoint, "Seed Team")

	const n = 4
	var wg sync.WaitGroup
	results := make([]*mcp.CallToolResult, n)
	texts := make([]string, n)
	sessions := make([]*mcp.ClientSession, n)
	for i := range sessions {
		sessions[i] = connectAs(t, endpoint, token)
	}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := sessions[i].CallTool(context.Background(), &mcp.CallToolParams{
				Name: "create_team", Arguments: createTeamArgs("Race Night", uuid.Must(uuid.NewV4()).String())})
			if err != nil {
				texts[i] = "transport: " + err.Error()
				return
			}
			results[i] = res
			if len(res.Content) > 0 {
				if tc, ok := res.Content[0].(*mcp.TextContent); ok {
					texts[i] = tc.Text
				}
			}
		}(i)
	}
	wg.Wait()

	created, refused := 0, 0
	for i := 0; i < n; i++ {
		switch {
		case results[i] != nil && !results[i].IsError:
			created++
		case results[i] != nil && strings.HasPrefix(texts[i], "already_exists: "):
			refused++
		default:
			t.Errorf("call %d neither created nor was refused as a duplicate: %s", i, redactSecrets(texts[i]))
		}
	}
	t.Logf("%d concurrent calls: %d created, %d refused already_exists", n, created, refused)
	if created != 1 || refused != n-1 {
		t.Errorf("created=%d refused=%d, want exactly 1 and %d", created, refused, n-1)
	}
	if got := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `team` WHERE `name` = 'Race Night'"); got != 1 {
		t.Errorf("%d teams named Race Night, want exactly 1", got)
	}
}
