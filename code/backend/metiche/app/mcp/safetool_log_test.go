package mcp

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// The "no agent identity header" diagnostic runs inside authTool, which wraps
// every tool call in production, next to a bearer token. Three things must
// hold:
//
//   - it logs header NAMES and never a value;
//   - it cannot panic on a request built without Params, Extra or a logger;
//   - it fires only for a LEGACY ACCOUNT token with no client-key header. With
//     agent tokens a missing header is the normal case, and a line per call
//     would bury the one anomaly it exists to surface.
//
// These tests need no database. The gate itself, which needs a token that
// really resolves to an account or to an agent, is pinned against MySQL by
// TestIntegrationMissingHeaderLogIsGatedOnAccountTokens.

// Obviously fake. Not a credential, never resolves to one.
const fakeBearerValue = "mtk_FAKE_TEST_VALUE_not_a_real_token_0000"

func observedHandler() (*Handler, *observer.ObservedLogs) {
	core, logs := observer.New(zapcore.DebugLevel)
	return NewHandler(nil, zap.New(core)), logs
}

func passThrough(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct{}, error) {
	return nil, struct{}{}, nil
}

// assertNoHeaderValues fails if any logged message or field carries a value.
func assertNoHeaderValues(t *testing.T, logs *observer.ObservedLogs, values ...string) {
	t.Helper()
	for _, entry := range logs.All() {
		dump := entry.Message + fmt.Sprint(entry.ContextMap())
		for _, secret := range values {
			if secret != "" && strings.Contains(dump, secret) {
				t.Fatalf("a header value %q leaked into the log: %s", secret, dump)
			}
		}
	}
}

func TestSafetoolLogNamesHeadersButNeverValues(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)

	req := &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{Name: "report_intent"},
		Extra:  &mcp.RequestExtra{Header: http.Header{}},
	}
	req.Extra.Header.Set("Authorization", "Bearer "+fakeBearerValue)
	req.Extra.Header.Set("User-Agent", "fake-agent-ua-value")
	logMissingIdentityHeader(zap.New(core), req)

	entries := logs.FilterMessage(noIdentityHeaderMsg).All()
	if len(entries) != 1 {
		t.Fatalf("got %d %q entries, want exactly 1 (all: %v)", len(entries), noIdentityHeaderMsg, logs.All())
	}
	e := entries[0]
	if e.Level != zapcore.InfoLevel {
		t.Fatalf("level = %v, want INFO", e.Level)
	}
	fields := e.ContextMap()
	if got := fields["tool"]; got != "report_intent" {
		t.Fatalf("tool = %v, want report_intent", got)
	}
	if got := fields["token_scope"]; got != "account" {
		t.Fatalf("token_scope = %v, want account", got)
	}
	got, ok := fields["headers_received"].([]interface{})
	if !ok {
		t.Fatalf("headers_received = %T %v, want a list of names", fields["headers_received"], fields["headers_received"])
	}
	want := []string{"Authorization", "User-Agent"}
	if len(got) != len(want) {
		t.Fatalf("headers_received = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("headers_received = %v, want %v", got, want)
		}
	}

	// No header VALUE anywhere in anything logged, not just in this field.
	assertNoHeaderValues(t, logs, fakeBearerValue, "Bearer ", "fake-agent-ua-value")
}

func TestSafetoolLogSurvivesNilParams(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	logger := zap.New(core)

	// Any panic here is the diagnostic's own, and must fail the test rather
	// than be recovered.
	req := &mcp.CallToolRequest{Extra: &mcp.RequestExtra{Header: http.Header{}}}
	req.Extra.Header.Set("Accept", "application/json")
	logMissingIdentityHeader(logger, req)
	logMissingIdentityHeader(logger, &mcp.CallToolRequest{})
	logMissingIdentityHeader(logger, nil)
	logMissingIdentityHeader(nil, req)

	entries := logs.FilterMessage(noIdentityHeaderMsg).All()
	if len(entries) != 3 {
		t.Fatalf("got %d %q entries, want 3 (the nil-logger call writes nothing)", len(entries), noIdentityHeaderMsg)
	}
	for _, e := range entries {
		if got := e.ContextMap()["tool"]; got != "" {
			t.Fatalf("tool = %v with nil Params, want empty", got)
		}
	}
}

// A bearer that resolves to nobody — here because the handler has no store at
// all — must neither panic nor trip the diagnostic: an unresolved token is not
// a legacy account token. This runs straight, with no recover. Before nil-db
// handling it dereferenced nil inside resolveToken.
func TestSafetoolUnresolvedTokenDoesNotPanicOrLogMissingHeader(t *testing.T) {
	hdl, logs := observedHandler()
	var sawIdentity bool
	probe := func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, struct{}, error) {
		_, sawIdentity = IdentityFromContext(ctx)
		return nil, struct{}{}, nil
	}
	wrapped := authTool[struct{}, struct{}](hdl, probe)

	req := &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{Name: "report_intent"},
		Extra:  &mcp.RequestExtra{Header: http.Header{}},
	}
	req.Extra.Header.Set("Authorization", "Bearer "+fakeBearerValue)
	req.Extra.Header.Set("User-Agent", "fake-agent-ua-value")
	if _, _, err := wrapped(context.Background(), req, struct{}{}); err != nil {
		t.Fatalf("an unresolvable token should pass through for the tool to refuse, got %v", err)
	}
	if sawIdentity {
		t.Fatal("a token that cannot resolve produced an identity")
	}
	if n := logs.FilterMessage(noIdentityHeaderMsg).Len(); n != 0 {
		t.Fatalf("got %d %q entries for an unresolved token, want 0", n, noIdentityHeaderMsg)
	}
	if n := logs.FilterMessage("a tool call carried a token that did not resolve").Len(); n != 1 {
		t.Fatalf("got %d unresolved-token warnings, want 1", n)
	}
	assertNoHeaderValues(t, logs, fakeBearerValue, "Bearer ", "fake-agent-ua-value")
}

// No token at all is create_team, join_team or health. None of them needs an
// agent, so none of them is the anomaly.
func TestSafetoolNoTokenDoesNotLogMissingHeader(t *testing.T) {
	hdl, logs := observedHandler()
	wrapped := authTool[struct{}, struct{}](hdl, passThrough)

	req := &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{Name: "join_team"},
		Extra:  &mcp.RequestExtra{Header: http.Header{}},
	}
	req.Extra.Header.Set("Accept", "application/json")
	if _, _, err := wrapped(context.Background(), req, struct{}{}); err != nil {
		t.Fatalf("no token should pass through, got %v", err)
	}
	if n := logs.FilterMessage(noIdentityHeaderMsg).Len(); n != 0 {
		t.Fatalf("got %d %q entries with no token, want 0", n, noIdentityHeaderMsg)
	}
}
