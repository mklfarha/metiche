package mcpclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mklfarha/metiche/cli/internal/secret"
)

// TestToolAllowlist pins the compile-time MCP tool list (docs/CLI.md §7).
func TestToolAllowlist(t *testing.T) {
	var got []string
	for k := range AllowedTools {
		got = append(got, k)
	}
	sort.Strings(got)
	want := "create_invite,create_team,get_team_state,health,list_invites,list_teams,open_board,revoke_invite,sign_out_browsers,whoami"
	if strings.Join(got, ",") != want {
		t.Errorf("allowed tools = %s\nwant           %s", strings.Join(got, ","), want)
	}
	c := &Client{}
	for _, forbidden := range []string{"join_team", "start_session", "end_session", "heartbeat", "declare_intent", "update_intent", "check_paths", "get_instructions", "report_back"} {
		err := c.Call(context.Background(), forbidden, nil, nil)
		if e, ok := err.(*Error); !ok || e.Kind != KindRefused {
			t.Errorf("Call(%s) = %v, want a refusal before any network use", forbidden, err)
		}
	}
}

func TestToolErrorCode(t *testing.T) {
	for text, want := range map[string]string{
		"already_exists: you are already on a team":                        "already_exists",
		"not_permitted: open_board needs this client's own token":          "not_permitted",
		"rate_limited: too many":                                           "rate_limited",
		"you are not a member of that team — join it with its join code":   "not_found",
		"no active team with slug \"x\"":                                   "not_found",
		"you are on 2 teams, so this call needs a team_slug: one of a, b.": "ambiguous_team",
		"code=provider_unavailable retryable=true":                         "unavailable",
		"something else entirely":                                          "",
	} {
		if got, _ := toolErrorCode(text); got != want {
			t.Errorf("toolErrorCode(%q) = %q, want %q", text, got, want)
		}
	}
}

func TestClassify(t *testing.T) {
	if e := classify(context.DeadlineExceeded, 401, "u"); e.Kind != KindUnauthorized {
		t.Errorf("401 -> %v", e.Kind)
	}
	if e := classify(context.DeadlineExceeded, 404, "u"); e.Kind != KindUnreachable || !strings.Contains(e.Detail, "/v1/mcp") {
		t.Errorf("404 -> %+v", e)
	}
	if e := classify(context.DeadlineExceeded, 503, "u"); e.Kind != KindUnreachable {
		t.Errorf("503 -> %v", e.Kind)
	}
	if e := classify(context.DeadlineExceeded, 0, "u"); e.Kind != KindUnreachable {
		t.Errorf("timeout -> %v", e.Kind)
	}
}

// TestRoundTripperSendsTheBearerAndNeverFollowsARedirect.
func TestRoundTripperSendsTheBearerAndNeverFollowsARedirect(t *testing.T) {
	const tok = "mtk_CANARY_roundtrip_0123456789abcdef"
	var elsewhere atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { elsewhere.Store(true) }))
	defer other.Close()
	var gotAuth, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotUA = r.Header.Get("Authorization"), r.Header.Get("User-Agent")
		http.Redirect(w, r, other.URL+"/v1/mcp", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	rt := &authRT{base: http.DefaultTransport, token: secret.New(tok)}
	_, err := newHTTPClient(rt, 5*time.Second).Get(srv.URL)
	if err == nil {
		t.Error("the redirect was followed")
	}
	if elsewhere.Load() {
		t.Error("a request reached the redirect target")
	}
	if gotAuth != "Bearer "+tok || !strings.HasPrefix(gotUA, "metiche-cli/") {
		t.Errorf("Authorization ok=%v, User-Agent=%q", gotAuth == "Bearer "+tok, gotUA)
	}
	if rt.status() != http.StatusTemporaryRedirect {
		t.Errorf("last status = %d", rt.status())
	}
}
