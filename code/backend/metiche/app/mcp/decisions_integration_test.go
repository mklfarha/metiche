package mcp

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

// Decisions, review context and judgements against a real MySQL. Run with
// METICHE_TEST_MYSQL_DSN set (see integration_test.go):
//
//	go test -p 1 ./app/mcp/ -run 'Decision|Review|Judgement' -v

// fastPathFixture is what declare_intent and update_intent returned, byte for
// byte, for fastPathScenario on a team with no decisions, captured from the
// build BEFORE the decision reviewer existed (METICHE_CAPTURE_FASTPATH=1 prints
// it). The reviewer's fast path must leave every one of these bytes alone.
const fastPathFixture = `["{\"ok\":true,\"key\":\"INT-7\",\"sequence\":7,\"revision\":6,\"pending\":{\"instructions\":0,\"conflicts\":0,\"reviews\":0},\"note\":\"INT-7 declared; holding 2 path(s) for write\"}","{\"ok\":true,\"key\":\"INT-8\",\"sequence\":8,\"revision\":6,\"pending\":{\"instructions\":0,\"conflicts\":1,\"reviews\":0},\"note\":\"INT-8 declared; holding 1 path(s) for write; 1 collision(s) on paths you just claimed — read conflicts[] before you edit\",\"conflicts\":[{\"key\":\"CF-8\",\"kind\":\"path_overlap\",\"severity\":\"critical\",\"with\":\"Ana (test)\",\"paths\":[\"web/src/auth/session.ts\"],\"suggested_action\":\"Ana (test) holds web/src/auth/session.ts (write, just now). You are both editing it: settle it between you — split the file or sequence the work; ask your human only if you can't.\"}]}","{\"ok\":true,\"key\":\"INT-8\",\"sequence\":9,\"revision\":6,\"pending\":{\"instructions\":0,\"conflicts\":1,\"reviews\":0},\"note\":\"INT-8 updated; 1 path(s) added\"}","{\"ok\":true,\"key\":\"INT-8\",\"sequence\":10,\"revision\":6,\"pending\":{\"instructions\":0,\"conflicts\":1,\"reviews\":0},\"note\":\"INT-8 updated\"}","{\"ok\":true,\"key\":\"INT-7\",\"sequence\":11,\"revision\":6,\"pending\":{\"instructions\":1,\"conflicts\":1,\"reviews\":0},\"note\":\"INT-7 is now active\"}","{\"ok\":true,\"key\":\"INT-12\",\"sequence\":12,\"revision\":6,\"pending\":{\"instructions\":1,\"conflicts\":1,\"reviews\":0},\"note\":\"INT-12 declared; holding 1 path(s) for read\"}"]`

// fastPathScenario runs two agents through declarations and updates that
// collide on a path, and returns every declare_intent and update_intent
// response in order. Nothing in it names a team slug, a token or a uuid, so
// the bytes are the same on every fresh harness.
func fastPathScenario(t *testing.T, hs *harness) []string {
	t.Helper()
	ana := hs.contractAgent(t, "Ana", "client-a")
	bob := hs.contractAgent(t, "Bob", "client-b")
	var out []string
	declare := func(a contractAgent, args DeclareIntentParams) {
		t.Helper()
		args.SessionKey = a.key
		res, _, err := hs.h.DeclareIntent(a.ctx, nil, args)
		if err != nil {
			t.Fatalf("declare_intent: %v", err)
		}
		out = append(out, resultText(t, res))
	}
	update := func(a contractAgent, args UpdateIntentParams) {
		t.Helper()
		args.SessionKey = a.key
		res, _, err := hs.h.UpdateIntent(a.ctx, nil, args)
		if err != nil {
			t.Fatalf("update_intent: %v", err)
		}
		out = append(out, resultText(t, res))
	}
	declare(ana, DeclareIntentParams{Summary: "move the session token into the auth service",
		Paths: []string{"web/src/auth/session.ts", "internal/auth/**"}, IdempotencyKey: "fp-ana-1"})
	// claim_path.created_at is a DATETIME, rounded to the second, so a
	// collision inside the same second can read as held for a negative time
	// and drop "just now" from the suggested action. A second's pause makes
	// the wording the same on every run.
	time.Sleep(1100 * time.Millisecond)
	declare(bob, DeclareIntentParams{Summary: "store the session token in localStorage after login",
		Paths: []string{"web/src/auth/session.ts"}, IdempotencyKey: "fp-bob-1"})
	update(bob, UpdateIntentParams{Summary: "read the session from the httpOnly cookie",
		AddPaths: []string{"web/src/api/client.ts"}, IdempotencyKey: "fp-bob-2"})
	update(bob, UpdateIntentParams{StatusLine: "wiring the cookie read", IdempotencyKey: "fp-bob-3"})
	update(ana, UpdateIntentParams{Status: "active", IdempotencyKey: "fp-ana-2"})
	declare(ana, DeclareIntentParams{Summary: "document the auth flow",
		Paths: []string{"docs/auth.md"}, Mode: "read", IdempotencyKey: "fp-ana-3"})
	return out
}

func renderFastPath(responses []string) string {
	b, _ := json.Marshal(responses)
	return string(b)
}

// TestIntegrationDecisionFastPathBytesUnchanged: a team with no accepted
// decisions gets exactly the bytes it got before decisions existed.
func TestIntegrationDecisionFastPathBytesUnchanged(t *testing.T) {
	hs := newHarness(t)
	hs.h.SetDetector(NewPathDetector(hs.core, zap.NewNop()))
	got := renderFastPath(fastPathScenario(t, hs))
	if os.Getenv("METICHE_CAPTURE_FASTPATH") == "1" {
		t.Logf("FIXTURE:%s", got)
		return
	}
	if got != fastPathFixture {
		t.Fatalf("declare/update bytes changed on a team with no decisions:\ngot:  %s\nwant: %s", got, fastPathFixture)
	}
	if !strings.Contains(got, "path_overlap") {
		t.Fatal("the scenario should include a path collision")
	}
}
