package mcp

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mklfarha/metiche/backend/enums"
)

// ─────────────────────────────────────────────
// Tool: get_team_state
// ─────────────────────────────────────────────

const (
	stateDefaultLimit = 20
	stateMaxLimit     = 50
)

type GetTeamStateParams struct {
	TeamSlug string `json:"team_slug,omitempty" jsonschema:"Which team's board to look at, by slug. Omit it if you are only on one team."`
	Scope    string `json:"scope,omitempty" jsonschema:"What to look at: 'sessions' (who is working, on what branch, with what status line), 'events' (the team's recent activity in order), or 'me' (your own sessions). Defaults to sessions."`
	Cursor   string `json:"cursor,omitempty" jsonschema:"Pass the next_cursor from the previous page to continue. Omit for the first page."`
	Limit    int    `json:"limit,omitempty" jsonschema:"Rows per page, 1-50. Defaults to 20."`
	Since    int64  `json:"since_sequence,omitempty" jsonschema:"For scope=events only: return events after this sequence. Use the sequence from your last response to catch up on exactly what you missed."`
}

// TeamStateResult is deliberately NOT the board.
//
// PLAN.md's rule — "never the board, never everybody's claims" — applies to
// the envelope that rides on every call. get_team_state is the explicit opt-in
// the other way, and it is still scoped, capped and paginated, and it still
// never returns claims or paths: "who else is in these files" is check_paths's
// job, answered against the paths the caller names rather than by handing over
// the whole map of who holds what.
type TeamStateResult struct {
	Envelope
	Scope      string           `json:"scope"`
	Sessions   []StateSession   `json:"sessions,omitempty"`
	Events     []StateEvent     `json:"events,omitempty"`
	NextCursor string           `json:"next_cursor,omitempty"`
	Truncated  bool             `json:"truncated,omitempty"`
	Counts     *StateTeamCounts `json:"counts,omitempty"`
}

type StateTeamCounts struct {
	LiveSessions int `json:"live_sessions"`
}

type StateSession struct {
	Key        string `json:"key"`
	Member     string `json:"member"`
	Agent      string `json:"agent"`
	Project    string `json:"project"`
	Branch     string `json:"branch,omitempty"`
	Goal       string `json:"goal,omitempty"`
	StatusLine string `json:"status_line,omitempty"`
	Status     string `json:"status"`
	LastSeen   string `json:"last_seen,omitempty"`
	Mine       bool   `json:"mine,omitempty"`
}

type StateEvent struct {
	Sequence   int64  `json:"sequence"`
	Kind       string `json:"kind"`
	SubjectKey string `json:"subject_key,omitempty"`
	Summary    string `json:"summary,omitempty"`
	Structural bool   `json:"structural,omitempty"`
	At         string `json:"at"`
}

// GetTeamState is the orientation call an agent makes once, at session start:
// who else is here and what have they been doing.
//
// Read-only, scoped, cursor-paginated and hard-capped, because the failure
// mode of a "show me everything" tool is not an error — it is a model that
// spends its context on other people's work and then skims the one line that
// mattered.
func (h *Handler) GetTeamState(ctx context.Context, _ *mcp.CallToolRequest, args GetTeamStateParams) (*mcp.CallToolResult, any, error) {
	// v3: one person can be on several teams with one token, so the board
	// this call reads has to be named rather than inferred from the
	// credential. RequireTeam also re-checks the membership on every call,
	// which is what makes a revoked member stop seeing the board immediately
	// rather than at their next join.
	who, err := h.RequireTeam(ctx, args.TeamSlug)
	if err != nil {
		return nil, nil, err
	}

	limit := args.Limit
	if limit <= 0 {
		limit = stateDefaultLimit
	}
	if limit > stateMaxLimit {
		limit = stateMaxLimit
	}

	scope := strings.ToLower(strings.TrimSpace(args.Scope))
	if scope == "" {
		scope = "sessions"
	}

	db := h.core.DB()
	var seq, rev int64
	if err := db.QueryRowContext(ctx,
		"SELECT `sequence`, `board_revision` FROM `team` WHERE `id` = ?", who.Team.ID.String()).
		Scan(&seq, &rev); err != nil {
		return nil, nil, retryable(err, "reading the team cursors")
	}

	out := TeamStateResult{
		Envelope: Envelope{OK: true, Key: who.Member.Key, Sequence: seq, Revision: rev},
		Scope:    scope,
	}

	switch scope {
	case "sessions", "me":
		mineOnly := scope == "me"
		sessions, next, err := h.listSessions(ctx, db, who.Team.ID, who.Member.ID, mineOnly, args.Cursor, limit)
		if err != nil {
			return nil, nil, err
		}
		out.Sessions = sessions
		out.NextCursor = next
		out.Truncated = next != ""
	case "events":
		events, next, err := h.listEvents(ctx, db, who.Team.ID, args.Cursor, args.Since, limit)
		if err != nil {
			return nil, nil, err
		}
		out.Events = events
		out.NextCursor = next
		out.Truncated = next != ""
	default:
		return nil, nil, fmt.Errorf("scope must be one of sessions, events, me (got %q)", args.Scope)
	}

	pending, err := h.pendingCounts(ctx, db, nil)
	if err != nil {
		return nil, nil, err
	}
	out.Envelope = out.Envelope.withPending(pending)
	return jsonValue(out)
}

// listSessions returns live and recently-stale sessions, keyset-paginated on
// (created_at, id).
//
// Keyset rather than OFFSET: a session starting mid-pagination would shift an
// offset window and silently skip a row, and "who is working right now" is
// exactly the list that changes while you read it.
// "mine" is the PERSON's, not the process's. v3 made an account a person
// across teams and a member their row on one team, so scope=me means every
// session of MY member row — including the ones my other agents are running,
// which is what a human asking "what am I doing" means.
func (h *Handler) listSessions(ctx context.Context, db *sql.DB, teamUUID, memberUUID interface {
	String() string
}, mineOnly bool, cursor string, limit int) ([]StateSession, string, error) {
	afterAt, afterID, err := decodeKeysetCursor(cursor)
	if err != nil {
		return nil, "", err
	}

	q := "SELECT s.`key`, m.`display_name`, a.`label`, p.`key`, s.`branch`, s.`goal`, s.`status_line`, " +
		"s.`status`, s.`last_heartbeat_at`, s.`member_uuid`, s.`created_at`, s.`id` " +
		"FROM `session` s " +
		"JOIN `member` m ON m.`id` = s.`member_uuid` " +
		"JOIN `agent` a ON a.`id` = s.`agent_uuid` " +
		"JOIN `project` p ON p.`id` = s.`project_uuid` " +
		"WHERE s.`team_uuid` = ? AND s.`status` IN (?, ?) "
	args := []any{teamUUID.String(), enums.SESSION_STATUS_LIVE, enums.SESSION_STATUS_STALE}
	if mineOnly {
		q += "AND s.`member_uuid` = ? "
		args = append(args, memberUUID.String())
	}
	if afterAt != nil {
		q += "AND (s.`created_at` > ? OR (s.`created_at` = ? AND s.`id` > ?)) "
		args = append(args, *afterAt, *afterAt, afterID)
	}
	// limit+1 so "is there another page" needs no second query.
	q += "ORDER BY s.`created_at` ASC, s.`id` ASC LIMIT ?"
	args = append(args, limit+1)

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, "", retryable(err, "listing sessions")
	}
	defer func() { _ = rows.Close() }()

	var (
		out      []StateSession
		lastAt   time.Time
		lastID   string
		overflow bool
	)
	for rows.Next() {
		var (
			key, member, label, project, memberID, id string
			branch, goal, statusLine                  sql.NullString
			status                                    int64
			lastSeen                                  sql.NullTime
			createdAt                                 time.Time
		)
		if err := rows.Scan(&key, &member, &label, &project, &branch, &goal, &statusLine,
			&status, &lastSeen, &memberID, &createdAt, &id); err != nil {
			return nil, "", err
		}
		if len(out) == limit {
			overflow = true
			break
		}
		s := StateSession{
			Key:        key,
			Member:     member,
			Agent:      label,
			Project:    project,
			Branch:     branch.String,
			Goal:       goal.String,
			StatusLine: statusLine.String,
			Status:     enums.SessionStatus(status).String(),
			Mine:       memberID == memberUUID.String(),
		}
		if lastSeen.Valid {
			s.LastSeen = lastSeen.Time.UTC().Format(time.RFC3339)
		}
		out = append(out, s)
		lastAt, lastID = createdAt, id
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}

	next := ""
	if overflow {
		next = encodeKeysetCursor(lastAt, lastID)
	}
	return out, next, nil
}

// listEvents walks the team's log forward from a cursor. The sequence IS the
// cursor — that is what it is for — so this needs no keyset encoding.
func (h *Handler) listEvents(ctx context.Context, db *sql.DB, teamUUID interface{ String() string },
	cursor string, since int64, limit int) ([]StateEvent, string, error) {

	after := since
	if cursor != "" {
		v, err := strconv.ParseInt(strings.TrimSpace(cursor), 10, 64)
		if err != nil {
			return nil, "", errors.New("cursor for scope=events must be a sequence number from a previous next_cursor")
		}
		after = v
	}

	rows, err := db.QueryContext(ctx,
		"SELECT `sequence`, `kind`, `subject_key`, `summary`, `structural`, `occurred_at` "+
			"FROM `team_event` WHERE `team_uuid` = ? AND `sequence` > ? ORDER BY `sequence` ASC LIMIT ?",
		teamUUID.String(), after, limit+1)
	if err != nil {
		return nil, "", retryable(err, "listing events")
	}
	defer func() { _ = rows.Close() }()

	var (
		out      []StateEvent
		lastSeq  int64
		overflow bool
	)
	for rows.Next() {
		var (
			seq        int64
			kind       int64
			subjectKey sql.NullString
			summary    sql.NullString
			structural bool
			at         time.Time
		)
		if err := rows.Scan(&seq, &kind, &subjectKey, &summary, &structural, &at); err != nil {
			return nil, "", err
		}
		if len(out) == limit {
			overflow = true
			break
		}
		out = append(out, StateEvent{
			Sequence:   seq,
			Kind:       enums.EventKind(kind).String(),
			SubjectKey: subjectKey.String,
			Summary:    summary.String,
			Structural: structural,
			At:         at.UTC().Format(time.RFC3339),
		})
		lastSeq = seq
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}

	next := ""
	if overflow {
		next = strconv.FormatInt(lastSeq, 10)
	}
	return out, next, nil
}

// encodeKeysetCursor packs (created_at, id) into one opaque token, so a client
// cannot take a dependency on the shape of our pagination.
func encodeKeysetCursor(at time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(at.UTC().Format(time.RFC3339Nano) + "|" + id))
}

func decodeKeysetCursor(cursor string) (*time.Time, string, error) {
	cursor = strings.TrimSpace(cursor)
	if cursor == "" {
		return nil, "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return nil, "", errors.New("cursor is not a cursor this server issued; omit it to start from the beginning")
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return nil, "", errors.New("cursor is not a cursor this server issued; omit it to start from the beginning")
	}
	at, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return nil, "", errors.New("cursor is not a cursor this server issued; omit it to start from the beginning")
	}
	return &at, parts[1], nil
}
