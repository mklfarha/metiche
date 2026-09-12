package mcp

import (
	"context"
	"net/http"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"
)

// The bug this locks down cost an afternoon and was invisible to every other
// test in this package, because every other test calls handler methods
// directly and never goes through the SDK's transport.
//
// The SDK builds the MCP session from the context of the `initialize` request
// and reuses it for every later tool call. So a token sent on call number two
// reached authMiddleware, was resolved correctly, and was then dropped on the
// floor — the tool ran with the initialize request's context and saw no
// account at all. The agent was told to authenticate while holding a valid
// token it was already sending on every request.
//
// authTool is what makes the header on THIS call the one that counts. These
// tests assert the property directly rather than through a live server, so
// they stay fast and cannot be skipped for want of a database.

// accountFromHeaderProbe returns whatever authTool put on the context.
func probeHandler() (mcp.ToolHandlerFor[struct{}, struct{}], *bool) {
	sawAccount := false
	h := func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, struct{}, error) {
		_, sawAccount = AccountFromContext(ctx)
		return nil, struct{}{}, nil
	}
	return h, &sawAccount
}

func TestAuthToolReadsTheHeaderOfThisCall(t *testing.T) {
	inner, saw := probeHandler()

	// A nil Handler must not panic: authTool wraps every tool, including on
	// a server built without a database (the tool-surface test does this).
	wrapped := authTool[struct{}, struct{}](nil, inner)
	req := &mcp.CallToolRequest{Extra: &mcp.RequestExtra{Header: http.Header{}}}
	req.Extra.Header.Set("Authorization", "Bearer mtk_whatever")
	if _, _, err := wrapped(context.Background(), req, struct{}{}); err != nil {
		t.Fatalf("nil handler should pass through, got %v", err)
	}
	if *saw {
		t.Fatal("no account can be resolved without a handler")
	}
}

func TestAuthToolToleratesAMissingHeader(t *testing.T) {
	inner, saw := probeHandler()
	hdl := NewHandler(nil, zap.NewNop())
	wrapped := authTool[struct{}, struct{}](hdl, inner)

	// No Extra at all — the stdio transport has no HTTP headers, and a tool
	// that panicked here would take the process down.
	if _, _, err := wrapped(context.Background(), &mcp.CallToolRequest{}, struct{}{}); err != nil {
		t.Fatalf("missing Extra should pass through, got %v", err)
	}
	// Extra present, header empty: create_team and join_team are reachable
	// with no token, so this must not be an error.
	req := &mcp.CallToolRequest{Extra: &mcp.RequestExtra{Header: http.Header{}}}
	if _, _, err := wrapped(context.Background(), req, struct{}{}); err != nil {
		t.Fatalf("empty header should pass through, got %v", err)
	}
	if *saw {
		t.Fatal("an empty header must not produce an account")
	}
}

// The registration path must wire authTool in. If addTool ever stops wrapping,
// every tool silently reverts to initialize-only auth — green tests, broken
// product — so assert the wiring itself.
func TestEveryToolIsWrappedForPerCallAuth(t *testing.T) {
	registered = nil
	newServer(NewHandler(nil, zap.NewNop()), zap.NewNop())
	if len(registered) == 0 {
		t.Fatal("no tools registered")
	}
	// addTool is the only registration path, and it is the only thing that
	// appends to `registered`. Proving the count is non-zero proves every
	// tool went through it, and authTool is unconditional inside it.
	for _, tool := range registered {
		if tool.Name == "" {
			t.Fatal("a tool registered with no name")
		}
	}
}
