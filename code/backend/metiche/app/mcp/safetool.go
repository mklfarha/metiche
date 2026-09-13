package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"
)

// addTool registers a tool exactly like mcp.AddTool, but wraps the handler so
// a panic becomes a failed tool call instead of a crashed process.
//
// This matters more here than in a standalone MCP server. The SDK dispatches
// each request in its own goroutine with no recover(), so a panic unwinds to
// the top of that goroutine and takes the process down — and this process is
// also serving the REST API and holding every open SSE stream to every browser
// watching a board. One malformed report from one agent would otherwise blank
// the whole team's view.
// It also re-resolves the caller's bearer token on EVERY call — see authTool,
// which is not optional and explains why.
func addTool[In, Out any](s *mcp.Server, hdl *Handler, logger *zap.Logger, t *mcp.Tool, h mcp.ToolHandlerFor[In, Out]) {
	if t.Annotations == nil {
		// Not a convenience default: MCP's destructiveHint DEFAULTS TO TRUE
		// when annotations are omitted, so an unannotated tool advertises
		// itself as destructive and clients gate it. Failing here is the only
		// way that stays impossible rather than merely discouraged.
		panic(fmt.Sprintf("tool %q registered without annotations: destructiveHint defaults to TRUE when omitted", t.Name))
	}
	registered = append(registered, t)
	mcp.AddTool(s, t, authTool(hdl, timeTool(logger, t.Name, recoverTool(logger, t.Name, h))))
}

// authTool re-reads the caller's bearer token from THIS request and puts the
// resolved account on the context the tool actually runs with.
//
// WHY THIS EXISTS, because it looks redundant next to authMiddleware and is
// not: the SDK's streamable HTTP transport builds the MCP session from the
// context of the *initialize* request and hands that same context to every
// later tool call (mcp/streamable.go, "Pass req.Context() here, to allow
// middleware to add context values"). authMiddleware runs per HTTP request
// and decorates a context that tool handlers never see. So without this,
// only the Authorization header sent on `initialize` has any effect, and
// every header after it is silently ignored.
//
// Silently is the operative word, and it is why this is a tool-layer concern
// rather than a transport one. The failure is not a 401 — the call reaches
// the tool, the tool finds no account, and the agent is told to authenticate
// while holding a perfectly good token it is already sending. That is a
// confusing enough dead end to have cost an afternoon; it is tested by
// TestAuthToolReadsTheHeaderOfThisCall, TestAuthToolToleratesAMissingHeader and
// TestEveryToolIsWrappedForPerCallAuth (authpercall_test.go), and end to end
// through the real streamable transport by
// TestIntegrationTwoAgentsTwoTokensNoHints (identity_integration_test.go), so it
// cannot come back.
//
// req.Extra.Header is the header of the request being served right now, which
// is exactly the thing the session context cannot tell us.
func authTool[In, Out any](hdl *Handler, next mcp.ToolHandlerFor[In, Out]) mcp.ToolHandlerFor[In, Out] {
	return func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		if hdl == nil || req == nil || req.Extra == nil || req.Extra.Header == nil {
			return next(ctx, req, in)
		}
		// The client_key override, if the connection declares one. Read before
		// the token and applied regardless of whether the token resolves,
		// because it is not a credential: for a legacy account token it selects
		// among that person's agents, and for an agent token RequireAgent
		// refuses it when it names a different agent. See ClientKeyFromContext.
		ck := strings.TrimSpace(req.Extra.Header.Get("X-Metiche-Client-Key"))
		if ck != "" {
			ctx = WithClientKey(ctx, ck)
		}

		token := BearerFromHeader(req.Extra.Header.Get("Authorization"))
		if token == "" {
			token = strings.TrimSpace(req.Extra.Header.Get("X-Metiche-Token"))
		}
		if token == "" {
			// No token on this call. Deliberately NOT an error: create_team,
			// join_team and health are reachable without one, and the tools
			// that do need an account say so themselves with a message that
			// tells the agent what to do about it.
			return next(ctx, req, in)
		}
		ident, err := hdl.resolveToken(ctx, token)
		if err != nil {
			// Same reasoning: let the tool refuse. Returning a transport
			// error here would break the whole MCP session over one bad
			// call, and an agent that has to reconnect to recover is an
			// agent that stops using the tool.
			if hdl.logger != nil {
				hdl.logger.Warn("a tool call carried a token that did not resolve",
					zap.String("tool", toolName(req)))
			}
			return next(ctx, req, in)
		}
		// Gated on "a LEGACY ACCOUNT token and no client-key header" and on
		// nothing wider. An agent token names its agent, so a missing header
		// is the normal case for every new install and a line per call would
		// be pure noise. No token at all is create_team, join_team or health,
		// which need no agent. The anomaly worth a line is the one left: a
		// person-level token that does not say which agent is calling.
		if ck == "" && ident.Agent == nil {
			logMissingIdentityHeader(hdl.logger, req)
		}
		return next(WithIdentity(ctx, ident), req, in)
	}
}

// noIdentityHeaderMsg is the diagnostic logMissingIdentityHeader writes.
const noIdentityHeaderMsg = "mcp call arrived with no agent identity header"

// logMissingIdentityHeader records WHICH HEADERS ACTUALLY ARRIVED on a call
// whose account token names no agent. Names only, never values — one of them
// is a bearer token.
//
// This exists because "the client is configured correctly" and "the server
// received it" are different claims, and confusing them cost a long evening:
// a config file on disk was read, verified and still produced a request with
// no identity on it, and nothing could say whether the client never sent it or
// something in between dropped it. It is how the server proved that Claude
// Code drops custom headers from a plugin's .mcp.json. INFO rather than Debug
// because zap.NewProduction is Info level, and a Debug line would be invisible
// exactly where it is needed.
//
// Nil-safe on the logger and on every part of the request: it runs inside
// authTool, which wraps every tool, and a log line must never be what takes a
// call down.
func logMissingIdentityHeader(logger *zap.Logger, req *mcp.CallToolRequest) {
	if logger == nil {
		return
	}
	var names []string
	if req != nil && req.Extra != nil {
		names = make([]string, 0, len(req.Extra.Header))
		for k := range req.Extra.Header {
			names = append(names, k)
		}
		sort.Strings(names)
	}
	logger.Info(noIdentityHeaderMsg,
		zap.String("tool", toolName(req)),
		zap.String("token_scope", "account"),
		zap.Strings("headers_received", names))
}

// toolName is the called tool's name for a log line, or "" when the request
// carries no Params. Real calls always have Params, but a log line is the last
// place that should be able to panic: authTool wraps every tool, and a nil
// dereference here takes down a call that would otherwise have succeeded.
func toolName(req *mcp.CallToolRequest) string {
	if req == nil || req.Params == nil {
		return ""
	}
	return req.Params.Name
}

// registered records every tool passed to addTool so a test can assert over
// the whole surface. The SDK's Server exposes no way to enumerate its tools,
// and the annotations are load-bearing.
var registered []*mcp.Tool

func timeTool[In, Out any](logger *zap.Logger, name string, next mcp.ToolHandlerFor[In, Out]) mcp.ToolHandlerFor[In, Out] {
	return func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		start := time.Now()
		res, out, err := next(ctx, req, in)
		took := time.Since(start).Round(time.Millisecond)
		switch {
		case err != nil:
			logger.Warn("mcp tool failed", zap.String("tool", name), zap.Duration("took", took), zap.Error(err))
		case res != nil && res.IsError:
			logger.Warn("mcp tool returned an error result", zap.String("tool", name), zap.Duration("took", took))
		default:
			logger.Debug("mcp tool ok", zap.String("tool", name), zap.Duration("took", took))
		}
		return res, out, err
	}
}

func recoverTool[In, Out any](logger *zap.Logger, name string, next mcp.ToolHandlerFor[In, Out]) mcp.ToolHandlerFor[In, Out] {
	return func(ctx context.Context, req *mcp.CallToolRequest, in In) (res *mcp.CallToolResult, out Out, err error) {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("PANIC in mcp tool",
					zap.String("tool", name), zap.Any("panic", r), zap.ByteString("stack", debug.Stack()))
				var zero Out
				out = zero
				// Reported through the result, not as a transport error, so
				// the agent sees one failed call rather than a broken session
				// it has to reconnect.
				err = nil
				res = &mcp.CallToolResult{
					IsError: true,
					Content: []mcp.Content{&mcp.TextContent{
						Text: fmt.Sprintf("internal error in tool %q — the call did not apply; retry it with the same idempotency_key", name)}},
				}
			}
		}()
		return next(ctx, req, in)
	}
}

// jsonResult renders already-marshalled JSON as the text content MCP clients
// expect.
//
// It takes bytes rather than a value on purpose: the bytes commit returns are
// the same bytes stored for a replay, and re-marshalling here would let the
// two drift.
func jsonResult(raw json.RawMessage) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(raw)}},
	}, nil, nil
}

// jsonValue renders a value for the read-only tools, which have no stored
// snapshot to stay identical to.
func jsonValue(v any) (*mcp.CallToolResult, any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, nil, err
	}
	return jsonResult(b)
}

// transient classifies an error as worth retrying, so a tool failure can tell
// an agent whether to back off or give up.
//
// The distinction matters because the two failures demand opposite behaviour:
// a dropped database connection should be retried with the same idempotency
// key and will converge, while a malformed session_key will fail identically
// forever. Reported as one generic error, an agent has to guess, and it
// guesses wrong in whichever direction is worse.
func transient(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded)
}

// retryable wraps an error with the structured hint an agent needs. The text
// is what an MCP client surfaces, so the code and the advice have to live in
// it.
func retryable(err error, what string) error {
	if err == nil {
		return nil
	}
	if transient(err) {
		return fmt.Errorf("code=provider_unavailable retryable=true retry_after_seconds=5 — %s failed transiently: %w. "+
			"Retry the same call with the same idempotency_key; it will not double-apply", what, err)
	}
	return fmt.Errorf("code=invalid_request retryable=false — %s failed: %w", what, err)
}
