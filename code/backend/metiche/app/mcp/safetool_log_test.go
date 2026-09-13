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

// The "no agent identity header" diagnostic runs on every tool call that lacks
// X-Metiche-Client-Key, in production, next to a bearer token. Two things must
// hold: it logs header NAMES and never a value, and it cannot panic on a
// request the SDK or a test builds without Params.

const noIdentityMsg = "mcp call arrived with no agent identity header"

// Obviously fake. Not a credential, never resolves to one.
const fakeBearerValue = "mtk_FAKE_TEST_VALUE_not_a_real_token_0000"

func observedHandler() (*Handler, *observer.ObservedLogs) {
	core, logs := observer.New(zapcore.DebugLevel)
	return NewHandler(nil, zap.New(core)), logs
}

func passThrough(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct{}, error) {
	return nil, struct{}{}, nil
}

// callIgnoringStorePanic runs the wrapped tool and swallows a panic that
// happens AFTER the diagnostic: a Handler built with no core has no database,
// so resolving the bearer token dereferences nil. That path is not what these
// tests are about, and the diagnostic line is written before it is reached.
func callIgnoringStorePanic(t *testing.T, wrapped mcp.ToolHandlerFor[struct{}, struct{}], req *mcp.CallToolRequest) {
	t.Helper()
	defer func() { _ = recover() }()
	_, _, _ = wrapped(context.Background(), req, struct{}{})
}

func TestSafetoolLogNamesHeadersButNeverValues(t *testing.T) {
	hdl, logs := observedHandler()
	wrapped := authTool[struct{}, struct{}](hdl, passThrough)

	req := &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{Name: "report_intent"},
		Extra:  &mcp.RequestExtra{Header: http.Header{}},
	}
	req.Extra.Header.Set("Authorization", "Bearer "+fakeBearerValue)
	req.Extra.Header.Set("User-Agent", "fake-agent-ua-value")
	callIgnoringStorePanic(t, wrapped, req)

	entries := logs.FilterMessage(noIdentityMsg).All()
	if len(entries) != 1 {
		t.Fatalf("got %d %q entries, want exactly 1 (all: %v)", len(entries), noIdentityMsg, logs.All())
	}
	e := entries[0]
	if e.Level != zapcore.InfoLevel {
		t.Fatalf("level = %v, want INFO", e.Level)
	}
	fields := e.ContextMap()
	if got := fields["tool"]; got != "report_intent" {
		t.Fatalf("tool = %v, want report_intent", got)
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
	for _, entry := range logs.All() {
		dump := entry.Message + fmt.Sprint(entry.ContextMap())
		for _, secret := range []string{fakeBearerValue, "Bearer ", "fake-agent-ua-value"} {
			if strings.Contains(dump, secret) {
				t.Fatalf("a header value %q leaked into the log: %s", secret, dump)
			}
		}
	}
}

func TestSafetoolLogSurvivesNilParams(t *testing.T) {
	hdl, logs := observedHandler()
	wrapped := authTool[struct{}, struct{}](hdl, passThrough)

	// No token, so nothing downstream touches the store: any panic here is
	// the diagnostic's own, and must fail the test rather than be recovered.
	req := &mcp.CallToolRequest{Extra: &mcp.RequestExtra{Header: http.Header{}}}
	req.Extra.Header.Set("Accept", "application/json")
	if _, _, err := wrapped(context.Background(), req, struct{}{}); err != nil {
		t.Fatalf("nil Params should pass through, got %v", err)
	}

	entries := logs.FilterMessage(noIdentityMsg).All()
	if len(entries) != 1 {
		t.Fatalf("got %d %q entries, want exactly 1", len(entries), noIdentityMsg)
	}
	if got := entries[0].ContextMap()["tool"]; got != "" {
		t.Fatalf("tool = %v with nil Params, want empty", got)
	}
}
