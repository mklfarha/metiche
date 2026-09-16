package app

import (
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/mklfarha/metiche/backend/app/webapi"
)

// Two things in ProvideCustomRoutes that are one line each and silent when
// they go missing.

// TestDecisionHistoryIsAllowlisted: the board's decision history answers from
// its own handler rather than from the deny layer.
//
// A route registered in app/webapi but absent from AllowedRoutes compiles,
// starts, logs one line at startup and then 404s every board request for it.
// This is the test that turns that into a failure on a laptop.
func TestDecisionHistoryIsAllowlisted(t *testing.T) {
	markDenials(t)
	if _, ok := AllowedRoutes[webapi.PathDecisionHistory]; !ok {
		t.Fatalf("%s is not in AllowedRoutes", webapi.PathDecisionHistory)
	}
	r := buildServer(t, true)
	rec := do(r, http.MethodGet, concrete(webapi.PathDecisionHistory)[0])
	if denied(rec) {
		t.Fatalf("GET %s was denied by the deny layer", webapi.PathDecisionHistory)
	}
	// The handler answers (503: this server has no database on purpose), not
	// the allowlist's 404.
	t.Logf("GET %s -> %d from its own handler", webapi.PathDecisionHistory, rec.Code)

	// A method it does not register still falls through to the deny layer.
	if post := do(r, http.MethodPost, concrete(webapi.PathDecisionHistory)[0]); !denied(post) {
		t.Errorf("POST %s = %d, want the deny layer's 404", webapi.PathDecisionHistory, post.Code)
	}
}

// TestInlineReviewStaysWiredIntoTheDetector pins the SetDetector line.
//
// The path detector and the decision reviewer are INSTALLED, not called: the
// handler runs them inside the transaction that already holds the team lock.
// Drop either from the chain and every tool still works, no conflict is ever
// found and no plan is ever paired with a decision — a silent, green, useless
// build. app/mcp's two-session integration tests fail if detection is absent
// at all; this one fails if the wiring in THIS file loses a hook, which is the
// edit a route change in the same function could plausibly make.
func TestInlineReviewStaysWiredIntoTheDetector(t *testing.T) {
	raw, err := os.ReadFile("rest.go")
	if err != nil {
		t.Fatalf("read rest.go: %v", err)
	}
	src := string(raw)
	for _, must := range []string{
		"handler.SetDetector(metichemcp.ChainDetectors(",
		"metichemcp.NewPathDetector(coreImpl, logger)",
		"metichemcp.NewDecisionReviewer(coreImpl, logger)",
	} {
		if !strings.Contains(src, must) {
			t.Errorf("app/rest.go no longer contains %q: the detector chain lost a hook", must)
		}
	}
}
