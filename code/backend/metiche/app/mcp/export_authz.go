package mcp

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/gofrs/uuid"

	account_entity "github.com/mklfarha/metiche/backend/entity/account"
	agent_entity "github.com/mklfarha/metiche/backend/entity/agent"
	"github.com/mklfarha/metiche/backend/enums"
)

// This file exists for exactly one caller outside this package: app/authz, the
// gate in front of app/webapi and app/stream.
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
// The direction of the dependency is one-way and must stay that way:
// app/authz imports app/mcp. Nothing in app/mcp may import app/authz.

// Identity is who a bearer token names.
//
// Since schema v4 a token names an AGENT: one client install (Claude Code on
// this laptop, Codex on that one), stored as agent.token_hash. Agent is then
// set, and Account is the person that agent belongs to.
//
// A token minted before v4 names only an ACCOUNT (account.token_hash). Agent is
// then nil, and which of that person's agents is calling has to come from
// somewhere else — the legacy client_key chain in RequireAgent. That path
// exists so installs made before v4 keep working until they are re-run; no new
// account is ever given a usable account token.
type Identity struct {
	Account account_entity.Account
	Agent   *agent_entity.Agent
}

// IdentityByToken resolves a presented bearer token to the identity it names,
// over a bare database handle. It is THE one implementation of "whose token is
// this?" in the process: resolveToken (every MCP request and every tool call)
// and AccountByToken (the board gate) both delegate here.
//
// Two lookups, in order:
//
//  1. agent.token_hash, joined to its account. A match must VerifyToken, and
//     the agent must be ACTIVE: a retired agent's token is dead at the edge,
//     on the MCP surface and the board alike, rather than authenticating a
//     caller who is then refused one tool at a time.
//  2. account.token_hash — the legacy anchor. Identity{Account, nil}.
//
// Semantics that did not change with v4:
//
//   - the lookup is by HashToken(token), because the hash is the stored value
//     and therefore the only thing that can be indexed;
//   - the match is then re-checked with VerifyToken, in constant time. The SQL
//     equality is not the security boundary; VerifyToken is;
//   - every failure — no such token, a hash that does not verify, a retired
//     agent, a database that is unhappy or absent, an unparseable row — comes
//     back as the one opaque ErrUnauthenticated. A caller that can tell those
//     apart has an oracle, and one of those callers is always a probe.
//
// It does NOT check account.Status: "who is this token" and "is this person
// still active" are two questions, and each caller answers the second.
//
// A nil db is ErrUnauthenticated, not a panic. Production always has one; a
// Handler built without a core (the tool-surface tests do this) must still be
// able to receive a bearer header without crashing the call.
//
// The token is never logged here, and never appears in the returned error.
func IdentityByToken(ctx context.Context, db *sql.DB, token string) (Identity, error) {
	// Trim BEFORE the empty check, not after: HashToken trims too, so a token
	// of pure whitespace would otherwise be hashed as the empty string and
	// sent to the database as a real-looking lookup.
	token = strings.TrimSpace(token)
	if token == "" || db == nil {
		return Identity{}, ErrUnauthenticated
	}
	hash := HashToken(token)

	var (
		agentID, agentKey, label, clientKey, agentAccount, agentHash string
		clientKind                                                   sql.NullString
		agentStatus                                                  int64
		agentLastSeen                                                sql.NullTime
		acct                                                         accountRow
	)
	err := db.QueryRowContext(ctx,
		"SELECT a.`id`, a.`key`, a.`label`, a.`client_kind`, a.`client_key`, a.`status`, a.`last_seen_at`, "+
			"a.`account_uuid`, a.`token_hash`, "+accountColumns("c")+" "+
			"FROM `agent` a JOIN `account` c ON c.`id` = a.`account_uuid` "+
			"WHERE a.`token_hash` = ? LIMIT 1", hash).
		Scan(append([]any{&agentID, &agentKey, &label, &clientKind, &clientKey, &agentStatus, &agentLastSeen,
			&agentAccount, &agentHash}, acct.dest()...)...)
	switch {
	case err == nil:
		if !VerifyToken(token, agentHash) {
			return Identity{}, ErrUnauthenticated
		}
		if enums.AgentStatus(agentStatus) != enums.AGENT_STATUS_ACTIVE {
			return Identity{}, ErrUnauthenticated
		}
		account, err := acct.entity()
		if err != nil {
			return Identity{}, ErrUnauthenticated
		}
		ag := agent_entity.Agent{
			Key:        agentKey,
			Label:      label,
			ClientKind: nullString(clientKind.String),
			ClientKey:  clientKey,
			Status:     enums.AgentStatus(agentStatus),
			TokenHash:  nullString(agentHash),
		}
		if agentLastSeen.Valid {
			ag.LastSeenAt = nullTime(agentLastSeen.Time)
		}
		if ag.ID, err = uuid.FromString(agentID); err != nil {
			return Identity{}, ErrUnauthenticated
		}
		if ag.AccountUUID, err = uuid.FromString(agentAccount); err != nil || ag.AccountUUID != account.ID {
			return Identity{}, ErrUnauthenticated
		}
		return Identity{Account: account, Agent: &ag}, nil
	case errors.Is(err, sql.ErrNoRows):
		// Not an agent token. Fall through to the legacy account lookup.
	default:
		// Never distinguish "no such token" from a database problem in the
		// message that reaches the caller — one of them is a probe.
		return Identity{}, ErrUnauthenticated
	}

	var legacy accountRow
	if err := db.QueryRowContext(ctx,
		"SELECT "+accountColumns("")+" FROM `account` WHERE `token_hash` = ? LIMIT 1", hash).
		Scan(legacy.dest()...); err != nil {
		return Identity{}, ErrUnauthenticated
	}
	if !VerifyToken(token, legacy.storedHash) {
		return Identity{}, ErrUnauthenticated
	}
	account, err := legacy.entity()
	if err != nil {
		return Identity{}, ErrUnauthenticated
	}
	return Identity{Account: account}, nil
}

// AccountByToken resolves a presented bearer token to its ACCOUNT: the person
// behind an agent token, or the account a legacy token names.
//
// It is a delegation to IdentityByToken and nothing more, so the board gate
// inherits every rule there — including that a retired agent's token resolves
// to nobody — without app/authz having to know agents exist.
func AccountByToken(ctx context.Context, db *sql.DB, token string) (account_entity.Account, error) {
	id, err := IdentityByToken(ctx, db, token)
	if err != nil {
		return account_entity.Account{}, err
	}
	return id.Account, nil
}

// accountRow is the scan target for the account columns both lookups read.
type accountRow struct {
	id, key, displayName, storedHash       string
	identityProvider, status               int64
	identitySubject, identityHandle, email sql.NullString
	claimedAt, lastSeenAt                  sql.NullTime
}

// accountColumns lists, in dest() order, the account columns the lookups read,
// qualified by alias when one is given.
func accountColumns(alias string) string {
	cols := []string{"id", "key", "display_name", "token_hash", "identity_provider", "identity_subject",
		"identity_handle", "email", "claimed_at", "status", "last_seen_at"}
	prefix := ""
	if alias != "" {
		prefix = alias + "."
	}
	for i, c := range cols {
		cols[i] = prefix + "`" + c + "`"
	}
	return strings.Join(cols, ", ")
}

func (r *accountRow) dest() []any {
	return []any{&r.id, &r.key, &r.displayName, &r.storedHash, &r.identityProvider, &r.identitySubject,
		&r.identityHandle, &r.email, &r.claimedAt, &r.status, &r.lastSeenAt}
}

func (r *accountRow) entity() (account_entity.Account, error) {
	a := account_entity.Account{
		Key:              r.key,
		DisplayName:      r.displayName,
		TokenHash:        r.storedHash,
		IdentityProvider: enums.IdentityProvider(r.identityProvider),
		IdentitySubject:  nullString(r.identitySubject.String),
		IdentityHandle:   nullString(r.identityHandle.String),
		Email:            nullString(r.email.String),
		Status:           enums.RecordStatus(r.status),
	}
	if r.claimedAt.Valid {
		a.ClaimedAt = nullTime(r.claimedAt.Time)
	}
	if r.lastSeenAt.Valid {
		a.LastSeenAt = nullTime(r.lastSeenAt.Time)
	}
	id, err := uuid.FromString(r.id)
	if err != nil {
		return account_entity.Account{}, err
	}
	a.ID = id
	return a, nil
}

// ── the client_key override, carried by the connection ───────────────────────

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
// SINCE v4 THIS IS AN OVERRIDE PATH, NOT THE MECHANISM. An agent token already
// says which agent is calling, and for such a token a client_key that names a
// DIFFERENT agent is refused (RequireAgent). The header matters only for a
// legacy account token, which names a person who may run several agents.
//
// History, because it explains why the token had to change: the header was
// the fix for "an agent cannot learn its own client_key" — the installer chose
// it and threw it away, and a model asked for it guessed. Then the server log
// showed that Claude Code drops custom headers from a plugin's .mcp.json, so
// the header never arrived. Authorization is the one header every MCP client
// reliably forwards, which is why identity now rides on it.
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
