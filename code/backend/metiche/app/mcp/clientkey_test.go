package mcp

import (
	"context"
	"net/http"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"
)

// An agent cannot learn its own client_key. It is chosen by the installer and
// then thrown away: it is not in the MCP config, the environment, or the
// repository. Asked to supply one, a model guesses -- "backend", "ui",
// "codex", "tests" -- and every guess is rejected, while a guess that happened
// to land would file the work under a different agent on a shared board.
//
// So the connection carries it, the way it carries the token. These tests pin
// that, and the rules around it.

func ctxKeyProbe() (mcp.ToolHandlerFor[struct{}, struct{}], *string) {
	seen := ""
	h := func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, struct{}, error) {
		seen = ClientKeyFromContext(ctx)
		return nil, struct{}{}, nil
	}
	return h, &seen
}

func callWithHeaders(t *testing.T, hdrs map[string]string) string {
	t.Helper()
	inner, seen := ctxKeyProbe()
	wrapped := authTool[struct{}, struct{}](NewHandler(nil, zap.NewNop()), inner)
	req := &mcp.CallToolRequest{Extra: &mcp.RequestExtra{Header: http.Header{}}}
	for k, v := range hdrs {
		req.Extra.Header.Set(k, v)
	}
	if _, _, err := wrapped(context.Background(), req, struct{}{}); err != nil {
		t.Fatalf("tool call: %v", err)
	}
	return *seen
}

func TestTheConnectionCarriesTheAgentsClientKey(t *testing.T) {
	if got := callWithHeaders(t, map[string]string{"X-Metiche-Client-Key": "codex-1"}); got != "codex-1" {
		t.Fatalf("client key from the connection = %q, want codex-1", got)
	}
}

// It must work with NO token. The client key is not a credential and selects
// nothing on its own, so it has no reason to depend on authentication -- and
// making it depend on one would mean the very first call of a session, before
// anything is resolved, could not say who it was.
func TestClientKeyIsIndependentOfTheToken(t *testing.T) {
	if got := callWithHeaders(t, map[string]string{"X-Metiche-Client-Key": "claude-1"}); got != "claude-1" {
		t.Fatalf("client key = %q with no Authorization header, want claude-1", got)
	}
}

func TestNoHeaderMeansNoClientKey(t *testing.T) {
	if got := callWithHeaders(t, map[string]string{}); got != "" {
		t.Fatalf("client key = %q with no header, want empty", got)
	}
	if got := callWithHeaders(t, map[string]string{"X-Metiche-Client-Key": "   "}); got != "" {
		t.Fatalf("whitespace-only header produced %q, want empty", got)
	}
}

// Whatever else changes, this must not: the header is a SELECTOR among agents
// that already belong to the authenticated account. It is not a credential and
// it must never be treated as one.
func TestClientKeyGrantsNothingByItself(t *testing.T) {
	inner, _ := ctxKeyProbe()
	wrapped := authTool[struct{}, struct{}](NewHandler(nil, zap.NewNop()), inner)
	req := &mcp.CallToolRequest{Extra: &mcp.RequestExtra{Header: http.Header{}}}
	req.Extra.Header.Set("X-Metiche-Client-Key", "claude-1")

	var sawAccount bool
	probe := func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, struct{}, error) {
		_, sawAccount = AccountFromContext(ctx)
		return nil, struct{}{}, nil
	}
	wrapped = authTool[struct{}, struct{}](NewHandler(nil, zap.NewNop()), probe)
	if _, _, err := wrapped(context.Background(), req, struct{}{}); err != nil {
		t.Fatalf("tool call: %v", err)
	}
	if sawAccount {
		t.Fatal("a client key with no token resolved an account; it is a selector, not a credential")
	}
}
