package mcp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gofrs/uuid"
	"go.uber.org/zap"

	accountmod "github.com/mklfarha/metiche/backend/core/module/account"
	account_types "github.com/mklfarha/metiche/backend/core/module/account/types"
	agentmod "github.com/mklfarha/metiche/backend/core/module/agent"
	agent_types "github.com/mklfarha/metiche/backend/core/module/agent/types"
	invitemod "github.com/mklfarha/metiche/backend/core/module/invite"
	invite_types "github.com/mklfarha/metiche/backend/core/module/invite/types"
	membermod "github.com/mklfarha/metiche/backend/core/module/member"
	member_types "github.com/mklfarha/metiche/backend/core/module/member/types"
	sessionmod "github.com/mklfarha/metiche/backend/core/module/session"
	session_types "github.com/mklfarha/metiche/backend/core/module/session/types"
	account_entity "github.com/mklfarha/metiche/backend/entity/account"
	agent_entity "github.com/mklfarha/metiche/backend/entity/agent"
	invite_entity "github.com/mklfarha/metiche/backend/entity/invite"
	member_entity "github.com/mklfarha/metiche/backend/entity/member"
	session_entity "github.com/mklfarha/metiche/backend/entity/session"
	team_entity "github.com/mklfarha/metiche/backend/entity/team"
	"github.com/mklfarha/metiche/backend/enums"
)

// metiche does its own auth. nuzur's auth integration is disabled for this
// project, and the MCP endpoint is public by design — this is a public
// repository and mcp.metiche.xyz answers anyone who asks. "No third-party
// credential required to run the server" is a constraint on DEPENDENCIES, not
// a licence to leave the door open: a team's board is not public.
//
// The scheme, end to end, on schema v3:
//
//   - a PERSON is an `account`. It is created anonymously on first contact —
//     no email, no signup, no OAuth — and may be claimed later;
//   - the bearer token is minted from crypto/rand, shown exactly once, and
//     only sha256(token) is stored, on account.token_hash. A dump of the
//     database yields nothing anyone can authenticate with;
//   - an `agent` is one client process belonging to that person, unique on
//     (account_uuid, client_key), so a restarted agent re-attaches to itself;
//   - a `member` is the JOIN of an account and a team, with revoked_at;
//   - an `invite` is the credential that admits an account to a team:
//     time-boxed, use-capped and revocable, replacing the single rotatable
//     team.join_code that v1 had.
//
// A token is never logged, never returned in an error, and never written to
// team_event.response_snapshot (see join.go). Neither is an invite code.
//
// ── MIGRATION: schema v3 moved identity out from under this file ────────────
//
// The v1 shape was: token hash on `agent`, an agent belongs to a team, a team
// carries one rotatable join_code. v3 replaced all three, and the consequence
// for this package is that resolveToken can no longer return "the agent, and
// therefore the team". It resolves to an ACCOUNT. The team is now a per-call
// SCOPE, taken from the tool's own arguments or from the session it names,
// plus a membership check.
//
// Every call site the v1 shape reached, and what it became:
//
//	auth.go      resolveToken          agent by token_hash  -> account by token_hash
//	auth.go      requireAgent          -> requireAccount (+ RequireTeam / RequireSession)
//	auth.go      WithAgent/AgentFrom…  -> WithAccount / AccountFromContext
//	handler.go   teamByJoinCode        -> redeemInvite (invite table)
//	handler.go   agentByClientKey      member_uuid          -> account_uuid
//	handler.go   sessionByKey(ag)      ag.TeamUUID          -> RequireSession
//	join.go      agent insert/update   team_uuid/member_uuid/token_hash
//	                                   -> account_uuid only
//	join.go      joinAs                team from the invite; account minted or
//	                                   carried on the request
//	createteam.go ensureTeam           team.JoinCode -> first invite row;
//	                                   + instance-default plan, + visibility
//	sessions.go  ag.TeamUUID/MemberUUID (start/end/heartbeat) -> Resolved
//	state.go     ag.TeamUUID/ag.Key/ag.ID -> Resolved (team scope argument)
//
// The transactional core in sequence.go, the envelope, the panic wrapper and
// the lock metric all take the team uuid as a parameter and are unaffected.

const (
	// tokenPrefix makes a leaked token greppable in a repo scan and tells a
	// human what they are looking at.
	tokenPrefix = "mtk_"
	// tokenEntropyBytes is 32 bytes = 256 bits from crypto/rand. Nothing here
	// is derived from time, a uuid, or a counter.
	tokenEntropyBytes = 32
)

// ErrUnauthenticated is what a tool returns when it needs a caller and the
// request carried no usable token.
var ErrUnauthenticated = errors.New(
	"this tool needs your metiche token: pass Authorization: Bearer <token> on the MCP connection. " +
		"Call join_team with your team's join code, or create_team, to get one")

// MintToken returns a fresh bearer token and the sha256 hex digest that is the
// only part of it the server keeps.
//
// The digest is 64 hex characters, which is exactly account.token_hash's
// width — not a coincidence, the column was sized for it.
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

// MintJoinCode returns a fresh invite code from crypto/rand.
//
// v3 renamed the thing it goes on — team.join_code became invite.code — but
// not the alphabet, and not the reason for it: unlike a bearer token, this is
// a SHARED secret that the person who made the team has to be able to read
// back and pass to their teammates, so it is stored in the clear because a
// hash cannot be read back. What makes that safe is that an invite is
// revocable, expirable and use-capped, which a column on the team never was.
// It is never logged.
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

type accountCtxKey struct{}

// WithAccount puts the authenticated account on the context. Exported so a
// test can drive a tool handler directly without an HTTP round trip.
func WithAccount(ctx context.Context, a account_entity.Account) context.Context {
	return context.WithValue(ctx, accountCtxKey{}, a)
}

// AccountFromContext returns the account the middleware resolved, if any.
func AccountFromContext(ctx context.Context) (account_entity.Account, bool) {
	a, ok := ctx.Value(accountCtxKey{}).(account_entity.Account)
	return a, ok
}

// AgentFromContext is the v1 spelling, kept because health.go asks it the
// only question that still has an answer: "did this request carry a usable
// credential?". A token resolves to an ACCOUNT in v3, so that is what comes
// back.
//
// Deprecated: use AccountFromContext.
func AgentFromContext(ctx context.Context) (account_entity.Account, bool) {
	return AccountFromContext(ctx)
}

// requireAccount is the guard every tool except create_team, join_team and
// health calls first.
//
// Three tools are deliberately open. create_team and join_team cannot require
// a token because minting one is part of what they do — they authenticate
// with an invite code, or with nothing at all on a first anonymous contact.
// health cannot require a token because its whole purpose is to answer "is
// the server up?" when something is wrong, and "your token was rejected" is
// not that answer.
//
// ── This is also the ACCOUNT-SCOPED seam, and list_teams is the one tool that
// stops here ────────────────────────────────────────────────────────────────
//
// Every other tool goes on from here into RequireSession, RequireTeam,
// RequireTeamByID or RequireAgent, and every one of those ends in a membership
// check against a team the caller NAMED. list_teams cannot: naming a team is
// the question it is asked. The installer is machine-global and mints ONE
// account which may be on several teams, so an agent opening a repo with no
// `.metiche` file has to find out what the choices are before it can pass a
// team_slug to anything at all.
//
// So there is deliberately no team-free resolver above this one — this is it,
// and it is unexported because nothing outside the package needs it. What a
// caller holding only this has is an account and NO CHECKED MEMBERSHIP, which
// bounds what it may read to what this account's own `member` rows say, and
// nothing else: no board, no claims, no other account's teams. Any second tool
// that stops here should be read with that sentence in hand. The four
// team-scoped resolvers below are unaffected and remain the only way to reach
// a team's data.
func (h *Handler) requireAccount(ctx context.Context) (account_entity.Account, error) {
	a, ok := AccountFromContext(ctx)
	if !ok {
		return account_entity.Account{}, ErrUnauthenticated
	}
	if a.Status != enums.RECORD_STATUS_ACTIVE {
		return account_entity.Account{}, errors.New(
			"this metiche account is no longer active; call create_team or join_team to start a new one")
	}
	return a, nil
}

// resolveToken turns a presented token into its ACCOUNT.
//
// The lookup is by hash — the hash is the stored value, so it is the only
// thing we can index on — and the match is then re-checked in constant time.
// The SQL equality is not the security boundary; VerifyToken is.
//
// Raw SQL because account.token_hash has no generated fetch-by-index: it is
// not a modelled index in nuzur, and adding one is a schema change that
// belongs in nuzur rather than in Go. The fix when the table stops being
// small is an index in the schema, not a cache here that would outlive a
// revocation.
func (h *Handler) resolveToken(ctx context.Context, token string) (account_entity.Account, error) {
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
	err := h.core.DB().QueryRowContext(ctx,
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

// mintAccount creates an anonymous person and hands back their one-time token.
//
// THIS IS THE WHOLE SIGNUP. There is no email, no password and no OAuth: a
// coding agent's first contact with metiche mints an identity, gets a token,
// and is a person from then on. account.identity_provider stays `none` until
// somebody claims the account, which is a later, optional step — the point of
// splitting `account` out of `agent` in v3 is that the same person can be on
// several teams with one credential, not that they must sign up.
//
// The token is returned once, here, and only its sha256 is stored.
func (h *Handler) mintAccount(ctx context.Context, displayName string) (account_entity.Account, string, error) {
	token, hash, err := MintToken()
	if err != nil {
		return account_entity.Account{}, "", err
	}
	id, err := uuid.NewV4()
	if err != nil {
		return account_entity.Account{}, "", err
	}
	name := truncate(displayName, 120)
	if name == "" {
		name = "anonymous"
	}
	now := time.Now().UTC()
	acct := account_entity.Account{
		ID:               id,
		Key:              accountKey(id),
		DisplayName:      name,
		TokenHash:        hash,
		IdentityProvider: enums.IDENTITY_PROVIDER_NONE,
		Status:           enums.RECORD_STATUS_ACTIVE,
		LastSeenAt:       nullTime(now),
	}
	if _, err := h.core.Account().Insert(ctx,
		account_types.UpsertRequest{Account: acct}, accountmod.WithSkipCache()); err != nil {
		return account_entity.Account{}, "", retryable(err, "creating your metiche account")
	}
	return acct, token, nil
}

// accountKey is the account's stable public handle: account.key is
// VARCHAR(32) and unique, and a uuid with its dashes removed is exactly 32
// characters. Derived rather than random so it cannot collide with the row's
// own primary key being unique.
func accountKey(id uuid.UUID) string {
	return strings.ReplaceAll(id.String(), "-", "")
}

// ─────────────────────────────────────────────
// Invites — the credential that admits an account to a team
// ─────────────────────────────────────────────

// ErrInviteNotUsable is deliberately one error for every reason an invite
// cannot be redeemed.
//
// An invite code is a credential, and a caller that can tell "no such code"
// from "expired code" from "used up" has an oracle for enumerating teams. The
// code itself never appears in the message, for the same reason a token never
// does.
var ErrInviteNotUsable = errors.New(
	"that join code is not valid: it may never have existed, or it may have expired, been revoked, or been used up. " +
		"Ask whoever set the team up for a fresh one")

// redeemInvite finds an invite by code and CONSUMES one use of it, atomically.
//
// The validation and the increment are one conditional UPDATE rather than a
// read followed by a write, because everything this checks — status, expiry,
// revocation, the use cap — is exactly the state a concurrent redemption
// changes. A SELECT-then-UPDATE would let two agents both pass the last-use
// check on a max_uses=1 invite. RowsAffected is the answer: 1 means this call
// owns a use, 0 means the invite was not redeemable and nothing moved.
//
// The `status` transition to exhausted is written in the same statement, and
// BEFORE the `uses` increment in the SET list, because MySQL evaluates
// assignments left to right and the CASE has to see the pre-increment value.
func (h *Handler) redeemInvite(ctx context.Context, code string) (invite_entity.Invite, team_entity.Team, error) {
	code = normalizeJoinCode(code)
	if code == "" {
		return invite_entity.Invite{}, team_entity.Team{}, errors.New("join_code is required")
	}

	res, err := h.core.Invite().FetchInviteByCode(ctx,
		invite_types.FetchInviteByCodeRequest{Code: code, Limit: 1}, invitemod.WithSkipCache())
	if err != nil {
		return invite_entity.Invite{}, team_entity.Team{}, retryable(err, "looking up the join code")
	}
	if len(res.Results) == 0 {
		return invite_entity.Invite{}, team_entity.Team{}, ErrInviteNotUsable
	}
	inv := res.Results[0]

	now := time.Now().UTC()
	out, err := h.core.DB().ExecContext(ctx,
		"UPDATE `invite` SET "+
			"`status` = CASE WHEN `max_uses` IS NOT NULL AND `uses` + 1 >= `max_uses` THEN ? ELSE `status` END, "+
			"`uses` = `uses` + 1, `last_used_at` = ?, `updated_at` = ? "+
			"WHERE `id` = ? AND `status` = ? AND `revoked_at` IS NULL "+
			"AND (`expires_at` IS NULL OR `expires_at` > ?) "+
			"AND (`max_uses` IS NULL OR `uses` < `max_uses`)",
		enums.INVITE_STATUS_EXHAUSTED, now, now,
		inv.ID.String(), enums.INVITE_STATUS_ACTIVE, now)
	if err != nil {
		return invite_entity.Invite{}, team_entity.Team{}, retryable(err, "redeeming the join code")
	}
	if n, _ := out.RowsAffected(); n == 0 {
		return invite_entity.Invite{}, team_entity.Team{}, ErrInviteNotUsable
	}

	team, err := h.teamByID(ctx, inv.TeamUUID)
	if err != nil || team.Status != enums.RECORD_STATUS_ACTIVE {
		// Same opaque answer: an invite pointing at a retired team must not
		// tell a stranger that the team was ever there.
		return invite_entity.Invite{}, team_entity.Team{}, ErrInviteNotUsable
	}
	inv.Uses++
	inv.LastUsedAt = nullTime(now)
	return inv, team, nil
}

// ─────────────────────────────────────────────
// Resolved — the per-call scope
// ─────────────────────────────────────────────

// Resolved is everything a work tool needs after auth: who is calling, in
// which team, on which session.
//
// v1 got all of this from the token, because the token was the agent and the
// agent carried its team. v3's token is a person who may be on several teams
// with several running agents, so the team is a per-call SCOPE and this
// struct is what a tool gets once that scope has been pinned down and the
// membership behind it checked.
//
// Agent and Session are zero on the team-scoped resolvers (RequireTeam);
// Session is zero on RequireAgent.
type Resolved struct {
	Account account_entity.Account
	Member  member_entity.Member
	Agent   agent_entity.Agent
	Session session_entity.Session
	Team    team_entity.Team
}

// RequireSession authenticates the caller, resolves the named session, and
// verifies the account is a live member of that session's team.
//
// The session key alone is the scope: a session knows its team, its agent and
// its member, so a tool that names one needs no team argument. Resolution is
// restricted to sessions started by one of THIS account's agents, which is
// both the ownership check v1 had on sessionByKey and the disambiguator for
// the case v3 introduced — session keys are short and per-team (S-7), and one
// person can now be on two teams that each have an S-7.
func (h *Handler) RequireSession(ctx context.Context, sessionKey string) (Resolved, error) {
	acct, err := h.requireAccount(ctx)
	if err != nil {
		return Resolved{}, err
	}
	key := strings.TrimSpace(sessionKey)
	if key == "" {
		return Resolved{}, errors.New("session_key is required")
	}

	rows, err := h.core.DB().QueryContext(ctx,
		"SELECT s.`id`, s.`team_uuid` FROM `session` s "+
			"JOIN `agent` a ON a.`id` = s.`agent_uuid` "+
			"WHERE s.`key` = ? AND a.`account_uuid` = ?",
		key, acct.ID.String())
	if err != nil {
		return Resolved{}, retryable(err, "looking up the session")
	}
	type ref struct{ session, team uuid.UUID }
	var mine []ref
	for rows.Next() {
		var sid, tid string
		if err := rows.Scan(&sid, &tid); err != nil {
			_ = rows.Close()
			return Resolved{}, err
		}
		s, err1 := uuid.FromString(sid)
		t, err2 := uuid.FromString(tid)
		if err1 != nil || err2 != nil {
			continue
		}
		mine = append(mine, ref{session: s, team: t})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return Resolved{}, retryable(err, "looking up the session")
	}
	_ = rows.Close()

	switch len(mine) {
	case 0:
		// Tell the two cases apart only as far as is safe: if the key names a
		// session on a team this account is a live member of, it belongs to a
		// teammate, and saying so is more useful than "no such session" and
		// leaks nothing the caller cannot already see on the board.
		var n int
		if err := h.core.DB().QueryRowContext(ctx,
			"SELECT COUNT(*) FROM `session` s JOIN `member` m ON m.`team_uuid` = s.`team_uuid` "+
				"WHERE s.`key` = ? AND m.`account_uuid` = ? AND m.`revoked_at` IS NULL AND m.`status` = ?",
			key, acct.ID.String(), enums.RECORD_STATUS_ACTIVE).Scan(&n); err == nil && n > 0 {
			return Resolved{}, fmt.Errorf("session %q belongs to another agent", key)
		}
		return Resolved{}, fmt.Errorf(
			"no session with key %q on any team you are a member of — call start_session first "+
				"(session keys are per team and case-sensitive)", key)
	case 1:
	default:
		return Resolved{}, fmt.Errorf(
			"session key %q names a session on more than one of your teams; end the ones you are not using", key)
	}

	res, err := h.RequireTeamByID(ctx, mine[0].team)
	if err != nil {
		return Resolved{}, err
	}

	sess, err := h.sessionByID(ctx, mine[0].session)
	if err != nil {
		return Resolved{}, err
	}
	ag, err := h.agentByID(ctx, sess.AgentUUID)
	if err != nil {
		return Resolved{}, err
	}
	if ag.Status != enums.AGENT_STATUS_ACTIVE {
		return Resolved{}, errors.New("this agent has been retired; call join_team again to reconnect it")
	}
	res.Session = sess
	res.Agent = ag
	return res, nil
}

// RequireTeam authenticates the caller and pins the call to one team, named
// by its slug.
//
// An empty ref is allowed and means "my only team": one person on one team is
// the overwhelmingly common case, and making every tool carry a team_slug for
// it would be ceremony. Two or more live memberships and the caller has to
// say which, by slug, because guessing would silently write to the wrong
// board.
func (h *Handler) RequireTeam(ctx context.Context, teamRef string) (Resolved, error) {
	acct, err := h.requireAccount(ctx)
	if err != nil {
		return Resolved{}, err
	}
	ref := strings.ToLower(strings.TrimSpace(teamRef))

	if ref == "" {
		members, err := h.liveMemberships(ctx, acct.ID)
		if err != nil {
			return Resolved{}, err
		}
		switch len(members) {
		case 0:
			return Resolved{}, errors.New(
				"you are not a member of any team — call join_team with a join code, or create_team")
		case 1:
			team, err := h.teamByID(ctx, members[0].TeamUUID)
			if err != nil {
				return Resolved{}, err
			}
			return Resolved{Account: acct, Member: members[0], Team: team}, nil
		default:
			slugs := make([]string, 0, len(members))
			for _, m := range members {
				if t, err := h.teamByID(ctx, m.TeamUUID); err == nil {
					slugs = append(slugs, t.Slug)
				}
			}
			return Resolved{}, fmt.Errorf(
				"you are on %d teams, so this call needs a team_slug: one of %s",
				len(members), strings.Join(slugs, ", "))
		}
	}

	team, err := h.teamBySlug(ctx, ref)
	if err != nil {
		return Resolved{}, err
	}
	member, err := h.liveMemberOf(ctx, acct.ID, team.ID)
	if err != nil {
		return Resolved{}, err
	}
	return Resolved{Account: acct, Member: member, Team: team}, nil
}

// RequireTeamByID is RequireTeam once the team is already known — from a
// session, or from an invite that was just redeemed. It is the membership
// check on its own, which is the half that must never be skipped.
func (h *Handler) RequireTeamByID(ctx context.Context, teamUUID uuid.UUID) (Resolved, error) {
	acct, err := h.requireAccount(ctx)
	if err != nil {
		return Resolved{}, err
	}
	team, err := h.teamByID(ctx, teamUUID)
	if err != nil {
		return Resolved{}, err
	}
	member, err := h.liveMemberOf(ctx, acct.ID, team.ID)
	if err != nil {
		return Resolved{}, err
	}
	return Resolved{Account: acct, Member: member, Team: team}, nil
}

// RequireAgent is RequireTeam plus "which of my running agents is calling".
//
// v1 never had to ask: the token WAS the agent. v3's token is the person, and
// a person can have a backend agent, a ui agent and a tests agent all alive at
// once, so the caller identifies itself with the same stable client_key it
// joined with. An empty client_key is allowed when the account has exactly one
// agent, for the same reason an empty team_slug is.
func (h *Handler) RequireAgent(ctx context.Context, teamRef, clientKey string) (Resolved, error) {
	res, err := h.RequireTeam(ctx, teamRef)
	if err != nil {
		return Resolved{}, err
	}

	if key := truncate(clientKey, 120); key != "" {
		ag, found, err := h.agentByClientKey(ctx, nil, res.Account.ID, key)
		if err != nil {
			return Resolved{}, err
		}
		if !found {
			return Resolved{}, errors.New(
				"no agent of yours has that client_key — call join_team with it first, " +
					"or omit client_key if you only run one agent")
		}
		if ag.Status != enums.AGENT_STATUS_ACTIVE {
			return Resolved{}, errors.New("this agent has been retired; call join_team again to reconnect it")
		}
		res.Agent = ag
		return res, nil
	}

	agents, err := h.activeAgents(ctx, res.Account.ID)
	if err != nil {
		return Resolved{}, err
	}
	switch len(agents) {
	case 0:
		return Resolved{}, errors.New(
			"you have no active agent — call join_team with a stable client_key first")
	case 1:
		res.Agent = agents[0]
		return res, nil
	default:
		return Resolved{}, fmt.Errorf(
			"you have %d active agents, so this call needs the client_key of the one making it", len(agents))
	}
}

// liveMemberships lists the teams this account actually belongs to right now.
// revoked_at is checked as well as status because revocation is the soft
// removal: the row stays for the history it is referenced by.
func (h *Handler) liveMemberships(ctx context.Context, accountUUID uuid.UUID) ([]member_entity.Member, error) {
	rows, err := h.core.DB().QueryContext(ctx,
		"SELECT `id` FROM `member` WHERE `account_uuid` = ? AND `revoked_at` IS NULL AND `status` = ? "+
			"ORDER BY `created_at` ASC, `id` ASC",
		accountUUID.String(), enums.RECORD_STATUS_ACTIVE)
	if err != nil {
		return nil, retryable(err, "listing your teams")
	}
	defer func() { _ = rows.Close() }()

	var ids []uuid.UUID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if u, err := uuid.FromString(id); err == nil {
			ids = append(ids, u)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, retryable(err, "listing your teams")
	}

	out := make([]member_entity.Member, 0, len(ids))
	for _, id := range ids {
		res, err := h.core.Member().FetchMemberByID(ctx,
			member_types.FetchMemberByIDRequest{ID: id}, membermod.WithSkipCache())
		if err != nil {
			return nil, retryable(err, "reading your membership")
		}
		if len(res.Results) > 0 {
			out = append(out, res.Results[0])
		}
	}
	return out, nil
}

// liveMemberOf is the membership check every team-scoped call makes: this
// account, this team, not revoked.
func (h *Handler) liveMemberOf(ctx context.Context, accountUUID, teamUUID uuid.UUID) (member_entity.Member, error) {
	m, found, err := h.memberByAccount(ctx, nil, accountUUID, teamUUID)
	if err != nil {
		return member_entity.Member{}, err
	}
	if !found {
		// Identical answer for "no such team" and "not your team", so this
		// cannot be used to discover which teams exist.
		return member_entity.Member{}, errors.New(
			"you are not a member of that team — join it with its join code first")
	}
	if m.RevokedAt.Valid || m.Status != enums.RECORD_STATUS_ACTIVE {
		return member_entity.Member{}, errors.New(
			"your membership of that team has been revoked; ask for a fresh join code")
	}
	return m, nil
}

// activeAgents lists this account's live agent processes across every team.
// An agent belongs to a person, not to a team, so this is not team-scoped.
func (h *Handler) activeAgents(ctx context.Context, accountUUID uuid.UUID) ([]agent_entity.Agent, error) {
	rows, err := h.core.DB().QueryContext(ctx,
		"SELECT `id` FROM `agent` WHERE `account_uuid` = ? AND `status` = ? ORDER BY `created_at` ASC, `id` ASC",
		accountUUID.String(), enums.AGENT_STATUS_ACTIVE)
	if err != nil {
		return nil, retryable(err, "listing your agents")
	}
	defer func() { _ = rows.Close() }()

	var ids []uuid.UUID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if u, err := uuid.FromString(id); err == nil {
			ids = append(ids, u)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, retryable(err, "listing your agents")
	}

	out := make([]agent_entity.Agent, 0, len(ids))
	for _, id := range ids {
		ag, err := h.agentByID(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, ag)
	}
	return out, nil
}

func (h *Handler) agentByID(ctx context.Context, id uuid.UUID) (agent_entity.Agent, error) {
	res, err := h.core.Agent().FetchAgentByID(ctx,
		agent_types.FetchAgentByIDRequest{ID: id}, agentmod.WithSkipCache())
	if err != nil {
		return agent_entity.Agent{}, retryable(err, "looking up the agent")
	}
	if len(res.Results) == 0 {
		return agent_entity.Agent{}, errors.New("that agent no longer exists; call join_team again")
	}
	return res.Results[0], nil
}

func (h *Handler) sessionByID(ctx context.Context, id uuid.UUID) (session_entity.Session, error) {
	res, err := h.core.Session().FetchSessionByID(ctx,
		session_types.FetchSessionByIDRequest{ID: id}, sessionmod.WithSkipCache())
	if err != nil {
		return session_entity.Session{}, retryable(err, "looking up the session")
	}
	if len(res.Results) == 0 {
		return session_entity.Session{}, errors.New("that session no longer exists")
	}
	return res.Results[0], nil
}

// ─────────────────────────────────────────────
// Middleware
// ─────────────────────────────────────────────

// authMiddleware resolves the bearer token on every MCP request and puts the
// ACCOUNT on the request context.
//
// It RESOLVES rather than rejects when no token is present, because the tools
// that hand out tokens live on this endpoint, and one tool exists to answer
// when everything else is failing. A request with no token reaches the server
// and can call create_team, join_team and health, and nothing else —
// requireAccount stops the rest. A request with a token that does not resolve
// is rejected here with 401, because a wrong token is a mistake worth failing
// fast and loudly, and letting it through as "anonymous" would turn a typo
// into a confusing "this tool needs a token" on a call that supplied one — or,
// worse, into a silently minted second identity.
//
// The token is never logged. The warning below records that a rejection
// happened and from where, and nothing about the credential.
func (h *Handler) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The caller's address rides on the context for the tools that answer
		// without a token: they are rate limited by it.
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
		acct, err := h.resolveToken(ctx, token)
		if err != nil {
			// The token itself is never logged — only that a rejection
			// happened, and from where.
			h.logger.Warn("rejected an MCP request with an unrecognised token",
				zap.String("remote_addr", r.RemoteAddr),
				zap.String("path", r.URL.Path))
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("WWW-Authenticate", `Bearer realm="metiche"`)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized","detail":"that metiche token is not valid; call join_team with your team's join code to get a new one"}`))
			return
		}
		next.ServeHTTP(w, r.WithContext(WithAccount(ctx, acct)))
	})
}
