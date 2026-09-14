package mcp

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mklfarha/metiche/backend/enums"
)

// ─────────────────────────────────────────────
// Tool: whoami (docs/CLI.md §4.1)
// ─────────────────────────────────────────────
//
// The gap: an agent cannot learn its own identity, and a human cannot prove
// which headers a client actually delivers. whoami answers "what does the
// server see of me": the agent the token names, the header NAMES this very
// request carried, and which user agents have recently arrived with this
// token. The last is what lets `metiche doctor` tell "Claude Code connected
// with its token" from "Claude Code connected, but without the Authorization
// header" — a ✔ Connected that proves nothing.
//
// What it guarantees:
//
//   - NO TOKEN, NO DATABASE. An unauthenticated call answers authenticated:
//     false from the request alone. A present but invalid token never reaches
//     this code: authMiddleware answers 401 first.
//   - NAMES, NEVER VALUES. headers_received is the canonicalized, sorted set of
//     header names; one of them is a bearer token.
//   - YOUR OWN EVIDENCE ONLY. recent_requests is keyed by the agent (or, for a
//     legacy token, the account) and returned only to a token of that same
//     agent or account.

type WhoamiParams struct{}

type WhoamiAgent struct {
	Key        string `json:"key"`
	Label      string `json:"label"`
	ClientKey  string `json:"client_key"`
	ClientKind string `json:"client_kind,omitempty"`
	Status     string `json:"status"`
}

type RecentRequest struct {
	UserAgent string `json:"user_agent"`
	LastAt    string `json:"last_at"`
}

// WhoamiResult is not an Envelope, for ListTeamsResult's reason: it is about
// no team, so there is no sequence or revision it could carry.
type WhoamiResult struct {
	OK            bool   `json:"ok"`
	Authenticated bool   `json:"authenticated"`
	TokenScope    string `json:"token_scope"`
	AccountKey    string `json:"account_key,omitempty"`
	// Agent is null unless token_scope is agent.
	Agent               *WhoamiAgent    `json:"agent"`
	Teams               int             `json:"teams"`
	RecentRequests      []RecentRequest `json:"recent_requests"`
	RecentRequestsScope string          `json:"recent_requests_scope"`
	HeadersReceived     []string        `json:"headers_received"`
	ServerTime          string          `json:"server_time"`
	ProtocolVersion     string          `json:"protocol_version"`
	Note                string          `json:"note"`
}

const (
	whoamiNoneNote = "No token arrived on this request. If your client's config has an Authorization header, the client did not send it."
	whoamiAcctNote = "This is a pre-per-agent account token: it names a person, not this client. Re-run the installer."
)

// Whoami answers who the token on THIS call names.
func (h *Handler) Whoami(ctx context.Context, req *mcp.CallToolRequest, _ WhoamiParams) (*mcp.CallToolResult, any, error) {
	now := time.Now().UTC()
	out := WhoamiResult{
		OK:                  true,
		TokenScope:          "none",
		RecentRequests:      []RecentRequest{},
		RecentRequestsScope: h.requestRecorder().scope(),
		HeadersReceived:     headerNames(req),
		ServerTime:          now.Format(time.RFC3339),
		ProtocolVersion:     ProtocolVersion,
		Note:                whoamiNoneNote,
	}

	id, ok := IdentityFromContext(ctx)
	if !ok {
		return jsonValue(out)
	}
	acct, err := h.requireAccount(ctx)
	if err != nil {
		return nil, nil, err
	}

	out.Authenticated = true
	out.AccountKey = acct.Key
	out.RecentRequests = h.requestRecorder().recent(identityRecordKey(id))
	if id.Agent != nil {
		out.TokenScope = "agent"
		out.Agent = &WhoamiAgent{
			Key:        id.Agent.Key,
			Label:      id.Agent.Label,
			ClientKey:  id.Agent.ClientKey,
			ClientKind: id.Agent.ClientKind.String,
			Status:     id.Agent.Status.String(),
		}
		out.Note = "Your token identifies agent " + id.Agent.ClientKey + " (" + id.Agent.Label + "). You never need client_key."
	} else {
		out.TokenScope = "account"
		out.Note = whoamiAcctNote
	}

	if err := h.core.DB().QueryRowContext(ctx,
		"SELECT COUNT(*) FROM `member` m JOIN `team` t ON t.`id` = m.`team_uuid` "+
			"WHERE m.`account_uuid` = ? AND m.`revoked_at` IS NULL AND m.`status` = ? AND t.`status` = ?",
		acct.ID.String(), enums.RECORD_STATUS_ACTIVE, enums.RECORD_STATUS_ACTIVE).Scan(&out.Teams); err != nil {
		return nil, nil, retryable(err, "counting your teams")
	}
	return jsonValue(out)
}

// ingressHeaders are added by a proxy, not sent by the client, so they are
// left out of headers_received: it reflects what the CLIENT sent.
var ingressHeaders = map[string]bool{
	"Forwarded":                 true,
	"X-Real-Ip":                 true,
	"X-Forwarded-For":           true,
	"X-Forwarded-Host":          true,
	"X-Forwarded-Proto":         true,
	"X-Forwarded-Port":          true,
	"X-Forwarded-Proto-Version": true,
	"X-Forwarded-Scheme":        true,
	"X-Forwarded-Server":        true,
	"X-Forwarded-Prefix":        true,
	"X-Request-Id":              true,
	"X-Scheme":                  true,
}

// headerNames is the sorted, canonical header names of the request being
// served. Values are never read.
func headerNames(req *mcp.CallToolRequest) []string {
	names := []string{}
	if req == nil || req.Extra == nil {
		return names
	}
	for k := range req.Extra.Header {
		c := http.CanonicalHeaderKey(k)
		if ingressHeaders[c] || strings.HasPrefix(c, "X-Forwarded-") {
			continue
		}
		names = append(names, c)
	}
	sort.Strings(names)
	return names
}

// ─────────────────────────────────────────────
// recent_requests: an in-process record kept at the HTTP edge
// ─────────────────────────────────────────────
//
// Recorded for EVERY HTTP request whose token resolves — initialize and
// tools/list as well as tool calls — because a client's own health check
// ("claude mcp get") runs no tool, and it is exactly that request doctor needs
// to see. In memory, not a column: no schema change, no database write on the
// hot path, and agent.last_seen_at keeps its meaning. v1 runs one pod
// (METICHE_ROLE=all), so the record is complete; recent_requests_scope says
// what it covers.

const (
	recentPerIdentity  = 8
	recentUserAgentMax = 80
	recentTTL          = time.Hour
	recentSweepEvery   = 5 * time.Minute
)

type recentUA struct {
	ua string
	at time.Time
}

type recentEntry struct {
	lastSeen time.Time
	uas      []recentUA // most recent first, unique by ua
}

type requestRecorder struct {
	mu        sync.Mutex
	started   time.Time
	lastSweep time.Time
	byKey     map[string]*recentEntry
}

// recordersByHandler holds one record per Handler, like accountLimitersByHandler:
// one per process in production, one per test server.
var recordersByHandler sync.Map

func (h *Handler) requestRecorder() *requestRecorder {
	if v, ok := recordersByHandler.Load(h); ok {
		return v.(*requestRecorder)
	}
	now := time.Now().UTC()
	v, _ := recordersByHandler.LoadOrStore(h, &requestRecorder{started: now, lastSweep: now, byKey: map[string]*recentEntry{}})
	return v.(*requestRecorder)
}

// identityRecordKey: the agent when the token names one, else the account.
func identityRecordKey(id Identity) string {
	if id.Agent != nil {
		return "agent:" + id.Agent.ID.String()
	}
	return "account:" + id.Account.ID.String()
}

// recordRequests sits INSIDE authMiddleware, so the identity it reads is the
// one the middleware resolved from this request's own token. A request with no
// token has no identity and is recorded nowhere.
func (h *Handler) recordRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id, ok := IdentityFromContext(r.Context()); ok {
			h.requestRecorder().record(identityRecordKey(id), r.UserAgent(), time.Now().UTC())
		}
		next.ServeHTTP(w, r)
	})
}

func (rr *requestRecorder) record(key, userAgent string, now time.Time) {
	ua := printableUserAgent(userAgent)
	rr.mu.Lock()
	defer rr.mu.Unlock()
	if now.Sub(rr.lastSweep) > recentSweepEvery {
		for k, e := range rr.byKey {
			if now.Sub(e.lastSeen) > recentTTL {
				delete(rr.byKey, k)
			}
		}
		rr.lastSweep = now
	}
	e := rr.byKey[key]
	if e == nil {
		e = &recentEntry{}
		rr.byKey[key] = e
	}
	e.lastSeen = now
	kept := make([]recentUA, 0, recentPerIdentity)
	kept = append(kept, recentUA{ua: ua, at: now})
	for _, prev := range e.uas {
		if prev.ua == ua {
			continue
		}
		if len(kept) == recentPerIdentity {
			break
		}
		kept = append(kept, prev)
	}
	e.uas = kept
}

func (rr *requestRecorder) recent(key string) []RecentRequest {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	out := []RecentRequest{}
	e := rr.byKey[key]
	if e == nil || time.Since(e.lastSeen) > recentTTL {
		return out
	}
	for _, u := range e.uas {
		out = append(out, RecentRequest{UserAgent: u.ua, LastAt: u.at.Format(time.RFC3339)})
	}
	return out
}

func (rr *requestRecorder) scope() string {
	return "this server process, since " + rr.started.Format(time.RFC3339)
}

// printableUserAgent keeps printable ASCII only, cut to 80 characters, so a
// hostile user agent cannot put control characters into a response.
func printableUserAgent(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r > 0x7e {
			continue
		}
		b.WriteRune(r)
		if b.Len() == recentUserAgentMax {
			break
		}
	}
	if b.Len() == 0 {
		return "(no user agent)"
	}
	return b.String()
}
