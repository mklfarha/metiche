package mcp

import (
	"context"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ─────────────────────────────────────────────
// Tool: health
// ─────────────────────────────────────────────

type HealthParams struct{}

type HealthResult struct {
	OK              bool   `json:"ok"`
	ProtocolVersion string `json:"protocol_version"`
	Database        string `json:"database"`
	Authenticated   bool   `json:"authenticated"`
	ServerTime      string `json:"server_time"`
	Note            string `json:"note,omitempty"`
}

// Health answers the question an agent cannot otherwise answer during an
// outage: is metiche down, or is my session broken?
//
// Without it both look identical — a transport error on whatever tool the
// agent happened to call next — and an agent that cannot tell them apart
// either abandons perfectly good work or keeps hammering a dead server.
//
// It touches the database, so a reachable server with an unreachable database
// reports unhealthy rather than cheerfully OK. And it is one of the two tools
// that does NOT require a token: "your credentials were rejected" is not an
// answer to "is the server up?", and an agent whose token has gone bad needs
// to be able to tell that apart from an outage.
func (h *Handler) Health(ctx context.Context, _ *mcp.CallToolRequest, _ HealthParams) (*mcp.CallToolResult, any, error) {
	_, authed := AgentFromContext(ctx)
	res := HealthResult{
		OK:              true,
		ProtocolVersion: ProtocolVersion,
		Database:        "reachable",
		Authenticated:   authed,
		ServerTime:      time.Now().UTC().Format(time.RFC3339),
	}

	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := h.core.DB().PingContext(pingCtx); err != nil {
		res.OK = false
		res.Database = "unreachable"
		res.Note = "the server is up but its database is not; every other tool will fail until this clears — retry rather than abandoning your session"
	} else if !authed {
		res.Note = "no agent token on this connection: only join_team and health will work. Call join_team with your team's join code."
	}
	return jsonValue(res)
}
