package mcp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gofrs/uuid"
	"go.uber.org/zap"

	agent_entity "github.com/mklfarha/metiche/backend/entity/agent"
	"github.com/mklfarha/metiche/backend/enums"
)

// metiche does its own auth. nuzur's auth integration is disabled for this
// project, and the MCP endpoint is public by design — this is a public
// repository and mcp.metiche.xyz answers anyone who asks. "No third-party
// credential required to run the server" is a constraint on DEPENDENCIES, not
// a licence to leave the door open: a team's board is not public.
//
// The scheme, end to end:
//
//   - a team has a rotatable join code, which is the only thing an agent needs
//     to know to get in;
//   - join_team exchanges it for a per-agent bearer token, minted here from
//     crypto/rand and shown exactly once;
//   - only sha256(token) is stored, on the agent row, so a dump of the
//     database yields nothing anyone can authenticate with;
//   - every later request carries the token, and the middleware resolves it to
//     an agent before a tool runs.
//
// A token is never logged, never returned in an error, and never written to
// team_event.response_snapshot (see join.go).
//
// ── MIGRATION: schema v3 moves identity out from under this file ────────────
//
// Everything below assumes the v1 shape: the token hash lives on `agent`, an
// agent belongs to a team, and a team carries a single rotatable join_code.
// v3 replaces all three:
//
//	account          new; the token hash lives here, and an account is a person
//	                 ACROSS teams (anonymous, optionally claimed later)
//	agent            loses token_hash, team_uuid and member_uuid; gains
//	                 account_uuid — an agent belongs to a person, not a team
//	member           becomes the JOIN of account and team (account_uuid,
//	                 revoked_at; unique on (account_uuid, team_uuid))
//	invite           replaces team.join_code: time-boxed, revocable, use-capped
//	team             gains visibility (private by default) and
//	                 requires_claimed_accounts
//
// The consequence for this package is that resolveToken can no longer return
// "the agent, and therefore the team". It resolves to an ACCOUNT, and the team
// becomes a per-call scope: every tool that reads ag.TeamUUID today has to take
// the team from its own arguments (or from the session it names) and check
// membership. That is a change to the identity layer only — the transactional
// core in sequence.go, the envelope, the panic wrapper and the lock metric all
// take the team uuid as a parameter and are unaffected.
//
// The full list of call sites is in the handover notes for this work.

const (
	// tokenPrefix makes a leaked token greppable in a repo scan and tells a
	// human what they are looking at.
	tokenPrefix = "mtk_"
	// tokenEntropyBytes is 32 bytes = 256 bits from crypto/rand. Nothing here
	// is derived from time, a uuid, or a counter.
	tokenEntropyBytes = 32
)

// ErrUnauthenticated is what a tool returns when it needs an agent and the
// request carried no usable token.
var ErrUnauthenticated = errors.New(
	"this tool needs an agent token: pass Authorization: Bearer <token> on the MCP connection. " +
		"Call join_team with your team's join code to get one")

// MintToken returns a fresh bearer token and the sha256 hex digest that is the
// only part of it the server keeps.
//
// The digest is 64 hex characters, which is exactly agent.token_hash's width —
// not a coincidence, the column was sized for it.
func MintToken() (token string, hash string, err error) {
	buf := make([]byte, tokenEntropyBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("could not generate a token: %w", err)
	}
	token = tokenPrefix + base64.RawURLEncoding.EncodeToString(buf)
	return token, HashToken(token), nil
}

// joinCodeAlphabet is Crockford base32: no I, L, O or U, so a code read aloud
// across a room or typed off a screen cannot be turned into a different valid
// code by the usual 0/O and 1/I confusions. 32 divides 256, so sampling a
// random byte modulo 32 is uniform with no rejection loop.
const joinCodeAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// joinCodeLength of 10 is 32^10 ≈ 1.1e15 codes. Guessing one is bounded by
// the per-IP rate limit on join_team rather than by the code alone, but the
// code should not be the weak half.
const joinCodeLength = 10

// MintJoinCode returns a fresh team join code from crypto/rand.
//
// Unlike a bearer token, a join code is stored in the clear: it is a SHARED
// secret that the person who made the team has to be able to read back and
// pass to their teammates, and a hash cannot be read back. It is rotatable
// for exactly that reason — see team.join_code_rotated_at — and it is never
// logged.
func MintJoinCode() (string, error) {
	buf := make([]byte, joinCodeLength)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("could not generate a join code: %w", err)
	}
	out := make([]byte, joinCodeLength)
	for i, b := range buf {
		out[i] = joinCodeAlphabet[int(b)%len(joinCodeAlphabet)]
	}
	return string(out), nil
}

// HashToken is the one definition of "the stored form of a token".
//
// A plain sha256 rather than a password hash on purpose: this is a 256-bit
// random value, not a human-chosen secret, so there is no dictionary to make
// bcrypt worth its latency on every single request — and this runs on the hot
// path of every tool call.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(sum[:])
}

// VerifyToken reports whether a presented token matches a stored hash, in
// constant time with respect to the hash contents.
//
// subtle.ConstantTimeCompare rather than ==: string equality returns at the
// first differing byte, and over enough requests that is a measurable oracle
// for guessing a hash prefix.
func VerifyToken(token, storedHash string) bool {
	if token == "" || len(storedHash) != sha256.Size*2 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(HashToken(token)), []byte(storedHash)) == 1
}

// BearerFromHeader pulls the token out of an Authorization header.
// Case-insensitive on the scheme, because clients disagree about it.
func BearerFromHeader(h string) string {
	h = strings.TrimSpace(h)
	if len(h) < 7 || !strings.EqualFold(h[:7], "bearer ") {
		return ""
	}
	return strings.TrimSpace(h[7:])
}

// ─────────────────────────────────────────────
// Request context
// ─────────────────────────────────────────────

type agentCtxKey struct{}

// WithAgent puts the authenticated agent on the context. Exported so a test
// can drive a tool handler directly without an HTTP round trip.
func WithAgent(ctx context.Context, a agent_entity.Agent) context.Context {
	return context.WithValue(ctx, agentCtxKey{}, a)
}

// AgentFromContext returns the agent the middleware resolved, if any.
func AgentFromContext(ctx context.Context) (agent_entity.Agent, bool) {
	a, ok := ctx.Value(agentCtxKey{}).(agent_entity.Agent)
	return a, ok
}

// requireAgent is the guard every tool except join_team and health calls
// first.
//
// Two tools are deliberately open. join_team cannot require a token because
// minting one is its entire job — it authenticates with the team's join code
// instead. health cannot require a token because its whole purpose is to
// answer "is the server up?" when something is wrong, and "your token was
// rejected" is not that answer.
func (h *Handler) requireAgent(ctx context.Context) (agent_entity.Agent, error) {
	a, ok := AgentFromContext(ctx)
	if !ok {
		return agent_entity.Agent{}, ErrUnauthenticated
	}
	if a.Status != enums.AGENT_STATUS_ACTIVE {
		return agent_entity.Agent{}, errors.New("this agent has been retired; call join_team again to get a new token")
	}
	return a, nil
}

// resolveToken turns a presented token into its agent.
//
// The lookup is by hash — the hash is the stored value, so it is the only
// thing we can index on — and the match is then re-checked in constant time.
// The SQL equality is not the security boundary; VerifyToken is.
//
// Raw SQL because agent.token_hash has no generated fetch-by-index: it is not
// a modelled index in nuzur, and adding one is a schema change that belongs in
// nuzur rather than in Go. The table is small (a team is ≤10 people × ≤5
// agents), so this is a short scan on a small table, and the fix when it stops
// being small is an index in the schema, not a cache here that would outlive a
// revocation.
func (h *Handler) resolveToken(ctx context.Context, token string) (agent_entity.Agent, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return agent_entity.Agent{}, ErrUnauthenticated
	}
	hash := HashToken(token)

	var (
		id, teamUUID, memberUUID, key, label, storedHash, clientKey string
		clientKind                                                  *string
		status                                                      int64
	)
	err := h.core.DB().QueryRowContext(ctx,
		"SELECT `id`, `team_uuid`, `member_uuid`, `key`, `label`, `client_kind`, `token_hash`, `client_key`, `status` "+
			"FROM `agent` WHERE `token_hash` = ? LIMIT 1", hash).
		Scan(&id, &teamUUID, &memberUUID, &key, &label, &clientKind, &storedHash, &clientKey, &status)
	if err != nil {
		// Never distinguish "no such token" from a database problem in the
		// message that reaches the caller — one of them is a probe.
		return agent_entity.Agent{}, ErrUnauthenticated
	}
	if !VerifyToken(token, storedHash) {
		return agent_entity.Agent{}, ErrUnauthenticated
	}

	a := agent_entity.Agent{
		Key:        key,
		Label:      label,
		TokenHash:  storedHash,
		ClientKey:  clientKey,
		Status:     enums.AgentStatus(status),
		ClientKind: nullString(derefString(clientKind)),
	}
	if a.ID, err = uuid.FromString(id); err != nil {
		return agent_entity.Agent{}, ErrUnauthenticated
	}
	if a.TeamUUID, err = uuid.FromString(teamUUID); err != nil {
		return agent_entity.Agent{}, ErrUnauthenticated
	}
	if a.MemberUUID, err = uuid.FromString(memberUUID); err != nil {
		return agent_entity.Agent{}, ErrUnauthenticated
	}
	return a, nil
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// authMiddleware resolves the bearer token on every MCP request and puts the
// agent on the request context.
//
// It RESOLVES rather than rejects when no token is present, because one tool
// on this endpoint exists to hand out tokens and one exists to answer when
// everything else is failing. A request with no token reaches the server and
// can call join_team and health, and nothing else — requireAgent stops the
// rest. A request with a token that does not resolve is rejected here with
// 401, because a wrong token is a mistake worth failing fast and loudly,
// and letting it through as "anonymous" would turn a typo into a confusing
// "this tool needs a token" on a call that supplied one.
//
// The token is never logged. The warning below records that a rejection
// happened and from where, and nothing about the credential.
func (h *Handler) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The caller's address rides on the context for the two tools that
		// answer without a token: they are rate limited by it.
		ctx := WithClientIP(r.Context(), clientIP(r))
		r = r.WithContext(ctx)

		token := BearerFromHeader(r.Header.Get("Authorization"))
		if token == "" {
			// Some MCP clients cannot set an Authorization header but can set
			// a custom one; accept the conventional fallback.
			token = strings.TrimSpace(r.Header.Get("X-Metiche-Token"))
		}
		if token == "" {
			next.ServeHTTP(w, r)
			return
		}
		ag, err := h.resolveToken(ctx, token)
		if err != nil {
			// The token itself is never logged — only that a rejection
			// happened, and from where.
			h.logger.Warn("rejected an MCP request with an unrecognised token",
				zap.String("remote_addr", r.RemoteAddr),
				zap.String("path", r.URL.Path))
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("WWW-Authenticate", `Bearer realm="metiche"`)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized","detail":"that agent token is not valid; call join_team with your team's join code to get a new one"}`))
			return
		}
		next.ServeHTTP(w, r.WithContext(WithAgent(ctx, ag)))
	})
}
