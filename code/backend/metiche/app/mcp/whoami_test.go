package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"

	"github.com/mklfarha/metiche/backend/enums"
)

// whoami (docs/CLI.md §4.1, §4.6).

// uaTransport is bearerOnly plus a chosen User-Agent and extra headers.
type uaTransport struct {
	token string
	ua    string
	extra map[string]string
}

func (u uaTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if u.token != "" {
		r.Header.Set("Authorization", "Bearer "+u.token)
	}
	if u.ua != "" {
		r.Header.Set("User-Agent", u.ua)
	}
	for k, v := range u.extra {
		r.Header.Set(k, v)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func connectWith(t *testing.T, endpoint string, rt http.RoundTripper) *mcp.ClientSession {
	t.Helper()
	cs, err := dialWith(endpoint, rt)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func dialWith(endpoint string, rt http.RoundTripper) (*mcp.ClientSession, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "metiche-whoami-test", Version: "0"}, nil)
	return client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             endpoint,
		HTTPClient:           &http.Client{Transport: rt, Timeout: 15 * time.Second},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
}

func whoami(t *testing.T, cs *mcp.ClientSession) (WhoamiResult, string) {
	t.Helper()
	res, text := callTool(t, cs, "whoami", map[string]any{})
	if res.IsError {
		t.Fatalf("whoami refused: %s", redactSecrets(text))
	}
	var out WhoamiResult
	decodeResult(t, res, &out)
	return out, text
}

// TestWhoamiWithoutATokenTouchesNoDatabase drives the handler with NO core: a
// database call would dereference nil and panic. It must answer from the
// request alone.
func TestWhoamiWithoutATokenTouchesNoDatabase(t *testing.T) {
	h := NewHandler(nil, zap.NewNop())
	req := &mcp.CallToolRequest{Extra: &mcp.RequestExtra{Header: http.Header{
		"User-Agent":      {"probe/1"},
		"X-Forwarded-For": {"203.0.113.9"},
		"Mcp-Session-Id":  {"abc"},
	}}}
	res, _, err := h.Whoami(context.Background(), req, WhoamiParams{})
	if err != nil {
		t.Fatalf("whoami with no token: %v", err)
	}
	var out WhoamiResult
	if err := json.Unmarshal([]byte(res.Content[0].(*mcp.TextContent).Text), &out); err != nil {
		t.Fatal(err)
	}
	if out.Authenticated || out.TokenScope != "none" || out.Agent != nil || out.Note != whoamiNoneNote {
		t.Errorf("unauthenticated whoami = %+v", out)
	}
	if strings.Join(out.HeadersReceived, ",") != "Mcp-Session-Id,User-Agent" {
		t.Errorf("headers_received = %v, want the client's names without the ingress one", out.HeadersReceived)
	}
}

// TestIntegrationWhoamiThroughTheTransport: an agent token names its agent;
// header names arrive without values; the user agents of this token's
// requests are recorded, and a tokenless request is recorded for nobody.
func TestIntegrationWhoamiThroughTheTransport(t *testing.T) {
	hs := newHarness(t)
	endpoint, _ := realServer(t, hs)
	ana := hs.join(t, "Ana", "laptop-claude")

	const canary = "CANARY-whoami-7f3a9c"
	a := connectWith(t, endpoint, uaTransport{token: ana.token, ua: "client-A/1.0", extra: map[string]string{"X-Canary-Header": canary}})
	b := connectWith(t, endpoint, uaTransport{token: ana.token, ua: "client-B/2.0"})
	anon := connectWith(t, endpoint, uaTransport{ua: "anon-C/3.0"})

	unauth, _ := whoami(t, anon)
	if unauth.Authenticated || unauth.TokenScope != "none" {
		t.Errorf("tokenless whoami = authenticated %v scope %q", unauth.Authenticated, unauth.TokenScope)
	}
	for _, n := range unauth.HeadersReceived {
		if n == "Authorization" {
			t.Error("a tokenless request reports an Authorization header")
		}
	}

	_, _ = whoami(t, b)
	got, text := whoami(t, a)
	t.Logf("whoami (agent token) -> %s", redactSecrets(text))

	if !got.Authenticated || got.TokenScope != "agent" || got.Agent == nil {
		t.Fatalf("agent whoami = %+v", got)
	}
	if got.Agent.ClientKey != "laptop-claude" || got.Agent.Key != ana.agent.Key || got.Agent.Status != "active" {
		t.Errorf("agent = %+v, want client_key laptop-claude, key %s, active", got.Agent, ana.agent.Key)
	}
	if got.AccountKey != ana.account.Key || got.Teams != 1 {
		t.Errorf("account_key %q (want %q), teams %d (want 1)", got.AccountKey, ana.account.Key, got.Teams)
	}
	if strings.Contains(text, canary) {
		t.Error("a header VALUE reached the whoami response")
	}
	if strings.Contains(text, ana.token) {
		t.Error("the token reached the whoami response")
	}
	names := strings.Join(got.HeadersReceived, ",")
	if !strings.Contains(names, "Authorization") || !strings.Contains(names, "X-Canary-Header") {
		t.Errorf("headers_received = %v, want Authorization and X-Canary-Header", got.HeadersReceived)
	}
	uas := map[string]bool{}
	for _, r := range got.RecentRequests {
		uas[r.UserAgent] = true
	}
	if !uas["client-A/1.0"] || !uas["client-B/2.0"] {
		t.Errorf("recent_requests = %+v, want both client-A/1.0 and client-B/2.0", got.RecentRequests)
	}
	if uas["anon-C/3.0"] {
		t.Error("a tokenless request was recorded against Ana's agent")
	}
	if !strings.HasPrefix(got.RecentRequestsScope, "this server process, since ") {
		t.Errorf("recent_requests_scope = %q", got.RecentRequestsScope)
	}

	// Bob's token sees none of Ana's evidence.
	bob := hs.join(t, "Bob", "bob-codex")
	bc := connectWith(t, endpoint, uaTransport{token: bob.token, ua: "bob-client/1"})
	bobSees, _ := whoami(t, bc)
	for _, r := range bobSees.RecentRequests {
		if strings.HasPrefix(r.UserAgent, "client-") {
			t.Errorf("Bob's whoami shows Ana's request %q", r.UserAgent)
		}
	}
}

// TestIntegrationWhoamiLegacyAndRetired: a legacy account token is scope
// account with no agent; a retired agent's token is an HTTP 401 before any
// tool runs.
func TestIntegrationWhoamiLegacyAndRetired(t *testing.T) {
	hs := newHarness(t)
	endpoint, _ := realServer(t, hs)
	ana := hs.join(t, "Ana", "laptop-claude")

	legacy, hash, err := MintToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hs.core.DB().Exec("UPDATE `account` SET `token_hash` = ? WHERE `id` = ?", hash, ana.account.ID.String()); err != nil {
		t.Fatal(err)
	}
	got, text := whoami(t, connectWith(t, endpoint, uaTransport{token: legacy, ua: "legacy/1"}))
	t.Logf("whoami (legacy account token) -> %s", redactSecrets(text))
	if got.TokenScope != "account" || got.Agent != nil || got.Note != whoamiAcctNote || !got.Authenticated {
		t.Errorf("legacy whoami = %+v", got)
	}

	if _, err := hs.core.DB().Exec("UPDATE `agent` SET `status` = ? WHERE `id` = ?", enums.AGENT_STATUS_RETIRED, ana.agent.ID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := dialWith(endpoint, uaTransport{token: ana.token, ua: "retired/1"}); err == nil {
		t.Error("a retired agent's token connected; want HTTP 401 at the edge")
	} else {
		t.Logf("retired agent token -> %v", err)
	}
}
