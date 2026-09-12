package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"runtime/debug"
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
func addTool[In, Out any](s *mcp.Server, logger *zap.Logger, t *mcp.Tool, h mcp.ToolHandlerFor[In, Out]) {
	if t.Annotations == nil {
		// Not a convenience default: MCP's destructiveHint DEFAULTS TO TRUE
		// when annotations are omitted, so an unannotated tool advertises
		// itself as destructive and clients gate it. Failing here is the only
		// way that stays impossible rather than merely discouraged.
		panic(fmt.Sprintf("tool %q registered without annotations: destructiveHint defaults to TRUE when omitted", t.Name))
	}
	registered = append(registered, t)
	mcp.AddTool(s, t, timeTool(logger, t.Name, recoverTool(logger, t.Name, h)))
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
