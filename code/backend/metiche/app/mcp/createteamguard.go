package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/gofrs/uuid"

	"github.com/mklfarha/metiche/backend/enums"
)

// ─────────────────────────────────────────────
// create_team: the duplicate-name guard (docs/CLI.md §4.8, decision §10 Q8)
// ─────────────────────────────────────────────
//
// create_team deduplicates by idempotency key and nothing else, so an agent
// that retries "create a team called X" with a fresh key — after a lost
// response, or in a new conversation — made a second X with a -xxxxxx slug.
// The guard refuses that unless the caller says allow_duplicate_name: true.
//
// Names are compared by slugKey(name, 40), the function that makes slugs, so
// "Hack Night", "hack-night" and " HACK  night " are one name exactly when they
// would have produced the same slug base.
//
// Only teams the caller is a LIVE member of, on ACTIVE teams, are compared and
// named. A same-named team the account is not on, has left, or that is
// inactive does not block, and never appears in the refusal: it leaks nothing.

// createTeamLockSeconds bounds how long a create_team waits for another
// create_team of the same account. Creating a team takes milliseconds; ten
// seconds is a stuck connection, and the caller is told to retry.
const createTeamLockSeconds = 10

// lockAccountCreates serializes create_team calls of one account, so two
// concurrent calls with different idempotency keys cannot both pass the
// duplicate check.
//
// DEVIATION from §4.8 step 4, which asks for `SELECT … FROM account … FOR
// UPDATE` in the transaction of ensureTeam's insert. That lock would be
// released when the team row commits, but the MEMBERSHIP the check reads is
// written afterwards by joinAs, in commit's own transaction on the team lock.
// A second call arriving in between would see no membership and pass. A MySQL
// named lock is held on one pooled connection across the whole create —
// check, team insert, membership — without touching teammod or commit, and it
// is released on every return path.
func (h *Handler) lockAccountCreates(ctx context.Context, accountID uuid.UUID) (func(), error) {
	conn, err := h.core.DB().Conn(ctx)
	if err != nil {
		return nil, retryable(err, "waiting for your other create_team calls")
	}
	name := "metiche:create_team:" + accountID.String()
	var got *int64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, ?)", name, createTeamLockSeconds).Scan(&got); err != nil {
		_ = conn.Close()
		return nil, retryable(err, "waiting for your other create_team calls")
	}
	if got == nil || *got != 1 {
		_ = conn.Close()
		return nil, fmt.Errorf(
			"unavailable: another create_team for your account is still running; retry this call with the same idempotency_key in a few seconds")
	}
	return func() {
		// A fresh context: the request's may already be cancelled, and a lock
		// left held would block this account's creates until the connection
		// is recycled.
		_, _ = conn.ExecContext(context.Background(), "SELECT RELEASE_LOCK(?)", name)
		_ = conn.Close()
	}, nil
}

// sameNamedTeam is one of the caller's teams that the requested name collides
// with.
type sameNamedTeam struct {
	Slug string
	Name string
}

// duplicateTeamName returns the already_exists refusal when this account is a
// live member of an active team whose name normalizes like teamName, and nil
// when it is not.
func (h *Handler) duplicateTeamName(ctx context.Context, accountID uuid.UUID, teamName string) (error, error) {
	matches, err := h.sameNamedTeams(ctx, accountID, teamName)
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		return nil, nil
	}
	return duplicateTeamRefusal(matches), nil
}

func (h *Handler) sameNamedTeams(ctx context.Context, accountID uuid.UUID, teamName string) ([]sameNamedTeam, error) {
	want := slugKey(teamName, 40)
	if want == "" {
		return nil, nil
	}
	rows, err := h.core.DB().QueryContext(ctx,
		"SELECT t.`slug`, t.`name` FROM `member` m JOIN `team` t ON t.`id` = m.`team_uuid` "+
			"WHERE m.`account_uuid` = ? AND m.`revoked_at` IS NULL AND m.`status` = ? AND t.`status` = ? "+
			"ORDER BY t.`created_at` ASC, t.`slug` ASC LIMIT ?",
		accountID.String(), enums.RECORD_STATUS_ACTIVE, enums.RECORD_STATUS_ACTIVE, listTeamsMaxTeams)
	if err != nil {
		return nil, retryable(err, "checking your teams for one with that name")
	}
	defer func() { _ = rows.Close() }()
	var out []sameNamedTeam
	for rows.Next() {
		var t sameNamedTeam
		if err := rows.Scan(&t.Slug, &t.Name); err != nil {
			return nil, err
		}
		if slugKey(t.Name, 40) == want {
			out = append(out, t)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, retryable(err, "checking your teams for one with that name")
	}
	return out, nil
}

// duplicateTeamRefusal is §4.8's text, listing every matching slug.
func duplicateTeamRefusal(matches []sameNamedTeam) error {
	slugs := make([]string, 0, len(matches))
	for _, m := range matches {
		slugs = append(slugs, m.Slug)
	}
	which := fmt.Sprintf("(slug: %s)", slugs[0])
	if len(slugs) > 1 {
		which = fmt.Sprintf("(slugs: %s)", strings.Join(slugs, ", "))
	}
	return fmt.Errorf(
		"already_exists: you are already on a team named %q %s. Use it: pass team_slug=%q on your calls; "+
			"to attach another client, call join_team with that team_slug. Only if the person explicitly wants a second team "+
			"with this name, call create_team again with allow_duplicate_name: true.",
		matches[0].Name, which, slugs[0])
}
