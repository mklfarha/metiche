package mcp

import (
	"context"
	"database/sql"
	"strings"

	"github.com/gofrs/uuid"

	account_entity "github.com/mklfarha/metiche/backend/entity/account"
	"github.com/mklfarha/metiche/backend/enums"
)

// This file exists for exactly one caller: app/authz, the gate in front of
// app/webapi and app/stream.
//
// Those two packages serve the board over the PUBLIC router, and a private
// team must only be readable by an account that is a member of it. To decide
// that they have to answer "whose token is this?" — and they have to answer it
// the same way this package does, because a second, subtly different
// implementation of a credential check is how one of them ends up skipping
// VerifyToken. Everything they need was already here; all that was missing was
// a door.
//
// Why it is not a method: resolveToken hangs off *Handler, which carries the
// MCP tool surface, the detector, the rate limiter and core.Implementation.
// The board's read path has none of that and should not have to construct it
// to look up a token — it holds a *sql.DB, which is all the lookup ever
// actually touches.
//
// Why it is its own file: it is not part of the MCP auth story, it is a seam
// cut for a different package, and keeping it separate makes that legible —
// and means auth.go itself is untouched.
//
// The direction of the dependency is one-way and must stay that way:
// app/authz imports app/mcp. Nothing in app/mcp may import app/authz.

// AccountByToken resolves a presented bearer token to its ACCOUNT, over a bare
// database handle.
//
// Semantics are identical to the unexported resolveToken that the MCP
// middleware uses, and deliberately so:
//
//   - the lookup is by HashToken(token), because the hash is the stored value
//     and therefore the only thing that can be indexed;
//   - the match is then re-checked with VerifyToken, in constant time. The SQL
//     equality is not the security boundary; VerifyToken is;
//   - every failure — no such token, a hash collision that does not verify, a
//     database that is unhappy, an unparseable row — comes back as the one
//     opaque ErrUnauthenticated. A caller that can tell those apart has an
//     oracle, and one of those callers is always a probe.
//
// It does NOT check account.Status: "who is this token" and "is this person
// still active" are two questions, and the caller decides what to do about the
// second. app/authz treats anything but RECORD_STATUS_ACTIVE as a denial.
//
// The token is never logged here, and never appears in the returned error.
//
// resolveToken, which the MCP auth middleware and every tool call go through,
// is a one-line delegation to this function — so there is exactly one
// implementation of "whose token is this?" in the process, and the board and
// the tool surface cannot drift apart on it.
func AccountByToken(ctx context.Context, db *sql.DB, token string) (account_entity.Account, error) {
	// Trim BEFORE the empty check, not after: HashToken trims too, so a token
	// of pure whitespace would otherwise be hashed as the empty string and
	// sent to the database as a real-looking lookup.
	token = strings.TrimSpace(token)
	if token == "" {
		return account_entity.Account{}, ErrUnauthenticated
	}
	hash := HashToken(token)

	var (
		id, key, displayName, storedHash string
		identityProvider, status         int64
		identitySubject, identityHandle  sql.NullString
		email                            sql.NullString
		claimedAt, lastSeenAt            sql.NullTime
	)
	err := db.QueryRowContext(ctx,
		"SELECT `id`, `key`, `display_name`, `token_hash`, `identity_provider`, `identity_subject`, "+
			"`identity_handle`, `email`, `claimed_at`, `status`, `last_seen_at` "+
			"FROM `account` WHERE `token_hash` = ? LIMIT 1", hash).
		Scan(&id, &key, &displayName, &storedHash, &identityProvider, &identitySubject,
			&identityHandle, &email, &claimedAt, &status, &lastSeenAt)
	if err != nil {
		// Never distinguish "no such token" from a database problem in the
		// message that reaches the caller — one of them is a probe.
		return account_entity.Account{}, ErrUnauthenticated
	}
	if !VerifyToken(token, storedHash) {
		return account_entity.Account{}, ErrUnauthenticated
	}

	a := account_entity.Account{
		Key:              key,
		DisplayName:      displayName,
		TokenHash:        storedHash,
		IdentityProvider: enums.IdentityProvider(identityProvider),
		IdentitySubject:  nullString(identitySubject.String),
		IdentityHandle:   nullString(identityHandle.String),
		Email:            nullString(email.String),
		Status:           enums.RecordStatus(status),
	}
	if claimedAt.Valid {
		a.ClaimedAt = nullTime(claimedAt.Time)
	}
	if lastSeenAt.Valid {
		a.LastSeenAt = nullTime(lastSeenAt.Time)
	}
	if a.ID, err = uuid.FromString(id); err != nil {
		return account_entity.Account{}, ErrUnauthenticated
	}
	return a, nil
}

// ── the agent's own identity, carried by the connection ──────────────────────

// clientKeyKey is the context key for X-Metiche-Client-Key.
type clientKeyKey struct{}

// WithClientKey puts the calling agent's client_key on the context. Set by
// authTool from the header of the request being served.
func WithClientKey(ctx context.Context, key string) context.Context {
	if key == "" {
		return ctx
	}
	return context.WithValue(ctx, clientKeyKey{}, key)
}

// ClientKeyFromContext returns the client_key this connection declared, if any.
//
// WHY THIS EXISTS. The token identifies the PERSON, deliberately: one
// credential covers every team you are on and every agent you run. The cost of
// that choice is that a person with three agents sends three identical-looking
// requests, and the server cannot tell which agent is calling.
//
// The obvious answer -- make the agent pass client_key -- turned out not to
// work, because AN AGENT HAS NO WAY TO LEARN ITS OWN client_key. It is chosen
// by the installer and then thrown away; it is not in the MCP config, the
// environment, or the repository. Asked for it, a model guesses: "backend",
// "ui", "codex". Every guess is rejected, and a guess that happened to hit
// would be worse -- it would file the work under someone else's agent on a
// board other people are reading.
//
// So the connection carries it, the same way it carries the token. The
// installer knows exactly which agent it is configuring, and writes the header
// alongside the Authorization one. The agent never has to know its own name,
// which is the only arrangement that cannot be guessed wrong.
//
// It is NOT a credential and grants nothing on its own: it only selects among
// agents that already belong to the authenticated account, and an unknown
// value is an error rather than a new agent.
func ClientKeyFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(clientKeyKey{}).(string); ok {
		return v
	}
	return ""
}
