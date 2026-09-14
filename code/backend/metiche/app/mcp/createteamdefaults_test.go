package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gofrs/uuid"
)

// create_team's identity defaults for an agent token (docs/CLI.md §4.8): what
// `metiche teams create` relies on to send no client_key, member_name or
// agent_label, and mint nothing.
func TestIntegrationCreateTeamWithAnAgentTokenMintsNothing(t *testing.T) {
	hs := newHarness(t)
	endpoint := guardServer(t, hs)
	token, _, _ := createFirstTeam(t, endpoint, "First Crew")
	cs := connectAs(t, endpoint, token)
	agentsBefore := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `agent`")

	res, text := callTool(t, cs, "create_team", map[string]any{
		"team_name": "Second Crew", "idempotency_key": uuid.Must(uuid.NewV4()).String()})
	t.Logf("create_team with only a name and a key -> isError=%v %s", res.IsError, redactSecrets(text))
	if res.IsError {
		t.Fatalf("refused: %s", redactSecrets(text))
	}
	var out CreateTeamResult
	decodeResult(t, res, &out)
	if !out.Created || !out.TokenKept || out.Token != "" || out.ClientKey != "laptop-claude" || out.MemberKey != "ana" {
		t.Errorf("created=%v token_kept=%v token present=%v client_key=%q member_key=%q",
			out.Created, out.TokenKept, out.Token != "", out.ClientKey, out.MemberKey)
	}
	if strings.Contains(text, `"token":`) || strings.Contains(text, "auth_header") {
		t.Error("the answer carries a token field")
	}
	if got := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `agent`"); got != agentsBefore {
		t.Errorf("agent rows %d -> %d: create_team minted an agent", agentsBefore, got)
	}
	var label string
	if err := hs.core.DB().QueryRow("SELECT `label` FROM `agent` WHERE `client_key` = 'laptop-claude'").Scan(&label); err != nil || label != "claude on laptop" {
		t.Errorf("the agent's label became %q (err %v), want it kept", label, err)
	}

	// A different client_key is NOT refused: install.sh creates as its first
	// client while carrying another client's anchor, and joinAs mints for it.
	res, text = callTool(t, cs, "create_team", map[string]any{
		"team_name": "Third Crew", "member_name": "Ana", "agent_label": "cursor on laptop",
		"client_key": "laptop-cursor", "idempotency_key": uuid.Must(uuid.NewV4()).String()})
	if res.IsError {
		t.Fatalf("a different client_key was refused: %s", redactSecrets(text))
	}
	var third CreateTeamResult
	decodeResult(t, res, &third)
	if third.Token == "" || third.TokenKept {
		t.Errorf("the other client got token present=%v kept=%v, want a new token", third.Token != "", third.TokenKept)
	}

	// Without a token the three stay required.
	res, text = callTool(t, connectAs(t, endpoint, ""), "create_team", map[string]any{
		"team_name": "Anon Crew", "idempotency_key": uuid.Must(uuid.NewV4()).String()})
	if !res.IsError || !strings.HasPrefix(text, "invalid_argument: member_name is required") {
		t.Errorf("tokenless create with no member_name = %v %q", res.IsError, text)
	}

	// And the schema says so: none of the three is required any more.
	tools, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if tool.Name != "create_team" {
			continue
		}
		raw, _ := json.Marshal(tool.InputSchema)
		var s struct {
			Required []string `json:"required"`
		}
		_ = json.Unmarshal(raw, &s)
		for _, r := range s.Required {
			if r == "member_name" || r == "agent_label" || r == "client_key" {
				t.Errorf("create_team's schema still requires %s", r)
			}
		}
	}
}
