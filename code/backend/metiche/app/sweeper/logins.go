package sweeper

import (
	"context"
	"time"
)

// ─────────────────────────────────────────────
// Login sweep (BOARD_LOGIN.md §3.3)
// ─────────────────────────────────────────────
//
// board_login_link and browser_session rows are credentials, not history.
// After a credential expires, the only safe thing to do with it is delete it.
// So this step is NOT gated on Options.RetentionEnabled. Retention rule 1 is a
// promise about a team's history, and an expired sign-in secret is not part of
// that history. It has its own switch, Options.LoginSweepEnabled, and that
// switch defaults to ON (decision 11, §9).
//
// The session check never relies on this step. Expiry is enforced inside the
// query that validates a session, just as claim TTL is enforced lazily. This
// step only removes rows that no query will ever accept again, once the
// explanatory tail has passed.
//
// It follows the same rules as retention.go rules 4 and 5:
//
//   - Bounded batches. Each DELETE carries LIMIT BatchSize (500 by default)
//     and is repeated until a batch comes back short, capped at MaxBatches
//     per table per pass. Anything left over is deleted on the next pass.
//   - Autocommit, and never the team row lock. Neither table is the team row,
//     no statement here runs inside a transaction, and nothing uses FOR UPDATE.

const (
	// LoginLinkTail is how long an expired sign-in link is kept, whether or
	// not it was consumed. The link lives for 10 minutes; a day of tail keeps
	// "was that link used?" answerable while someone is still asking.
	LoginLinkTail = 24 * time.Hour

	// BrowserSessionTail is how long a session is kept after its absolute
	// expiry or its revocation. It keeps "why was I signed out" answerable on
	// the account page for a week.
	BrowserSessionTail = 7 * 24 * time.Hour

	// BrowserSessionIdleTail applies to last_seen_at. It is the 7-day idle
	// limit plus the same 7-day tail: the session went idle-expired a week ago.
	BrowserSessionIdleTail = 7*24*time.Hour + BrowserSessionTail
)

// LoginsReport is what the login sweep did in one pass.
type LoginsReport struct {
	Enabled         bool `json:"enabled"`
	LinksDeleted    int  `json:"links_deleted"`
	SessionsDeleted int  `json:"sessions_deleted"`
	Batches         int  `json:"batches"`
}

// loginSweepOn reports whether the login sweep runs. A nil pointer means the
// key was never configured, and unconfigured means ON. This check does not
// rely on withDefaults having run.
func (o Options) loginSweepOn() bool {
	return o.LoginSweepEnabled == nil || *o.LoginSweepEnabled
}

// sweepLogins is step 5 of RunOnce. It runs once per pass across the whole
// instance rather than once per team, because neither table has a team
// column. Both are keyed by account.
func (s *Sweeper) sweepLogins(ctx context.Context, now time.Time, rep *Report) error {
	if !s.opts.loginSweepOn() {
		return nil
	}

	links, lbatches, lerr := s.deleteLoginRowsInBatches(ctx,
		"DELETE FROM `board_login_link` WHERE `expires_at` < ? LIMIT ?",
		now.Add(-LoginLinkTail))
	rep.Logins.LinksDeleted += links
	rep.Logins.Batches += lbatches
	if lerr != nil {
		rep.addErr("deleting expired sign-in links", lerr)
	}

	// A NULL revoked_at or last_seen_at never compares true, so those clauses
	// only match sessions that really were revoked or really went idle.
	sessions, sbatches, serr := s.deleteLoginRowsInBatches(ctx,
		"DELETE FROM `browser_session` WHERE `expires_at` < ? OR `revoked_at` < ? OR `last_seen_at` < ? LIMIT ?",
		now.Add(-BrowserSessionTail), now.Add(-BrowserSessionTail), now.Add(-BrowserSessionIdleTail))
	rep.Logins.SessionsDeleted += sessions
	rep.Logins.Batches += sbatches
	if serr != nil {
		return serr
	}
	return lerr
}

// deleteLoginRowsInBatches runs query with args plus a LIMIT of BatchSize.
// It repeats until a batch deletes fewer rows than the limit or the MaxBatches
// budget runs out. Each statement autocommits on its own.
func (s *Sweeper) deleteLoginRowsInBatches(ctx context.Context, query string, args ...any) (deleted, batches int, err error) {
	args = append(args, s.opts.BatchSize)
	for batches < s.opts.MaxBatches {
		if ctx.Err() != nil {
			return deleted, batches, ctx.Err()
		}
		n, err := s.execCount(ctx, query, args...)
		if err != nil {
			return deleted, batches, err
		}
		batches++
		deleted += n
		if n < s.opts.BatchSize {
			return deleted, batches, nil
		}
	}
	// Out of budget with rows possibly left. This is not an error: the next
	// pass continues where this one stopped.
	return deleted, batches, nil
}
